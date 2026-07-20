# assistant

A **multi-tenant** Telegram assistant powered by **headless Claude** (`claude -p`).

Every chat (group or DM) is an **isolated tenant**: its own persona, memory (incl.
long-term), reminders/crons, models, cron-admins, and **bot token**. Nothing crosses
chat boundaries. You configure chats from a password-gated web UI dashboard. Runs as a
single small Go binary (plus the `claude` CLI) on a 1 GB Debian box.

## Core model

- **`chat_id` is the unit of isolation.** Each chat lives in `chats/<id>/` with its own
  config, persona, memory, sessions, and schedules.
- **Tokens are transport.** Global `bot_tokens` are the bots the daemon connects (one
  long-poll per unique token). Each chat picks one `bot_token` to send with; many chats
  can share a token, and a chat is served only by its designated token.
- **Unconfigured chats are inert.** When the bot is @mentioned in a new chat it replies
  `"This chat isn't configured yet (chat_id: …)"` — **no LLM, no memory** — and the chat
  appears in the dashboard for an admin to enable.

## Security

- **The model has NO filesystem tools — ever.** Every `claude` call runs with
  `--permission-mode default` (anything not allow-listed is denied in headless mode), an
  `--allowedTools` list of just `WebSearch` + this chat's MCP servers, and a
  `--disallowedTools` hard-block on the dangerous built-ins (`Bash`, `Read`, `Write`,
  `Edit`, `NotebookEdit`, `Glob`, `Grep`, `WebFetch`, `Task`) as defense-in-depth.
  Consolidation runs with no MCP server and an empty allow-list, so it's pure text in/out.
  (We deliberately do **not** pass `--tools` — on claude 2.1.181 it suppresses the deferred
  MCP tools; the gate is `--allowedTools` + `--permission-mode default` instead.) The daemon
  **injects** all memory and **performs all file writes**; the LLM only exchanges text.
  "ls everything / print secrets.yaml" gets nothing.
- **Per-chat isolation.** Memory, crons, and personas never leak across chats. Each chat's
  `mcp.json` is generated to expose only its enabled servers (`--strict-mcp-config`), and
  each tool call is gated by the chat's allow-lists — enforced both in the offered toolset
  (`--allowedTools`) and server-side in the MCP handler (`ToolAllowed`). Only a chat's
  **cron-admins** (its `admin_user_ids`, or a `global_admin_user_id`) can use admin-only
  servers (reminders by default).
- **Web UI is password-gated** (`webui_password`), localhost-bound, secrets write-only.
- **No per-chat rate limiting.** Replies spend your Claude subscription; set
  `max_budget_usd` and keep chats disabled until vetted.
- **Prompt injection is possible** — never rely on a persona for secrecy (no secrets are
  placed in prompts).

## Data layout

```
<data-dir>/
  config.yaml          GLOBAL: ports, concurrency, global_admin_user_ids, per-chat defaults
  secrets.yaml         GLOBAL: webui_password, bot_tokens, claude oauth token (0600)
  mcp_servers.yaml     GLOBAL: registered external MCP servers (local stdio), 0600
  registry.json        known chats (incl. unconfigured) with last-seen names
  chats/<chat_id>/
    chat.yaml          enabled, bot_token, model, tz, admin_user_ids, all/admin_allowed_tools, …
    persona.md
    mcp.json           generated: built-in memory/reminders/moderation + this chat's enabled externals (0600)
    schedules.json   sessions.json
    memory/
      long_term_memory.md
      daily_memory/{index.md, DD-MM-YY.md, _raw/DD-MM-YY.jsonl}
      users/<telegram_user_id>.md
```

The LLM has no file tools, so the daemon reads config/persona and injects them, and does
all memory writes. Global `config.yaml`/`secrets.yaml` are edited via the web UI; per-chat
`chat.yaml`/`persona.md` via each chat's page.

## What it does

