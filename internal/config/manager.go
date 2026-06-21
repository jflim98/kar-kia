package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

const (
	configFile     = "config.yaml"
	secretsFile    = "secrets.yaml"
	mcpServersFile = "mcp_servers.yaml"
)

// Manager is the live, hot-reloadable source of truth for GLOBAL config + secrets.
// All access is goroutine-safe. Mutations persist to disk and notify subscribers.
type Manager struct {
	mu         sync.RWMutex
	dataDir    string
	cfg        Global
	secrets    Secrets
	mcpServers []MCPServer
	subs       []func()
}

// Load reads config.yaml and secrets.yaml from dataDir (filling defaults for any
// missing config fields) and applies env-var overrides for secrets.
func Load(dataDir string) (*Manager, error) {
	m := &Manager{dataDir: dataDir, cfg: DefaultGlobal()}

	if err := readYAML(filepath.Join(dataDir, configFile), &m.cfg); err != nil {
		return nil, fmt.Errorf("read %s: %w", configFile, err)
	}
	if err := readYAML(filepath.Join(dataDir, secretsFile), &m.secrets); err != nil {
		return nil, fmt.Errorf("read %s: %w", secretsFile, err)
	}
	if err := readYAML(filepath.Join(dataDir, mcpServersFile), &m.mcpServers); err != nil {
		return nil, fmt.Errorf("read %s: %w", mcpServersFile, err)
	}
	m.applyEnvSecrets()
	return m, nil
}

// applyEnvSecrets lets env vars override secret values on bootstrap. BOT_TOKENS is a
// comma-separated list merged into bot_tokens.
func (m *Manager) applyEnvSecrets() {
	if v := os.Getenv("WEBUI_PASSWORD"); v != "" {
		m.secrets.WebUIPassword = v
	}
	if v := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"); v != "" {
		m.secrets.ClaudeCodeOAuthToken = v
	}
	if v := os.Getenv("BOT_TOKENS"); v != "" {
		for t := range strings.SplitSeq(v, ",") {
			if t = strings.TrimSpace(t); t != "" && !slices.Contains(m.secrets.BotTokens, t) {
				m.secrets.BotTokens = append(m.secrets.BotTokens, t)
			}
		}
	}
}

// DataDir returns the data directory backing this manager.
func (m *Manager) DataDir() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.dataDir
}

// Get returns a copy of the current global config.
func (m *Manager) Get() Global {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

// Secrets returns a copy of the current secrets.
func (m *Manager) Secrets() Secrets {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.secrets
}

// MCPServers returns a copy of the registered external MCP servers.
func (m *Manager) MCPServers() []MCPServer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]MCPServer(nil), m.mcpServers...)
}

// MCPServerByName returns a registered external server by name.
func (m *Manager) MCPServerByName(name string) (MCPServer, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, s := range m.mcpServers {
		if s.Name == name {
			return s, true
		}
	}
	return MCPServer{}, false
}

// SetMCPServers replaces and persists the external MCP server registry (0600, env may be
// secret). It does not notify config subscribers; the caller regenerates per-chat mcp.json.
func (m *Manager) SetMCPServers(servers []MCPServer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mcpServers = append([]MCPServer(nil), servers...)
	return writeYAML(filepath.Join(m.dataDir, mcpServersFile), m.mcpServers, 0o600)
}

// OnChange registers a callback invoked (synchronously) after every successful Update.
func (m *Manager) OnChange(fn func()) {
	m.mu.Lock()
	m.subs = append(m.subs, fn)
	m.mu.Unlock()
}

// Update applies fn to the live config/secrets, persists both files, then notifies
// subscribers. The mutation runs under the write lock; subscribers run after unlock.
func (m *Manager) Update(fn func(*Global, *Secrets)) error {
	m.mu.Lock()
	fn(&m.cfg, &m.secrets)
	if err := m.saveLocked(); err != nil {
		m.mu.Unlock()
		return err
	}
	subs := append([]func(){}, m.subs...)
	m.mu.Unlock()

	for _, s := range subs {
		s()
	}
	return nil
}

// Save persists the current config + secrets without mutating or notifying.
func (m *Manager) Save() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saveLocked()
}

func (m *Manager) saveLocked() error {
	if err := writeYAML(filepath.Join(m.dataDir, configFile), m.cfg, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", configFile, err)
	}
	if err := writeYAML(filepath.Join(m.dataDir, secretsFile), m.secrets, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", secretsFile, err)
	}
	return nil
}

// readYAML decodes a YAML file into dst. A missing file is not an error (defaults stand).
func readYAML(path string, dst any) error {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return yaml.Unmarshal(b, dst)
}

// writeYAML atomically writes v as YAML to path with the given perms.
func writeYAML(path string, v any, perm os.FileMode) error {
	b, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
