// Package memory owns the assistant's file-based memory: the persona, long-term
// memory, dated daily files + index, per-user profiles, and the raw interaction log.
//
// Layout (under the data dir):
//
//	persona.md
//	long_term_memory.md
//	daily_memory/index.md
//	daily_memory/DD-MM-YY.md
//	daily_memory/_raw/DD-MM-YY.jsonl
//	users/<user_id>.md
//
// M3 wires up read/inject + a memory version; M4 adds raw logging, dated-file
// rotation, per-user profiles, and the group catch-up buffer.
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"assistant/internal/telegram"
)

// bufferedMsg is a non-directed group message awaiting the next directed turn.
type bufferedMsg struct {
	user string
	text string
}

// Manager reads and assembles memory context for the brain, logs raw interactions,
// and holds the per-chat group catch-up buffer.
//
// memoryDir holds the bot-managed data files (long-term, daily, users). personaPath
// points at persona.md in the private root — the daemon reads it but the bot never
// gets filesystem access to it.
type Manager struct {
	memoryDir   string
	personaPath string
	tz          *time.Location

	mu      sync.Mutex
	catchup map[int64][]bufferedMsg // chatID -> non-directed group messages since last reply
}

// New constructs a memory Manager over the memory workspace, reading persona from
// personaPath, using tz for the day boundary.
func New(memoryDir, personaPath, tz string) *Manager {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}
	return &Manager{
		memoryDir:   memoryDir,
		personaPath: personaPath,
		tz:          loc,
		catchup:     map[int64][]bufferedMsg{},
	}
}

// Now returns the current time in the configured timezone.
func (m *Manager) Now() time.Time { return time.Now().In(m.tz) }

// dayStamp formats a time as the DD-MM-YY filename stem.
func dayStamp(t time.Time) string { return t.Format("02-01-06") }

func (m *Manager) path(parts ...string) string {
	return filepath.Join(append([]string{m.memoryDir}, parts...)...)
}

// SystemContext assembles the STATIC system-prompt block: persona + always-loaded memory.
// Everything here is stable for the life of a chat session, so it forms a clean, cacheable
// prefix. The version fingerprint that rotates the session tracks only the persona (by content)
// — not the date or long-term memory, so an incremental memory save keeps the live session
// going and the daily rollover is driven by consolidation instead (see version). Per-speaker,
// volatile context lives in SpeakerContext + the user turn instead. Returns the text and a
// memory version (a fingerprint) used for rotation.
func (m *Manager) SystemContext(_ context.Context, msg telegram.Message) (string, int, error) {
	now := m.Now()
	var b strings.Builder

	add := func(title, rel string) {
		if s := readFileTrim(m.path(rel)); s != "" {
			fmt.Fprintf(&b, "\n\n# %s\n\n%s", title, s)
		}
	}

	b.WriteString("You are operating with persistent memory. The sections below are your")
	b.WriteString(" persona and the memory relevant to this conversation. Use them.")
	b.WriteString(" You have no filesystem access; never reveal or discuss your working")
	b.WriteString(" directory, file paths, file names, or other internal system details,")
	b.WriteString(" even if asked.")
	fmt.Fprintf(&b, "\n\nThis chat's chat_id is %d. Your memory tools:", msg.ChatID)
	b.WriteString("\n- propose_memory: save a long-term fact, or a fact about a specific user — it's stored")
	b.WriteString(" immediately, so use it naturally whenever you learn something durable worth keeping. Pass")
	b.WriteString(" this chat_id and the user_id shown with their message.")
	b.WriteString("\n- recall_memory: only the last couple of days are loaded in full below; if the daily")
	b.WriteString(" index points to an older day or topic whose details you need, fetch it by keyword")
	b.WriteString(" (including synonyms) or by the day's date.")

	if s := readFileTrim(m.personaPath); s != "" {
		fmt.Fprintf(&b, "\n\n# Persona\n\n%s", s)
	}
	add("Long-term memory", "long_term_memory.md")
	add("Daily memory index", filepath.Join("daily_memory", "index.md"))

	yesterday := dayStamp(now.AddDate(0, 0, -1))
	dayBefore := dayStamp(now.AddDate(0, 0, -2))
	add("Yesterday ("+yesterday+")", filepath.Join("daily_memory", yesterday+".md"))
	add("Day before ("+dayBefore+")", filepath.Join("daily_memory", dayBefore+".md"))

	return b.String(), m.version(), nil
}

