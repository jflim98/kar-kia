// Package chat is the multi-tenant orchestrator: each Telegram chat (group/DM) is an
// isolated tenant with its own persona, memory, sessions, schedules, and config. The
// Manager discovers chats, builds/reloads tenants, and implements telegram.Handler to
// route inbound messages. Nothing crosses chat boundaries.
package chat

import (
	"context"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"assistant/internal/brain"
	"assistant/internal/config"
	"assistant/internal/consolidate"
	"assistant/internal/memory"
	"assistant/internal/schedule"
	"assistant/internal/session"
	"assistant/internal/telegram"
)

// tenant is one chat's isolated set of components.
type tenant struct {
	id     int64
	cfg    config.Chat
	mem    *memory.Manager
	sess   *session.Store
	brain  *brain.Brain
	sched  *schedule.Scheduler
	cancel context.CancelFunc // stops this tenant's scheduler
}

// Manager owns the registry + the loaded tenants.
type Manager struct {
	dataDir  string
	global   *config.Manager
	limiter  brain.Limiter
	registry *registry

	mu        sync.Mutex
	chats     map[int64]*tenant
	speakers  map[int64]int64 // chatID -> real sender of the in-flight invocation (for tool auth)
	ctx       context.Context
	sender    telegram.Sender
	onTok     func() // wake the gateway when the per-chat token set changes
	proposals CallbackRouter
}

// New builds a Manager. onTokensChanged is called when a chat's bot token changes (so
// the gateway can reconcile connections). sender is injected later via SetSender.
func New(dataDir string, global *config.Manager, limiter brain.Limiter, onTokensChanged func()) (*Manager, error) {
	reg, err := loadRegistry(filepath.Join(dataDir, "registry.json"))
	if err != nil {
		return nil, err
	}
	return &Manager{
		dataDir:  dataDir,
		global:   global,
		limiter:  limiter,
		registry: reg,
		chats:    map[int64]*tenant{},
		speakers: map[int64]int64{},
		onTok:    onTokensChanged,
	}, nil
}

// setSpeaker records the real sender for a chat's in-flight invocation (called by the brain
// under its per-chat lock). speakerFor reads it back during tool authorization.
func (m *Manager) setSpeaker(chatID, userID int64) {
	m.mu.Lock()
	m.speakers[chatID] = userID
	m.mu.Unlock()
}

func (m *Manager) speakerFor(chatID int64) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.speakers[chatID]
}

// SetSender injects the Telegram sender (the gateway) used for replies/reminders/confirms.
func (m *Manager) SetSender(s telegram.Sender) { m.sender = s }

func (m *Manager) chatsDir() string { return filepath.Join(m.dataDir, "chats") }
func (m *Manager) chatDir(id int64) string {
	return filepath.Join(m.chatsDir(), strconv.FormatInt(id, 10))
}
func (m *Manager) chatCfgPath(id int64) string { return filepath.Join(m.chatDir(id), "chat.yaml") }
func (m *Manager) personaPath(id int64) string { return filepath.Join(m.chatDir(id), "persona.md") }

// Start loads enabled tenants and starts their schedulers. ctx governs their lifetime.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()

	entries, _ := os.ReadDir(m.chatsDir())
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id, err := strconv.ParseInt(e.Name(), 10, 64)
		if err != nil {
			continue
		}
		cfg, ok, err := config.LoadChat(m.chatCfgPath(id))
		if err != nil || !ok || !cfg.Enabled {
			continue
		}
		if err := m.activate(id, cfg); err != nil {
			log.Printf("chat %d: activate failed: %v", id, err)
		}
	}
	return nil
}

// consolidationSpec returns a ~03:00 cron spec with a per-chat minute offset (0–10) derived
// from the chat ID, so chats consolidate at slightly staggered times instead of all colliding
// at once. Deterministic (FNV hash of the decimal ID, which handles the negative IDs Telegram
// uses for groups) for a stable, even spread across restarts.
func consolidationSpec(id int64) string {
	h := fnv.New32a()
	fmt.Fprintf(h, "%d", id)
	return fmt.Sprintf("%d 3 * * *", h.Sum32()%11)
}

