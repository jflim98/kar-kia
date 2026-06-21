package config

import (
	"os"
	"slices"

	"gopkg.in/yaml.v3"
)

// Chat is a single chat's configuration (chats/<id>/chat.yaml). Empty model/tz fields
// fall back to global defaults via Resolved.
type Chat struct {
	Enabled  bool   `yaml:"enabled" json:"enabled"`
	Name     string `yaml:"name" json:"name"`           // last-seen group title / user name
	Type     string `yaml:"type" json:"type"`           // dm | group
	BotToken string `yaml:"bot_token" json:"bot_token"` // bot used to send in this chat

	Model              string `yaml:"model" json:"model"`
	ConsolidationModel string `yaml:"consolidation_model" json:"consolidation_model"`
	TZ                 string `yaml:"tz" json:"tz"`

	// AdminUserIDs are this chat's cron-admins (may schedule reminders/jobs here).
	AdminUserIDs       []int64 `yaml:"admin_user_ids" json:"admin_user_ids"`
	GroupResponseMode  string  `yaml:"group_response_mode" json:"group_response_mode"`
	RecordGroupChatter bool    `yaml:"record_group_chatter" json:"record_group_chatter"`
	ImagesEnabled      bool    `yaml:"images_enabled" json:"images_enabled"` // accept photos (vision); default off

	MemoryRetentionDays int     `yaml:"memory_retention_days" json:"memory_retention_days"`
	RawRetentionDays    int     `yaml:"raw_retention_days" json:"raw_retention_days"`
	SessionTTLDays      int     `yaml:"session_ttl_days" json:"session_ttl_days"`
	RotateTurnCap       int     `yaml:"rotate_turn_cap" json:"rotate_turn_cap"`
	MaxBudgetUSD        float64 `yaml:"max_budget_usd" json:"max_budget_usd"`

	// MCP server allow-lists. Entries are server names (built-in "memory"/"reminders" plus any
	// registered external server). AllAllowedTools = available to everyone; AdminAllowedTools =
	// cron-admins only. Their union is the set of servers loaded into this chat's mcp.json.
	AllAllowedTools   []string `yaml:"all_allowed_tools" json:"all_allowed_tools"`
	AdminAllowedTools []string `yaml:"admin_allowed_tools" json:"admin_allowed_tools"`
}

// NewChat builds a chat config seeded from global defaults (disabled until configured).
func NewChat(g Global, name, ctype, botToken string) Chat {
	return Chat{
		Enabled:             false,
		Name:                name,
		Type:                ctype,
		BotToken:            botToken,
		Model:               g.DefaultModel,
		ConsolidationModel:  g.DefaultConsolidationModel,
		TZ:                  g.DefaultTZ,
		GroupResponseMode:   GroupModeMention,
		MemoryRetentionDays: g.DefaultMemoryRetentionDays,
		RawRetentionDays:    g.DefaultRawRetentionDays,
		SessionTTLDays:      g.DefaultSessionTTLDays,
		RotateTurnCap:       g.DefaultRotateTurnCap,
		MaxBudgetUSD:        g.MaxBudgetUSD,
		AllAllowedTools:     append([]string(nil), g.DefaultAllAllowedTools...),
		AdminAllowedTools:   append([]string(nil), g.DefaultAdminAllowedTools...),
	}
}

// Resolved fills empty string fields from global defaults (safety net for hand-edits).
func (c Chat) Resolved(g Global) Chat {
	if c.Model == "" {
		c.Model = g.DefaultModel
	}
	if c.ConsolidationModel == "" {
		c.ConsolidationModel = g.DefaultConsolidationModel
	}
	if c.TZ == "" {
		c.TZ = g.DefaultTZ
	}
	if c.GroupResponseMode == "" {
		c.GroupResponseMode = GroupModeMention
	}
	// Retention/TTL of 0 would mean "age out / prune everything immediately" — never a
	// valid intent, so treat 0 as unset and use the global default. (rotate_turn_cap is
	// left as-is: 0 legitimately means "never rotate on turns".)
	if c.MemoryRetentionDays <= 0 {
		c.MemoryRetentionDays = g.DefaultMemoryRetentionDays
	}
	if c.RawRetentionDays <= 0 {
		c.RawRetentionDays = g.DefaultRawRetentionDays
	}
	if c.SessionTTLDays <= 0 {
		c.SessionTTLDays = g.DefaultSessionTTLDays
	}
	// Allow-lists: backfill only when nil (never configured). An explicitly empty list means
	// the operator cleared it (no tools) and must be preserved.
	if c.AllAllowedTools == nil {
		c.AllAllowedTools = append([]string(nil), g.DefaultAllAllowedTools...)
	}
	if c.AdminAllowedTools == nil {
		c.AdminAllowedTools = append([]string(nil), g.DefaultAdminAllowedTools...)
	}
	return c
}

// ServersFor returns the MCP servers available to a speaker: the all-users list, plus the
// admin-only list when isAdmin. Order is stable (all-list first, then admin-only extras).
// Expects resolved config (allow-lists backfilled). Single source of truth for both the
// pre-approved tool set (brain.allowedTools) and the per-speaker "tools you may use" line.
func (c Chat) ServersFor(isAdmin bool) []string {
	out := append([]string(nil), c.AllAllowedTools...)
	if isAdmin {
		for _, s := range c.AdminAllowedTools {
			if !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
	}
	return out
}

// EnabledServers is the union of both allow-lists: the MCP servers loaded into this chat's
// mcp.json (which connects every server any speaker might use).
func (c Chat) EnabledServers() []string { return c.ServersFor(true) }

// IsCronAdmin reports whether a user may manage crons in this chat (chat-admin or, via
// the global config passed in, a global admin).
func (c Chat) IsCronAdmin(g Global, userID int64) bool {
	return g.IsGlobalAdmin(userID) || slices.Contains(c.AdminUserIDs, userID)
}

// LoadChat reads a chat.yaml (missing file => zero Chat, ok=false).
func LoadChat(path string) (Chat, bool, error) {
	var c Chat
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return c, false, nil
	}
	if err != nil {
		return c, false, err
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		return c, false, err
	}
	return c, true, nil
}

// SaveChat atomically writes a chat.yaml.
func SaveChat(path string, c Chat) error {
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
