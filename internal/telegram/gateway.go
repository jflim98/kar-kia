// Package telegram is the multi-token Telegram gateway. It opens one long-poll per
// unique bot token, parses inbound messages (resolving @mention/reply per the receiving
// bot's identity), and delegates handling to an injected Handler. It implements Sender
// so the handler can react/reply/confirm on a specific token's connection.
package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

const (
	ackEmoji   = "👀"
	errEmoji   = "😢"
	errMessage = "Sorry — I hit an error handling that."
)

var errNotConnected = errors.New("telegram: token not connected")

// CallbackResult tells the gateway how to answer an inline-button press.
type CallbackResult struct {
	Answer     string
	EditChatID int64
	EditMsgID  int
	EditText   string
}

// Sender sends outbound actions on a specific token's connection.
type Sender interface {
	React(ctx context.Context, token string, chatID int64, msgID int, emoji string)
	ClearReaction(ctx context.Context, token string, chatID int64, msgID int)
	// Reply sends text (Markdown→HTML, length-split) to a chat, optionally threaded
	// under replyTo (0 = none).
	Reply(ctx context.Context, token string, chatID int64, replyTo int, text string) error
	SendConfirm(ctx context.Context, token string, chatID int64, text, approveData, rejectData string) (int, error)
	// DownloadFile fetches a Telegram file by id on a token's connection, returning the
	// bytes and a media type (e.g. image/jpeg).
	DownloadFile(ctx context.Context, token, fileID string) ([]byte, string, error)
}

// Handler processes inbound events. Implemented by the chat dispatcher.
type Handler interface {
	OnMessage(ctx context.Context, s Sender, token string, m Message)
	OnCallback(ctx context.Context, s Sender, token, data string) CallbackResult
}

// AckEmoji / ErrEmoji / ErrMessage are exported so the dispatcher can use the same
// reaction vocabulary.
const (
	AckEmoji = ackEmoji
	ErrEmoji = errEmoji
)

// ErrMessageText is the default error reply text.
const ErrMessageText = errMessage

type conn struct {
	token       string
	bot         *bot.Bot
	botID       int64
	botUsername string
	cancel      context.CancelFunc
}

// Gateway manages a connection per unique bot token.
type Gateway struct {
	handler  Handler
	tokensFn func() []string // current desired set of tokens to connect

	mu    sync.Mutex
	conns map[string]*conn
	wake  chan struct{}
}

// New builds a gateway. tokensFn returns the current desired token set (deduped by the
// gateway); call Wake when it changes.
func New(handler Handler, tokensFn func() []string) *Gateway {
	return &Gateway{
		handler:  handler,
		tokensFn: tokensFn,
		conns:    map[string]*conn{},
		wake:     make(chan struct{}, 1),
	}
}

// Wake tells the gateway to reconcile its connections with the current token set.
func (g *Gateway) Wake() {
	select {
	case g.wake <- struct{}{}:
	default:
	}
}

// Run reconciles connections and serves until ctx is cancelled.
func (g *Gateway) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		g.reconcile(ctx)
		select {
		case <-ctx.Done():
		case <-g.wake:
		}
	}
	return ctx.Err()
}

func (g *Gateway) reconcile(ctx context.Context) {
	desired := map[string]bool{}
	for _, t := range g.tokensFn() {
		if t != "" {
			desired[t] = true
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for tok, c := range g.conns {
		if !desired[tok] {
			c.cancel()
			delete(g.conns, tok)
		}
	}
	for tok := range desired {
		if _, ok := g.conns[tok]; ok {
			continue
		}
		cctx, cancel := context.WithCancel(ctx)
		g.conns[tok] = &conn{token: tok, cancel: cancel}
		go g.supervise(cctx, tok)
	}
}

// supervise keeps one token connected, reconnecting on error until its ctx is cancelled.
func (g *Gateway) supervise(ctx context.Context, token string) {
	for ctx.Err() == nil {
		b, err := bot.New(token,
			bot.WithDefaultHandler(func(hctx context.Context, _ *bot.Bot, upd *models.Update) {
				g.handle(hctx, token, upd)
			}),
			// Telegram's getUpdates uses the PREVIOUS allowed_updates when the param is omitted,
			// so a stale restricted setting can silently drop callback_query (inline-button clicks).
			// Specify it explicitly every poll so memory-confirmation buttons always reach us.
			bot.WithAllowedUpdates(bot.AllowedUpdates{"message", "callback_query"}),
		)
		if err != nil {
			log.Printf("telegram: bad token (…%s): %v", tail(token), err)
			if !sleepOrDone(ctx, 60*time.Second) {
				return
			}
			continue
		}
		me, err := b.GetMe(ctx)
		if err != nil {
			log.Printf("telegram: getMe failed (…%s): %v", tail(token), err)
			if !sleepOrDone(ctx, 30*time.Second) {
				return
			}
			continue
		}
		g.mu.Lock()
		if c := g.conns[token]; c != nil {
			c.bot, c.botID, c.botUsername = b, me.ID, me.Username
		}
		g.mu.Unlock()
		log.Printf("telegram: connected @%s (id %d, …%s)", me.Username, me.ID, tail(token))

		b.Start(ctx) // blocks until ctx cancelled

		g.mu.Lock()
		if c := g.conns[token]; c != nil {
			c.bot = nil
		}
		g.mu.Unlock()
		if ctx.Err() == nil {
			sleepOrDone(ctx, 5*time.Second)
		}
	}
}

func (g *Gateway) handle(ctx context.Context, token string, upd *models.Update) {
	if upd.CallbackQuery != nil {
		g.handleCallback(ctx, token, upd.CallbackQuery)
		return
	}
	msg := upd.Message
	if msg == nil || (msg.Text == "" && msg.Caption == "" && len(msg.Photo) == 0) {
		return // ignore non-text, non-photo updates
	}
	g.mu.Lock()
	c := g.conns[token]
	g.mu.Unlock()
	if c == nil {
		return
	}
	m := parseMessage(msg, c.botID, c.botUsername)
	go g.handler.OnMessage(ctx, g, token, m)
}

func (g *Gateway) handleCallback(ctx context.Context, token string, cq *models.CallbackQuery) {
	r := g.handler.OnCallback(ctx, g, token, cq.Data)
	b := g.botFor(token)
	if b == nil {
		return
	}
	if _, err := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cq.ID, Text: r.Answer}); err != nil {
		log.Printf("telegram: answer callback failed: %v", err)
	}
	if r.EditChatID != 0 {
		if _, err := b.EditMessageText(ctx, &bot.EditMessageTextParams{ChatID: r.EditChatID, MessageID: r.EditMsgID, Text: r.EditText}); err != nil {
			log.Printf("telegram: edit message failed: %v", err)
		}
	}
}

