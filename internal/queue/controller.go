// Package queue implements the admission state machine on top of a store.
package queue

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/davidaparicio/gowait/internal/metrics"
	"github.com/davidaparicio/gowait/internal/store"
)

// Decision tells the gatekeeper what to do with a request.
type Decision int

const (
	DecisionProxy Decision = iota // admitted → reverse-proxy to the backend
	DecisionWait                  // queued → serve the waiting page
)

// Result is the outcome of an admission check.
type Result struct {
	Decision    Decision
	Position    int
	QueueLength int
	ActiveCount int
	ETA         time.Duration
}

// Config holds the admission policy. Capacity is the initial value; it can
// change at runtime via SetCapacity or a store-side override.
type Config struct {
	Capacity  int
	ActiveTTL time.Duration
	QueueTTL  time.Duration

	// MinReconcileGap rate-limits request-triggered reconciles: once one has
	// run, requests within the gap skip theirs. 0 reconciles on every request.
	// Set it for remote stores, where each reconcile is a network round trip
	// and a server-side scan; the janitor still guarantees one per tick, so
	// promotions lag by at most the gap. FIFO order is unaffected.
	MinReconcileGap time.Duration

	// StatsCacheTTL caches store stats for ETA computation, saving one store
	// round trip per queued status poll. 0 fetches fresh stats every time.
	// The ETA only needs the session-duration EMA, which moves slowly; the
	// waiting page's poll interval is the natural TTL.
	StatsCacheTTL time.Duration
}

type Controller struct {
	store         store.Store
	cfg           Config
	capacity      atomic.Int64
	now           func() time.Time
	metrics       *metrics.Registry // nil-safe; set before serving
	lastReconcile atomic.Int64      // unixnano of the newest reconcile
	statsCache    atomic.Pointer[cachedStats]
}

type cachedStats struct {
	stats store.Stats
	at    time.Time
}

// New creates a Controller. now may be nil, in which case time.Now is used.
func New(s store.Store, cfg Config, now func() time.Time) *Controller {
	if now == nil {
		now = time.Now
	}
	c := &Controller{store: s, cfg: cfg, now: now}
	c.capacity.Store(int64(cfg.Capacity))
	return c
}

// SetMetrics attaches a metrics registry. Call before Run or serving.
func (c *Controller) SetMetrics(r *metrics.Registry) { c.metrics = r }

// Stats exposes the store's live stats (for the metrics/admin endpoints).
func (c *Controller) Stats(ctx context.Context) (store.Stats, error) {
	return c.store.Stats(ctx)
}

// Flush empties the queue (admin operation). Active sessions are untouched;
// flushed users re-admit or re-enqueue on their next request.
func (c *Controller) Flush(ctx context.Context) (int, error) {
	return c.store.Flush(ctx)
}

// Capacity returns the currently effective capacity.
func (c *Controller) Capacity() int { return int(c.capacity.Load()) }

// SetCapacity changes the effective capacity at runtime, writing through to
// the store so other instances sharing it pick the value up (via their
// janitor, within about a second).
func (c *Controller) SetCapacity(ctx context.Context, n int) error {
	if n < 1 {
		return fmt.Errorf("capacity must be >= 1, got %d", n)
	}
	if err := c.store.SetCapacity(ctx, n); err != nil {
		return err
	}
	c.capacity.Store(int64(n))
	return nil
}

// refreshCapacity adopts a store-side capacity override, if any. Called by
// the janitor so overrides set elsewhere (another replica, a persisted value
// from before a restart) propagate without traffic. When no override exists
// (e.g. the key was deleted), the configured value is restored.
func (c *Controller) refreshCapacity(ctx context.Context) {
	n, set, err := c.store.GetCapacity(ctx)
	switch {
	case err != nil:
		// Store unreachable: keep the current value.
	case set && n >= 1:
		c.capacity.Store(int64(n))
	default:
		c.capacity.Store(int64(c.cfg.Capacity))
	}
}

// maybeReconcile is the request-path entry: it honors MinReconcileGap so a
// crowd of pollers doesn't hammer the store with redundant reconciles. The
// CAS picks one winner per gap; losers proceed with slightly stale state,
// which the janitor bounds to one tick.
func (c *Controller) maybeReconcile(ctx context.Context, now time.Time) error {
	if gap := c.cfg.MinReconcileGap; gap > 0 {
		last := c.lastReconcile.Load()
		if now.UnixNano()-last < int64(gap) || !c.lastReconcile.CompareAndSwap(last, now.UnixNano()) {
			return nil
		}
	}
	return c.reconcile(ctx, now)
}

// reconcile cranks the store's state machine and feeds the metrics registry
// with what happened — the single funnel for all call sites.
func (c *Controller) reconcile(ctx context.Context, now time.Time) error {
	c.lastReconcile.Store(now.UnixNano())
	res, err := c.store.Reconcile(ctx, c.Capacity(), c.cfg.ActiveTTL, c.cfg.QueueTTL, now)
	if err != nil {
		return err
	}
	c.metrics.AddReconcile(res)
	for _, waited := range res.WaitedSecs {
		c.metrics.ObserveAdmission(waited)
	}
	return nil
}

