package memory

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"assistant/internal/telegram"
)

func readRaw(t *testing.T, dir string) []rawEntry {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(dir, "daily_memory", "_raw", "*.jsonl"))
	if len(matches) != 1 {
		t.Fatalf("want 1 raw file, got %v", matches)
	}
	f, err := os.Open(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []rawEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e rawEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("bad raw line: %v", err)
		}
		out = append(out, e)
	}
	return out
}

func TestRecordAppendsRawAndLogsReply(t *testing.T) {
	dir := t.TempDir()
	m := New(dir, filepath.Join(dir, "persona.md"), "UTC")
	ctx := context.Background()

	dm := telegram.Message{ChatID: 5, UserID: 5, UserName: "@al", MessageID: 1, Text: "hello"}
	m.Record(ctx, dm, true)
	m.LogReply(ctx, dm, "hi there")

	entries := readRaw(t, dir)
	if len(entries) != 2 {
		t.Fatalf("want 2 raw entries, got %d", len(entries))
	}
	if entries[0].Role != "user" || entries[0].Text != "hello" || !entries[0].Directed {
		t.Fatalf("user entry wrong: %+v", entries[0])
	}
	if entries[1].Role != "assistant" || entries[1].Text != "hi there" {
		t.Fatalf("assistant entry wrong: %+v", entries[1])
	}
}

func TestGroupCatchupBuffer(t *testing.T) {
	dir := t.TempDir()
	m := New(dir, filepath.Join(dir, "persona.md"), "UTC")
	ctx := context.Background()

	g := func(id int64, name, text string) telegram.Message {
		return telegram.Message{ChatID: -100, IsGroup: true, UserID: id, UserName: name, Text: text}
	}

	// Two non-directed group messages get buffered (not addressed to the bot).
	m.Record(ctx, g(1, "@al", "anyone seen the report?"), false)
	m.Record(ctx, g(2, "@bo", "not me"), false)

	// A directed message: BuildPrompt should prepend the buffered chatter.
	directed := g(1, "@al", "@assistant_bot summarize the above")
	prompt := m.BuildPrompt(ctx, directed)

	for _, want := range []string{"since you last replied", "@al: anyone seen the report?", "@bo: not me", "summarize the above"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}

	// Buffer must be drained: a second BuildPrompt has no carryover.
	prompt2 := m.BuildPrompt(ctx, g(1, "@al", "next"))
	if strings.Contains(prompt2, "since you last replied") {
		t.Fatalf("catch-up buffer not drained:\n%s", prompt2)
	}
}

func TestBuildPromptIncludesReplyQuote(t *testing.T) {
	dir := t.TempDir()
	m := New(dir, filepath.Join(dir, "persona.md"), "UTC")
	ctx := context.Background()

	// @mention of the bot in a message that replies to @bob's earlier message.
	msg := telegram.Message{
		ChatID: -100, IsGroup: true, UserID: 1, UserName: "@al",
		Text:        "@assistant_bot summarize this",
		ReplyToUser: "@bob", ReplyToText: "the deploy is failing on the VM",
	}
	got := m.BuildPrompt(ctx, msg)
	for _, want := range []string{"in reply to @bob", "the deploy is failing on the VM", "summarize this"} {
		if !strings.Contains(got, want) {
			t.Fatalf("BuildPrompt missing %q:\n%s", want, got)
		}
	}

	// No reply => no quote prefix.
	plain := m.BuildPrompt(ctx, telegram.Message{ChatID: -100, IsGroup: true, UserID: 1, Text: "hi"})
	if strings.Contains(plain, "in reply to") {
		t.Fatalf("non-reply message should have no quote:\n%s", plain)
	}
}

func TestRecentChatContext(t *testing.T) {
	dir := t.TempDir()
	m := New(dir, filepath.Join(dir, "persona.md"), "UTC")
	ctx := context.Background()

	dm := func(text string) telegram.Message {
		return telegram.Message{ChatID: 5, UserID: 5, UserName: "@al", Text: text}
	}
	other := telegram.Message{ChatID: 9, UserID: 9, UserName: "@bo", Text: "different chat"}

	m.Record(ctx, dm("first"), true)
	m.LogReply(ctx, dm(""), "first reply")
	m.Record(ctx, other, true) // different chat — must be excluded
	m.Record(ctx, dm("second, the current turn"), true)

	got := m.RecentChatContext(5, 10)
	if strings.Contains(got, "different chat") {
		t.Fatalf("leaked another chat's context:\n%s", got)
	}
	// The trailing current user turn is dropped; earlier turns are kept.
	if strings.Contains(got, "second, the current turn") {
		t.Fatalf("current turn should be dropped:\n%s", got)
	}
	for _, want := range []string{"@al: first", "Assistant: first reply"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}

	// No prior context (only the current turn) => empty.
	dir2 := t.TempDir()
	m2 := New(dir2, filepath.Join(dir2, "persona.md"), "UTC")
	m2.Record(ctx, telegram.Message{ChatID: 1, UserID: 1, Text: "hi"}, true)
	if got := m2.RecentChatContext(1, 10); got != "" {
		t.Fatalf("want empty carry-over for a brand-new chat, got %q", got)
	}
}

