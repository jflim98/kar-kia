package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"assistant/internal/schedule"

	"github.com/mark3labs/mcp-go/mcp"
)

// fakeChats implements ChatScheduler + ToolGate for one active chat (id 100). The "reminders"
// server is admin-only (admin = user 7); the "memory" server is open to all.
type fakeChats struct {
	sched   *schedule.Scheduler
	added   []schedule.Schedule
	removed []string
}

func (f *fakeChats) Scheduler(chatID int64) (*schedule.Scheduler, bool) {
	return f.sched, chatID == 100
}
func (f *fakeChats) ToolAllowed(chatID, userID int64, server string) bool {
	if chatID != 100 {
		return false
	}
	switch server {
	case "memory":
		return true
	case "reminders":
		return userID == 7
	}
	return false
}

type fakeSink struct{ called bool }

func (f *fakeSink) Propose(context.Context, int64, int64, string, string) (string, error) {
	f.called = true
	return "asked", nil
}

type fakeRecall struct {
	gotQuery, gotDate string
	gotLimit          int
}

func (f *fakeRecall) Recall(chatID int64, query, date string, limit int) (string, error) {
	f.gotQuery, f.gotDate, f.gotLimit = query, date, limit
	return "snippet", nil
}

func call(name string, args map[string]any) mcp.CallToolRequest {
	var r mcp.CallToolRequest
	r.Params.Name = name
	r.Params.Arguments = args
	return r
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

func newServer(t *testing.T) (*Server, *fakeChats, *fakeSink, *fakeRecall) {
	t.Helper()
	store, err := schedule.LoadStore(t.TempDir() + "/s.json")
	if err != nil {
		t.Fatal(err)
	}
	sched := schedule.New(store, time.UTC, func(context.Context, schedule.Schedule) {})
	sched.Start(t.Context())
	fc := &fakeChats{sched: sched}
	fs := &fakeSink{}
	fr := &fakeRecall{}
	return New(fc, fs, fr, fc), fc, fs, fr
}

func TestScheduleRequiresAdmin(t *testing.T) {
	s, _, _, _ := newServer(t)
	// user 9 is not an admin of chat 100.
	res, _ := s.handleSchedule(context.Background(), call("schedule_reminder", map[string]any{
		"chat_id": float64(100), "user_id": float64(9), "prompt": "x", "delay_minutes": float64(5),
	}))
	if !res.IsError || !strings.Contains(resultText(t, res), "not allowed") {
		t.Fatalf("non-admin should be rejected: %+v", resultText(t, res))
	}
}

func TestScheduleAdminSucceeds(t *testing.T) {
	s, _, _, _ := newServer(t)
	res, err := s.handleSchedule(context.Background(), call("schedule_reminder", map[string]any{
		"chat_id": float64(100), "user_id": float64(7), "prompt": "hydrate", "delay_minutes": float64(10),
	}))
	if err != nil || res.IsError {
		t.Fatalf("admin schedule failed: %s", resultText(t, res))
	}
	if !strings.Contains(resultText(t, res), "Scheduled") {
		t.Fatalf("unexpected: %s", resultText(t, res))
	}
}

func TestScheduleInactiveChat(t *testing.T) {
	s, _, _, _ := newServer(t)
	res, _ := s.handleSchedule(context.Background(), call("schedule_reminder", map[string]any{
		"chat_id": float64(200), "user_id": float64(7), "prompt": "x", "delay_minutes": float64(5),
	}))
	if !res.IsError {
		t.Fatal("scheduling in an unknown chat should error (admin check fails first)")
	}
}

func TestProposeRoutes(t *testing.T) {
	s, _, sink, _ := newServer(t)
	res, _ := s.handlePropose(context.Background(), call("propose_memory", map[string]any{
		"chat_id": float64(100), "content": "remember this", "scope": "long_term",
	}))
	if res.IsError || !sink.called {
		t.Fatalf("propose should route to the sink: %s", resultText(t, res))
	}
}

func TestRecallRoutesAndRequiresQueryOrDate(t *testing.T) {
	s, _, _, rec := newServer(t)

	// Neither query nor date -> error, no routing.
	res, _ := s.handleRecall(context.Background(), call("recall_memory", map[string]any{
		"chat_id": float64(100),
	}))
	if !res.IsError {
		t.Fatalf("recall without query/date should error: %s", resultText(t, res))
	}

	// A query routes through with the limit.
	res, _ = s.handleRecall(context.Background(), call("recall_memory", map[string]any{
		"chat_id": float64(100), "query": "trip vacation", "limit": float64(3),
	}))
	if res.IsError || resultText(t, res) != "snippet" {
		t.Fatalf("recall should route to the recaller: %s", resultText(t, res))
	}
	if rec.gotQuery != "trip vacation" || rec.gotLimit != 3 {
		t.Fatalf("recaller got wrong args: %+v", rec)
	}
}
