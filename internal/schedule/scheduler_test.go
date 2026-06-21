package schedule

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestOneOffFiresAndIsRemoved(t *testing.T) {
	store, err := LoadStore(filepath.Join(t.TempDir(), "schedules.json"))
	if err != nil {
		t.Fatal(err)
	}
	fired := make(chan Schedule, 1)
	s := New(store, time.UTC, func(_ context.Context, sc Schedule) { fired <- sc })

	s.Start(t.Context())

	if _, err := s.Add(Schedule{ChatID: 42, Kind: KindOnce, FireAt: time.Now().Add(60 * time.Millisecond), Prompt: "ping"}); err != nil {
		t.Fatal(err)
	}

	select {
	case sc := <-fired:
		if sc.ChatID != 42 || sc.Prompt != "ping" {
			t.Fatalf("fired wrong schedule: %+v", sc)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("one-off did not fire")
	}

	// Give the post-fire cleanup a moment, then confirm it was removed.
	time.Sleep(50 * time.Millisecond)
	if got := s.List(0); len(got) != 0 {
		t.Fatalf("one-off not removed after firing: %+v", got)
	}
}

func TestCronAddListRemove(t *testing.T) {
	store, err := LoadStore(filepath.Join(t.TempDir(), "schedules.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(store, time.UTC, func(context.Context, Schedule) {})
	s.Start(t.Context())

	saved, err := s.Add(Schedule{ChatID: 1, Kind: KindCron, Spec: "*/5 * * * *", Prompt: "standup"})
	if err != nil {
		t.Fatalf("add cron: %v", err)
	}
	if got := s.List(1); len(got) != 1 {
		t.Fatalf("want 1 schedule, got %d", len(got))
	}
	if got := s.List(2); len(got) != 0 {
		t.Fatalf("chat filter failed: %d", len(got))
	}

	ok, err := s.Remove(saved.ID)
	if err != nil || !ok {
		t.Fatalf("remove: ok=%v err=%v", ok, err)
	}
	if got := s.List(0); len(got) != 0 {
		t.Fatalf("schedule not removed: %+v", got)
	}
}

func TestBadCronSpecErrors(t *testing.T) {
	store, _ := LoadStore(filepath.Join(t.TempDir(), "schedules.json"))
	s := New(store, time.UTC, func(context.Context, Schedule) {})
	s.Start(t.Context())

	if _, err := s.Add(Schedule{ChatID: 1, Kind: KindCron, Spec: "not a cron", Prompt: "x"}); err == nil {
		t.Fatal("expected error for invalid cron spec")
	}
}
