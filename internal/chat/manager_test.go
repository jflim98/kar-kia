package chat

import (
	"os"
	"slices"
	"testing"

	"assistant/internal/config"
)

func TestToolAllowed(t *testing.T) {
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{global: cfg, chats: map[int64]*tenant{}, speakers: map[int64]int64{}}
	// Active chat 100: admin user 7; memory open to all, reminders admin-only.
	m.chats[100] = &tenant{id: 100, cfg: config.Chat{
		AdminUserIDs:      []int64{7},
		AllAllowedTools:   []string{config.ServerMemory},
		AdminAllowedTools: []string{config.ServerReminders},
	}}

	// The model-supplied user_id (3rd arg) is ignored; auth uses the recorded real sender.
	// Pass an admin id as the model arg throughout to prove it can't be used to escalate.
	cases := []struct {
		name        string
		chat        int64
		realSpeaker int64
		server      string
		want        bool
	}{
		{"memory open to all", 100, 999, config.ServerMemory, true},
		{"reminders denied non-admin", 100, 999, config.ServerReminders, false},
		{"reminders allowed admin", 100, 7, config.ServerReminders, true},
		{"unlisted server denied", 100, 7, "weather", false},
		{"inactive chat denied", 200, 7, config.ServerMemory, false},
	}
	for _, c := range cases {
		m.setSpeaker(c.chat, c.realSpeaker)
		// Always pass admin id 7 as the (untrusted) model user_id — must not affect the result.
		if got := m.ToolAllowed(c.chat, 7, c.server); got != c.want {
			t.Fatalf("%s: ToolAllowed=%v want %v (real speaker %d)", c.name, got, c.want, c.realSpeaker)
		}
	}
}

func TestLoadChatConfigBackfillsAllowListsForDisplay(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, cfg, nil, func() {})
	if err != nil {
		t.Fatal(err)
	}
	// A pre-feature chat.yaml: enabled, but the allow-list keys are entirely absent (written
	// raw, since the current binary would serialize them as empty []).
	if err := os.MkdirAll(m.chatDir(42), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.chatCfgPath(42), []byte("enabled: true\nmodel: sonnet\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, ok := m.LoadChatConfig(42)
	if !ok {
		t.Fatal("chat should load")
	}
	if !slices.Equal(got.AllAllowedTools, []string{config.ServerMemory, config.ServerModeration}) {
		t.Fatalf("all-list should show defaults for a legacy chat, got %v", got.AllAllowedTools)
	}
	if !slices.Equal(got.AdminAllowedTools, []string{config.ServerReminders}) {
		t.Fatalf("admin-list should show defaults for a legacy chat, got %v", got.AdminAllowedTools)
	}
}

func TestBlacklistProtectsAdminsAndPersists(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(g *config.Global, _ *config.Secrets) {
		g.GlobalAdminUserIDs = []int64{1}
	}); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, cfg, nil, func() {})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(m.chatDir(-42), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.chatCfgPath(-42), []byte("enabled: true\nadmin_user_ids: [7]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(t.Context()); err != nil {
		t.Fatal(err)
	}

	// Inactive chat, admins, global admins, the DM owner, and id 0 are all refused.
	refused := []struct {
		name           string
		chatID, target int64
	}{
		{"inactive chat", 999, 5},
		{"chat admin", -42, 7},
		{"global admin", -42, 1},
		{"zero id", -42, 0},
	}
	for _, c := range refused {
		if _, err := m.Blacklist(c.chatID, c.target, "x"); err == nil {
			t.Fatalf("%s: Blacklist(%d, %d) should be refused", c.name, c.chatID, c.target)
		}
	}

	// A regular user is added, persisted to disk, and live in the reloaded tenant.
	if _, err := m.Blacklist(-42, 99, "spam"); err != nil {
		t.Fatalf("Blacklist: %v", err)
	}
	onDisk, ok := m.LoadChatConfig(-42)
	if !ok || !slices.Contains(onDisk.BlacklistedUserIDs, 99) {
		t.Fatalf("blacklist not persisted: %v", onDisk.BlacklistedUserIDs)
	}
	tn, ok := m.get(-42)
	if !ok || !slices.Contains(tn.cfg.BlacklistedUserIDs, 99) {
		t.Fatalf("tenant not reloaded with blacklist: %+v", tn)
	}

	// Blacklisting again is a no-op, not a duplicate.
	if _, err := m.Blacklist(-42, 99, "spam again"); err != nil {
		t.Fatalf("repeat Blacklist: %v", err)
	}
	onDisk, _ = m.LoadChatConfig(-42)
	if n := len(onDisk.BlacklistedUserIDs); n != 1 {
		t.Fatalf("want 1 blacklist entry, got %d: %v", n, onDisk.BlacklistedUserIDs)
	}
}

func TestLoadChatConfigSeedsBotTokenFromRegistry(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, cfg, nil, func() {})
	if err != nil {
		t.Fatal(err)
	}
	// A chat the bot has seen via token "T1:abc" but that has no chat.yaml yet.
	m.registry.observe(555, "My Group", config.TypeGroup, "T1:abc")

	got, ok := m.LoadChatConfig(555)
	if ok {
		t.Fatal("chat should be unconfigured (no chat.yaml)")
	}
	if got.BotToken != "T1:abc" {
		t.Fatalf("bot_token should be seeded from the registry, got %q", got.BotToken)
	}
	if got.Name != "My Group" || got.Type != config.TypeGroup {
		t.Fatalf("name/type should be seeded too: %+v", got)
	}
}
