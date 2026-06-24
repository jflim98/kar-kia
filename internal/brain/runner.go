package brain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// runner shells out to the `claude` CLI in headless (-p) mode.
type runner struct {
	cliPath   string // "claude" (or absolute path)
	memoryDir string // cwd for the CLI, so built-in Read/Glob reach memory files
	mcpConfig string // path to mcp.json; empty => no custom MCP server
}

// disallowedBuiltins are the built-in claude tools we hard-block on every call (--disallowedTools)
// as defense-in-depth. The model only ever gets WebSearch + this chat's MCP servers (via
// --allowedTools); --permission-mode default already denies anything un-allow-listed, and this
// makes the dangerous ones an explicit deny that can't be approved. The bot does no file/shell I/O.
//
// Names must match the running claude's tool set or it logs "deny rule matches no known tool".
// These are valid on claude 2.1.181 (older aliases like MultiEdit/NotebookRead/LS were dropped;
// --permission-mode default covers anything not named here anyway).
var disallowedBuiltins = []string{
	"Bash", "Read", "Write", "Edit", "NotebookEdit", "Glob", "Grep", "WebFetch", "Task",
}

// runInput is one headless invocation.
type runInput struct {
	Model          string
	SystemPrompt   string   // the full system prompt via --system-prompt (persona + memory + tool stub)
	AllowedTools   []string // --allowedTools: the allow-list (WebSearch + this chat's MCP servers)
	PermissionMode string   // default "default"
	SessionID      string   // session UUID
	Resume         bool     // true => --resume SessionID, false => --session-id SessionID
	Prompt         string   // the user message / task
	MaxBudgetUSD   float64  // per-call cap (0 => unset)
	OAuthToken     string   // CLAUDE_CODE_OAUTH_TOKEN (server auth); empty => inherit login
	DisableMCP     bool     // true => don't attach the MCP server (pure text in/out)

	// ImageB64 (with ImageMedia, e.g. image/jpeg) sends an inline image with the prompt
	// via stream-json input — vision without granting any file tools.
	ImageB64   string
	ImageMedia string
}

// runResult is the parsed outcome of a headless invocation.
type runResult struct {
	Text            string
	IsError         bool
	CostUSD         float64
	CacheReadTokens int
	OutputTokens    int
	SessionID       string
}

// cliResult mirrors the relevant fields of `claude -p --output-format json`.
type cliResult struct {
	Type         string  `json:"type"`
	Subtype      string  `json:"subtype"`
	IsError      bool    `json:"is_error"`
	Result       string  `json:"result"`
	SessionID    string  `json:"session_id"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	Usage        struct {
		OutputTokens         int `json:"output_tokens"`
		CacheReadInputTokens int `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

// commonArgs builds the flags shared by both the text and stream (image) modes.
func (r *runner) commonArgs(in runInput) []string {
	a := []string{"--model", in.Model}
	if in.SystemPrompt != "" {
		// --system-prompt (full override), NOT --append: this bot is a chat persona, not a coding
		// agent, so we replace claude's default coding-agent system prompt entirely rather than
		// fight it. The override also drops the per-machine cwd/env/git/memory-path sections that
		// the default prompt would otherwise inject. SystemContext carries a tool-usage stub since
		// we no longer inherit claude's built-in tool scaffolding.
		a = append(a, "--system-prompt", in.SystemPrompt)
	}
	if r.mcpConfig != "" && !in.DisableMCP {
		a = append(a, "--mcp-config", r.mcpConfig, "--strict-mcp-config")
	}
	// The tool gate is --allowedTools (the allow-list) + --permission-mode default: anything not
	// allow-listed is denied in headless mode. We do NOT use --tools — on claude 2.1.181 it
	// suppresses MCP tools (they're deferred/ToolSearch-loaded) and it never actually restricted
	// built-ins anyway. --disallowedTools hard-blocks the dangerous built-ins as defense-in-depth.
	if len(in.AllowedTools) > 0 {
		a = append(a, "--allowedTools", strings.Join(in.AllowedTools, ","))
	}
	a = append(a, "--disallowedTools", strings.Join(disallowedBuiltins, ","))
	mode := in.PermissionMode
	if mode == "" {
		mode = "default"
	}
	a = append(a, "--permission-mode", mode)
	// Set ASSISTANT_CLAUDE_DEBUG=1 to make claude print MCP connection logs to stderr (which we
	// capture and log) — useful for diagnosing why an MCP tool isn't reaching the model.
	if os.Getenv("ASSISTANT_CLAUDE_DEBUG") != "" {
		a = append(a, "--debug")
	}
	if in.Resume {
		a = append(a, "--resume", in.SessionID)
	} else {
		a = append(a, "--session-id", in.SessionID)
	}
	if in.MaxBudgetUSD > 0 {
		a = append(a, "--max-budget-usd", strconv.FormatFloat(in.MaxBudgetUSD, 'f', -1, 64))
	}
	return a
}

func (r *runner) newCmd(ctx context.Context, in runInput, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, r.cliPath, args...)
	cmd.Dir = r.memoryDir
	cmd.Env = os.Environ()
	if in.OAuthToken != "" {
		cmd.Env = append(cmd.Env, "CLAUDE_CODE_OAUTH_TOKEN="+in.OAuthToken)
	}
	return cmd
}

