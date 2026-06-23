package proposals

import (
	"context"
	"strings"
	"testing"

	"assistant/internal/memory"
	"assistant/internal/telegram"
)

// fakeSender records the confirm it was asked to send.
type fakeSender struct {
	token       string
	lastApprove string
	lastReject  string
}

func (f *fakeSender) React(context.Context, string, int64, int, string)       {}
func (f *fakeSender) ClearReaction(context.Context, string, int64, int)       {}
func (f *fakeSender) Reply(context.Context, string, int64, int, string) error { return nil }
func (f *fakeSender) DownloadFile(context.Context, string, string) ([]byte, string, error) {
	return nil, "", nil
}
func (f *fakeSender) SendConfirm(_ context.Context, token string, _ int64, _, approve, reject string) (int, error) {
	f.token, f.lastApprove, f.lastReject = token, approve, reject
	return 99, nil
}

var _ telegram.Sender = (*fakeSender)(nil)

type fakeCommitter struct {
	scope   string
	userID  int64
	content string
	calls   int
}

func (f *fakeCommitter) AppendKnowledge(scope string, userID int64, content string) error {
	f.scope, f.userID, f.content, f.calls = scope, userID, content, f.calls+1
	return nil
}

func newMgr(s *fakeSender, c *fakeCommitter) *Manager {
	return New(s,
		func(chatID int64) (Committer, bool) { return c, chatID == 100 },
		func(chatID int64) (string, bool) { return "tok-" + itoa(chatID), chatID == 100 },
	)
}

func itoa(n int64) string {
	if n == 100 {
		return "100"
	}
	return "x"
}

func TestProposeUsesChatTokenAndCommitsOnApprove(t *testing.T) {
	s, c := &fakeSender{}, &fakeCommitter{}
	m := newMgr(s, c)

	if _, err := m.Propose(context.Background(), 100, 7, "user", "Al likes tea"); err != nil {
		t.Fatal(err)
	}
	if s.token != "tok-100" {
		t.Fatalf("confirm sent on wrong token: %q", s.token)
	}
	res := m.HandleCallback(context.Background(), s.lastApprove)
	if c.calls != 1 || c.scope != memory.ScopeUser || c.userID != 7 || c.content != "Al likes tea" {
		t.Fatalf("commit wrong: %+v", c)
	}
	if res.EditChatID != 100 || res.EditMsgID != 99 || !strings.Contains(res.EditText, "Saved") {
		t.Fatalf("edit result wrong: %+v", res)
	}
}

func TestSaveCommitsImmediatelyWithoutConfirm(t *testing.T) {
	s, c := &fakeSender{}, &fakeCommitter{}
	m := newMgr(s, c)

	if _, err := m.Save(context.Background(), 100, 7, "user", "Al likes tea"); err != nil {
		t.Fatal(err)
	}
	if c.calls != 1 || c.scope != memory.ScopeUser || c.userID != 7 || c.content != "Al likes tea" {
		t.Fatalf("save should commit immediately: %+v", c)
	}
	if s.token != "" {
		t.Fatalf("save must not send a confirmation, but SendConfirm ran on token %q", s.token)
	}
}

func TestSaveUnconfiguredChatErrors(t *testing.T) {
	s, c := &fakeSender{}, &fakeCommitter{}
	m := newMgr(s, c)
	if _, err := m.Save(context.Background(), 999, 1, "long_term", "x"); err == nil {
		t.Fatal("expected error saving in an unconfigured chat")
	}
	if c.calls != 0 {
		t.Fatalf("nothing should commit for an unconfigured chat: %+v", c)
	}
}

func TestRejectDoesNotCommit(t *testing.T) {
	s, c := &fakeSender{}, &fakeCommitter{}
	m := newMgr(s, c)
	if _, err := m.Propose(context.Background(), 100, 0, "long_term", "sky is blue"); err != nil {
		t.Fatal(err)
	}
	res := m.HandleCallback(context.Background(), s.lastReject)
	if c.calls != 0 || !strings.Contains(res.EditText, "Not saved") {
		t.Fatalf("reject wrong: calls=%d res=%+v", c.calls, res)
	}
}

func TestProposeUnconfiguredChatErrors(t *testing.T) {
	s, c := &fakeSender{}, &fakeCommitter{}
	m := newMgr(s, c)
	if _, err := m.Propose(context.Background(), 999, 1, "long_term", "x"); err == nil {
		t.Fatal("expected error proposing in an unconfigured chat")
	}
}

func TestExpiredTokenIgnored(t *testing.T) {
	m := newMgr(&fakeSender{}, &fakeCommitter{})
	res := m.HandleCallback(context.Background(), prefixApprove+"nope")
	if !strings.Contains(res.Answer, "expired") {
		t.Fatalf("want expired, got %+v", res)
	}
}
