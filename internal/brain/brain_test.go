package brain

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"assistant/internal/config"
	"assistant/internal/session"
	"assistant/internal/telegram"
)

// fakeMem satisfies the Memory interface; pickSession only consults Now().
type fakeMem struct {
	now                  time.Time
	speakerLine, profile string
}

func (f fakeMem) SystemContext(context.Context, telegram.Message) (string, int, error) {
	return "", 0, nil
}
func (f fakeMem) SpeakerLine(m telegram.Message) string {
	if m.UserID == 0 {
		return ""
	}
	return f.speakerLine
}
func (f fakeMem) UserProfile(m telegram.Message) string {
	if m.UserID == 0 {
		return ""
	}
	return f.profile
}
func (f fakeMem) BuildPrompt(context.Context, telegram.Message) string { return "" }
func (f fakeMem) Record(context.Context, telegram.Message, bool)       {}
func (f fakeMem) LogReply(context.Context, telegram.Message, string)   {}
func (f fakeMem) RecentChatContext(int64, int) string                  { return "" }
func (f fakeMem) Now() time.Time                                       { return f.now }
func (f fakeMem) TZName() string                                       { return "UTC" }

func TestReplyToolsNeverIncludeFilesystemAndGateByList(t *testing.T) {
	b := &Brain{runner: &runner{mcpConfig: "x"}}
	// Default-style lists: memory for everyone, reminders for admins only.
	chat := config.Chat{
		AllAllowedTools:   []string{config.ServerMemory},
		AdminAllowedTools: []string{config.ServerReminders},
	}

	if got := b.builtinTools(); len(got) != 1 || got[0] != "WebSearch" {
		t.Fatalf("builtinTools must be [WebSearch], got %v", got)
	}

	// Non-admin: WebSearch + the all-users memory server, but NO filesystem and NO reminders.
	nonAdmin := b.allowedTools(chat, false)
	for _, banned := range []string{"Read", "Glob", "Grep", "Bash", "WebFetch", "Write", "Edit",
		"mcp__reminders"} {
		if slices.Contains(nonAdmin, banned) {
			t.Fatalf("non-admin tools must not include %q; got %v", banned, nonAdmin)
		}
	}
	if !slices.Contains(nonAdmin, "WebSearch") || !slices.Contains(nonAdmin, "mcp__memory") {
		t.Fatalf("expected WebSearch + mcp__memory; got %v", nonAdmin)
	}

	// Admin: also gets the admin-only reminders server.
	admin := b.allowedTools(chat, true)
	if !slices.Contains(admin, "mcp__reminders") || !slices.Contains(admin, "mcp__memory") {
		t.Fatalf("admin tools must include mcp__memory + mcp__reminders; got %v", admin)
	}

	// An external server placed in the all-users list is approved for everyone.
	chat2 := config.Chat{AllAllowedTools: []string{config.ServerMemory, "weather"}}
	if got := b.allowedTools(chat2, false); !slices.Contains(got, "mcp__weather") {
		t.Fatalf("expected mcp__weather for all users; got %v", got)
	}
}

// The argv must NOT carry --tools (it suppresses MCP tools on claude 2.1.181); the gate is
// --allowedTools + --permission-mode default, with --disallowedTools hard-blocking file/shell.
func TestCommonArgsGating(t *testing.T) {
	r := &runner{cliPath: "claude", mcpConfig: "/x/mcp.json"}
	args := r.commonArgs(runInput{
		Model:        "sonnet",
		AllowedTools: []string{"WebSearch", "mcp__memory", "mcp__smogon-vgc"},
	})

	if slices.Contains(args, "--tools") {
		t.Fatalf("--tools must not be passed (breaks MCP on 2.1.181): %v", args)
	}

	i := slices.Index(args, "--allowedTools")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("--allowedTools missing: %v", args)
	}
	for _, want := range []string{"WebSearch", "mcp__memory", "mcp__smogon-vgc"} {
		if !strings.Contains(args[i+1], want) {
			t.Fatalf("--allowedTools %q missing %q", args[i+1], want)
		}
	}

	j := slices.Index(args, "--disallowedTools")
	if j < 0 || j+1 >= len(args) {
		t.Fatalf("--disallowedTools missing: %v", args)
	}
	for _, want := range []string{"Bash", "Read", "Write", "Edit", "WebFetch"} {
		if !strings.Contains(args[j+1], want) {
			t.Fatalf("--disallowedTools %q missing %q", args[j+1], want)
		}
	}

	if k := slices.Index(args, "--permission-mode"); k < 0 || args[k+1] != "default" {
		t.Fatalf("expected --permission-mode default: %v", args)
	}
}

