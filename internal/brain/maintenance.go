package brain

import (
	"context"

	"assistant/internal/session"
)

// Summarize runs the consolidation model on an instruction with NO tools and no MCP
// access — pure text in, text out. The daemon uses this to summarize memory content
// during consolidation, so the LLM never touches the filesystem.
func (b *Brain) Summarize(ctx context.Context, instruction string) (string, error) {
	chat := b.chat.Resolved(b.global.Get())
	res, err := b.runner.Run(ctx, runInput{
		Model:  chat.ConsolidationModel,
		Effort: chat.ConsolidationEffort,
		// No AllowedTools + DisableMCP => no MCP servers and nothing allow-listed, so
		// --permission-mode default denies every tool (plus --disallowedTools). Pure text in/out.
		DisableMCP:   true,
		SessionID:    session.NewSession(0).ID, // fresh, no resume
		Prompt:       instruction,
		MaxBudgetUSD: chat.MaxBudgetUSD,
		OAuthToken:   b.global.Secrets().ClaudeCodeOAuthToken,
	})
	if err != nil {
		return "", err
	}
	return res.Text, nil
}
