package webui

import (
	"encoding/json"
	"net/http"
	"strconv"

	"assistant/internal/config"
)

// --- global settings ---

type globalView struct {
	Concurrency                int     `json:"concurrency"`
	MaxBudgetUSD               float64 `json:"max_budget_usd"`
	GlobalAdminUserIDs         []int64 `json:"global_admin_user_ids"`
	DefaultModel               string  `json:"default_model"`
	DefaultConsolidationModel  string  `json:"default_consolidation_model"`
	DefaultEffort              string  `json:"default_effort"`
	DefaultConsolidationEffort string  `json:"default_consolidation_effort"`
	DefaultTZ                  string  `json:"default_tz"`
	DefaultMemoryRetentionDays int     `json:"default_memory_retention_days"`
	DefaultSessionTTLDays      int     `json:"default_session_ttl_days"`
	DefaultRotateTurnCap       int     `json:"default_rotate_turn_cap"`

	// Static UI metadata (read-only): the choices the model/effort dropdowns are built from, so
	// the option lists live in Go (config.ModelAliases / config.EffortLevels) as the single source.
	ModelOptions  []string `json:"model_options"`
	EffortOptions []string `json:"effort_options"`

	// Bot tokens are shown (so edits round-trip and don't reset). The webui password and
	// oauth token stay write-only (more sensitive) — only their presence is reported.
	BotTokens        []string `json:"bot_tokens"`
	WebUIPasswordSet bool     `json:"webui_password_set"`
	OAuthTokenSet    bool     `json:"claude_code_oauth_token_set"`
}

type globalPatch struct {
	Concurrency                *int     `json:"concurrency"`
	MaxBudgetUSD               *float64 `json:"max_budget_usd"`
	GlobalAdminUserIDs         *[]int64 `json:"global_admin_user_ids"`
	DefaultModel               *string  `json:"default_model"`
	DefaultConsolidationModel  *string  `json:"default_consolidation_model"`
	DefaultEffort              *string  `json:"default_effort"`
	DefaultConsolidationEffort *string  `json:"default_consolidation_effort"`
	DefaultTZ                  *string  `json:"default_tz"`
	DefaultMemoryRetentionDays *int     `json:"default_memory_retention_days"`
	DefaultSessionTTLDays      *int     `json:"default_session_ttl_days"`
	DefaultRotateTurnCap       *int     `json:"default_rotate_turn_cap"`

	// Secrets (write-only; applied only when non-empty / non-nil).
	WebUIPassword        *string   `json:"webui_password"`
	BotTokens            *[]string `json:"bot_tokens"`
	ClaudeCodeOAuthToken *string   `json:"claude_code_oauth_token"`
}

