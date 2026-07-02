package metrics

import (
	"strings"
	"testing"

	"github.com/davidaparicio/gowait/internal/store"
)

func TestNilRegistryIsSafe(t *testing.T) {
	var r *Registry
	r.ObserveAdmission(1)
	r.AddReconcile(store.ReconcileResult{Expired: 2})
	r.IncRequest(DecisionProxied)
	r.WriteTo(nil, store.Stats{}, 0) // must not panic or write
}

func TestExposition(t *testing.T) {
	r := New("1.2.3")
	r.ObserveAdmission(0)   // direct admission → le="1" bucket
	r.ObserveAdmission(20)  // waited 20s → le="30" bucket
	r.ObserveAdmission(999) // waited past the last bound → +Inf only
	r.AddReconcile(store.ReconcileResult{Expired: 2, Evicted: 1})
	r.IncRequest(DecisionProxied)
	r.IncRequest(DecisionProxied)
	r.IncRequest(DecisionQueued)
	r.IncRequest(DecisionAdmin)

	var b strings.Builder
	r.WriteTo(&b, store.Stats{QueueLength: 4, ActiveCount: 7, AvgSessionSecs: 12.5}, 10)
	out := b.String()

	for _, want := range []string{
		"gowait_queue_length 4",
		"gowait_active_users 7",
		"gowait_capacity 10",
		"gowait_avg_session_seconds 12.5",
		"gowait_admissions_total 3",
		"gowait_expirations_total 2",
		"gowait_evictions_total 1",
		`gowait_requests_total{decision="proxied"} 2`,
		`gowait_requests_total{decision="queued"} 1`,
		`gowait_requests_total{decision="admin"} 1`,
		`gowait_wait_seconds_bucket{le="1"} 1`,
		`gowait_wait_seconds_bucket{le="15"} 1`,
		`gowait_wait_seconds_bucket{le="30"} 2`,   // cumulative: 0s + 20s
		`gowait_wait_seconds_bucket{le="600"} 2`,  // 999s not yet included
		`gowait_wait_seconds_bucket{le="+Inf"} 3`, // everything
		"gowait_wait_seconds_sum 1019",
		"gowait_wait_seconds_count 3",
		`gowait_build_info{version="1.2.3"} 1`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("exposition missing line %q\nfull output:\n%s", want, out)
		}
	}
}
