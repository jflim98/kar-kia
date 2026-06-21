// Package session tracks one persistent headless-Claude session per chat, so
// follow-up turns can be resumed (--resume) to keep the prompt cache warm.
//
// The store is the chat -> session_id map persisted to sessions.json. The decision
// of when to start a fresh session (rotation) lives in the brain; this package only
// stores, retrieves, and prunes.
package session

import (
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Session records a single chat's current Claude session.
type Session struct {
	ID            string    `json:"id"`             // session UUID (passed to --session-id / --resume)
	CreatedAt     time.Time `json:"created_at"`     // when this session was started
	LastUsed      time.Time `json:"last_used"`      // last activity (for TTL pruning)
	TurnCount     int       `json:"turn_count"`     // turns taken in this session
	MemoryVersion int       `json:"memory_version"` // memory snapshot baked into the prefix
}

// Store is the persistent chat -> Session map.
type Store struct {
	mu       sync.Mutex
	path     string
	sessions map[string]Session
}

// Load reads sessions.json (an empty/missing file yields an empty store).
func Load(path string) (*Store, error) {
	s := &Store{path: path, sessions: map[string]Session{}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &s.sessions); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Get returns the current session for a chat, if any.
func (s *Store) Get(chatKey string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[chatKey]
	return sess, ok
}

// NewSession returns a fresh Session (new UUID) stamped at now, without storing it.
func NewSession(memoryVersion int) Session {
	now := time.Now()
	return Session{
		ID:            uuid.NewString(),
		CreatedAt:     now,
		LastUsed:      now,
		MemoryVersion: memoryVersion,
	}
}

// Put stores/replaces a chat's session and persists the store.
func (s *Store) Put(chatKey string, sess Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[chatKey] = sess
	return s.saveLocked()
}

// Delete removes a chat's session and persists.
func (s *Store) Delete(chatKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, chatKey)
	return s.saveLocked()
}

// PruneOlderThan removes sessions unused for longer than d. Returns the count removed.
func (s *Store) PruneOlderThan(d time.Duration) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-d)
	var removed int
	for k, sess := range s.sessions {
		if sess.LastUsed.Before(cutoff) {
			delete(s.sessions, k)
			removed++
		}
	}
	if removed > 0 {
		if err := s.saveLocked(); err != nil {
			return removed, err
		}
	}
	return removed, nil
}

// Keys returns the chat keys currently tracked.
func (s *Store) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.sessions))
	for k := range s.sessions {
		keys = append(keys, k)
	}
	return keys
}

func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(s.sessions, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
