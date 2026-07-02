package memory

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/davidaparicio/gowait/internal/store"
)

var (
	ctx       = context.Background()
	t0        = time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	activeTTL = 60 * time.Second
	queueTTL  = 30 * time.Second
)

func TestTryAdmitUpToCapacity(t *testing.T) {
	s := New()
	for i := 0; i < 3; i++ {
		ok, err := s.TryAdmit(ctx, fmt.Sprintf("u%d", i), 3, t0)
		if err != nil || !ok {
			t.Fatalf("user %d not admitted: ok=%v err=%v", i, ok, err)
		}
	}
	if ok, _ := s.TryAdmit(ctx, "u3", 3, t0); ok {
		t.Fatal("user admitted beyond capacity")
	}
}

func TestTryAdmitIdempotentForActive(t *testing.T) {
	s := New()
	_, _ = s.TryAdmit(ctx, "a", 1, t0)
	ok, _ := s.TryAdmit(ctx, "a", 1, t0.Add(time.Second))
	if !ok {
		t.Fatal("re-admit of active user failed")
	}
	stats, _ := s.Stats(ctx)
	if stats.ActiveCount != 1 {
		t.Fatalf("ActiveCount = %d, want 1", stats.ActiveCount)
	}
}

func TestTryAdmitFIFOFairness(t *testing.T) {
	s := New()
	_, _ = s.TryAdmit(ctx, "a", 1, t0)
	_, _ = s.Enqueue(ctx, "waiting", t0)

	// A slot frees up conceptually (capacity raised), but "newcomer" must not
	// jump the queued user.
	if ok, _ := s.TryAdmit(ctx, "newcomer", 2, t0); ok {
		t.Fatal("newcomer jumped a non-empty queue despite free slot")
	}
}

func TestEnqueueFIFOPositions(t *testing.T) {
	s := New()
	for i := 1; i <= 3; i++ {
		snap, err := s.Enqueue(ctx, fmt.Sprintf("q%d", i), t0)
		if err != nil {
			t.Fatal(err)
		}
		if snap.Position != i {
			t.Fatalf("q%d Position = %d, want %d", i, snap.Position, i)
		}
	}
	// Re-enqueue is a no-op that keeps the position.
	snap, _ := s.Enqueue(ctx, "q1", t0.Add(time.Second))
	if snap.Position != 1 {
		t.Fatalf("re-enqueued q1 Position = %d, want 1", snap.Position)
	}
}

func TestReconcileExpiresAndPromotes(t *testing.T) {
	s := New()
	_, _ = s.TryAdmit(ctx, "a", 1, t0)
	_, _ = s.Enqueue(ctx, "b", t0)

	// b keeps polling; a goes idle past the TTL.
	later := t0.Add(activeTTL + time.Second)
	_, _ = s.Lookup(ctx, "b", later)
	res, err := s.Reconcile(ctx, 1, activeTTL, queueTTL, later)
	if err != nil {
		t.Fatal(err)
	}
	if res.Promoted != 1 || res.Expired != 1 {
		t.Fatalf("Reconcile = %+v, want Promoted=1 Expired=1", res)
	}
	if len(res.WaitedSecs) != 1 || res.WaitedSecs[0] != (activeTTL+time.Second).Seconds() {
		t.Fatalf("WaitedSecs = %v, want [%v]", res.WaitedSecs, (activeTTL + time.Second).Seconds())
	}
	snapA, _ := s.Lookup(ctx, "a", later)
	if snapA.Status != store.StatusUnknown {
		t.Fatalf("expired a: Status = %v, want Unknown", snapA.Status)
	}
	snapB, _ := s.Lookup(ctx, "b", later)
	if snapB.Status != store.StatusActive {
		t.Fatalf("promoted b: Status = %v, want Active", snapB.Status)
	}
}

func TestReconcileEvictsGhosts(t *testing.T) {
	s := New()
	_, _ = s.TryAdmit(ctx, "a", 1, t0)
	_, _ = s.Enqueue(ctx, "ghost", t0)
	_, _ = s.Enqueue(ctx, "alive", t0)

	// alive polls, ghost doesn't.
	mid := t0.Add(queueTTL + time.Second)
	_, _ = s.Touch(ctx, "a", mid)
	_, _ = s.Lookup(ctx, "alive", mid)
	// Overwrite ghost's heartbeat? No — Lookup refreshes, so don't look it up.
	if _, err := s.Reconcile(ctx, 1, activeTTL, queueTTL, mid); err != nil {
		t.Fatal(err)
	}

	snapGhost, _ := s.Lookup(ctx, "ghost", mid)
	if snapGhost.Status != store.StatusUnknown {
		t.Fatalf("ghost Status = %v, want Unknown (evicted)", snapGhost.Status)
	}
	snapAlive, _ := s.Lookup(ctx, "alive", mid)
	if snapAlive.Position != 1 {
		t.Fatalf("alive Position = %d, want 1 after ghost eviction", snapAlive.Position)
	}
}

func TestReconcileFeedsEMA(t *testing.T) {
	s := New()
	_, _ = s.TryAdmit(ctx, "a", 1, t0)
	// a is last seen 40s after admission, then idles out.
	_, _ = s.Touch(ctx, "a", t0.Add(40*time.Second))
	_, _ = s.Reconcile(ctx, 1, activeTTL, queueTTL, t0.Add(40*time.Second).Add(activeTTL+time.Second))

	stats, _ := s.Stats(ctx)
	if stats.AvgSessionSecs != 40 {
		t.Fatalf("AvgSessionSecs = %v, want 40 (first sample seeds EMA)", stats.AvgSessionSecs)
	}
}

func TestCapacityRoundTrip(t *testing.T) {
	s := New()
	if _, set, _ := s.GetCapacity(ctx); set {
		t.Fatal("fresh store reports a capacity override")
	}
	if err := s.SetCapacity(ctx, 7); err != nil {
		t.Fatal(err)
	}
	n, set, err := s.GetCapacity(ctx)
	if err != nil || !set || n != 7 {
		t.Fatalf("GetCapacity = (%d, %v, %v), want (7, true, nil)", n, set, err)
	}
}

func TestConcurrentAccess(t *testing.T) {
	s := New()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("u%d", i)
			now := t0.Add(time.Duration(i) * time.Millisecond)
			if ok, _ := s.TryAdmit(ctx, id, 10, now); !ok {
				_, _ = s.Enqueue(ctx, id, now)
			}
			_, _ = s.Lookup(ctx, id, now)
			_, _ = s.Touch(ctx, id, now)
			_, _ = s.Reconcile(ctx, 10, activeTTL, queueTTL, now)
			_, _ = s.Stats(ctx)
		}(i)
	}
	wg.Wait()

	stats, _ := s.Stats(ctx)
	if stats.ActiveCount > 10 {
		t.Fatalf("ActiveCount = %d exceeds capacity 10", stats.ActiveCount)
	}
	if stats.ActiveCount+stats.QueueLength != 50 {
		t.Fatalf("active+queued = %d, want 50", stats.ActiveCount+stats.QueueLength)
	}
}
