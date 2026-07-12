package brain_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"assistant/internal/brain"
	"assistant/internal/config"
	"assistant/internal/memory"
	"assistant/internal/session"
	"assistant/internal/telegram"
)

// TestBrainReplyAndResume exercises the real `claude` CLI end-to-end. Skipped unless
// CLAUDE_IT=1 (a couple of cheap haiku calls; needs a logged-in claude or oauth token).
func TestBrainReplyAndResume(t *testing.T) {
	if os.Getenv("CLAUDE_IT") == "" {
		t.Skip("set CLAUDE_IT=1 to run the live claude integration test")
	}

	dir := t.TempDir()
	personaPath := filepath.Join(dir, "persona.md")
	if err := os.WriteFile(personaPath, []byte("You are a terse test bot."), 0o644); err != nil {
		t.Fatal(err)
	}
	memDir := filepath.Join(dir, "memory")
	if err := os.MkdirAll(filepath.Join(memDir, "daily_memory", "_raw"), 0o755); err != nil {
		t.Fatal(err)
	}

	g, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.Load(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	mem := memory.New(memDir, personaPath, "UTC")
	chatCfg := config.NewChat(g.Get(), "test", config.TypeDM, "")
	chatCfg.Model = "haiku"
	br := brain.New(g, 123, chatCfg, sessions, mem, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	msg := telegram.Message{ChatID: 123, UserID: 123, UserName: "@tester", MessageID: 1,
		Text: "Reply with exactly the single word: pong"}
	res, err := br.Reply(ctx, msg)
	if err != nil {
		t.Fatalf("first Reply: %v", err)
	}
	if !strings.Contains(strings.ToLower(res), "pong") {
		t.Fatalf("first reply missing pong: %q", res)
	}
	s1, ok := sessions.Get("123")
	if !ok || s1.TurnCount != 1 {
		t.Fatalf("session not stored after first turn: %+v ok=%v", s1, ok)
	}

	msg2 := telegram.Message{ChatID: 123, UserID: 123, UserName: "@tester", MessageID: 2,
		Text: "What single word did I just ask you to reply with? Answer with only that word."}
	res2, err := br.Reply(ctx, msg2)
	if err != nil {
		t.Fatalf("second Reply: %v", err)
	}
	if !strings.Contains(strings.ToLower(res2), "pong") {
		t.Fatalf("resume failed; second reply did not recall pong: %q", res2)
	}
	s2, _ := sessions.Get("123")
	if s2.ID != s1.ID || s2.TurnCount != 2 {
		t.Fatalf("session not reused: %+v -> %+v", s1, s2)
	}
}

// TestBrainImageVision sends a solid-green image through the stream-json path. Skipped
// unless CLAUDE_IT=1.
func TestBrainImageVision(t *testing.T) {
	if os.Getenv("CLAUDE_IT") == "" {
		t.Skip("set CLAUDE_IT=1 to run the live image vision test")
	}
	dir := t.TempDir()
	personaPath := filepath.Join(dir, "persona.md")
	_ = os.WriteFile(personaPath, []byte("You are a terse test bot."), 0o644)
	memDir := filepath.Join(dir, "memory")
	_ = os.MkdirAll(filepath.Join(memDir, "daily_memory", "_raw"), 0o755)

	g, _ := config.Load(dir)
	sessions, _ := session.Load(filepath.Join(dir, "sessions.json"))
	mem := memory.New(memDir, personaPath, "UTC")
	chatCfg := config.NewChat(g.Get(), "test", config.TypeDM, "")
	chatCfg.Model = "haiku"
	br := brain.New(g, 7, chatCfg, sessions, mem, nil)

	// A 64x64 solid-green PNG.
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.RGBA{0, 200, 0, 255})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	b64 := base64.StdEncoding.EncodeToString(buf.Bytes())

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	msg := telegram.Message{ChatID: 7, UserID: 7, MessageID: 1,
		Text: "What color is this image? Answer with one word.", ImageB64: b64, ImageMedia: "image/png"}
	res, err := br.Reply(ctx, msg)
	if err != nil {
		t.Fatalf("image Reply: %v", err)
	}
	if !strings.Contains(strings.ToLower(res), "green") {
		t.Fatalf("vision failed; expected 'green', got %q", res)
	}
	t.Logf("OK: vision read the image: %q", res)
}
