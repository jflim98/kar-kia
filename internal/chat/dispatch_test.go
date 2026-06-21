package chat

import (
	"testing"

	"assistant/internal/config"
	"assistant/internal/telegram"
)

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