func toResult(cr cliResult) (runResult, error) {
	res := runResult{
		Text:            strings.TrimSpace(cr.Result),
		IsError:         cr.IsError,
		CostUSD:         cr.TotalCostUSD,
		CacheReadTokens: cr.Usage.CacheReadInputTokens,
		OutputTokens:    cr.Usage.OutputTokens,
		SessionID:       cr.SessionID,
	}
	if cr.IsError {
		return res, fmt.Errorf("claude reported error (subtype=%s): %s", cr.Subtype, truncate(cr.Result, 300))
	}
	return res, nil
}

// Run executes one headless invocation. Text mode uses `-p <prompt>`; with an image it
// switches to stream-json input so the picture is sent inline (no file tools needed).
func (r *runner) Run(ctx context.Context, in runInput) (runResult, error) {
	if in.ImageB64 != "" {
		return r.runStream(ctx, in)
	}
	// The prompt goes on stdin, not as a `-p <prompt>` argv element: Linux caps any single
	// argument at MAX_ARG_STRLEN (128 KiB), so a large transcript would fail execve with E2BIG
	// before claude even starts. stdin has no such limit.
	args := append([]string{"-p", "--output-format", "json"}, r.commonArgs(in)...)
	cmd := r.newCmd(ctx, in, args)
	cmd.Stdin = strings.NewReader(in.Prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// claude writes MCP connection status/warnings to stderr; capture it so a failure surfaces
	// the reason instead of an opaque exit error. Set ASSISTANT_CLAUDE_DEBUG=1 for verbose logs.
	if err := cmd.Run(); err != nil {
		return runResult{}, fmt.Errorf("claude -p failed: %w: %s", err, truncate(stderr.String(), 500))
	}
	var cr cliResult
	if err := json.Unmarshal(stdout.Bytes(), &cr); err != nil {
		return runResult{}, fmt.Errorf("parse claude json: %w: %s", err, truncate(stdout.String(), 500))
	}
	return toResult(cr)
}

// runStream sends the prompt + an inline base64 image via stream-json input and parses
// the streamed events for the final result.
func (r *runner) runStream(ctx context.Context, in runInput) (runResult, error) {
	args := append([]string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose"},
		r.commonArgs(in)...)
	cmd := r.newCmd(ctx, in, args)

	msg, err := buildImageMessage(in.Prompt, in.ImageB64, in.ImageMedia)
	if err != nil {
		return runResult{}, err
	}
	cmd.Stdin = bytes.NewReader(msg)

	out, err := cmd.Output()
	if err != nil {
		return runResult{}, cmdErr("claude -p (stream)", err)
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if line == "" {
			continue
		}
		var cr cliResult
		if json.Unmarshal([]byte(line), &cr) == nil && cr.Type == "result" {
			return toResult(cr)
		}
	}
	return runResult{}, fmt.Errorf("no result in stream output: %s", truncate(string(out), 300))
}

// buildImageMessage builds one stream-json user message with text + an image block.
func buildImageMessage(prompt, b64, media string) ([]byte, error) {
	type source struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	}
	type block struct {
		Type   string  `json:"type"`
		Text   string  `json:"text,omitempty"`
		Source *source `json:"source,omitempty"`
	}
	if media == "" {
		media = "image/jpeg"
	}
	if prompt == "" {
		prompt = "Describe this image."
	}
	m := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role": "user",
			"content": []block{
				{Type: "text", Text: prompt},
				{Type: "image", Source: &source{Type: "base64", MediaType: media, Data: b64}},
			},
		},
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func cmdErr(what string, err error) error {
	var stderr string
	if ee, ok := err.(*exec.ExitError); ok {
		stderr = strings.TrimSpace(string(ee.Stderr))
	}
	return fmt.Errorf("%s failed: %w: %s", what, err, truncate(stderr, 500))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