func (s *Server) handleGlobal(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		c, sec := s.cfg.Get(), s.cfg.Secrets()
		writeJSON(w, globalView{
			Concurrency:                c.Concurrency,
			MaxBudgetUSD:               c.MaxBudgetUSD,
			GlobalAdminUserIDs:         c.GlobalAdminUserIDs,
			DefaultModel:               c.DefaultModel,
			DefaultConsolidationModel:  c.DefaultConsolidationModel,
			DefaultEffort:              c.DefaultEffort,
			DefaultConsolidationEffort: c.DefaultConsolidationEffort,
			DefaultTZ:                  c.DefaultTZ,
			DefaultMemoryRetentionDays: c.DefaultMemoryRetentionDays,
			DefaultSessionTTLDays:      c.DefaultSessionTTLDays,
			DefaultRotateTurnCap:       c.DefaultRotateTurnCap,
			ModelOptions:               config.ModelAliases,
			EffortOptions:              config.EffortLevels,
			BotTokens:                  sec.BotTokens,
			WebUIPasswordSet:           sec.WebUIPassword != "",
			OAuthTokenSet:              sec.ClaudeCodeOAuthToken != "",
		})
	case http.MethodPatch, http.MethodPost:
		var p globalPatch
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		err := s.cfg.Update(func(c *config.Global, sec *config.Secrets) {
			setInt(&c.Concurrency, p.Concurrency)
			setFloat(&c.MaxBudgetUSD, p.MaxBudgetUSD)
			if p.GlobalAdminUserIDs != nil {
				c.GlobalAdminUserIDs = *p.GlobalAdminUserIDs
			}
			setStr(&c.DefaultModel, p.DefaultModel)
			setStr(&c.DefaultConsolidationModel, p.DefaultConsolidationModel)
			setStr(&c.DefaultEffort, p.DefaultEffort)
			setStr(&c.DefaultConsolidationEffort, p.DefaultConsolidationEffort)
			setStr(&c.DefaultTZ, p.DefaultTZ)
			setInt(&c.DefaultMemoryRetentionDays, p.DefaultMemoryRetentionDays)
			setInt(&c.DefaultSessionTTLDays, p.DefaultSessionTTLDays)
			setInt(&c.DefaultRotateTurnCap, p.DefaultRotateTurnCap)
			if p.WebUIPassword != nil && *p.WebUIPassword != "" {
				sec.WebUIPassword = *p.WebUIPassword
			}
			if p.BotTokens != nil {
				sec.BotTokens = *p.BotTokens
			}
			if p.ClaudeCodeOAuthToken != nil && *p.ClaudeCodeOAuthToken != "" {
				sec.ClaudeCodeOAuthToken = *p.ClaudeCodeOAuthToken
			}
		})
		if err != nil {
			http.Error(w, "save failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		s.handleGlobal(w, &http.Request{Method: http.MethodGet})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// --- external MCP server registry ---

// handleMCPServers GETs the registered external MCP servers, or PUTs a replacement list. A PUT
// persists the registry and regenerates every active chat's mcp.json so the change takes effect.
func (s *Server) handleMCPServers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		servers := s.cfg.MCPServers()
		if servers == nil {
			servers = []config.MCPServer{}
		}
		writeJSON(w, servers)
	case http.MethodPut, http.MethodPost:
		var servers []config.MCPServer
		if err := json.NewDecoder(r.Body).Decode(&servers); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		for i, sv := range servers {
			if sv.Name == "" || sv.Command == "" {
				http.Error(w, "server #"+strconv.Itoa(i+1)+": name and command are required", http.StatusBadRequest)
				return
			}
			if config.IsBuiltinServer(sv.Name) {
				http.Error(w, "server #"+strconv.Itoa(i+1)+": '"+sv.Name+"' is a reserved built-in name", http.StatusBadRequest)
				return
			}
		}
		if err := s.cfg.SetMCPServers(servers); err != nil {
			http.Error(w, "save failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		s.chats.RegenerateMCPConfigs()
		writeJSON(w, servers)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// --- chat list ---

func (s *Server) handleChats(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.chats.List())
}

// --- per-chat config + persona ---

type chatGet struct {
	Config  config.Chat `json:"config"`
	Persona string      `json:"persona"`
}

type chatSave struct {
	Config  *config.Chat `json:"config"`
	Persona *string      `json:"persona"`
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad chat id", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet:
		cfg, _ := s.chats.LoadChatConfig(id)
		persona, _ := s.chats.Persona(id)
		writeJSON(w, chatGet{Config: cfg, Persona: persona})
	case http.MethodPatch, http.MethodPost:
		var body chatSave
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if body.Persona != nil {
			if err := s.chats.SavePersona(id, *body.Persona); err != nil {
				http.Error(w, "save persona: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		if body.Config != nil {
			if err := s.chats.SaveChatConfig(id, *body.Config); err != nil {
				http.Error(w, "save config: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		cfg, _ := s.chats.LoadChatConfig(id)
		persona, _ := s.chats.Persona(id)
		writeJSON(w, chatGet{Config: cfg, Persona: persona})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func setStr(dst, src *string) {
	if src != nil {
		*dst = *src
	}
}
func setInt(dst, src *int) {
	if src != nil {
		*dst = *src
	}
}
func setFloat(dst, src *float64) {
	if src != nil {
		*dst = *src
	}
}
