package config

import (
	"path/filepath"
	"testing"
)

func TestLoadDefaultsWhenMissing(t *testing.T) {
	dir := t.TempDir()
	m, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg := m.Get()
	if cfg.DefaultModel != "sonnet" || cfg.DefaultConsolidationModel != "opus" {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
}

func TestUpdatePersistsAndNotifies(t *testing.T) {
	dir := t.TempDir()
	m, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var notified int
	m.OnChange(func() { notified++ })

	err = m.Update(func(c *Global, s *Secrets) {
		c.DefaultModel = "opus"
		c.GlobalAdminUserIDs = []int64{42}
		s.WebUIPassword = "hunter2"
		s.BotTokens = []string{"tok1"}
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if notified != 1 {
		t.Fatalf("want 1 notification, got %d", notified)
	}

	m2, err := Load(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := m2.Get().DefaultModel; got != "opus" {
		t.Fatalf("model not persisted: %q", got)
	}
	if !m2.Get().IsGlobalAdmin(42) {
		t.Fatalf("global admin not persisted")
	}
	if got := m2.Secrets().WebUIPassword; got != "hunter2" {
		t.Fatalf("secret not persisted: %q", got)
	}
	if len(m2.Secrets().BotTokens) != 1 || m2.Secrets().BotTokens[0] != "tok1" {
		t.Fatalf("bot tokens not persisted: %v", m2.Secrets().BotTokens)
	}

	assertPerm(t, filepath.Join(dir, secretsFile), 0o600)
	assertPerm(t, filepath.Join(dir, configFile), 0o644)
}

func TestEnvOverridesSecrets(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WEBUI_PASSWORD", "envpass")
	t.Setenv("BOT_TOKENS", "a, b ,a")
	m, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := m.Secrets().WebUIPassword; got != "envpass" {
		t.Fatalf("env override failed: %q", got)
	}
	if got := m.Secrets().BotTokens; len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("BOT_TOKENS merge failed: %v", got)
	}
}

func TestChatConfigResolveAndAdmins(t *testing.T) {
	g := DefaultGlobal()
	g.GlobalAdminUserIDs = []int64{1}

	c := NewChat(g, "Test Chat", TypeGroup, "tok")
	if c.Model != "sonnet" || c.ConsolidationModel != "opus" || c.TZ != "UTC" {
		t.Fatalf("NewChat defaults wrong: %+v", c)
	}
	if c.Enabled {
		t.Fatal("new chat must be disabled by default")
	}

	// Empty strings + zero retention resolve from global defaults.
	c.Model = ""
	c.MemoryRetentionDays = 0
	c.SessionTTLDays = 0
	r := c.Resolved(g)
	if r.Model != "sonnet" || r.MemoryRetentionDays != g.DefaultMemoryRetentionDays || r.SessionTTLDays != g.DefaultSessionTTLDays {
		t.Fatalf("Resolved fallback wrong: %+v", r)
	}

	// Admin predicate: chat-admin OR global-admin.
	c.AdminUserIDs = []int64{7}
	if !c.IsCronAdmin(g, 7) || !c.IsCronAdmin(g, 1) || c.IsCronAdmin(g, 9) {
		t.Fatalf("IsCronAdmin wrong")
	}
}

func TestChatLoadSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat.yaml")
	if _, ok, _ := LoadChat(path); ok {
		t.Fatal("missing chat.yaml should be ok=false")
	}
	c := NewChat(DefaultGlobal(), "G", TypeGroup, "tok")
	c.Enabled = true
	c.AdminUserIDs = []int64{5}
	if err := SaveChat(path, c); err != nil {
		t.Fatal(err)
	}
	got, ok, err := LoadChat(path)
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	if !got.Enabled || got.Name != "G" || len(got.AdminUserIDs) != 1 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}
