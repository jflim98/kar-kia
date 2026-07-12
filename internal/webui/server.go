// Package webui serves a password-gated admin dashboard: a global-settings + chat-list
// page, and a per-chat config/persona page. It binds to localhost by default. Config
// changes apply live (global via the config manager; per-chat via ChatManager.Reload).
// Secret values are write-only (never returned, only presence reported).
package webui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"assistant/internal/chat"
	"assistant/internal/config"
)

//go:embed assets/index.html
var assets embed.FS

const (
	cookieName    = "asst_admin"
	sessionMaxAge = 24 * time.Hour
)

// Server is the admin HTTP server.
type Server struct {
	cfg   *config.Manager
	chats *chat.Manager

	mu       sync.Mutex
	sessions map[string]time.Time // cookie token -> expiry
}

// New constructs the admin server.
func New(cfg *config.Manager, chats *chat.Manager) *Server {
	return &Server{cfg: cfg, chats: chats, sessions: map[string]time.Time{}}
}

// Serve runs the admin server until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/admin/login", s.handleLogin)
	mux.HandleFunc("/admin/logout", s.handleLogout)
	mux.HandleFunc("/admin/api/global", s.requireAuth(s.handleGlobal))
	mux.HandleFunc("/admin/api/mcp-servers", s.requireAuth(s.handleMCPServers))
	mux.HandleFunc("/admin/api/chats", s.requireAuth(s.handleChats))
	mux.HandleFunc("/admin/api/chats/{id}", s.requireAuth(s.handleChat))

	httpSrv := &http.Server{Addr: addr, Handler: mux}
	errc := make(chan error, 1)
	go func() {
		log.Printf("webui: serving on http://%s (login required)", addr)
		errc <- httpSrv.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutCtx)
	case err := <-errc:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, err := assets.ReadFile("assets/index.html")
	if err != nil {
		http.Error(w, "ui unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}

// --- auth ---

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	want := s.cfg.Secrets().WebUIPassword
	if want == "" {
		http.Error(w, "web UI password not set (set webui_password in secrets.yaml)", http.StatusForbidden)
		return
	}
	if subtle.ConstantTimeCompare([]byte(body.Password), []byte(want)) != 1 {
		time.Sleep(300 * time.Millisecond)
		http.Error(w, "invalid password", http.StatusUnauthorized)
		return
	}

	token := newToken()
	s.mu.Lock()
	s.sessions[token] = time.Now().Add(sessionMaxAge)
	s.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: int(sessionMaxAge.Seconds()),
	})
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authed(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) authed(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.sessions[c.Value]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.sessions, c.Value)
		return false
	}
	return true
}

func newToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
