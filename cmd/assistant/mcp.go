package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"assistant/internal/config"
)

// cmdMCP dispatches the external-MCP-registry subcommands. These edit mcp_servers.yaml in the
// data dir; a running daemon only picks the change up on restart (or a re-save in the web UI).
func cmdMCP(args []string) {
	if len(args) == 0 {
		mcpUsage()
		os.Exit(2)
	}
	switch args[0] {
	case "ls", "list":
		cmdMCPList(args[1:])
	case "add":
		cmdMCPAdd(args[1:])
	case "import":
		cmdMCPImport(args[1:])
	case "rm", "remove":
		cmdMCPRemove(args[1:])
	case "-h", "--help", "help":
		mcpUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown mcp subcommand %q\n\n", args[0])
		mcpUsage()
		os.Exit(2)
	}
}

func mcpUsage() {
	fmt.Fprint(os.Stderr, `assistant mcp - manage the external MCP server registry (local stdio servers)

Usage:
  assistant mcp ls                      list registered external servers (alias: list)
  assistant mcp add NAME --command CMD [--arg A]... [--env K=V]...
  assistant mcp import [--from FILE] [--overwrite]   import from a Claude/MCP config
  assistant mcp rm NAME                 remove a registered server (alias: remove)

Common flags: --data-dir DIR (default $ASSISTANT_DATA_DIR or ./data)

After any change, restart the daemon (or re-save MCP servers in the web UI) to apply,
then enable the server per chat via the web UI's Tools list.
`)
}

// stringSlice is a repeatable string flag (e.g. --arg X --arg Y).
type stringSlice []string

func (s *stringSlice) String() string     { return strings.Join(*s, " ") }
func (s *stringSlice) Set(v string) error { *s = append(*s, v); return nil }

func addDataDirFlag(fs *flag.FlagSet) *string {
	def := os.Getenv("ASSISTANT_DATA_DIR")
	if def == "" {
		def = "data"
	}
	return fs.String("data-dir", def, "data directory")
}

func absDir(s string) string {
	if abs, err := filepath.Abs(s); err == nil {
		return abs
	}
	return s
}

// loadRegistry loads the current registry via the config manager (defaults if files missing).
func loadRegistry(dir string) (*config.Manager, []config.MCPServer) {
	m, err := config.Load(dir)
	if err != nil {
		log.Fatalf("load config from %s: %v", dir, err)
	}
	return m, m.MCPServers()
}

// saveRegistry persists the registry and prints the apply reminder.
func saveRegistry(m *config.Manager, dir string, servers []config.MCPServer) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Fatalf("create %s: %v", dir, err)
	}
	if err := m.SetMCPServers(servers); err != nil {
		log.Fatalf("save mcp_servers.yaml: %v", err)
	}
	fmt.Printf("Saved %s/mcp_servers.yaml (%d server(s)).\n", dir, len(servers))
	fmt.Println("Restart the daemon (or re-save MCP servers in the web UI) to apply, then enable it per chat.")
}

