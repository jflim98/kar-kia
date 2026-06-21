package chat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"assistant/internal/config"
)

func TestWriteChatMCP(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")

	reg := map[string]config.MCPServer{
		"weather": {Name: "weather", Command: "npx", Args: []string{"-y", "weather-mcp"}, Env: map[string]string{"K": "v"}},
	}
	lookup := func(name string) (config.MCPServer, bool) { s, ok := reg[name]; return s, ok }

	// memory (built-in HTTP) + weather (external stdio); "reminders" intentionally omitted, and
	// "ghost" refers to a server not in the registry (should be skipped silently).
	if err := writeChatMCP(path, "127.0.0.1:8766", []string{"memory", "weather", "ghost"}, lookup); err != nil {
		t.Fatal(err)
	}

	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mcp.json perm = %v, want 0600", fi.Mode().Perm())
	}

	var doc struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.MCPServers) != 2 {
		t.Fatalf("want 2 servers (memory, weather), got %v", doc.MCPServers)
	}
	mem := doc.MCPServers["memory"]
	if mem["type"] != "http" || mem["url"] != "http://127.0.0.1:8766/mcp/memory" {
		t.Fatalf("memory entry wrong: %v", mem)
	}
	wx := doc.MCPServers["weather"]
	if wx["type"] != "stdio" || wx["command"] != "npx" {
		t.Fatalf("weather entry wrong: %v", wx)
	}
	if _, ok := doc.MCPServers["ghost"]; ok {
		t.Fatal("unregistered server 'ghost' should be skipped")
	}
	if _, ok := doc.MCPServers["reminders"]; ok {
		t.Fatal("non-enabled 'reminders' should not appear")
	}
}