// activate builds and stores a tenant (caller ensures cfg.Enabled). Not locked.
func (m *Manager) activate(id int64, cfg config.Chat) error {
	dir := m.chatDir(id)
	memDir := filepath.Join(dir, "memory")
	if err := os.MkdirAll(filepath.Join(memDir, "daily_memory", "_raw"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(memDir, "users"), 0o755); err != nil {
		return err
	}
	g := m.global.Get()
	resolved := cfg.Resolved(g)
	mem := memory.New(memDir, m.personaPath(id), resolved.TZ)
	sess, err := session.Load(filepath.Join(dir, "sessions.json"))
	if err != nil {
		return err
	}
	mcpPath := filepath.Join(dir, "mcp.json")
	if err := writeChatMCP(mcpPath, g.MCPAddr, resolved.EnabledServers(), m.global.MCPServerByName); err != nil {
		return err
	}
	br := brain.New(m.global, id, cfg, sess, mem, m.limiter,
		brain.WithMCPConfig(mcpPath), brain.WithWorkdir(memDir),
		brain.WithSpeakerSink(func(uid int64) { m.setSpeaker(id, uid) }))

	loc, lerr := time.LoadLocation(resolved.TZ)
	if lerr != nil {
		log.Printf("chat %d: load timezone %q failed (%v); falling back to UTC", id, resolved.TZ, lerr)
		loc = time.UTC
	}
	schedStore, err := schedule.LoadStore(filepath.Join(dir, "schedules.json"))
	if err != nil {
		return err
	}

	m.mu.Lock()
	base := m.ctx
	m.mu.Unlock()
	tctx, cancel := context.WithCancel(base)

	fire := func(ctx context.Context, sc schedule.Schedule) {
		text, err := br.RunInChat(ctx, "[scheduled reminder fired] "+sc.Prompt)
		if err != nil || text == "" {
			text = sc.Prompt
		}
		if m.sender != nil {
			_ = m.sender.Reply(ctx, cfg.BotToken, id, 0, text)
		}
	}
	sched := schedule.New(schedStore, loc, fire)
	sched.Start(tctx)
	// Per-chat nightly jobs. Consolidation compacts the day's memory, then rolls the session
	// over so the next message starts fresh with the just-written daily note in its prefix.
	_ = sched.AddBuiltin(consolidationSpec(id), func() {
		if err := consolidate.Run(tctx, mem, br, resolved.MemoryRetentionDays, resolved.RawRetentionDays, mem.Now()); err != nil {
			log.Printf("chat %d: consolidation failed: %v", id, err)
			return
		}
		if err := br.RolloverSession(); err != nil {
			log.Printf("chat %d: session rollover failed: %v", id, err)
		} else {
			log.Printf("chat %d: consolidated, session rolled over", id)
		}
	})
	_ = sched.AddBuiltin("20 1 * * *", func() {
		if n, err := sess.PruneOlderThan(time.Duration(resolved.SessionTTLDays) * 24 * time.Hour); err == nil && n > 0 {
			log.Printf("chat %d: pruned %d idle sessions", id, n)
		}
	})

	m.mu.Lock()
	m.chats[id] = &tenant{id: id, cfg: cfg, mem: mem, sess: sess, brain: br, sched: sched, cancel: cancel}
	m.mu.Unlock()
	log.Printf("chat %d (%s): enabled, model=%s", id, resolved.Name, resolved.Model)
	return nil
}

// Reload re-reads a chat's config, rebuilding or tearing down its tenant. Called by the
// web UI after an edit/enable. Wakes the gateway if the bot token changed.
func (m *Manager) Reload(id int64) error {
	cfg, ok, err := config.LoadChat(m.chatCfgPath(id))
	if err != nil {
		return err
	}
	m.mu.Lock()
	old := m.chats[id]
	if old != nil {
		old.cancel()
		delete(m.chats, id)
	}
	m.mu.Unlock()

	oldToken := ""
	if old != nil {
		oldToken = old.cfg.BotToken
	}
	if ok && cfg.Enabled {
		if err := m.activate(id, cfg); err != nil {
			return err
		}
	}
	if m.onTok != nil && (!ok || oldToken != cfg.BotToken) {
		m.onTok()
	}
	return nil
}

// Get returns the loaded, enabled tenant for a chat.
func (m *Manager) get(id int64) (*tenant, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.chats[id]
	return t, ok
}

// Tokens returns the distinct bot tokens of enabled chats (for the gateway).
func (m *Manager) Tokens() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, t := range m.chats {
		if tok := t.cfg.BotToken; tok != "" && !slices.Contains(out, tok) {
			out = append(out, tok)
		}
	}
	return out
}

