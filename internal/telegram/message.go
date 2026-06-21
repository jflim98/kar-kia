package telegram

import (
	"strings"
	"unicode/utf16"

	"github.com/go-telegram/bot/models"
)

// Chat types reported on a Message.
const (
	ChatDM    = "dm"
	ChatGroup = "group"
)

// Message is a normalized inbound Telegram message. Routing/handling decisions (which
// chat, directed vs. chatter, admin checks) live in the chat dispatcher, not here.
type Message struct {
	ChatID    int64
	ChatType  string // "dm" | "group"
	ChatName  string // group title or user display name (for the registry)
	IsGroup   bool
	UserID    int64
	UserName  string // display name (first name or @username)
	MessageID int
	Text      string

	PhotoFileID string // largest photo's file_id, if this message has a photo
	ImageB64    string // populated by the dispatcher after download (base64), if used
	ImageMedia  string // media type for ImageB64 (e.g. image/jpeg)

	MentionsBot  bool // group: text @-mentions the bot
	RepliesToBot bool // this message replies to one of the bot's messages

	// If this message replies to another, ReplyToText/ReplyToUser carry that message's content
	// and sender, so the bot can see the quoted context (e.g. "@bot summarize ⤷ <A>").
	ReplyToText string
	ReplyToUser string
}

// HasPhoto reports whether the message carried a photo.
func (m Message) HasPhoto() bool { return m.PhotoFileID != "" }

// parseMessage normalizes a Telegram message, resolving mention/reply-to-bot relative
// to the given bot identity (per-connection).
func parseMessage(msg *models.Message, botID int64, botUsername string) Message {
	// A photo carries its text in Caption (with CaptionEntities for the @mention).
	text, entities := msg.Text, msg.Entities
	if text == "" && msg.Caption != "" {
		text, entities = msg.Caption, msg.CaptionEntities
	}
	out := Message{
		ChatID:    msg.Chat.ID,
		IsGroup:   msg.Chat.Type == models.ChatTypeGroup || msg.Chat.Type == models.ChatTypeSupergroup,
		MessageID: msg.ID,
		Text:      text,
	}
	if n := len(msg.Photo); n > 0 {
		out.PhotoFileID = msg.Photo[n-1].FileID // last = largest
	}
	if out.IsGroup {
		out.ChatType = ChatGroup
		out.ChatName = msg.Chat.Title
	} else {
		out.ChatType = ChatDM
	}
	if msg.From != nil {
		out.UserID = msg.From.ID
		out.UserName = displayName(msg.From)
		if !out.IsGroup && out.ChatName == "" {
			out.ChatName = out.UserName
		}
	}
	if r := msg.ReplyToMessage; r != nil {
		rtext := r.Text
		if rtext == "" {
			rtext = r.Caption
		}
		out.ReplyToText = rtext
		out.ReplyToUser = displayName(r.From)
		if r.From != nil && r.From.ID == botID {
			out.RepliesToBot = true
		}
	}
	out.MentionsBot = mentionsBot(text, entities, botID, botUsername)
	return out
}

func mentionsBot(text string, entities []models.MessageEntity, botID int64, botUsername string) bool {
	at := "@" + strings.ToLower(botUsername)
	for _, e := range entities {
		switch e.Type {
		case models.MessageEntityTypeMention:
			if botUsername != "" && strings.EqualFold(entityText(text, e), at) {
				return true
			}
		case models.MessageEntityTypeTextMention:
			if e.User != nil && e.User.ID == botID {
				return true
			}
		}
	}
	return false
}

// entityText slices the entity out of text using UTF-16 offsets (Telegram convention).
func entityText(text string, e models.MessageEntity) string {
	u := utf16.Encode([]rune(text))
	if e.Offset < 0 || e.Offset+e.Length > len(u) {
		return ""
	}
	return string(utf16.Decode(u[e.Offset : e.Offset+e.Length]))
}

func displayName(u *models.User) string {
	if u == nil {
		return ""
	}
	if u.Username != "" {
		return "@" + u.Username
	}
	name := u.FirstName
	if u.LastName != "" {
		name += " " + u.LastName
	}
	return name
}