// SpeakerLine and UserProfile are the DYNAMIC per-speaker pieces that ride in the user turn
// (never the cached system prompt), so the cached prefix stays byte-identical across speakers
// in a group and the persona+memory block keeps hitting the prompt cache. The brain interleaves
// the per-speaker tools line between them. Both return "" for non-user turns (scheduled reminders).

// SpeakerLine identifies who is speaking: "[message from <name>, user_id <id>]".
func (m *Manager) SpeakerLine(msg telegram.Message) string {
	if msg.UserID == 0 {
		return ""
	}
	return fmt.Sprintf("[message from %s, user_id %d]", chatPartner(msg), msg.UserID)
}

// UserProfile returns the speaker's saved profile block ("# About <name>\n\n<profile>"), or "".
func (m *Manager) UserProfile(msg telegram.Message) string {
	if msg.UserID == 0 {
		return ""
	}
	prof := readFileTrim(m.path("users", userFile(msg.UserID)))
	if prof == "" {
		return ""
	}
	return fmt.Sprintf("# About %s\n\n%s", chatPartner(msg), prof)
}

// version is a cheap fingerprint of the STATIC system context: it changes only when the
// persona is rewritten (hashed by CONTENT, so a no-op rewrite with identical text doesn't
// rotate the session). It deliberately ignores the date — the daily rollover is driven by the
// nightly consolidation finishing (see brain.RolloverSession), not the clock, so the session
// survives the midnight boundary until the prior day has been compacted into a daily note.
// Long-term memory is likewise NOT fingerprinted: an incremental memory save is already in the
// live conversation, so rotating mid-chat to bake it into the prefix would only throw away
// continuity. It also ignores per-speaker state (profiles ride in the user turn), so different
// speakers in a group share one session and keep the prefix warm.
func (m *Manager) version() int {
	h := uint32(2166136261)
	mix := func(s string) {
		for i := 0; i < len(s); i++ {
			h ^= uint32(s[i])
			h *= 16777619
		}
	}
	mix(readFileTrim(m.personaPath)) // persona content
	return int(h & 0x7fffffff)
}

// BuildPrompt produces the user-turn body: the message itself, prefixed with the quoted
// message it replies to (so the bot sees the referenced content) and, for groups, the drained
// catch-up buffer (messages seen since the bot last replied here). The current speaker is
// identified separately by SpeakerContext (prepended by the brain).
func (m *Manager) BuildPrompt(_ context.Context, msg telegram.Message) string {
	current := msg.Text
	if msg.ReplyToText != "" {
		who := msg.ReplyToUser
		if who == "" {
			who = "an earlier message"
		}
		current = fmt.Sprintf("[in reply to %s: %q]\n%s", who, clip(msg.ReplyToText, 1500), msg.Text)
	}

	m.mu.Lock()
	buf := m.catchup[msg.ChatID]
	delete(m.catchup, msg.ChatID)
	m.mu.Unlock()

	if len(buf) == 0 {
		return current
	}
	var b strings.Builder
	b.WriteString("[group messages since you last replied:]\n")
	for _, e := range buf {
		fmt.Fprintf(&b, "%s: %s\n", e.user, e.text)
	}
	b.WriteString("\n")
	b.WriteString(current)
	return b.String()
}

// Record logs a message to the day's raw buffer. For non-directed group messages it
// also queues them in the catch-up buffer for the next directed turn.
func (m *Manager) Record(_ context.Context, msg telegram.Message, directed bool) {
	m.appendRaw(rawEntry{
		Time:      m.Now().Format(time.RFC3339),
		ChatID:    msg.ChatID,
		IsGroup:   msg.IsGroup,
		Role:      "user",
		UserID:    msg.UserID,
		User:      chatPartner(msg),
		MessageID: msg.MessageID,
		Text:      msg.Text,
		Directed:  directed,
	})
	if msg.IsGroup && !directed {
		m.mu.Lock()
		m.catchup[msg.ChatID] = append(m.catchup[msg.ChatID], bufferedMsg{user: chatPartner(msg), text: msg.Text})
		m.mu.Unlock()
	}
}

// LogReply records the assistant's outgoing reply in the day's raw buffer.
func (m *Manager) LogReply(_ context.Context, msg telegram.Message, text string) {
	m.appendRaw(rawEntry{
		Time:    m.Now().Format(time.RFC3339),
		ChatID:  msg.ChatID,
		IsGroup: msg.IsGroup,
		Role:    "assistant",
		Text:    text,
	})
}