// --- accessors used by the MCP server (chat-aware tools) ---

// Scheduler returns a chat's scheduler if enabled.
func (m *Manager) Scheduler(id int64) (*schedule.Scheduler, bool) {
	if t, ok := m.get(id); ok {
		return t.sched, true
	}
	return nil, false
}

// MemoryFor returns a chat's memory manager if enabled.
func (m *Manager) MemoryFor(id int64) (*memory.Manager, bool) {
	if t, ok := m.get(id); ok {
		return t.mem, true
	}
	return nil, false
}

// Recall searches a chat's memory (backs the recall_memory MCP tool). Confined to that
// chat's tenant; returns an error if the chat isn't active.
func (m *Manager) Recall(chatID int64, query, date string, limit int) (string, error) {
	t, ok := m.get(chatID)
	if !ok {
		return "", fmt.Errorf("this chat is not active")
	}
	return t.mem.Recall(query, date, limit)
}

// TokenFor returns a chat's bot token if enabled.
func (m *Manager) TokenFor(id int64) (string, bool) {
	if t, ok := m.get(id); ok {
		return t.cfg.BotToken, true
	}
	return "", false
}

// IsCronAdmin reports whether a user may manage crons in a chat: the DM owner
// (user_id == chat_id), a chat admin, or a global admin.
func (m *Manager) IsCronAdmin(id, userID int64) bool {
	t, ok := m.get(id)
	if !ok {
		return false
	}
	return userID == id || t.cfg.IsCronAdmin(m.global.Get(), userID)
}

// ToolAllowed reports whether the named MCP server may be used in a chat: yes if the server is
// in the chat's all-users list; if it's in the admin-only list, only for cron-admins.
//
// SECURITY: the modelUserID argument (the user_id the model put in the tool call) is IGNORED —
// it's untrusted (the model can supply any id, e.g. an admin's from session history). Admin
// checks use the REAL Telegram sender recorded by the brain for this chat's in-flight run.
func (m *Manager) ToolAllowed(chatID, modelUserID int64, server string) bool {
	t, ok := m.get(chatID)
	if !ok {
		return false
	}
	cfg := t.cfg.Resolved(m.global.Get())
	if slices.Contains(cfg.AllAllowedTools, server) {
		return true
	}
	if slices.Contains(cfg.AdminAllowedTools, server) {
		return m.IsCronAdmin(chatID, m.speakerFor(chatID))
	}
	return false
}

// RegenerateMCPConfigs rewrites every active chat's mcp.json. Called after the external MCP
// registry changes (server defs changed but tool names didn't, so no brain rebuild is needed —
// claude re-reads mcp.json on each invocation).
func (m *Manager) RegenerateMCPConfigs() {
	g := m.global.Get()
	m.mu.Lock()
	type job struct {
		id      int64
		servers []string
	}
	jobs := make([]job, 0, len(m.chats))
	for id, t := range m.chats {
		jobs = append(jobs, job{id, t.cfg.Resolved(g).EnabledServers()})
	}
	m.mu.Unlock()

	for _, j := range jobs {
		path := filepath.Join(m.chatDir(j.id), "mcp.json")
		if err := writeChatMCP(path, g.MCPAddr, j.servers, m.global.MCPServerByName); err != nil {
			log.Printf("chat %d: regenerate mcp.json failed: %v", j.id, err)
		}
	}
}

// --- web UI support ---

// ChatView is a summary for the dashboard list.
type ChatView struct {
	ChatID   int64     `json:"chat_id"`
	Name     string    `json:"name"`
	Type     string    `json:"type"`
	Enabled  bool      `json:"enabled"`
	HasToken bool      `json:"has_token"`
	LastSeen time.Time `json:"last_seen"`
}

