package initcmd

const configTemplate = `# Global assistant configuration. Per-chat settings (persona, model, crons, memory)
# live under chats/<id>/ and are edited via the web UI dashboard.

webui_addr: 127.0.0.1:8765    # admin UI listen address, use 0.0.0.0 to expose to public on gcp, remember to enable port
mcp_addr: 127.0.0.1:8766      # HTTP MCP server for the headless claude (loopback)
concurrency: 1                # max concurrent 'claude -p' across ALL chats (1GB box -> 1)
max_budget_usd: 0             # per-call spend cap (0 = disabled)

global_admin_user_ids: []     # operators: implicitly cron-admins in every chat
blacklisted_user_ids: []      # telegram user ids ignored in ALL chats (per-chat lists live in chat.yaml)

# Defaults applied to each new chat (overridable per-chat in the dashboard):
default_model: sonnet
default_consolidation_model: opus
default_tz: Asia/Singapore
default_memory_retention_days: 14
default_raw_retention_days: 14
default_session_ttl_days: 2
default_rotate_turn_cap: 0   # rotate a chat session after N turns (0 = never)
`

const secretsTemplate = `# Global secrets (mode 0600). Do not commit. Env overrides on startup:
#   WEBUI_PASSWORD, CLAUDE_CODE_OAUTH_TOKEN, BOT_TOKENS (comma-separated, merged)

webui_password: ""            # password for the admin web UI login
bot_tokens: []                # bot tokens the daemon connects (one long-poll each).
                              # A chat's bot_token (set per-chat) should be one of these.
# claude_code_oauth_token: "" # uncomment + set on a headless server ('claude setup-token').
                              # Leave commented when 'claude' is already logged in.
`

const mcpServersTemplate = `# Registered external MCP servers (local stdio only). Managed via the web UI; env may hold
# secrets, so this file is mode 0600. Each chat enables a subset via its all_allowed_tools /
# admin_allowed_tools lists. The built-in "memory", "reminders" and "moderation" servers are always available
# and need no entry here. Example:
#
# - name: everything
#   command: npx
#   args: ["-y", "@modelcontextprotocol/server-everything"]
#   env: {}
[]
`