func TestComposeTurnOrderingAndToolsLine(t *testing.T) {
	b := &Brain{
		runner: &runner{mcpConfig: "x"},
		mem:    fakeMem{speakerLine: "[message from @al, user_id 7]", profile: "# About @al\n\nlikes tea"},
	}
	chat := config.Chat{
		AllAllowedTools:   []string{config.ServerMemory, "weather"}, // weather = external, no description
		AdminAllowedTools: []string{config.ServerReminders},
	}
	speaker := telegram.Message{UserID: 7}

	// Non-admin: order is speaker line -> tools line -> profile -> body.
	out := b.composeTurn(speaker, chat, false, "BODY")
	iLine := strings.Index(out, "message from @al")
	iTools := strings.Index(out, "You may use these tools")
	iProf := strings.Index(out, "About @al")
	iBody := strings.Index(out, "BODY")
	if !(iLine >= 0 && iLine < iTools && iTools < iProf && iProf < iBody) {
		t.Fatalf("sections out of order (line=%d tools=%d prof=%d body=%d):\n%s", iLine, iTools, iProf, iBody, out)
	}
	if !strings.Contains(out, "web search") || !strings.Contains(out, "memory (recall and save facts)") {
		t.Fatalf("tools line should list web search + memory:\n%s", out)
	}
	if !strings.Contains(out, "weather") { // external server listed by bare name
		t.Fatalf("external server should be listed by name:\n%s", out)
	}
	if strings.Contains(out, "reminders") {
		t.Fatalf("non-admin must not be told about reminders:\n%s", out)
	}
	if strings.Contains(out, "scheduling reminders") {
		t.Fatalf("non-admin must not get the reminder scheduling hint:\n%s", out)
	}

	// Admin: reminders appears with its description, plus the chat_id scheduling hint.
	admin := b.composeTurn(speaker, chat, true, "BODY")
	if !strings.Contains(admin, "reminders (schedule, list, and cancel reminders)") {
		t.Fatalf("admin should see reminders:\n%s", admin)
	}
	if !strings.Contains(admin, "When scheduling reminders, pass chat_id") {
		t.Fatalf("admin should get the reminder scheduling hint:\n%s", admin)
	}

	// No speaker (scheduled reminder): body only, no header.
	if got := b.composeTurn(telegram.Message{}, chat, false, "BODY"); got != "BODY" {
		t.Fatalf("no-speaker turn should be body only, got %q", got)
	}
}

func TestPickSessionRotation(t *testing.T) {
	dir := t.TempDir()
	store, err := session.Load(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	b := &Brain{sessions: store, mem: fakeMem{now: now}}
	cap := 50

	if _, resume := b.pickSession("1", 7, cap); resume {
		t.Fatal("first call should be a fresh session")
	}
	put := func(memVer, turns int, created time.Time) {
		_ = store.Put("1", session.Session{ID: "sid", CreatedAt: created, MemoryVersion: memVer, TurnCount: turns})
	}

	put(7, 10, now)
	if _, resume := b.pickSession("1", 7, cap); !resume {
		t.Fatal("should resume under the turn cap")
	}
	put(7, 50, now)
	if _, resume := b.pickSession("1", 7, cap); resume {
		t.Fatal("at the turn cap should rotate")
	}
	put(7, 9999, now)
	if _, resume := b.pickSession("1", 7, 0); !resume {
		t.Fatal("cap<=0 must never rotate on turns")
	}
	put(7, 1, now)
	if _, resume := b.pickSession("1", 8, cap); resume {
		t.Fatal("memory version change should rotate")
	}
	// The date no longer rotates the session — the daily rollover is driven by consolidation
	// (RolloverSession), not the clock. A previous-day session still resumes.
	put(7, 1, now.AddDate(0, 0, -1))
	if _, resume := b.pickSession("1", 7, cap); !resume {
		t.Fatal("a previous-day session should resume (date no longer rotates)")
	}
}

func TestRolloverSession(t *testing.T) {
	dir := t.TempDir()
	store, err := session.Load(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	b := &Brain{chatID: 1, sessions: store}
	if err := store.Put("1", session.NewSession(7)); err != nil {
		t.Fatal(err)
	}
	if err := b.RolloverSession(); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Get("1"); ok {
		t.Fatal("RolloverSession should drop the chat's session")
	}
	// Rolling over an already-empty store is a no-op, not an error.
	if err := b.RolloverSession(); err != nil {
		t.Fatalf("rollover on empty store: %v", err)
	}
}
