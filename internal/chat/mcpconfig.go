package chat

import (
	"encoding/json"
	"os"

	"assistant/internal/config"
)

// writeChatMCP generates a chat's mcp.json (read by `claude --mcp-config`). It emits one entry
// per enabled server: built-in servers (memory/reminders) as HTTP entries pointing at the
// daemon's loopback endpoint, and registered external servers as local stdio entries. With
// --strict-mcp-config, claude connects to exactly these — so a chat sees only its enabled set,
// and only its enabled stdio servers ever spawn. Written 0600 (external env may hold secrets).
func writeChatMCP(path, mcpAddr string, servers []string, lookup func(string) (config.MCPServer, bool)) error {
	type httpEntry struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	}
	type stdioEntry struct {
		Type    string            `json:"type"`
		Command string            `json:"command"`
		Args    []string          `json:"args,omitempty"`
		Env     map[string]string `json:"env,omitempty"`
	}

	entries := map[string]any{}
	for _, name := range servers {
		if p, ok := config.BuiltinMCPServers[name]; ok {
			entries[name] = httpEntry{Type: "http", URL: "http://" + mcpAddr + p}
			continue
		}
		if def, ok := lookup(name); ok {
			entries[name] = stdioEntry{Type: "stdio", Command: def.Command, Args: def.Args, Env: def.Env}
		}
		// Unknown name (registry entry since removed): skip — the server simply isn't offered.
	}

	b, err := json.MarshalIndent(map[string]any{"mcpServers": entries}, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
