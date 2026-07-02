// Package store defines the pluggable state backend for the waiting room.
//
// Each method is a single atomic operation so that a distributed
// implementation (e.g. Redis, one Lua script per method) can stay race-free
// across multiple gowait replicas. Policy values (capacity, TTLs) are passed
// in by the caller and never stored here. The caller also owns the clock:
// implementations must use the provided now value instead of time.Now().
package store

import (
	"context"
	"time"
)

// Status is the lifecycle state of a ticket.
type Status int

const (
	// StatusUnknown means the ticket was never seen, or was evicted/expired.
	StatusUnknown Status = iota
	StatusQueued
	StatusActive
)

// Snapshot is a point-in-time view of one ticket.
type Snapshot struct {
	Status      Status
	Position    int // 1-based, only meaningful when StatusQueued
	QueueLength int
	ActiveCount int
}

// Stats summarizes the whole room.
type Stats struct {
	QueueLength    int
	ActiveCount    int
	AvgSessionSecs float64 // EMA of completed session durations; 0 = no data yet
}

// ReconcileResult reports what one Reconcile pass did. Because each event is
// produced by exactly one Reconcile call cluster-wide, counters fed from it
// can be summed across instances without double counting.
type ReconcileResult struct {
	Promoted   int       // queued users admitted into free slots
	Expired    int       // active sessions reaped for inactivity
	Evicted    int       // queued ghosts removed for not polling
	WaitedSecs []float64 // time each promoted user spent queued
}

type Store interface {
	// TryAdmit atomically admits id if ActiveCount < capacity AND the queue
	// is empty (FIFO fairness: new arrivals never jump queued users).
	// Idempotent for an already-active id (acts as Touch).
	TryAdmit(ctx context.Context, id string, capacity int, now time.Time) (bool, error)

	// Enqueue appends id to the queue tail (no-op if already queued or
	// active) and returns its snapshot.
	Enqueue(ctx context.Context, id string, now time.Time) (Snapshot, error)

	// Lookup returns the ticket state and refreshes its lastSeen heartbeat,
	// so queued users polling for status are not evicted as ghosts.
	Lookup(ctx context.Context, id string, now time.Time) (Snapshot, error)

	// Touch slides the inactivity window of an active ticket. Returns false
	// if the ticket is not active (its slot was reaped).
	Touch(ctx context.Context, id string, now time.Time) (bool, error)

	// Reconcile cranks the state machine once, safely callable from many
	// goroutines: (1) expire actives with lastSeen older than activeTTL,
	// (2) evict queued entries with lastSeen older than queueTTL,
	// (3) promote queue heads while ActiveCount < capacity.
	Reconcile(ctx context.Context, capacity int, activeTTL, queueTTL time.Duration, now time.Time) (ReconcileResult, error)

	Stats(ctx context.Context) (Stats, error)

	// SetCapacity stores a runtime capacity override, shared by every
	// instance using this store. It does not change admission behavior by
	// itself: capacity is still passed into TryAdmit/Reconcile by the
	// caller — this is the channel through which callers learn the value.
	SetCapacity(ctx context.Context, capacity int) error

	// GetCapacity returns the runtime capacity override. set=false means no
	// override exists and the caller should use its configured value.
	GetCapacity(ctx context.Context) (capacity int, set bool, err error)

	// Flush empties the queue, returning how many entries were removed.
	// Active sessions are never touched; flushed users become StatusUnknown
	// and re-admit or re-enqueue on their next request.
	Flush(ctx context.Context) (removed int, err error)
}
