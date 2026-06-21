// Package initcmd scaffolds the global data directory. Per-chat directories
// (chats/<id>/) are created on demand by the chat manager / web UI, so init only lays
// down the global files.
package initcmd

import (
	"fmt"
	"os"
	"path/filepath"
)

// Run scaffolds dataDir idempotently (creates only what's missing). Layout:
//
//	<dataDir>/config.yaml       global config
//	<dataDir>/secrets.yaml      global secrets (0600)
//	<dataDir>/mcp_servers.yaml  registered external MCP servers (0600)
//	<dataDir>/registry.json     known-chats list
//	<dataDir>/chats/            per-chat tenants (created on demand; mcp.json generated per chat)
func Run(dataDir string) ([]string, error) {
	var created []string

	for _, d := range []string{dataDir, filepath.Join(dataDir, "chats")} {
		if madeNew, err := ensureDir(d); err != nil {
			return created, err
		} else if madeNew {
			created = append(created, d+"/")
		}
	}

	files := []struct {
		path    string
		content string
		perm    os.FileMode
	}{
		{filepath.Join(dataDir, "config.yaml"), configTemplate, 0o644},
		{filepath.Join(dataDir, "secrets.yaml"), secretsTemplate, 0o600},
		{filepath.Join(dataDir, "mcp_servers.yaml"), mcpServersTemplate, 0o600},
		{filepath.Join(dataDir, "registry.json"), "[]\n", 0o644},
	}
	for _, f := range files {
		if madeNew, err := writeIfMissing(f.path, f.content, f.perm); err != nil {
			return created, err
		} else if madeNew {
			created = append(created, f.path)
		}
	}
	return created, nil
}

// NextSteps returns a short human-readable summary printed after init.
func NextSteps(dataDir string) string {
	return fmt.Sprintf(`Next steps:
  1. Edit %[1]s/secrets.yaml -> set webui_password and bot_tokens
     (on a server, also set claude_code_oauth_token from 'claude setup-token').
  2. Run: assistant run --data-dir %[1]s
  3. Open the web UI, set global_admin_user_ids, then add the bot to chats. Each new
     chat appears in the dashboard; open it to set its persona/model and enable it.
`, dataDir)
}

func ensureDir(path string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	return true, os.MkdirAll(path, 0o755)
}

func writeIfMissing(path, content string, perm os.FileMode) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	return true, os.WriteFile(path, []byte(content), perm)
}