func TestSystemContextIncludesPersona(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "persona.md"), []byte("I am Nova, a terse assistant."), 0o644); err != nil {
		t.Fatal(err)
	}
	m := New(dir, filepath.Join(dir, "persona.md"), "UTC")
	sys, ver, err := m.SystemContext(context.Background(), telegram.Message{ChatID: 1, UserID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sys, "Nova") {
		t.Fatalf("system context missing persona:\n%s", sys)
	}
	if ver == 0 {
		t.Fatal("expected non-zero memory version")
	}
}

func TestPruneRawLogs(t *testing.T) {
	dir := t.TempDir()
	m := New(dir, filepath.Join(dir, "persona.md"), "UTC")
	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)

	raw := func(day string) string { return filepath.Join("daily_memory", "_raw", day+".jsonl") }
	today := "23-06-26"
	recent := "18-06-26" // 5 days ago, within the 14-day window
	old := "03-06-26"    // 20 days ago, beyond the window
	for _, day := range []string{today, recent, old} {
		writeMem(t, dir, raw(day), "{}\n")
	}

	n, err := m.PruneRawLogs(14, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 raw log pruned, got %d", n)
	}
	if _, err := os.Stat(filepath.Join(dir, raw(old))); !os.IsNotExist(err) {
		t.Fatal("old raw log should be deleted")
	}
	for _, day := range []string{today, recent} {
		if _, err := os.Stat(filepath.Join(dir, raw(day))); err != nil {
			t.Fatalf("%s raw log should be kept: %v", day, err)
		}
	}
}

func TestVersionRotation(t *testing.T) {
	dir := t.TempDir()
	personaPath := filepath.Join(dir, "persona.md")
	m := New(dir, personaPath, "UTC")
	ver := func() int {
		_, v, err := m.SystemContext(context.Background(), telegram.Message{ChatID: 1, UserID: 1})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}

	writeMem(t, dir, "persona.md", "I am Nova.")
	base := ver()

	// A no-op rewrite with identical content must NOT rotate (the reported config-save bug:
	// the web UI re-saves the same persona, which used to bump mtime and drop the session).
	writeMem(t, dir, "persona.md", "I am Nova.")
	if got := ver(); got != base {
		t.Fatalf("identical persona rewrite must not change version: base=%d got=%d", base, got)
	}

	// Appending to long-term memory must NOT rotate: the fact is already in the live
	// conversation; the next session picks up long_term_memory.md naturally.
	writeMem(t, dir, "long_term_memory.md", "- (2026-06-23) The user prefers terse replies.")
	if got := ver(); got != base {
		t.Fatalf("long-term memory append must not change version: base=%d got=%d", base, got)
	}

	// A genuine persona edit MUST rotate so the new behavior takes effect.
	writeMem(t, dir, "persona.md", "I am Nova, a terse assistant.")
	if got := ver(); got == base {
		t.Fatal("persona content change must change version")
	}
}

// The cache invariant: the static system prompt (and its version) must be identical across
// different speakers in the same chat, and must not carry per-speaker data. That volatile
// context belongs to SpeakerContext (the user turn) instead.
func TestSystemContextStableAcrossSpeakers(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "persona.md"), []byte("persona"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "users"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "users", "7.md"), []byte("likes tea"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := New(dir, filepath.Join(dir, "persona.md"), "UTC")
	ctx := context.Background()

	a := telegram.Message{ChatID: -100, IsGroup: true, UserID: 7, UserName: "@al"}
	b := telegram.Message{ChatID: -100, IsGroup: true, UserID: 8, UserName: "@bo"}

	sysA, verA, _ := m.SystemContext(ctx, a)
	sysB, verB, _ := m.SystemContext(ctx, b)
	if sysA != sysB || verA != verB {
		t.Fatalf("system context/version must not depend on the speaker:\nA=%q v=%d\nB=%q v=%d", sysA, verA, sysB, verB)
	}
	for _, leak := range []string{"user_id 7", "user_id 8", "About @al", "likes tea"} {
		if strings.Contains(sysA, leak) {
			t.Fatalf("per-speaker data leaked into the cached system prompt: %q", leak)
		}
	}
	// Reminders are admin-only: the static, speaker-independent prompt must not mention them
	// (that guidance rides in the admin-gated user turn instead).
	if strings.Contains(strings.ToLower(sysA), "reminder") {
		t.Fatalf("reminder guidance must not be in the static system prompt:\n%s", sysA)
	}

	// The per-speaker pieces carry identity (line) and profile, and are empty for non-user turns.
	if line := m.SpeakerLine(a); !strings.Contains(line, "@al") || !strings.Contains(line, "user_id 7") {
		t.Fatalf("SpeakerLine missing identity: %q", line)
	}
	if prof := m.UserProfile(a); !strings.Contains(prof, "likes tea") || !strings.Contains(prof, "About @al") {
		t.Fatalf("UserProfile missing profile: %q", prof)
	}
	none := telegram.Message{ChatID: -100}
	if m.SpeakerLine(none) != "" || m.UserProfile(none) != "" {
		t.Fatal("non-user turn should have empty SpeakerLine and UserProfile")
	}
}
