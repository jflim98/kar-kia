package schedule

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// FireFunc is invoked when a schedule fires. It runs in its own goroutine.
type FireFunc func(ctx context.Context, s Schedule)

// Scheduler activates schedules from the store: cron entries for recurring ones and
// timers for one-offs. It also hosts internal "built-in" recurring jobs (consolidation,
// pruning) that are not user schedules and not persisted.
type Scheduler struct {
	store *Store
	loc   *time.Location
	fire  FireFunc

	mu      sync.Mutex
	cron    *cron.Cron
	entries map[string]cron.EntryID // schedule ID -> cron entry
	timers  map[string]*time.Timer  // schedule ID -> one-off timer
	ctx     context.Context
}

// New constructs a Scheduler. fire is called (in a goroutine) for every firing.
func New(store *Store, loc *time.Location, fire FireFunc) *Scheduler {
	if loc == nil {
		loc = time.UTC
	}
	return &Scheduler{
		store:   store,
		loc:     loc,
		fire:    fire,
		entries: map[string]cron.EntryID{},
		timers:  map[string]*time.Timer{},
	}
}

// Start activates all stored schedules and starts the cron engine. It stops when
// ctx is cancelled.
func (s *Scheduler) Start(ctx context.Context) {
	s.mu.Lock()
	s.ctx = ctx
	s.cron = cron.New(cron.WithLocation(s.loc))
	s.mu.Unlock()

	for _, sc := range s.store.List(0) {
		if err := s.activate(sc); err != nil {
			log.Printf("schedule: activate %s failed: %v", sc.ID, err)
		}
	}
	s.cron.Start()

	go func() {
		<-ctx.Done()
		s.cron.Stop()
		s.mu.Lock()
		for _, t := range s.timers {
			t.Stop()
		}
		s.mu.Unlock()
	}()
}

// AddBuiltin registers an internal recurring job (not persisted, not user-visible).
func (s *Scheduler) AddBuiltin(spec string, fn func()) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.cron.AddFunc(spec, fn)
	return err
}

// Add persists a schedule and activates it.
func (s *Scheduler) Add(sc Schedule) (Schedule, error) {
	saved, err := s.store.Put(sc)
	if err != nil {
		return Schedule{}, err
	}
	if err := s.activate(saved); err != nil {
		return saved, err
	}
	return saved, nil
}

// Remove deactivates and deletes a schedule. Returns whether it existed.
func (s *Scheduler) Remove(id string) (bool, error) {
	s.deactivate(id)
	return s.store.Delete(id)
}

// List returns schedules (chatID 0 => all).
func (s *Scheduler) List(chatID int64) []Schedule { return s.store.List(chatID) }

func (s *Scheduler) activate(sc Schedule) error {
	switch sc.Kind {
	case KindCron:
		s.mu.Lock()
		defer s.mu.Unlock()
		id, err := s.cron.AddFunc(sc.Spec, func() { s.run(sc) })
		if err != nil {
			return err
		}
		s.entries[sc.ID] = id
	default: // KindOnce
		d := max(time.Until(sc.FireAt), 0)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.timers[sc.ID] = time.AfterFunc(d, func() {
			s.run(sc)
			// one-off: remove after firing
			if _, err := s.Remove(sc.ID); err != nil {
				log.Printf("schedule: cleanup %s failed: %v", sc.ID, err)
			}
		})
	}
	return nil
}

func (s *Scheduler) deactivate(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if eid, ok := s.entries[id]; ok {
		s.cron.Remove(eid)
		delete(s.entries, id)
	}
	if t, ok := s.timers[id]; ok {
		t.Stop()
		delete(s.timers, id)
	}
}

// run fires a schedule in its own goroutine, guarding against a nil ctx.
func (s *Scheduler) run(sc Schedule) {
	s.mu.Lock()
	ctx := s.ctx
	s.mu.Unlock()
	if ctx == nil {
		return
	}
	go s.fire(ctx, sc)
}
