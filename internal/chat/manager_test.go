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
	if !slices.Equal(got.AllAllowedTools, []string{config.ServerMemory}) {
		t.Fatalf("all-list should show defaults for a legacy chat, got %v", got.AllAllowedTools)
	}
	if !slices.Equal(got.AdminAllowedTools, []string{config.ServerReminders}) {
		t.Fatalf("admin-list should show defaults for a legacy chat, got %v", got.AdminAllowedTools)
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
