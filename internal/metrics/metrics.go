// Package metrics is a minimal, dependency-free Prometheus text-format
// exporter for gowait's handful of series.
//
// Counters and the wait-time histogram are per-instance and fed exactly once
// per event (from ReconcileResult), so summing across replicas is correct.
// Gauges are read live from shared store state at scrape time; with multiple
// replicas on one Valkey, aggregate them with max(), not sum().
//
// All methods are safe on a nil *Registry, so callers can wire metrics
// optionally without nil checks.
package metrics

import (
	"fmt"
	"io"
	"math"
	"sync/atomic"

	"github.com/davidaparicio/gowait/internal/store"
)

// waitBuckets are the histogram upper bounds, in seconds.
var waitBuckets = []float64{1, 5, 15, 30, 60, 120, 300, 600}

// Request decisions, the label values of gowait_requests_total.
const (
	DecisionProxied = "proxied"
	DecisionQueued  = "queued"
	DecisionAdmin   = "admin"
)

type Registry struct {
	version string

	admissions  atomic.Int64
	expirations atomic.Int64
	evictions   atomic.Int64

	reqProxied atomic.Int64
	reqQueued  atomic.Int64
	reqAdmin   atomic.Int64

	waitCounts  []atomic.Int64 // per-bucket, last = beyond the final bound
	waitSumBits atomic.Uint64  // float64 bits of the sum
	waitTotal   atomic.Int64
}

func New(version string) *Registry {
	return &Registry{
		version:    version,
		waitCounts: make([]atomic.Int64, len(waitBuckets)+1),
	}
}

// ObserveAdmission records one user entering the site after waiting waitSecs
// in the queue (0 for direct admission with a free slot).
func (r *Registry) ObserveAdmission(waitSecs float64) {
	if r == nil {
		return
	}
	r.admissions.Add(1)
	i := len(waitBuckets)
	for b, ub := range waitBuckets {
		if waitSecs <= ub {
			i = b
			break
		}
	}
	r.waitCounts[i].Add(1)
	r.waitTotal.Add(1)
	for {
		old := r.waitSumBits.Load()
		nu := math.Float64bits(math.Float64frombits(old) + waitSecs)
		if r.waitSumBits.CompareAndSwap(old, nu) {
			return
		}
	}
}

// AddReconcile records the expirations and evictions of one Reconcile pass.
// Promotions go through ObserveAdmission by the caller.
func (r *Registry) AddReconcile(res store.ReconcileResult) {
	if r == nil {
		return
	}
	r.expirations.Add(int64(res.Expired))
	r.evictions.Add(int64(res.Evicted))
}

// IncRequest counts one gatekeeper decision.
func (r *Registry) IncRequest(decision string) {
	if r == nil {
		return
	}
	switch decision {
	case DecisionProxied:
		r.reqProxied.Add(1)
	case DecisionQueued:
		r.reqQueued.Add(1)
	case DecisionAdmin:
		r.reqAdmin.Add(1)
	}
}

// WriteTo emits the exposition text: live gauges from stats/capacity plus
// this instance's counters.
func (r *Registry) WriteTo(w io.Writer, stats store.Stats, capacity int) {
	if r == nil {
		return
	}
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(w, format+"\n", args...) }

	p("# HELP gowait_queue_length Users currently waiting in the queue.")
	p("# TYPE gowait_queue_length gauge")
	p("gowait_queue_length %d", stats.QueueLength)
	p("# HELP gowait_active_users Users currently admitted to the backend.")
	p("# TYPE gowait_active_users gauge")
	p("gowait_active_users %d", stats.ActiveCount)
	p("# HELP gowait_capacity Effective maximum concurrent active users.")
	p("# TYPE gowait_capacity gauge")
	p("gowait_capacity %d", capacity)
	p("# HELP gowait_avg_session_seconds EMA of completed session durations.")
	p("# TYPE gowait_avg_session_seconds gauge")
	p("gowait_avg_session_seconds %g", stats.AvgSessionSecs)

	p("# HELP gowait_admissions_total Users admitted to the backend.")
	p("# TYPE gowait_admissions_total counter")
	p("gowait_admissions_total %d", r.admissions.Load())
	p("# HELP gowait_expirations_total Active sessions reaped for inactivity.")
	p("# TYPE gowait_expirations_total counter")
	p("gowait_expirations_total %d", r.expirations.Load())
	p("# HELP gowait_evictions_total Queued users evicted for not polling.")
	p("# TYPE gowait_evictions_total counter")
	p("gowait_evictions_total %d", r.evictions.Load())

	p("# HELP gowait_requests_total Gatekeeper decisions by outcome.")
	p("# TYPE gowait_requests_total counter")
	p(`gowait_requests_total{decision="proxied"} %d`, r.reqProxied.Load())
	p(`gowait_requests_total{decision="queued"} %d`, r.reqQueued.Load())
	p(`gowait_requests_total{decision="admin"} %d`, r.reqAdmin.Load())

	p("# HELP gowait_wait_seconds Time users spent queued before admission.")
	p("# TYPE gowait_wait_seconds histogram")
	cum := int64(0)
	for i, ub := range waitBuckets {
		cum += r.waitCounts[i].Load()
		p(`gowait_wait_seconds_bucket{le="%g"} %d`, ub, cum)
	}
	cum += r.waitCounts[len(waitBuckets)].Load()
	p(`gowait_wait_seconds_bucket{le="+Inf"} %d`, cum)
	p("gowait_wait_seconds_sum %g", math.Float64frombits(r.waitSumBits.Load()))
	p("gowait_wait_seconds_count %d", r.waitTotal.Load())

	p("# HELP gowait_build_info Build information.")
	p("# TYPE gowait_build_info gauge")
	p(`gowait_build_info{version=%q} 1`, r.version)
}
