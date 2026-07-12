# syntax=docker/dockerfile:1

# --- build the static Go binary ---
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/assistant ./cmd/assistant

# --- runtime: Node (for the claude CLI) + the Go binary ---
FROM node:22-bookworm-slim

# claude CLI needs HTTPS; tini gives us clean signal handling (PID 1).
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tini \
 && rm -rf /var/lib/apt/lists/* \
 && npm install -g @anthropic-ai/claude-code \
 && npm cache clean --force

COPY --from=build /out/assistant /usr/local/bin/assistant

# Data dir (config, secrets, memory, sessions) — mount a volume here to persist it.
ENV ASSISTANT_DATA_DIR=/data
VOLUME ["/data"]

# Admin web UI. It binds to 127.0.0.1 by default (reach it via SSH tunnel). To publish
# it, set webui_addr: 0.0.0.0:8765 in config.yaml and publish this port.
EXPOSE 8765

# Auth on a headless server: provide a subscription token. Locally you don't need this.
#   docker run -e CLAUDE_CODE_OAUTH_TOKEN=... -e TELEGRAM_BOT_TOKEN=... -e WEBUI_PASSWORD=...
ENTRYPOINT ["/usr/bin/tini", "--", "assistant"]
CMD ["run"]