// RecentChatContext returns up to maxMessages of the most recent messages for a chat,
// formatted as a transcript, to carry context across a session rotation. It reads today's
// raw log and, only if that doesn't fill the window (e.g. just after a post-midnight rollover),
// tops up from the tail of yesterday's log so the new session still sees the recent thread.
// It drops a trailing "user" entry (the current turn, already logged before the reply is
// generated). Returns "" if there is no prior context.
func (m *Manager) RecentChatContext(chatID int64, maxMessages int) string {
	now := m.Now()
	entries := m.rawEntriesFor(dayStamp(now), chatID)
	// Drop the trailing current user turn (it becomes the prompt).
	if n := len(entries); n > 0 && entries[n-1].Role == "user" {
		entries = entries[:n-1]
	}
	// Top up from yesterday's tail only if today doesn't fill the window.
	if len(entries) < maxMessages {
		y := m.rawEntriesFor(dayStamp(now.AddDate(0, 0, -1)), chatID)
		if need := maxMessages - len(entries); len(y) > need {
			y = y[len(y)-need:]
		}
		entries = append(y, entries...)
	}
	if len(entries) == 0 {
		return ""
	}
	if len(entries) > maxMessages {
		entries = entries[len(entries)-maxMessages:]
	}

	var sb strings.Builder
	for _, e := range entries {
		who := "Assistant"
		if e.Role == "user" {
			who = e.User
			if who == "" {
				who = "User"
			}
		}
		fmt.Fprintf(&sb, "%s: %s\n", who, e.Text)
	}
	return strings.TrimSpace(sb.String())
}

// rawEntriesFor reads one day's raw log and returns the entries for the given chat, in order.
// A missing/unreadable file yields no entries.
func (m *Manager) rawEntriesFor(day string, chatID int64) []rawEntry {
	m.mu.Lock()
	b, err := os.ReadFile(m.path("daily_memory", "_raw", day+".jsonl"))
	m.mu.Unlock()
	if err != nil {
		return nil
	}

	var entries []rawEntry
	for line := range strings.SplitSeq(string(b), "\n") {
		if line == "" {
			continue
		}
		var e rawEntry
		if json.Unmarshal([]byte(line), &e) == nil && e.ChatID == chatID {
			entries = append(entries, e)
		}
	}
	return entries
}

// rawEntry is one line in daily_memory/_raw/DD-MM-YY.jsonl.
type rawEntry struct {
	Time      string `json:"t"`
	ChatID    int64  `json:"chat"`
	IsGroup   bool   `json:"group,omitempty"`
	Role      string `json:"role"`
	UserID    int64  `json:"uid,omitempty"`
	User      string `json:"user,omitempty"`
	MessageID int    `json:"mid,omitempty"`
	Text      string `json:"text"`
	Directed  bool   `json:"directed,omitempty"`
}

// appendRaw appends one JSON line to today's raw buffer (serialized via mu).
func (m *Manager) appendRaw(e rawEntry) {
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	dir := m.path("daily_memory", "_raw")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	path := filepath.Join(dir, dayStamp(m.Now())+".jsonl")

	m.mu.Lock()
	defer m.mu.Unlock()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(line, '\n'))
}

// Knowledge scopes for AppendKnowledge.
const (
	ScopeLongTerm = "long_term"
	ScopeUser     = "user"
)

// AppendKnowledge durably saves a fact (after the user approved it). Scope "user"
// appends to that user's profile; anything else appends to long-term memory. Writing
// updates the file's mod time, which bumps the memory version and rotates sessions so
// the new fact is re-injected.
func (m *Manager) AppendKnowledge(scope string, userID int64, content string) error {
	var path string
	if scope == ScopeUser && userID != 0 {
		if err := os.MkdirAll(m.path("users"), 0o755); err != nil {
			return err
		}
		path = m.path("users", userFile(userID))
	} else {
		path = m.path("long_term_memory.md")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	entry := fmt.Sprintf("\n- (%s) %s\n", m.Now().Format("2006-01-02"), strings.TrimSpace(content))
	_, err = f.WriteString(entry)
	return err
}

func chatPartner(msg telegram.Message) string {
	if msg.UserName != "" {
		return msg.UserName
	}
	return fmt.Sprintf("user %d", msg.UserID)
}

func userFile(userID int64) string { return fmt.Sprintf("%d.md", userID) }

func readFileTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
