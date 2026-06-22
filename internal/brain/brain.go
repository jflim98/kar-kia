// Package brain orchestrates one chat's reply: it assembles persona + memory context,
// picks/rotates the chat's headless session, invokes `claude -p`, and records the
// interaction. One Brain per chat; all brains share a global concurrency Limiter.
package brain

import (
	"context"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"assistant/internal/config"
	"assistant/internal/session"
	"assistant/internal/telegram"
)

// Limiter is a global concurrency cap shared across all chats (RAM is global).
type Limiter chan struct{}

// NewLimiter makes a limiter allowing n concurrent claude calls (min 1).
func NewLimiter(n int) Limiter { return make(Limiter, max(n, 1)) }

func (l Limiter) acquire(ctx context.Context) bool {
	if l == nil {
		return true
	}
	select {
	case l <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}
func (l Limiter) release() {
	if l != nil {
		<-l
	}
}

// Memory is the subset of the memory manager the brain depends on.
type Memory interface {
	SystemContext(ctx context.Context, msg telegram.Message) (string, int, error)
	SpeakerLine(msg telegram.Message) string
	UserProfile(msg telegram.Message) string
	BuildPrompt(ctx context.Context, msg telegram.Message) string
	Record(ctx context.Context, msg telegram.Message, directed bool)
	LogReply(ctx context.Context, msg telegram.Message, text string)
	RecentChatContext(chatID int64, maxMessages int) string
	Now() time.Time
}

// carryOverTurns is how many recent in-chat messages to re-inject when a session rotates.
const carryOverTurns = 12

// Brain ties one chat's config, sessions, memory, and the Claude runner together.
type Brain struct {
	global   *config.Manager
	chatID   int64
	chat     config.Chat
	sessions *session.Store
	mem      Memory
	runner   *runner
	limiter  Limiter
	speaker  func(int64) // records the real Telegram sender for server-side tool authorization
	mu       sync.Mutex  // serializes this chat's calls
}

// Option configures a Brain.
type Option func(*Brain)

// WithCLIPath overrides the `claude` executable path.
func WithCLIPath(p string) Option { return func(b *Brain) { b.runner.cliPath = p } }

// WithMCPConfig enables the custom MCP server by pointing at its mcp.json.
func WithMCPConfig(path string) Option { return func(b *Brain) { b.runner.mcpConfig = path } }

// WithWorkdir sets the CLI working directory (the chat's memory workspace).
func WithWorkdir(dir string) Option { return func(b *Brain) { b.runner.memoryDir = dir } }

// WithSpeakerSink registers a callback the brain invokes (under its per-chat lock, before each
// claude run) with the REAL Telegram sender's user id. The MCP server authorizes admin-gated
// tools against this — never the user_id the model passes, which is untrusted.
func WithSpeakerSink(fn func(int64)) Option { return func(b *Brain) { b.speaker = fn } }

// New constructs a Brain for one chat. limiter (may be nil) caps global concurrency.
func New(global *config.Manager, chatID int64, chat config.Chat, sessions *session.Store, mem Memory, limiter Limiter, opts ...Option) *Brain {
	b := &Brain{
		global:   global,
		chatID:   chatID,
		chat:     chat,
		sessions: sessions,
		mem:      mem,
		runner:   &runner{cliPath: "claude"},
		limiter:  limiter,
	}
	for _, o := range opts {
		o(b)
	}
	return b
}

// Record passively logs a message into this chat's memory.
func (b *Brain) Record(ctx context.Context, msg telegram.Message, directed bool) {
	b.mem.Record(ctx, msg, directed)
}

// Reply assembles context and replies as the chat's persona for a user message.
func (b *Brain) Reply(ctx context.Context, msg telegram.Message) (string, error) {
	return b.invoke(ctx, msg, b.mem.BuildPrompt(ctx, msg))
}

// RunInChat runs an instruction (e.g. a fired reminder) in the chat's session.
func (b *Brain) RunInChat(ctx context.Context, instruction string) (string, error) {
	return b.invoke(ctx, telegram.Message{ChatID: b.chatID}, instruction)
}

func (b *Brain) invoke(ctx context.Context, msg telegram.Message, prompt string) (string, error) {
	g := b.global.Get()
	chat := b.chat.Resolved(g)

	sys, memVer, err := b.mem.SystemContext(ctx, msg)
	if err != nil {
		return "", err
	}
	// The DM owner (user_id == chat_id, true only in DMs) is always a cron-admin of
	// their own DM; groups require explicit admin_user_ids / global admins. The admin
	// hint rides in the user turn (see composeTurn), not the cached system prompt.
	isAdmin := msg.UserID == b.chatID || b.chat.IsCronAdmin(g, msg.UserID)

	chatKey := strconv.FormatInt(b.chatID, 10)
	sess, resume := b.pickSession(chatKey, memVer, chat.RotateTurnCap)
	if !resume {
		if recent := b.mem.RecentChatContext(msg.ChatID, carryOverTurns); recent != "" {
			sys += "\n\n# Recent conversation in this chat (carried over from earlier today)\n\n" + recent
		}
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.limiter.acquire(ctx) {
		return "", ctx.Err()
	}
	defer b.limiter.release()

	// Record the real sender so the MCP server can authorize admin-gated tools against it (not
	// the user_id the model supplies). Set under b.mu, so it's stable for this chat's in-flight run.
	if b.speaker != nil {
		b.speaker(msg.UserID)
	}

	// The allow-list is WebSearch + this chat's enabled MCP servers. --permission-mode default
	// denies anything else (file/shell built-ins are also hard-blocked via --disallowedTools).
	// We deliberately do NOT pass --tools: on claude 2.1.181 it suppresses the (deferred) MCP tools.
	tools := b.allowedTools(chat, isAdmin)
	res, err := b.runner.Run(ctx, runInput{
		Model:        chat.Model,
		SystemPrompt: sys,
		AllowedTools: tools,
		SessionID:    sess.ID,
		Resume:       resume,
		Prompt:       b.composeTurn(msg, chat, isAdmin, prompt),
		MaxBudgetUSD: chat.MaxBudgetUSD,
		OAuthToken:   b.global.Secrets().ClaudeCodeOAuthToken,
		ImageB64:     msg.ImageB64,
		ImageMedia:   msg.ImageMedia,
	})
	if err != nil {
		return "", err
	}

	sess.TurnCount++
	sess.LastUsed = time.Now()
	if res.SessionID != "" {
		sess.ID = res.SessionID
	}
	if err := b.sessions.Put(chatKey, sess); err != nil {
		log.Printf("brain: persist session for chat %s failed: %v", chatKey, err)
	}
	log.Printf("brain: chat=%s user=%d admin=%v model=%s resume=%v turns=%d cost=$%.4f cache_read=%d out=%d",
		chatKey, msg.UserID, isAdmin, chat.Model, resume, sess.TurnCount, res.CostUSD, res.CacheReadTokens, res.OutputTokens)

	if res.Text != "" {
		b.mem.LogReply(ctx, msg, res.Text)
	}
	return res.Text, nil
}

// composeTurn builds the dynamic per-turn header — speaker identity, the tools this speaker may
// use, and their profile — then the message body, in that order. It rides in the user turn, never
// the cached system prompt, so swapping speakers (or a user's admin status) doesn't bust the
// prompt cache. No speaker (e.g. a scheduled reminder) returns the body unchanged.
func (b *Brain) composeTurn(msg telegram.Message, chat config.Chat, isAdmin bool, body string) string {
	line := b.mem.SpeakerLine(msg)
	if line == "" {
		return body
	}
	parts := []string{line, b.toolsLine(chat, isAdmin)}
	// Reminder guidance is admin-only and rides here (never the cached system prompt), gated by the
	// SAME source as the tool allow-list so a non-admin is never told reminders exist or how to use
	// them. Only surface it when reminders is actually enabled for this speaker.
	if b.runner.mcpConfig != "" && slices.Contains(chat.ServersFor(isAdmin), config.ServerReminders) {
		parts = append(parts, fmt.Sprintf("When scheduling reminders, pass chat_id %d.", msg.ChatID))
	}
	if prof := b.mem.UserProfile(msg); prof != "" {
		parts = append(parts, prof)
	}
	parts = append(parts, body)
	return strings.Join(parts, "\n\n")
}

// toolsLine is a brief, permission-aware summary of what this speaker may use, derived from the
// SAME source as the pre-approved tool set (chat.ServersFor) so a newly enabled server appears
// automatically. External servers (no built-in description) are listed by name.
func (b *Brain) toolsLine(chat config.Chat, isAdmin bool) string {
	items := []string{"web search"}
	if b.runner.mcpConfig != "" {
		for _, s := range chat.ServersFor(isAdmin) {
			if d := config.ServerDescription(s); d != "" {
				s += " (" + d + ")"
			}
			items = append(items, s)
		}
	}
	return "You may use these tools: " + strings.Join(items, "; ") + "."
}

func (b *Brain) pickSession(chatKey string, memVer, rotateTurnCap int) (session.Session, bool) {
	cur, ok := b.sessions.Get(chatKey)
	underCap := rotateTurnCap <= 0 || cur.TurnCount < rotateTurnCap
	if ok && cur.MemoryVersion == memVer && underCap {
		return cur, true
	}
	return session.NewSession(memVer), false
}

// RolloverSession drops the chat's current session so the next message starts a fresh one.
// Called by the nightly consolidation once memory has been compacted, so the new session's
// cached prefix includes the just-written daily note (this is what rotates a chat across the
// day boundary now that pickSession no longer rotates on the date).
func (b *Brain) RolloverSession() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessions.Delete(strconv.FormatInt(b.chatID, 10))
}

// builtinTools: the model never gets filesystem tools — only WebSearch.
func (b *Brain) builtinTools() []string { return []string{"WebSearch"} }

// allowedTools returns the tool set this chat exposes to the model: WebSearch plus the MCP
// servers this speaker may use (chat.ServersFor — the all-users list, plus the admin-only list
// for cron-admins). `mcp__<server>` covers all of that server's tools, so external servers need
// no per-tool introspection. Passed as --allowedTools; --permission-mode default denies the rest.
// chat must be the resolved config (lists backfilled).
func (b *Brain) allowedTools(chat config.Chat, isAdmin bool) []string {
	tools := b.builtinTools()
	if b.runner.mcpConfig == "" {
		return tools
	}
	for _, s := range chat.ServersFor(isAdmin) {
		tools = append(tools, "mcp__"+s)
	}
	return tools
}