// --- Sender ---

func (g *Gateway) botFor(token string) *bot.Bot {
	g.mu.Lock()
	defer g.mu.Unlock()
	if c := g.conns[token]; c != nil {
		return c.bot
	}
	return nil
}

func (g *Gateway) React(ctx context.Context, token string, chatID int64, msgID int, emoji string) {
	b := g.botFor(token)
	if b == nil {
		return
	}
	if _, err := b.SetMessageReaction(ctx, &bot.SetMessageReactionParams{
		ChatID: chatID, MessageID: msgID,
		Reaction: []models.ReactionType{{Type: models.ReactionTypeTypeEmoji, ReactionTypeEmoji: &models.ReactionTypeEmoji{Emoji: emoji}}},
	}); err != nil {
		log.Printf("telegram: set reaction %q failed: %v", emoji, err)
	}
}

func (g *Gateway) ClearReaction(ctx context.Context, token string, chatID int64, msgID int) {
	b := g.botFor(token)
	if b == nil {
		return
	}
	if _, err := b.SetMessageReaction(ctx, &bot.SetMessageReactionParams{
		ChatID: chatID, MessageID: msgID, Reaction: []models.ReactionType{},
	}); err != nil {
		log.Printf("telegram: clear reaction failed: %v", err)
	}
}

// Reply renders Markdown→HTML, splits to the length limit, and sends each chunk
// (threading the first under replyTo). Falls back to plain text if HTML is rejected.
func (g *Gateway) Reply(ctx context.Context, token string, chatID int64, replyTo int, text string) error {
	b := g.botFor(token)
	if b == nil {
		return errNotConnected
	}
	chunks := splitSource(text)
	for i, src := range chunks {
		var rp *models.ReplyParameters
		if i == 0 && replyTo != 0 {
			rp = &models.ReplyParameters{MessageID: replyTo}
		}
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID, Text: toTelegramHTML(src), ParseMode: models.ParseModeHTML, ReplyParameters: rp,
		}); err == nil {
			continue
		} else {
			log.Printf("telegram: HTML send to chat %d failed (%v); retrying plain", chatID, err)
		}
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: src, ReplyParameters: rp}); err != nil {
			log.Printf("telegram: plain send to chat %d failed: %v", chatID, err)
			return err
		}
	}
	return nil
}

// DownloadFile fetches a Telegram file's bytes + media type on the token's connection.
func (g *Gateway) DownloadFile(ctx context.Context, token, fileID string) ([]byte, string, error) {
	b := g.botFor(token)
	if b == nil {
		return nil, "", errNotConnected
	}
	f, err := b.GetFile(ctx, &bot.GetFileParams{FileID: fileID})
	if err != nil {
		return nil, "", err
	}
	link := b.FileDownloadLink(f)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("download %s: status %d", fileID, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20)) // cap at 20 MB
	if err != nil {
		return nil, "", err
	}
	return data, mediaTypeFor(f.FilePath), nil
}

func mediaTypeFor(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	default:
		return "image/jpeg"
	}
}

func (g *Gateway) SendConfirm(ctx context.Context, token string, chatID int64, text, approveData, rejectData string) (int, error) {
	b := g.botFor(token)
	if b == nil {
		return 0, errNotConnected
	}
	m, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID, Text: text,
		ReplyMarkup: &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{
			{Text: "✅ Save", CallbackData: approveData},
			{Text: "❌ No", CallbackData: rejectData},
		}}},
	})
	if err != nil {
		return 0, err
	}
	return m.ID, nil
}

func tail(token string) string {
	if len(token) <= 6 {
		return token
	}
	return token[len(token)-6:]
}

func sleepOrDone(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
