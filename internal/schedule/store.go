// Package schedule provides recurring (cron) and one-off reminders/jobs, persisted
// to schedules.json and delivered to their origin chat when they fire.
package schedule

import (
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Schedule kinds.
const (
	KindCron = "cron" // recurring, driven by Spec (5-field cron)
	KindOnce = "once" // one-off, driven by FireAt
)

// Schedule is a reminder or job to run in a specific chat.
type Schedule struct {
	ID        string    `json:"id"`
	ChatID    int64     `json:"chat_id"`
	Kind      string    `json:"kind"`
	Spec      string    `json:"spec,omitempty"`   // cron expression (KindCron)
	FireAt    time.Time `json:"fire_at,omitzero"` // absolute fire time (KindOnce)
	Prompt    string    `json:"prompt"`           // instruction the assistant runs when it fires
	CreatedAt time.Time `json:"created_at"`
}

// Store is the persistent list of schedules (schedules.json).
type Store struct {
	mu   sync.Mutex
	path string
	all  map[string]Schedule
}

// LoadStore reads schedules.json (missing/empty => empty store).
func LoadStore(path string) (*Store, error) {
	s := &Store{path: path, all: map[string]Schedule{}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) > 0 {
		var list []Schedule
		if err := json.Unmarshal(b, &list); err != nil {
			return nil, err
		}
		for _, sc := range list {
			s.all[sc.ID] = sc
		}
	}
	return s, nil
}

// Put adds or replaces a schedule and persists. A blank ID gets a new UUID.
func (s *Store) Put(sc Schedule) (Schedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sc.ID == "" {
		sc.ID = uuid.NewString()
	}
	if sc.CreatedAt.IsZero() {
		sc.CreatedAt = time.Now()
	}
	s.all[sc.ID] = sc
	return sc, s.saveLocked()
}

// Delete removes a schedule by ID and persists. Returns whether it existed.
func (s *Store) Delete(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.all[id]; !ok {
		return false, nil
	}
	delete(s.all, id)
	return true, s.saveLocked()
}

// List returns schedules, optionally filtered to a chat (chatID 0 => all).
func (s *Store) List(chatID int64) []Schedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Schedule, 0, len(s.all))
	for _, sc := range s.all {
		if chatID == 0 || sc.ChatID == chatID {
			out = append(out, sc)
		}
	}
	return out
}

func (s *Store) saveLocked() error {
	list := make([]Schedule, 0, len(s.all))
	for _, sc := range s.all {
		list = append(list, sc)
	}
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
