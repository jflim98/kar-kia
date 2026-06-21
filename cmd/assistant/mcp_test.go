package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadMCPConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude.json")
	doc := `{
      "mcpServers": {
        "everything": {"type":"stdio","command":"npx","args":["-y","srv"]},
        "remote":     {"type":"http","url":"https://x/mcp"},
        "memory":     {"command":"nope"}
      },
      "projects": {
        "/p": {"mcpServers": {"notes": {"command":"uvx","args":["notes"],"env":{"T":"1"}}}}
      }
    }`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := readMCPConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	// Only the two local stdio servers; http + built-in "memory" are skipped.
	if len(got) != 2 {
		t.Fatalf("want 2 stdio servers, got %d: %+v", len(got), got)
	}
	byName := map[string]bool{}
	for _, s := range got {
		byName[s.Name] = true
	}
	if !byName["everything"] || !byName["notes"] {
		t.Fatalf("expected everything+notes, got %+v", got)
	}
	if byName["remote"] || byName["memory"] {
		t.Fatalf("http/built-in must be skipped, got %+v", got)
	}
}

func TestParseEnvAndUpsert(t *testing.T) {
	env, err := parseEnv([]string{"A=1", "B=x=y"})
	if err != nil {
		t.Fatal(err)
	}
	if env["A"] != "1" || env["B"] != "x=y" {
		t.Fatalf("parseEnv wrong: %v", env)
	}
	if _, err := parseEnv([]string{"bad"}); err == nil {
		t.Fatal("parseEnv should reject a value without '='")
	}
}