// List returns every known chat (registry ∪ configured), newest-seen first.
func (m *Manager) List() []ChatView {
	views := map[int64]ChatView{}
	for _, e := range m.registry.list() {
		views[e.ChatID] = ChatView{ChatID: e.ChatID, Name: e.Name, Type: e.Type, LastSeen: e.LastSeen}
	}
	// Overlay configured state.
	entries, _ := os.ReadDir(m.chatsDir())
	for _, e := range entries {
		id, err := strconv.ParseInt(e.Name(), 10, 64)
		if err != nil {
			continue
		}
		cfg, ok, _ := config.LoadChat(m.chatCfgPath(id))
		if !ok {
			continue
		}
		v := views[id]
		v.ChatID = id
		if cfg.Name != "" {
			v.Name = cfg.Name
		}
		if cfg.Type != "" {
			v.Type = cfg.Type
		}
		v.Enabled = cfg.Enabled
		v.HasToken = cfg.BotToken != ""
		views[id] = v
	}
	out := make([]ChatView, 0, len(views))
	for _, v := range views {
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b ChatView) int { return b.LastSeen.Compare(a.LastSeen) })
	return out
}

// LoadChatConfig returns a chat's on-disk config (or a defaults-seeded one if absent). For a
// not-yet-configured chat it seeds name/type and the bot_token from the registry — i.e. the
// bot that last delivered a message here — so the dashboard's token field comes pre-filled and
// persists on save instead of starting blank.
func (m *Manager) LoadChatConfig(id int64) (config.Chat, bool) {
	cfg, ok, _ := config.LoadChat(m.chatCfgPath(id))
	if !ok {
		name, ctype, token := "", "", ""
		if e, found := m.registry.get(id); found {
			name, ctype, token = e.Name, e.Type, e.LastToken
		}
		return config.NewChat(m.global.Get(), name, ctype, token), false
	}
	// Backfill the tool allow-lists for display when a (pre-feature) chat.yaml omits them, so the
	// dashboard shows their effective default state (memory=all, reminders=admins) rather than
	// rendering every server "off" — which a save would then lock in. Nil = never configured;
	// an explicitly empty list (operator cleared it) is left untouched.
	g := m.global.Get()
	if cfg.AllAllowedTools == nil {
		cfg.AllAllowedTools = append([]string(nil), g.DefaultAllAllowedTools...)
	}
	if cfg.AdminAllowedTools == nil {
		cfg.AdminAllowedTools = append([]string(nil), g.DefaultAdminAllowedTools...)
	}
	return cfg, true
}

// SaveChatConfig writes a chat's config and reloads the tenant.
func (m *Manager) SaveChatConfig(id int64, cfg config.Chat) error {
	if err := os.MkdirAll(m.chatDir(id), 0o755); err != nil {
		return err
	}
	if err := config.SaveChat(m.chatCfgPath(id), cfg); err != nil {
		return err
	}
	return m.Reload(id)
}

// ConsolidateAll runs nightly consolidation for every enabled chat (used by the
// `consolidate` subcommand). The daemon does the file I/O; the LLM only summarizes.
func (m *Manager) ConsolidateAll(ctx context.Context) {
	m.mu.Lock()
	tenants := make([]*tenant, 0, len(m.chats))
	for _, t := range m.chats {
		tenants = append(tenants, t)
	}
	m.mu.Unlock()
	for _, t := range tenants {
		resolved := t.cfg.Resolved(m.global.Get())
		log.Printf("chat %d: consolidating", t.id)
		if err := consolidate.Run(ctx, t.mem, t.brain, resolved.MemoryRetentionDays, resolved.RawRetentionDays, t.mem.Now()); err != nil {
			log.Printf("chat %d: consolidation failed: %v", t.id, err)
		}
	}
}

// Persona reads/writes a chat's persona.md.
func (m *Manager) Persona(id int64) (string, error) {
	b, err := os.ReadFile(m.personaPath(id))
	if os.IsNotExist(err) {
		return "", nil
	}
	return string(b), err
}
func (m *Manager) SavePersona(id int64, text string) error {
	if err := os.MkdirAll(m.chatDir(id), 0o755); err != nil {
		return err
	}
	return os.WriteFile(m.personaPath(id), []byte(text), 0o644)
}