// Check is the single entry point the gatekeeper calls per proxied request:
// reconcile, then admit/touch/enqueue the ticket as appropriate.
func (c *Controller) Check(ctx context.Context, ticketID string) (Result, error) {
	now := c.now()
	if err := c.maybeReconcile(ctx, now); err != nil {
		return Result{}, err
	}

	snap, err := c.store.Lookup(ctx, ticketID, now)
	if err != nil {
		return Result{}, err
	}

	switch snap.Status {
	case store.StatusActive:
		if _, err := c.store.Touch(ctx, ticketID, now); err != nil {
			return Result{}, err
		}
		return c.result(ctx, DecisionProxy, snap)
	case store.StatusQueued:
		return c.result(ctx, DecisionWait, snap)
	default: // StatusUnknown: new ticket, or expired one re-joining
		admitted, err := c.store.TryAdmit(ctx, ticketID, c.Capacity(), now)
		if err != nil {
			return Result{}, err
		}
		if admitted {
			c.metrics.ObserveAdmission(0) // free slot, no queue wait
			snap.Status = store.StatusActive
			snap.ActiveCount++
			return c.result(ctx, DecisionProxy, snap)
		}
		snap, err = c.store.Enqueue(ctx, ticketID, now)
		if err != nil {
			return Result{}, err
		}
		return c.result(ctx, DecisionWait, snap)
	}
}

// StatusOf backs GET /gowait/status: reconcile + lookup (which refreshes the
// heartbeat), but never enqueues.
func (c *Controller) StatusOf(ctx context.Context, ticketID string) (Result, error) {
	now := c.now()
	if err := c.maybeReconcile(ctx, now); err != nil {
		return Result{}, err
	}
	snap, err := c.store.Lookup(ctx, ticketID, now)
	if err != nil {
		return Result{}, err
	}
	decision := DecisionWait
	switch snap.Status {
	case store.StatusActive:
		decision = DecisionProxy
	case store.StatusUnknown:
		// Ticket expired or was evicted while the page was polling. If a real
		// request would be admitted right now, say "active" so the page
		// reloads into the site; otherwise report the tail position it would
		// get on its next request.
		if snap.ActiveCount < c.Capacity() && snap.QueueLength == 0 {
			decision = DecisionProxy
		} else {
			snap.Position = snap.QueueLength + 1
		}
	}
	return c.result(ctx, decision, snap)
}

// Run is the janitor loop: it keeps the queue draining even when nobody is
// sending requests. Blocks until ctx is done.
func (c *Controller) Run(ctx context.Context) {
	interval := c.cfg.ActiveTTL / 4
	if interval > time.Second {
		interval = time.Second
	}
	if interval <= 0 {
		interval = time.Second
	}
	c.refreshCapacity(ctx) // adopt a persisted override immediately on start
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.refreshCapacity(ctx)
			_ = c.reconcile(ctx, c.now())
		}
	}
}

func (c *Controller) result(ctx context.Context, d Decision, snap store.Snapshot) (Result, error) {
	res := Result{
		Decision:    d,
		Position:    snap.Position,
		QueueLength: snap.QueueLength,
		ActiveCount: snap.ActiveCount,
	}
	if d == DecisionWait && snap.Position > 0 {
		res.ETA = c.eta(ctx, snap.Position)
	}
	return res, nil
}

// eta estimates the wait as position × avgSession / capacity: slots drain at
// roughly capacity/avgSession per second. Falls back to ActiveTTL when no
// session has completed yet. An estimate, not a promise — which is why a
// briefly cached EMA (see etaStats) is good enough.
func (c *Controller) eta(ctx context.Context, position int) time.Duration {
	avg := c.cfg.ActiveTTL
	if stats, err := c.etaStats(ctx); err == nil && stats.AvgSessionSecs > 0 {
		avg = time.Duration(stats.AvgSessionSecs * float64(time.Second))
	}
	return time.Duration(position) * avg / time.Duration(c.Capacity())
}

// etaStats returns store stats for ETA math, cached for StatsCacheTTL. Stats
// (the exported method, backing metrics and the admin API) stays uncached.
func (c *Controller) etaStats(ctx context.Context) (store.Stats, error) {
	ttl := c.cfg.StatsCacheTTL
	if ttl <= 0 {
		return c.store.Stats(ctx)
	}
	now := c.now()
	if p := c.statsCache.Load(); p != nil && now.Sub(p.at) < ttl {
		return p.stats, nil
	}
	stats, err := c.store.Stats(ctx)
	if err != nil {
		return stats, err
	}
	c.statsCache.Store(&cachedStats{stats: stats, at: now})
	return stats, nil
}
