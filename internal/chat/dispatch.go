package chat

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"

	"assistant/internal/config"
	"assistant/internal/telegram"
)

// CallbackRouter handles inline-button presses (memory confirmations). The proposals
// manager implements it; injected via SetProposals to avoid an import cycle.
type CallbackRouter interface {
	HandleCallback(ctx context.Context, data string) telegram.CallbackResult
}

// SetProposals injects the memory-confirmation callback router.
func (m *Manager) SetProposals(r CallbackRouter) { m.proposals = r }

// OnMessage implements telegram.Handler. It records the chat in the registry, routes by
// configuration/token, and either replies (configured) or sends the canned
// "unconfigured" notice (directed messages only). No LLM/memory for unconfigured chats.
func (m *Manager) OnMessage(ctx context.Context, s telegram.Sender, token string, msg telegram.Message) {
	m.registry.observe(msg.ChatID, msg.ChatName, msg.ChatType, token)

	t, ok := m.get(msg.ChatID)
	if !ok {
		if directedRaw(msg) {
			_ = s.Reply(ctx, token, msg.ChatID, msg.MessageID,
				fmt.Sprintf("This chat isn't configured yet (chat_id: %d). An admin can enable it in the dashboard.", msg.ChatID))
		}
		return
	}
	// A configured chat is served only by its designated token (a group may host several
	// of your bots — the others ignore it here, preventing double replies).
	if t.cfg.BotToken != "" && t.cfg.BotToken != token {
		return
	}

	directed := directedFor(t.cfg, msg)
	if !shouldHandle(t.cfg, msg, directed) {
		return
	}

	// Photos: download + attach for vision only when this chat enables images. A photo on
	// the current message wins; otherwise fall back to one on the message being replied to,
	// so "@bot what's this?" in reply to an earlier image can see it.
	photoFileID := msg.PhotoFileID
	if photoFileID == "" {
		photoFileID = msg.ReplyToPhotoFileID
	}
	if photoFileID != "" {
		if t.cfg.ImagesEnabled {
			if data, media, err := s.DownloadFile(ctx, token, photoFileID); err == nil {
				msg.ImageB64 = base64.StdEncoding.EncodeToString(data)
				msg.ImageMedia = media
			} else {
				log.Printf("chat %d: image download failed: %v", msg.ChatID, err)
			}
		} else if msg.Text == "" {
			// Image-only message in a chat without images enabled: nothing to do.
			return
		}
	}

	t.brain.Record(ctx, msg, directed)
	if !directed {
		return
	}

	s.React(ctx, token, msg.ChatID, msg.MessageID, telegram.AckEmoji)
	text, err := t.brain.Reply(ctx, msg)
	if err != nil {
		log.Printf("chat %d: brain error: %v", msg.ChatID, err)
		s.React(ctx, token, msg.ChatID, msg.MessageID, telegram.ErrEmoji)
		_ = s.Reply(ctx, token, msg.ChatID, msg.MessageID, telegram.ErrMessageText)
		return
	}
	if text != "" {
		_ = s.Reply(ctx, token, msg.ChatID, msg.MessageID, text)
	}
	s.ClearReaction(ctx, token, msg.ChatID, msg.MessageID)
}

// OnCallback implements telegram.Handler — routes memory-confirmation button presses.
func (m *Manager) OnCallback(ctx context.Context, _ telegram.Sender, _ string, data string) telegram.CallbackResult {
	if m.proposals == nil {
		return telegram.CallbackResult{}
	}
	return m.proposals.HandleCallback(ctx, data)
}

// directedFor reports whether the bot should actively reply (vs. only record), per the
// chat's response mode.
func directedFor(cfg config.Chat, m telegram.Message) bool {
	if !m.IsGroup {
		return true
	}
	switch cfg.GroupResponseMode {
	case config.GroupModeAll:
		return true
	case config.GroupModeReply:
		return m.RepliesToBot
	default: // mention
		return m.MentionsBot || m.RepliesToBot
	}
}

// shouldHandle: non-directed group chatter is ignored unless recording is enabled.
func shouldHandle(cfg config.Chat, m telegram.Message, directed bool) bool {
	if !m.IsGroup || directed {
		return true
	}
	return cfg.RecordGroupChatter
}

// directedRaw decides whether to answer in an UNconfigured chat (no config to consult):
// DMs and explicit @mentions / replies-to-bot.
func directedRaw(m telegram.Message) bool {
	return !m.IsGroup || m.MentionsBot || m.RepliesToBot
}
