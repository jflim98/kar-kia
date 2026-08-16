package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"assistant/internal/chat"
	"assistant/internal/config"
)

func testServer(t *testing.T) (*Server, *config.Manager, *chat.Manager) {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(_ *config.Global, s *config.Secrets) { s.WebUIPassword = "secret" }); err != nil {
		t.Fatal(err)
	}
	chats, err := chat.New(dir, cfg, nil, func() {})
	if err != nil {
		t.Fatal(err)
	}
	if err := chats.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	return New(cfg, chats), cfg, chats
}

func login(t *testing.T, s *Server, pw string) *http.Cookie {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"password": pw})
	w := httptest.NewRecorder()
	s.handleLogin(w, httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader(string(body))))
	if w.Code != http.StatusOK {
		return nil
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieName {
			return c
		}
	}
	return nil
}

func TestAuthGating(t *testing.T) {
	s, _, _ := testServer(t)

	w := httptest.NewRecorder()
	s.requireAuth(s.handleGlobal)(w, httptest.NewRequest(http.MethodGet, "/admin/api/global", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without cookie, got %d", w.Code)
	}
	if c := login(t, s, "wrong"); c != nil {
		t.Fatal("wrong password should not yield a cookie")
	}
	c := login(t, s, "secret")
	if c == nil {
		t.Fatal("correct password did not yield a cookie")
	}
	r := httptest.NewRequest(http.MethodGet, "/admin/api/global", nil)
	r.AddCookie(c)
	w2 := httptest.NewRecorder()
	s.requireAuth(s.handleGlobal)(w2, r)
	if w2.Code != http.StatusOK {
		t.Fatalf("authorized request failed: %d", w2.Code)
	}
}

func TestGlobalSecretsWriteOnlyAndHotApply(t *testing.T) {
	s, cfg, _ := testServer(t)
	c := login(t, s, "secret")

	r := httptest.NewRequest(http.MethodGet, "/admin/api/global", nil)
	r.AddCookie(c)
	w := httptest.NewRecorder()
	s.requireAuth(s.handleGlobal)(w, r)
	if strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("GET leaked a secret:\n%s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"webui_password_set":true`) {
		t.Fatalf("expected webui_password_set true:\n%s", w.Body.String())
	}

	patch := `{"default_model":"opus","global_admin_user_ids":[42],"blacklisted_user_ids":[9],"bot_tokens":["t1","t2"]}`
	r2 := httptest.NewRequest(http.MethodPatch, "/admin/api/global", strings.NewReader(patch))
	r2.AddCookie(c)
	w2 := httptest.NewRecorder()
	s.requireAuth(s.handleGlobal)(w2, r2)
	if w2.Code != http.StatusOK {
		t.Fatalf("patch failed: %d %s", w2.Code, w2.Body.String())
	}
	if cfg.Get().DefaultModel != "opus" || !cfg.Get().IsGlobalAdmin(42) || !cfg.Get().IsBlacklisted(9) {
		t.Fatalf("global patch not applied: %+v", cfg.Get())
	}
	if len(cfg.Secrets().BotTokens) != 2 {
		t.Fatalf("bot tokens not applied: %v", cfg.Secrets().BotTokens)
	}
}

func TestMCPServersRoundTrip(t *testing.T) {
	s, cfg, _ := testServer(t)
	c := login(t, s, "secret")

	// PUT a registry.
	put := `[{"name":"weather","command":"npx","args":["-y","weather-mcp"],"env":{"K":"v"}}]`
	r := httptest.NewRequest(http.MethodPut, "/admin/api/mcp-servers", strings.NewReader(put))
	r.AddCookie(c)
	w := httptest.NewRecorder()
	s.requireAuth(s.handleMCPServers)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("put failed: %d %s", w.Code, w.Body.String())
	}
	if got := cfg.MCPServers(); len(got) != 1 || got[0].Name != "weather" || got[0].Env["K"] != "v" {
		t.Fatalf("registry not persisted: %v", got)
	}

	// A built-in name is rejected.
	bad := `[{"name":"memory","command":"x"}]`
	rb := httptest.NewRequest(http.MethodPut, "/admin/api/mcp-servers", strings.NewReader(bad))
	rb.AddCookie(c)
	wb := httptest.NewRecorder()
	s.requireAuth(s.handleMCPServers)(wb, rb)
	if wb.Code != http.StatusBadRequest {
		t.Fatalf("reserved built-in name should be rejected, got %d", wb.Code)
	}

	// GET returns the saved list, requires auth.
	wg := httptest.NewRecorder()
	rg := httptest.NewRequest(http.MethodGet, "/admin/api/mcp-servers", nil)
	rg.AddCookie(c)
	s.requireAuth(s.handleMCPServers)(wg, rg)
	if !strings.Contains(wg.Body.String(), "weather") {
		t.Fatalf("GET should list weather: %s", wg.Body.String())
	}
}

func TestChatConfigRoundTrip(t *testing.T) {
	s, _, chats := testServer(t)
	c := login(t, s, "secret")

	patch := `{"config":{"enabled":false,"model":"haiku","admin_user_ids":[5],"blacklisted_user_ids":[13],"group_response_mode":"all",` +
		`"all_allowed_tools":["memory","weather"],"admin_allowed_tools":["reminders"]},"persona":"You are Nova."}`
	r := httptest.NewRequest(http.MethodPatch, "/admin/api/chats/123", strings.NewReader(patch))
	r.SetPathValue("id", "123")
	r.AddCookie(c)
	w := httptest.NewRecorder()
	s.requireAuth(s.handleChat)(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("chat patch failed: %d %s", w.Code, w.Body.String())
	}

	// Read it back from disk via the manager.
	cfg, ok := chats.LoadChatConfig(123)
	if !ok || cfg.Model != "haiku" || cfg.GroupResponseMode != "all" || len(cfg.AdminUserIDs) != 1 {
		t.Fatalf("chat config not persisted: %+v ok=%v", cfg, ok)
	}
	if len(cfg.BlacklistedUserIDs) != 1 || cfg.BlacklistedUserIDs[0] != 13 {
		t.Fatalf("chat blacklist not persisted: %v", cfg.BlacklistedUserIDs)
	}
	if len(cfg.AllAllowedTools) != 2 || cfg.AllAllowedTools[1] != "weather" || len(cfg.AdminAllowedTools) != 1 {
		t.Fatalf("allow-lists not persisted: all=%v admin=%v", cfg.AllAllowedTools, cfg.AdminAllowedTools)
	}
	persona, _ := chats.Persona(123)
	if persona != "You are Nova." {
		t.Fatalf("persona not persisted: %q", persona)
	}

	// Unauthenticated chat access is rejected.
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodGet, "/admin/api/chats/123", nil)
	r2.SetPathValue("id", "123")
	s.requireAuth(s.handleChat)(w2, r2)
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("chat access must require auth, got %d", w2.Code)
	}
}