- **Telegram**: per configured chat — reacts 👀 on a message it will answer, replies,
  then clears the reaction (😢 on error). DMs always reply; groups reply on
  @mention/reply (per the chat's `group_response_mode`).
- **Brain**: each chat gets its own persistent headless-Claude session (`--session-id`
  then `--resume`) so its persona + memory prefix stays cached. Built-in `WebSearch`.
- **Memory** (per chat, daemon-managed; the LLM only ever sees injected text):
  - `persona.md` + `memory/long_term_memory.md` — always injected.
  - `memory/daily_memory/{index.md, DD-MM-YY.md}` — dated summaries + an index of gists.
  - `memory/daily_memory/_raw/DD-MM-YY.jsonl` — raw log (consolidation source).
  - `memory/users/<id>.md` — per-user facts.
  - A nightly **1am** job per chat (or `assistant consolidate`) compacts the day's raw
    log into a dated note + index gist, and ages old notes into long-term memory. The
    daemon does all file I/O; Claude only summarizes the text it's handed.
- **MCP tools, per chat.** Three built-in servers (loopback HTTP, no subprocess):
  - **`memory`** — `recall_memory` (keyword/date search over older notes, long-term, and
    profiles; the daemon does the I/O) and `propose_memory`.
  - **`reminders`** — `schedule_reminder` / `list_reminders` / `cancel_reminder`; fired
    reminders run in that chat's session, delivered via its token.
  - **`moderation`** — `blacklist_user`; lets the assistant autonomously blacklist a
    persistently abusive/spamming user from that chat (admins and the DM owner can never
    be blacklisted; only the dashboard can undo it).

  Each chat picks which servers are available, and to whom, via two allow-lists
  (`all_allowed_tools` / `admin_allowed_tools`). Defaults: `memory` + `moderation` for
  everyone, `reminders` for cron-admins.
- **Blacklists.** Per-chat (`blacklisted_user_ids` in chat.yaml) and global (same key in
  config.yaml, all chats) — both editable in the web UI. A blacklisted sender's messages
  are dropped at the gateway edge: never recorded, never answered, never sent to claude.
- **External MCP servers (local stdio).** Operators register servers (command + args + env)
  in the web UI **or** via the `assistant mcp` CLI; per chat, add them to either allow-list.
  Each `claude` invocation spawns only that chat's enabled stdio servers (mind RAM on small
  boxes). The server's runtime (e.g. `node`/`npx`, `uv`/`uvx`) must be installed on the host.
