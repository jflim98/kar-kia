// Package config holds the assistant's configuration, split into a GLOBAL layer
// (infra + defaults, in config.yaml / secrets.yaml at the data-dir root) and a per-chat
// layer (chats/<id>/chat.yaml). The global Manager is live and hot-reloadable; per-chat
// configs are loaded/saved by the chat package via LoadChat/SaveChat.
package config

import "slices"

// Group response modes (per-chat).
const (
	GroupModeMention = "mention"
	GroupModeReply   = "reply"
	GroupModeAll     = "all"
)

// Chat types.
const (
	TypeDM    = "dm"
	TypeGroup = "group"
)

// ModelAliases are the model choices offered in the web UI dropdowns (single source of truth).
// The claude CLI also accepts a full model name (e.g. "claude-opus-4-8"); a saved value outside
// this list is preserved by the UI rather than reset. Order is lightest → heaviest.
var ModelAliases = []string{"haiku", "sonnet", "opus", "fable"}

// EffortLevels are the reasoning-effort choices the claude CLI accepts (--effort), and the single
// source of truth for both the web UI dropdown and the runner's validation. An empty value means
// "inherit the CLI default" (no --effort passed) and is not listed here.
var EffortLevels = []string{"low", "medium", "high", "xhigh", "max"}

// IsValidEffort reports whether s is a recognized (non-empty) effort level.
func IsValidEffort(s string) bool { return slices.Contains(EffortLevels, s) }

// Built-in MCP server names and their HTTP paths on the daemon's MCP endpoint. These are the
// always-available servers (no subprocess); external servers come from the registry. The map
// is the single source of truth shared by the MCP server (which paths to serve) and the
// per-chat mcp.json generator (which URLs to emit).
const (
	ServerMemory    = "memory"
	ServerReminders = "reminders"
)

// BuiltinMCPServers maps a built-in server name to its path on the daemon MCP endpoint.
var BuiltinMCPServers = map[string]string{
	ServerMemory:    "/mcp/memory",
	ServerReminders: "/mcp/reminders",
}

// IsBuiltinServer reports whether name is one of the daemon's built-in MCP servers.
func IsBuiltinServer(name string) bool {
	_, ok := BuiltinMCPServers[name]
	return ok
}

// serverDescriptions are short human labels for built-in servers, shown to the model in the
// per-speaker "tools you may use" line. Every key in BuiltinMCPServers MUST have an entry here
// (enforced by a test) so a new built-in server can't be added without describing it.
var serverDescriptions = map[string]string{
	ServerMemory:    "recall and save facts",
	ServerReminders: "schedule, list, and cancel reminders",
}

// ServerDescription returns a short label for a server, or "" if none (external servers, which
// are listed by name).
func ServerDescription(name string) string { return serverDescriptions[name] }

// MCPServer is a registered external MCP server (local stdio only). Env may hold secrets, so
// the registry file is written 0600.
type MCPServer struct {
	Name    string            `yaml:"name" json:"name"`
	Command string            `yaml:"command" json:"command"`
	Args    []string          `yaml:"args" json:"args"`
	Env     map[string]string `yaml:"env" json:"env"`
}

// Global is the non-secret, hot-reloadable global settings (config.yaml at the root).
type Global struct {
	WebUIAddr    string  `yaml:"webui_addr" json:"webui_addr"`
	MCPAddr      string  `yaml:"mcp_addr" json:"mcp_addr"`
	Concurrency  int     `yaml:"concurrency" json:"concurrency"`
	MaxBudgetUSD float64 `yaml:"max_budget_usd" json:"max_budget_usd"`

	// GlobalAdminUserIDs are operators: implicitly cron-admins in every chat.
	GlobalAdminUserIDs []int64 `yaml:"global_admin_user_ids" json:"global_admin_user_ids"`

	// Defaults applied when a new chat is created / a chat field is empty.
	DefaultModel               string `yaml:"default_model" json:"default_model"`
	DefaultConsolidationModel  string `yaml:"default_consolidation_model" json:"default_consolidation_model"`
	DefaultEffort              string `yaml:"default_effort" json:"default_effort"`                             // conversational reasoning effort: "" (CLI default), low, medium, high, xhigh, max
	DefaultConsolidationEffort string `yaml:"default_consolidation_effort" json:"default_consolidation_effort"` // effort for background consolidation (same values; "" => CLI default)
	DefaultTZ                  string `yaml:"default_tz" json:"default_tz"`
	DefaultMemoryRetentionDays int    `yaml:"default_memory_retention_days" json:"default_memory_retention_days"`
	DefaultRawRetentionDays    int    `yaml:"default_raw_retention_days" json:"default_raw_retention_days"`
	DefaultSessionTTLDays      int    `yaml:"default_session_ttl_days" json:"default_session_ttl_days"`
	DefaultRotateTurnCap       int    `yaml:"default_rotate_turn_cap" json:"default_rotate_turn_cap"`

	// Default MCP server allow-lists for new chats (entries are server names: the built-in
	// "memory"/"reminders" plus any registered external servers). all = everyone, admin =
	// cron-admins only.
	DefaultAllAllowedTools   []string `yaml:"default_all_allowed_tools" json:"default_all_allowed_tools"`
	DefaultAdminAllowedTools []string `yaml:"default_admin_allowed_tools" json:"default_admin_allowed_tools"`
}

// Secrets are the global secrets (secrets.yaml, mode 0600).
type Secrets struct {
	WebUIPassword        string   `yaml:"webui_password"`
	BotTokens            []string `yaml:"bot_tokens"`
	ClaudeCodeOAuthToken string   `yaml:"claude_code_oauth_token"`
}

// IsGlobalAdmin reports whether a user is a global operator.
func (g Global) IsGlobalAdmin(userID int64) bool {
	return slices.Contains(g.GlobalAdminUserIDs, userID)
}

// DefaultGlobal returns the global config with sensible defaults.
func DefaultGlobal() Global {
	return Global{
		WebUIAddr:                  "127.0.0.1:8765",
		MCPAddr:                    "127.0.0.1:8766",
		Concurrency:                1,
		DefaultModel:               "sonnet",
		DefaultConsolidationModel:  "opus",
		DefaultTZ:                  "UTC",
		DefaultMemoryRetentionDays: 14,
		DefaultRawRetentionDays:    14,
		DefaultSessionTTLDays:      2,
		DefaultRotateTurnCap:       50,
		DefaultAllAllowedTools:     []string{ServerMemory},
		DefaultAdminAllowedTools:   []string{ServerReminders},
	}
}
