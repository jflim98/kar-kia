package initcmd

import (
	"os"
	"path/filepath"
	"testing"
)

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestRunScaffoldsGlobalLayout(t *testing.T) {
	dir := t.TempDir()
	if _, err := Run(dir); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"config.yaml", "secrets.yaml", "mcp_servers.yaml", "registry.json", "chats"} {
		if !exists(filepath.Join(dir, p)) {
			t.Fatalf("missing %s", p)
		}
	}
	for _, p := range []string{"secrets.yaml", "mcp_servers.yaml"} {
		fi, _ := os.Stat(filepath.Join(dir, p))
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s perm = %v, want 0600", p, fi.Mode().Perm())
		}
	}

	created, err := Run(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 0 {
		t.Fatalf("re-run should be idempotent, created %v", created)
	}
}