- **Save with permission**: `propose_memory` asks the user to confirm via ✅/❌ buttons;
  only on approval is the fact written (to that chat's memory).
- **Web UI dashboard**: global settings, the MCP-server registry, and a chat list; click a
  chat to set its persona, model, token, cron-admins, tool allow-lists, and enable it —
  applied live.

## Quick start (local, macOS/Linux)

You need Go 1.25+ and the `claude` CLI **logged in** (`claude` once, interactively).

```sh
go build -o assistant ./cmd/assistant
./assistant init --data-dir ./data
# edit ./data/secrets.yaml  -> webui_password, bot_tokens: [ "123:ABC..." ]
./assistant run --data-dir ./data
# open http://127.0.0.1:8765, log in, set global_admin_user_ids, then:
#   DM the bot (or @mention it in a group) -> it appears in the dashboard -> open it,
#   set persona/model + its bot_token, and Enable.
```

Locally no Claude token is needed — `claude -p` uses your existing login.

## Telegram bot setup (@BotFather)

1. `/newbot` → get a **bot token** → add it to `secrets.yaml` `bot_tokens` (you can run
   several bots; add each token).
2. **Turn group privacy OFF** so the bot can see/remember group messages:
   `/setprivacy` → select the bot → **Disable** (then re-add it to existing groups).
3. Add the bot to a chat. @mention it → it replies "not configured yet" and shows up in
   the dashboard. Open that chat, set its `bot_token` (the one whose bot is in it) and
   persona, add cron-admins, and Enable.

## Web UI

The admin UI binds to `127.0.0.1:8765` and requires the `webui_password` from
`secrets.yaml`. Reach it safely from your machine with an SSH tunnel:

```sh
ssh -N -L 8765:127.0.0.1:8765 user@your-server   # then open http://127.0.0.1:8765
```

Secrets are **write-only** in the UI (it shows only whether each is set). Changes apply
live without a restart (a bot-token change reconnects Telegram; a concurrency change
needs a restart). To expose the UI directly instead of tunneling, set
`webui_addr: 0.0.0.0:8765` in `config.yaml` and publish the port — only behind a
firewall / TLS reverse proxy, never raw on the internet.

## Deploy with Docker (Debian server, ~1 GB RAM)

On a headless server there's no interactive login, so create a long-lived subscription
token once (on a machine where you're logged in):

```sh
claude setup-token        # prints a token; keep it secret
```

Build and run:

```sh
docker build -t assistant .
docker volume create assistant-data

docker run -d --name assistant --restart unless-stopped \
  -v assistant-data:/data \
  -e BOT_TOKENS=123:abc,456:def \
  -e WEBUI_PASSWORD=choose-a-strong-one \
  -e CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat-... \
  assistant
```

`BOT_TOKENS` is comma-separated (merged into `bot_tokens`). On first run it auto-scaffolds
`/data`; then configure chats from the web UI. Env vars override the matching secrets each
start.

**RAM note:** each reply spawns a `claude -p` (Node) process, the heavy memory user.
`concurrency` defaults to **1** — keep it at 1–2 on a 1 GB box. Passive group logging
makes no Claude call, so cost/limits scale with replies, not message volume.

## Deploy to a GCP VM without Docker (bare binary)

Lighter than Docker on a 1 GB box: cross-compile the static binary, install the `claude`
CLI on the VM, and run it under systemd. The VM needs **Node + the `claude` CLI** because
the app shells out to `claude -p`.

Two auth options on the server:
- **Already logged in** (`claude` interactively, or `claude setup-token` applied): leave
  `claude_code_oauth_token` **blank** — the existing `~/.claude` login is used. The service
  must then run **as that same user** with `HOME` pointing at their home dir.
- **Token**: set `CLAUDE_CODE_OAUTH_TOKEN` (env or `secrets.yaml`).

> Setting `claude_code_oauth_token` to a stale/placeholder value **overrides** a working
> login and makes every reply fail with an auth error. Blank it unless it's a real token.

### 1. Build (on your machine)

GCP VMs are amd64 unless you chose the T2A (Arm) family — check `uname -m` on the VM
(`x86_64` → amd64, `aarch64` → arm64):

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o assistant-linux ./cmd/assistant
gcloud compute scp assistant-linux VM_NAME:~/ --zone=YOUR_ZONE
```

### 2. Prepare the VM (`gcloud compute ssh VM_NAME --zone=YOUR_ZONE`)

```sh
whoami                                   # the user you'll run as (USER below)
claude --version                         # confirm the CLI is installed + logged in

# If the claude CLI isn't installed yet:
#   curl -fsSL https://deb.nodesource.com/setup_22.x | sudo -E bash -
#   sudo apt-get install -y nodejs && sudo npm install -g @anthropic-ai/claude-code

# Swap is important on a 1 GB VM — a claude -p spike can otherwise trigger an OOM kill:
sudo fallocate -l 2G /swapfile && sudo chmod 600 /swapfile
sudo mkswap /swapfile && sudo swapon /swapfile
echo '/swapfile none swap sw 0 0' | sudo tee -a /etc/fstab

sudo mv ~/assistant-linux /usr/local/bin/assistant && sudo chmod +x /usr/local/bin/assistant
```

### 3. Configure

```sh
assistant init --data-dir ~/asst-data
```
Edit `~/asst-data/secrets.yaml` (`webui_password`, `bot_tokens`, and on a headless server
`claude_code_oauth_token`). Smoke-test: `assistant run --data-dir ~/asst-data`, then
configure chats via the web UI; `Ctrl-C` when done.

### 4. Run under systemd

Replace **USER** (three places) with your `whoami`. The `HOME` line is required so the
`claude` CLI finds `~/.claude` — without it, replies fail with auth errors.

```ini
# /etc/systemd/system/assistant.service
[Unit]
Description=Telegram assistant
After=network-online.target
Wants=network-online.target

[Service]
User=USER
Environment=HOME=/home/USER
Environment=ASSISTANT_DATA_DIR=/home/USER/asst-data
ExecStart=/usr/local/bin/assistant run
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now assistant
journalctl -u assistant -f
```

### 5. Reach the web UI (no firewall changes)

The admin UI stays on `127.0.0.1` — tunnel to it instead of exposing a port:

```sh
gcloud compute ssh VM_NAME --zone=YOUR_ZONE -- -L 8765:127.0.0.1:8765
# then open http://127.0.0.1:8765 and log in with webui_password
```

**Common pitfalls:** (1) systemd `HOME` not set → `claude` can't find its login → every
reply errors. (2) Turn **BotFather privacy mode OFF** for group messages, and set a chat's
`bot_token` to the bot that's actually in it.

## Configuration reference

**Global** (`config.yaml`; secrets in `secrets.yaml`, mode 0600):

| key | meaning |
| --- | --- |
| `concurrency` | max concurrent `claude -p` across all chats (restart to change) |
| `max_budget_usd` | per-call spend cap (0 = off) |
| `global_admin_user_ids` | operators; implicitly cron-admins in every chat |
| `blacklisted_user_ids` | Telegram user ids ignored in ALL chats (dropped before claude) |
| `default_model` / `default_consolidation_model` / `default_tz` | seeded into new chats |
| `default_memory_retention_days` / `default_raw_retention_days` / `default_session_ttl_days` / `default_rotate_turn_cap` | new-chat defaults |
| `default_all_allowed_tools` / `default_admin_allowed_tools` | new-chat tool allow-lists (default `[memory, moderation]` / `[reminders]`) |
| `webui_addr` / `mcp_addr` | admin UI / internal MCP listen addresses |
| secrets: `webui_password`, `bot_tokens`, `claude_code_oauth_token` | env: `WEBUI_PASSWORD`, `BOT_TOKENS`, `CLAUDE_CODE_OAUTH_TOKEN` |
| `mcp_servers.yaml` | registered external MCP servers (managed in the web UI; 0600) |

**Per-chat** (`chats/<id>/chat.yaml`, edited via the dashboard):

| key | meaning |
| --- | --- |
| `enabled` | the bot only acts in enabled chats |
| `bot_token` | the bot used to send here (must be a bot that's in this chat) |
| `model` / `consolidation_model` / `tz` | overrides; empty = global default |
| `admin_user_ids` | this chat's cron-admins (may use admin-only servers) |
| `blacklisted_user_ids` | Telegram user ids ignored in THIS chat; the assistant may append via `blacklist_user` |
| `group_response_mode` | `mention` / `reply` / `all` |
| `record_group_chatter` | `false` = act only on @mentions/replies |
| `all_allowed_tools` / `admin_allowed_tools` | MCP servers available to everyone / cron-admins (names: `memory`, `reminders`, `moderation`, + registered externals) |
| `images_enabled` | accept photos for vision (default off) |
| `max_budget_usd` | per-call spend cap for this chat (overrides the global default; 0 = off) |
| `memory_retention_days` / `raw_retention_days` / `session_ttl_days` / `rotate_turn_cap` | per-chat tuning (`rotate_turn_cap: 0` = never) |

## Adding external MCP servers

External servers are **local stdio** (a command on the host). Make sure its runtime is
installed on the VM first (`node`/`npx`, `uv`/`uvx`, …) — `claude` launches it as a subprocess.

Register it, then enable it per chat (web UI → chat → Tools → all-users / admins-only).

```sh
# Register by hand:
assistant mcp add everything --command npx --arg -y --arg @modelcontextprotocol/server-everything
assistant mcp add notion --command npx --arg -y --arg @some/notion-mcp --env NOTION_TOKEN=secret

# Or import from an existing Claude config (e.g. after `claude mcp add …`):
assistant mcp import                 # reads ~/.claude.json (or ./.mcp.json)
assistant mcp import --from ./.mcp.json --overwrite

assistant mcp ls                     # list registered servers
assistant mcp rm everything          # remove one
```

CLI edits write `mcp_servers.yaml` and need a **daemon restart** to apply (the web UI applies
live). Only local stdio entries are imported; HTTP/SSE servers and the reserved `memory` /
`reminders` / `moderation` names are skipped. `claude mcp add` registers with *Claude Code*, not the
assistant (we run `--strict-mcp-config`), which is exactly why `import` exists.

## Development

```sh
go test ./...                 # unit tests
CLAUDE_IT=1 go test ./internal/brain/   # live end-to-end test (uses the claude CLI)
```
