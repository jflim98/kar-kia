package config

import (
	"slices"
	"testing"
)

func TestMCPServerRegistryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.MCPServers()) != 0 {
		t.Fatalf("expected empty registry, got %v", m.MCPServers())
	}

	want := []MCPServer{
		{Name: "weather", Command: "npx", Args: []string{"-y", "weather-mcp"}, Env: map[string]string{"API_KEY": "x"}},
		{Name: "notes", Command: "uvx", Args: []string{"notes-mcp"}},
	}
	if err := m.SetMCPServers(want); err != nil {
		t.Fatal(err)
	}

	// Persisted to disk: a fresh Load sees them.
	m2, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := m2.MCPServers(); len(got) != 2 || got[0].Name != "weather" || got[0].Env["API_KEY"] != "x" {
		t.Fatalf("registry did not round-trip: %v", got)
	}
	if s, ok := m2.MCPServerByName("notes"); !ok || s.Command != "uvx" {
		t.Fatalf("MCPServerByName(notes) failed: %v %v", s, ok)
	}
	if _, ok := m2.MCPServerByName("missing"); ok {
		t.Fatal("MCPServerByName should miss for an unknown name")
	}
}

func TestResolvedAllowListBackfill(t *testing.T) {
	g := DefaultGlobal() // all=[memory], admin=[reminders]

	// nil (never configured) -> backfilled to defaults.
	got := Chat{}.Resolved(g)
	if !slices.Equal(got.AllAllowedTools, []string{ServerMemory}) {
		t.Fatalf("nil all-list should backfill to defaults, got %v", got.AllAllowedTools)
	}
	if !slices.Equal(got.AdminAllowedTools, []string{ServerReminders}) {
		t.Fatalf("nil admin-list should backfill to defaults, got %v", got.AdminAllowedTools)
	}

	// Explicitly empty (operator cleared) -> preserved, NOT backfilled.
	cleared := Chat{AllAllowedTools: []string{}, AdminAllowedTools: []string{}}.Resolved(g)
	if len(cleared.AllAllowedTools) != 0 || len(cleared.AdminAllowedTools) != 0 {
		t.Fatalf("explicit empty lists must be preserved, got all=%v admin=%v",
			cleared.AllAllowedTools, cleared.AdminAllowedTools)
	}
}

func TestEveryBuiltinServerHasDescription(t *testing.T) {
	for name := range BuiltinMCPServers {
		if ServerDescription(name) == "" {
			t.Fatalf("built-in server %q has no description — add one to serverDescriptions", name)
		}
	}
}

func TestServersForAdminGating(t *testing.T) {
	c := Chat{AllAllowedTools: []string{"memory"}, AdminAllowedTools: []string{"reminders"}}
	if got := c.ServersFor(false); !slices.Equal(got, []string{"memory"}) {
		t.Fatalf("non-admin should see only the all-list, got %v", got)
	}
	if got := c.ServersFor(true); !slices.Contains(got, "memory") || !slices.Contains(got, "reminders") {
		t.Fatalf("admin should see both lists, got %v", got)
	}
}

func TestEnabledServersUnion(t *testing.T) {
	c := Chat{AllAllowedTools: []string{"memory", "weather"}, AdminAllowedTools: []string{"reminders", "weather"}}
	got := c.EnabledServers()
	for _, want := range []string{"memory", "weather", "reminders"} {
		if !slices.Contains(got, want) {
			t.Fatalf("EnabledServers missing %q: %v", want, got)
		}
	}
	if len(got) != 3 {
		t.Fatalf("EnabledServers should dedupe to 3, got %v", got)
	}
}
