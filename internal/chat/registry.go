package chat

import (
	"encoding/json"
	"os"
	"sort"
	"sync"
	"time"
)

// RegEntry is a known chat in registry.json (configured or not).
type RegEntry struct {
	ChatID    int64     `json:"chat_id"`
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	LastToken string    `json:"last_token"` // token whose bot last saw this chat
	LastSeen  time.Time `json:"last_seen"`
}

// registry is the persisted discovery list of chats the bot has encountered.
type registry struct {
	mu      sync.Mutex
	path    string
	entries map[int64]RegEntry
}

func loadRegistry(path string) (*registry, error) {
	r := &registry{path: path, entries: map[int64]RegEntry{}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) > 0 {
		var list []RegEntry
		if err := json.Unmarshal(b, &list); err != nil {
			return nil, err
		}
		for _, e := range list {
			r.entries[e.ChatID] = e
		}
	}
	return r, nil
}

// observe records/refreshes a chat's last-seen name, type, and token. Returns whether
// anything changed (to avoid needless writes).
func (r *registry) observe(chatID int64, name, ctype, token string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[chatID]
	changed := !ok || (name != "" && e.Name != name) || e.Type != ctype || e.LastToken != token
	e.ChatID = chatID
	if name != "" {
		e.Name = name
	}
	e.Type = ctype
	e.LastToken = token
	e.LastSeen = time.Now()
	r.entries[chatID] = e
	if changed {
		_ = r.saveLocked()
	}
	return changed
}

func (r *registry) get(chatID int64) (RegEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[chatID]
	return e, ok
}

// list returns all known entries, newest-seen first.
func (r *registry) list() []RegEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RegEntry, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	return out
}

func (r *registry) saveLocked() error {
	list := make([]RegEntry, 0, len(r.entries))
	for _, e := range r.entries {
		list = append(list, e)
	}
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}