func cmdMCPList(args []string) {
	fs := flag.NewFlagSet("mcp ls", flag.ExitOnError)
	dir := addDataDirFlag(fs)
	_ = fs.Parse(args)
	_, servers := loadRegistry(absDir(*dir))
	if len(servers) == 0 {
		fmt.Println("No external MCP servers registered. (Built-in 'memory', 'reminders' and 'moderation' are always available.)")
		return
	}
	for _, s := range servers {
		line := s.Name + "\t" + s.Command + " " + strings.Join(s.Args, " ")
		if len(s.Env) > 0 {
			keys := make([]string, 0, len(s.Env))
			for k := range s.Env {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			line += "  (env: " + strings.Join(keys, ",") + ")"
		}
		fmt.Println(line)
	}
}

func cmdMCPAdd(args []string) {
	// Allow a leading positional NAME: `mcp add weather --command npx`.
	var name string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("mcp add", flag.ExitOnError)
	dir := addDataDirFlag(fs)
	nameFlag := fs.String("name", "", "server name")
	command := fs.String("command", "", "command to launch the server (e.g. npx)")
	var argv, envv stringSlice
	fs.Var(&argv, "arg", "an argument to the command (repeatable)")
	fs.Var(&envv, "env", "an env var as KEY=VALUE (repeatable)")
	_ = fs.Parse(args)
	if name == "" {
		name = *nameFlag
	}

	if name == "" || *command == "" {
		log.Fatal("mcp add: NAME and --command are required")
	}
	if config.IsBuiltinServer(name) {
		log.Fatalf("mcp add: %q is a reserved built-in server name", name)
	}
	env, err := parseEnv(envv)
	if err != nil {
		log.Fatalf("mcp add: %v", err)
	}

	m, servers := loadRegistry(absDir(*dir))
	servers = upsert(servers, config.MCPServer{Name: name, Command: *command, Args: argv, Env: env})
	saveRegistry(m, absDir(*dir), servers)
}

func cmdMCPRemove(args []string) {
	var name string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("mcp rm", flag.ExitOnError)
	dir := addDataDirFlag(fs)
	_ = fs.Parse(args)
	if name == "" {
		log.Fatal("mcp rm: NAME is required")
	}
	m, servers := loadRegistry(absDir(*dir))
	out := servers[:0]
	found := false
	for _, s := range servers {
		if s.Name == name {
			found = true
			continue
		}
		out = append(out, s)
	}
	if !found {
		log.Fatalf("mcp rm: no registered server named %q", name)
	}
	saveRegistry(m, absDir(*dir), out)
}

// cmdMCPImport seeds the registry from a Claude/MCP config (the `{ "mcpServers": {...} }`
// shape, as written by `claude mcp add` into ~/.claude.json, or any .mcp.json). Only local
// stdio servers are imported; HTTP/SSE entries and built-in names are skipped with a note.
func cmdMCPImport(args []string) {
	fs := flag.NewFlagSet("mcp import", flag.ExitOnError)
	dir := addDataDirFlag(fs)
	from := fs.String("from", "", "config file to import (default: ~/.claude.json, then ./.mcp.json)")
	overwrite := fs.Bool("overwrite", false, "replace existing registry entries with the same name")
	_ = fs.Parse(args)

	path := *from
	if path == "" {
		path = defaultImportPath()
		if path == "" {
			log.Fatal("mcp import: no --from given and neither ~/.claude.json nor ./.mcp.json found")
		}
	}
	imported, err := readMCPConfig(path)
	if err != nil {
		log.Fatalf("mcp import: %v", err)
	}
	if len(imported) == 0 {
		fmt.Printf("No local stdio MCP servers found in %s.\n", path)
		return
	}

	m, servers := loadRegistry(absDir(*dir))
	existing := map[string]bool{}
	for _, s := range servers {
		existing[s.Name] = true
	}
	var added, updated, skipped []string
	for _, s := range imported {
		switch {
		case existing[s.Name] && !*overwrite:
			skipped = append(skipped, s.Name)
		case existing[s.Name]:
			servers = upsert(servers, s)
			updated = append(updated, s.Name)
		default:
			servers = append(servers, s)
			added = append(added, s.Name)
		}
	}
	fmt.Printf("Imported from %s: %d added, %d updated, %d skipped.\n", path, len(added), len(updated), len(skipped))
	if len(added) > 0 {
		fmt.Println("  added:   " + strings.Join(added, ", "))
	}
	if len(updated) > 0 {
		fmt.Println("  updated: " + strings.Join(updated, ", "))
	}
	if len(skipped) > 0 {
		fmt.Println("  skipped (already registered; use --overwrite): " + strings.Join(skipped, ", "))
	}
	if len(added)+len(updated) == 0 {
		return
	}
	saveRegistry(m, absDir(*dir), servers)
}

func defaultImportPath() string {
	if home, err := os.UserHomeDir(); err == nil {
		if p := filepath.Join(home, ".claude.json"); fileExists(p) {
			return p
		}
	}
	if fileExists(".mcp.json") {
		return ".mcp.json"
	}
	return ""
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// readMCPConfig parses a Claude/MCP config and returns its local stdio servers as registry
// entries. It reads the top-level `mcpServers` plus any `projects.<path>.mcpServers`.
func readMCPConfig(path string) ([]config.MCPServer, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	type entry struct {
		Type    string            `json:"type"`
		Command string            `json:"command"`
		Args    []string          `json:"args"`
		Env     map[string]string `json:"env"`
		URL     string            `json:"url"`
	}
	var doc struct {
		MCPServers map[string]entry `json:"mcpServers"`
		Projects   map[string]struct {
			MCPServers map[string]entry `json:"mcpServers"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	seen := map[string]bool{}
	var out []config.MCPServer
	collect := func(servers map[string]entry) {
		for name, e := range servers {
			if seen[name] {
				continue
			}
			seen[name] = true
			if config.IsBuiltinServer(name) {
				fmt.Fprintf(os.Stderr, "  skip %q: reserved built-in name\n", name)
				continue
			}
			if e.Command == "" || e.URL != "" || (e.Type != "" && e.Type != "stdio") {
				fmt.Fprintf(os.Stderr, "  skip %q: not a local stdio server (only stdio is supported)\n", name)
				continue
			}
			out = append(out, config.MCPServer{Name: name, Command: e.Command, Args: e.Args, Env: e.Env})
		}
	}
	collect(doc.MCPServers)
	for _, p := range doc.Projects {
		collect(p.MCPServers)
	}
	return out, nil
}

func parseEnv(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	env := map[string]string{}
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("--env %q must be KEY=VALUE", p)
		}
		env[k] = v
	}
	return env, nil
}

// upsert replaces an entry with the same name, or appends it.
func upsert(servers []config.MCPServer, s config.MCPServer) []config.MCPServer {
	for i := range servers {
		if servers[i].Name == s.Name {
			servers[i] = s
			return servers
		}
	}
	return append(servers, s)
}
