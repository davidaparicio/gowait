// Package prober adjusts capacity from backend health with AIMD: halve on a
// failed probe (shed load fast), +1 after three consecutive successes
// (recover gradually), always within configured min/max bounds. Adjustments
// go through the store-backed capacity channel from Phase 2, so every
// replica adopts them within a janitor tick.
//
// With multiple replicas sharing a store.Locker, one lease per interval
// picks the adjuster. The success streak is per-replica, so when the lease
// hops between replicas recovery is a little slower than 3 intervals per
// step — halving, the side that matters, is always immediate.
package prober

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/davidaparicio/gowait/internal/store"
)

// successesPerIncrease is how many consecutive healthy probes earn one
// capacity increment.
const successesPerIncrease = 3

// lockName identifies the adjuster lease in the store.Locker namespace.
const lockName = "prober"

// Controller is the slice of queue.Controller the prober drives.
type Controller interface {
	Capacity() int
	SetCapacity(ctx context.Context, n int) error
}

// Config bounds the prober. Interval is the probe cadence, the probe
// timeout, and (in multi-replica mode) the lock lease duration.
type Config struct {
	URL      string
	Interval time.Duration
	Min, Max int
}

type Prober struct {
	cfg       Config
	ctrl      Controller
	locker    store.Locker // nil when the store is single-instance
	client    *http.Client
	successes int
}

// New creates a Prober. locker may be nil, meaning no cross-replica
// coordination is needed (memory store).
func New(cfg Config, ctrl Controller, locker store.Locker) *Prober {
	return &Prober{
		cfg:    cfg,
		ctrl:   ctrl,
		locker: locker,
		client: &http.Client{Timeout: cfg.Interval},
	}
}

// Run probes every interval until ctx is done.
func (p *Prober) Run(ctx context.Context) {
	t := time.NewTicker(p.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.tick(ctx)
		}
	}
}

// tick runs one probe+adjust round, unless another replica holds the
// adjuster lease this interval.
func (p *Prober) tick(ctx context.Context) {
	if p.locker != nil {
		ok, err := p.locker.TryLock(ctx, lockName, p.cfg.Interval)
		if err != nil {
			slog.Warn("prober: lock unavailable, skipping round", "err", err)
			return
		}
		if !ok {
			return
		}
	}
	p.adjust(ctx, p.probe(ctx))
}

// probe reports whether the backend answered the health URL with a
// non-error status (200–399, the Kubernetes probe convention).
func (p *Prober) probe(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.URL, nil)
	if err != nil {
		slog.Warn("prober: bad probe request", "url", p.cfg.URL, "err", err)
		return false
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096)) // drain for conn reuse
	return resp.StatusCode >= 200 && resp.StatusCode < 400
}

// adjust applies one AIMD step to the shared capacity.
func (p *Prober) adjust(ctx context.Context, healthy bool) {
	cur := p.ctrl.Capacity()
	var next int
	if healthy {
		p.successes++
		if p.successes < successesPerIncrease {
			return
		}
		p.successes = 0
		next = cur + 1
	} else {
		p.successes = 0
		next = cur / 2
	}
	if next < p.cfg.Min {
		next = p.cfg.Min
	}
	if next > p.cfg.Max {
		next = p.cfg.Max
	}
	if next == cur {
		return
	}
	if err := p.ctrl.SetCapacity(ctx, next); err != nil {
		slog.Warn("prober: setting capacity failed", "err", err)
		return
	}
	slog.Info("prober adjusted capacity", "from", cur, "to", next, "backend_healthy", healthy)
}
