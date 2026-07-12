package telegram

import (
	"testing"

	"github.com/go-telegram/bot/models"
)

func TestParseMessageMentionAndReply(t *testing.T) {
	const botID = int64(555)
	const botUser = "assistant_bot"

	msg := &models.Message{
		ID:   12,
		Chat: models.Chat{ID: -100, Type: models.ChatTypeSupergroup, Title: "My Group"},
		From: &models.User{ID: 7, Username: "alice"},
		Text: "@assistant_bot hi",
		Entities: []models.MessageEntity{
			{Type: models.MessageEntityTypeMention, Offset: 0, Length: len("@assistant_bot")},
		},
	}
	m := parseMessage(msg, botID, botUser)
	if !m.IsGroup || !m.MentionsBot || m.RepliesToBot {
		t.Fatalf("expected group mention, got %+v", m)
	}
	if m.ChatType != ChatGroup || m.ChatName != "My Group" {
		t.Fatalf("chat meta wrong: %+v", m)
	}
	if m.UserName != "@alice" {
		t.Fatalf("display name = %q", m.UserName)
	}

	reply := &models.Message{
		ID:             13,
		Chat:           models.Chat{ID: 7, Type: models.ChatTypePrivate},
		From:           &models.User{ID: 7, FirstName: "Al"},
		Text:           "thanks",
		ReplyToMessage: &models.Message{ID: 12, From: &models.User{ID: botID}},
	}
	rm := parseMessage(reply, botID, botUser)
	if rm.MentionsBot || !rm.RepliesToBot {
		t.Fatalf("expected reply-to-bot, got %+v", rm)
	}
	if rm.ChatType != ChatDM || rm.ChatName != "Al" {
		t.Fatalf("dm meta wrong: %+v", rm)
	}
}

func TestParseMessageCapturesRepliedContent(t *testing.T) {
	const botID = int64(555)

	// @mention of the bot in a message replying to @bob's earlier (non-bot) message.
	msg := &models.Message{
		ID:   20,
		Chat: models.Chat{ID: -100, Type: models.ChatTypeSupergroup},
		From: &models.User{ID: 7, Username: "alice"},
		Text: "@assistant_bot summarize this",
		Entities: []models.MessageEntity{
			{Type: models.MessageEntityTypeMention, Offset: 0, Length: len("@assistant_bot")},
		},
		ReplyToMessage: &models.Message{ID: 19, From: &models.User{ID: 8, Username: "bob"}, Text: "the deploy is failing"},
	}
	m := parseMessage(msg, botID, "assistant_bot")
	if m.RepliesToBot {
		t.Fatalf("reply is to @bob, not the bot: %+v", m)
	}
	if m.ReplyToText != "the deploy is failing" || m.ReplyToUser != "@bob" {
		t.Fatalf("replied-to content not captured: text=%q user=%q", m.ReplyToText, m.ReplyToUser)
	}

	// Caption is used when the replied-to message is a photo (no Text).
	photoReply := &models.Message{
		ID: 21, Chat: models.Chat{ID: -100, Type: models.ChatTypeSupergroup}, From: &models.User{ID: 7},
		Text:           "what is this",
		ReplyToMessage: &models.Message{ID: 18, From: &models.User{ID: 8, Username: "bob"}, Caption: "our new logo"},
	}
	if pm := parseMessage(photoReply, botID, "assistant_bot"); pm.ReplyToText != "our new logo" {
		t.Fatalf("caption of replied-to photo not captured: %q", pm.ReplyToText)
	}

	// The replied-to photo's file_id (largest) is captured so the dispatcher can attach it.
	imgReply := &models.Message{
		ID: 22, Chat: models.Chat{ID: -100, Type: models.ChatTypeSupergroup}, From: &models.User{ID: 7},
		Text: "@assistant_bot what is this",
		Entities: []models.MessageEntity{
			{Type: models.MessageEntityTypeMention, Offset: 0, Length: len("@assistant_bot")},
		},
		ReplyToMessage: &models.Message{
			ID: 17, From: &models.User{ID: 8},
			Photo: []models.PhotoSize{{FileID: "small"}, {FileID: "large"}},
		},
	}
	if pm := parseMessage(imgReply, botID, "assistant_bot"); pm.ReplyToPhotoFileID != "large" {
		t.Fatalf("largest replied-to photo file_id not captured: %q", pm.ReplyToPhotoFileID)
	}
}
