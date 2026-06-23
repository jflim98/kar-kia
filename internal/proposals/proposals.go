// Package proposals implements "save to memory with permission", chat-aware. The
// headless Claude calls propose_memory; instead of writing, the daemon asks the user
// (in that chat, via that chat's bot token) to confirm with ✅/❌ and only commits to
// THAT chat's memory on approval.
package proposals

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"assistant/internal/memory"
	"assistant/internal/telegram"

	"github.com/google/uuid"
)

// Committer durably writes an approved fact (a chat's memory.Manager satisfies this).
type Committer interface {
	AppendKnowledge(scope string, userID int64, content string) error
}

const (
	prefixApprove = "ok:"
	prefixReject  = "no:"
)

type pending struct {
	chatID, userID int64
	token          string
	scope, content string
	confirmMsgID   int
}

// Manager holds pending proposals across all chats.
type Manager struct {
	sender  telegram.Sender
	memOf   func(chatID int64) (Committer, bool)
	tokenOf func(chatID int64) (string, bool)

	mu      sync.Mutex
	pending map[string]pending
}

// New wires the sender plus per-chat lookups for memory and bot token.
func New(sender telegram.Sender, memOf func(int64) (Committer, bool), tokenOf func(int64) (string, bool)) *Manager {
	return &Manager{sender: sender, memOf: memOf, tokenOf: tokenOf, pending: map[string]pending{}}
}

// Save commits a fact to chat chatID's memory immediately, with no confirmation. Backs
// the auto-save memory tool: the model decides what's worth remembering and it lands right
// away; the nightly consolidation reconciles user profiles into coherent prose afterwards.
func (m *Manager) Save(_ context.Context, chatID, userID int64, scope, content string) (string, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return "", fmt.Errorf("empty content")
	}
	mem, ok := m.memOf(chatID)
	if !ok {
		return "", fmt.Errorf("chat %d is not configured", chatID)
	}
	scope = normalizeScope(scope)
	if err := mem.AppendKnowledge(scope, userID, content); err != nil {
		return "", err
	}
	return fmt.Sprintf("Saved to %s.", scopeLabel(scope, userID)), nil
}

// Propose asks the user (in chat chatID) to confirm saving a fact. Backs propose_memory.
func (m *Manager) Propose(ctx context.Context, chatID, userID int64, scope, content string) (string, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return "", fmt.Errorf("empty content")
	}
	token, ok := m.tokenOf(chatID)
	if !ok || token == "" {
		return "", fmt.Errorf("chat %d is not configured", chatID)
	}
	scope = normalizeScope(scope)
	key := uuid.NewString()

	text := fmt.Sprintf("I'd like to remember this (%s):\n\n%q\n\nSave it?", scopeLabel(scope, userID), content)
	msgID, err := m.sender.SendConfirm(ctx, token, chatID, text, prefixApprove+key, prefixReject+key)
	if err != nil {
		return "", err
	}

	m.mu.Lock()
	m.pending[key] = pending{chatID: chatID, userID: userID, token: token, scope: scope, content: content, confirmMsgID: msgID}
	m.mu.Unlock()
	return "I've asked the user to confirm saving that to memory.", nil
}

// HandleCallback applies an approve/reject decision. Implements chat.CallbackRouter.
func (m *Manager) HandleCallback(_ context.Context, data string) telegram.CallbackResult {
	var approve bool
	var key string
	switch {
	case strings.HasPrefix(data, prefixApprove):
		approve, key = true, strings.TrimPrefix(data, prefixApprove)
	case strings.HasPrefix(data, prefixReject):
		approve, key = false, strings.TrimPrefix(data, prefixReject)
	default:
		return telegram.CallbackResult{}
	}

	m.mu.Lock()
	p, ok := m.pending[key]
	delete(m.pending, key)
	m.mu.Unlock()
	if !ok {
		return telegram.CallbackResult{Answer: "This confirmation has expired."}
	}

	if !approve {
		return telegram.CallbackResult{Answer: "Discarded", EditChatID: p.chatID, EditMsgID: p.confirmMsgID, EditText: "❌ Not saved."}
	}
	mem, ok := m.memOf(p.chatID)
	if !ok {
		return telegram.CallbackResult{Answer: "Chat unavailable", EditChatID: p.chatID, EditMsgID: p.confirmMsgID, EditText: "⚠️ Could not save (chat not active)."}
	}
	if err := mem.AppendKnowledge(p.scope, p.userID, p.content); err != nil {
		return telegram.CallbackResult{Answer: "Failed to save", EditChatID: p.chatID, EditMsgID: p.confirmMsgID, EditText: "⚠️ Could not save that to memory."}
	}
	return telegram.CallbackResult{Answer: "Saved", EditChatID: p.chatID, EditMsgID: p.confirmMsgID, EditText: "✅ Saved to memory:\n\n" + p.content}
}

func normalizeScope(scope string) string {
	if strings.EqualFold(scope, memory.ScopeUser) {
		return memory.ScopeUser
	}
	return memory.ScopeLongTerm
}

func scopeLabel(scope string, userID int64) string {
	if scope == memory.ScopeUser {
		return fmt.Sprintf("about user %d", userID)
	}
	return "long-term memory"
}
