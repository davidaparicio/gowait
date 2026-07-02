// Package memory implements store.Store in process memory, guarded by a
// single mutex. Suitable for a single gowait instance.
package memory

import (
	"container/list"
	"context"
	"sync"
	"time"

	"github.com/davidaparicio/gowait/internal/store"
)

// emaAlpha is the smoothing factor for the average-session-duration EMA.
const emaAlpha = 0.2

type entry struct {
	id         string
	status     store.Status
	enqueuedAt time.Time
	admittedAt time.Time
	lastSeen   time.Time
	elem       *list.Element // non-nil while queued
}

type Store struct {
	mu         sync.Mutex
	entries    map[string]*entry
	queue      *list.List // FIFO of *entry
	active     int
	avgSession float64 // seconds, EMA
}

func New() *Store {
	return &Store{
		entries: make(map[string]*entry),
		queue:   list.New(),
	}
}

func (s *Store) TryAdmit(_ context.Context, id string, capacity int, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if e, ok := s.entries[id]; ok && e.status == store.StatusActive {
		e.lastSeen = now
		return true, nil
	}
	if s.active >= capacity || s.queue.Len() > 0 {
		return false, nil
	}
	e := s.entries[id]
	if e == nil {
		e = &entry{id: id}
		s.entries[id] = e
	}
	s.admitLocked(e, now)
	return true, nil
}

func (s *Store) Enqueue(_ context.Context, id string, now time.Time) (store.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.entries[id]
	if !ok {
		e = &entry{id: id, status: store.StatusQueued, enqueuedAt: now}
		s.entries[id] = e
		e.elem = s.queue.PushBack(e)
	}
	e.lastSeen = now
	return s.snapshotLocked(e), nil
}

func (s *Store) Lookup(_ context.Context, id string, now time.Time) (store.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.entries[id]
	if !ok {
		return store.Snapshot{Status: store.StatusUnknown, QueueLength: s.queue.Len(), ActiveCount: s.active}, nil
	}
	e.lastSeen = now
	return s.snapshotLocked(e), nil
}

func (s *Store) Touch(_ context.Context, id string, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.entries[id]
	if !ok || e.status != store.StatusActive {
		return false, nil
	}
	e.lastSeen = now
	return true, nil
}

func (s *Store) Reconcile(_ context.Context, capacity int, activeTTL, queueTTL time.Duration, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 1. Expire idle actives, feeding the session-duration EMA.
	for id, e := range s.entries {
		if e.status == store.StatusActive && now.Sub(e.lastSeen) > activeTTL {
			duration := e.lastSeen.Sub(e.admittedAt).Seconds()
			if s.avgSession == 0 {
				s.avgSession = duration
			} else {
				s.avgSession = emaAlpha*duration + (1-emaAlpha)*s.avgSession
			}
			delete(s.entries, id)
			s.active--
		}
	}

	// 2. Evict ghost queuers that stopped polling.
	for el := s.queue.Front(); el != nil; {
		next := el.Next()
		e := el.Value.(*entry)
		if now.Sub(e.lastSeen) > queueTTL {
			s.queue.Remove(el)
			delete(s.entries, e.id)
		}
		el = next
	}

	// 3. Promote queue heads into free slots.
	promoted := 0
	for s.active < capacity {
		el := s.queue.Front()
		if el == nil {
			break
		}
		e := s.queue.Remove(el).(*entry)
		e.elem = nil
		s.admitLocked(e, now)
		promoted++
	}
	return promoted, nil
}

func (s *Store) Stats(_ context.Context) (store.Stats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return store.Stats{
		QueueLength:    s.queue.Len(),
		ActiveCount:    s.active,
		AvgSessionSecs: s.avgSession,
	}, nil
}

func (s *Store) admitLocked(e *entry, now time.Time) {
	e.status = store.StatusActive
	e.admittedAt = now
	e.lastSeen = now
	s.active++
}

func (s *Store) snapshotLocked(e *entry) store.Snapshot {
	snap := store.Snapshot{
		Status:      e.status,
		QueueLength: s.queue.Len(),
		ActiveCount: s.active,
	}
	if e.status == store.StatusQueued {
		// O(n) walk; fine for v1 queue sizes. Replace with monotonic sequence
		// counters if queues reach 1e5+.
		pos := 1
		for el := s.queue.Front(); el != nil; el = el.Next() {
			if el == e.elem {
				break
			}
			pos++
		}
		snap.Position = pos
	}
	return snap
}
