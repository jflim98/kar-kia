package chat

import (
	"context"
	"testing"

	"assistant/internal/config"
	"assistant/internal/telegram"
)

// recordingSender captures Reply calls; all other Sender methods are no-ops.
type recordingSender struct{ replies []string }

func (r *recordingSender) React(context.Context, string, int64, int, string) {}
func (r *recordingSender) ClearReaction(context.Context, string, int64, int) {}
func (r *recordingSender) Reply(_ context.Context, _ string, _ int64, _ int, text string) error {
	r.replies = append(r.replies, text)
	return nil
}
func (r *recordingSender) SendConfirm(context.Context, string, int64, string, string, string) (int, error) {
	return 0, nil
}
func (r *recordingSender) DownloadFile(context.Context, string, string) ([]byte, string, error) {
	return nil, "", nil
}

// TestOnMessageDropsBlacklistedSenders: blacklisted users are dropped before ANY handling —
// no reply, no recording, nothing sent to claude. The configured-chat tenant here has a nil
// brain, so reaching Record/Reply would panic: not panicking proves the early drop.
func TestOnMessageDropsBlacklistedSenders(t *testing.T) {
	dir := t.TempDir()
	global, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := global.Update(func(g *config.Global, _ *config.Secrets) {
		g.BlacklistedUserIDs = []int64{666}
	}); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, global, nil, func() {})
	if err != nil {
		t.Fatal(err)
	}
	m.chats[100] = &tenant{id: 100, cfg: config.Chat{Enabled: true, BlacklistedUserIDs: []int64{777}}}
	s := &recordingSender{}

	// Globally blacklisted sender in an UNconfigured DM: not even the canned notice.
	m.OnMessage(context.Background(), s, "tok", telegram.Message{ChatID: 555, UserID: 666, Text: "hi", MessageID: 1})
	if len(s.replies) != 0 {
		t.Fatalf("globally blacklisted sender must get no reply, got %v", s.replies)
	}

	// Globally blacklisted sender in the configured chat: dropped (nil brain would panic otherwise).
	m.OnMessage(context.Background(), s, "tok", telegram.Message{ChatID: 100, UserID: 666, Text: "hi", MessageID: 2})

	// Locally blacklisted sender in the configured chat: likewise dropped.
	m.OnMessage(context.Background(), s, "tok", telegram.Message{ChatID: 100, UserID: 777, Text: "hi", MessageID: 3})
	if len(s.replies) != 0 {
		t.Fatalf("blacklisted senders must get no reply, got %v", s.replies)
	}

	// Control: a non-blacklisted sender in the unconfigured chat still gets the canned notice.
	m.OnMessage(context.Background(), s, "tok", telegram.Message{ChatID: 555, UserID: 5, Text: "hi", MessageID: 4})
	if len(s.replies) != 1 {
		t.Fatalf("non-blacklisted sender should get the unconfigured notice, got %v", s.replies)
	}
}

func TestDirectedFor(t *testing.T) {
	dm := telegram.Message{IsGroup: false}
	if !directedFor(config.Chat{}, dm) {
		t.Fatal("DMs are always directed")
	}

	grp := func(mention, reply bool) telegram.Message {
		return telegram.Message{IsGroup: true, MentionsBot: mention, RepliesToBot: reply}
	}
	cases := []struct {
		mode           string
		mention, reply bool
		want           bool
	}{
		{config.GroupModeMention, false, false, false},
		{config.GroupModeMention, true, false, true},
		{config.GroupModeMention, false, true, true},
		{config.GroupModeReply, true, false, false},
		{config.GroupModeReply, false, true, true},
		{config.GroupModeAll, false, false, true},
	}
	for _, c := range cases {
		cfg := config.Chat{GroupResponseMode: c.mode}
		if got := directedFor(cfg, grp(c.mention, c.reply)); got != c.want {
			t.Fatalf("mode=%s mention=%v reply=%v: got %v want %v", c.mode, c.mention, c.reply, got, c.want)
		}
	}
}

func TestShouldHandle(t *testing.T) {
	group := telegram.Message{IsGroup: true}
	// Non-directed group chatter: ignored by default, kept when recording is on.
	if shouldHandle(config.Chat{}, group, false) {
		t.Fatal("non-directed group chatter must be ignored by default")
	}
	if !shouldHandle(config.Chat{RecordGroupChatter: true}, group, false) {
		t.Fatal("record_group_chatter=true should keep chatter")
	}
	// Directed and DMs always handled.
	if !shouldHandle(config.Chat{}, group, true) {
		t.Fatal("directed group message must be handled")
	}
	if !shouldHandle(config.Chat{}, telegram.Message{IsGroup: false}, false) {
		t.Fatal("DMs must always be handled")
	}
}

func TestDirectedRaw(t *testing.T) {
	if !directedRaw(telegram.Message{IsGroup: false}) {
		t.Fatal("unconfigured DM should get the canned reply")
	}
	if !directedRaw(telegram.Message{IsGroup: true, MentionsBot: true}) {
		t.Fatal("unconfigured group @mention should get the canned reply")
	}
	if directedRaw(telegram.Message{IsGroup: true}) {
		t.Fatal("unconfigured group chatter should be ignored")
	}
}
