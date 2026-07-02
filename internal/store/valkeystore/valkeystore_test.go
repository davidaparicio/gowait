package valkeystore

// These tests need a real Valkey (or Redis). They connect to
// GOWAIT_TEST_VALKEY_URL (default 127.0.0.1:6390) and are skipped when no
// server answers. Quick way to run them:
//
//	docker run -d --rm --name gowait-test-valkey -p 6390:6379 valkey/valkey:9.1.0-alpine
//	go test ./internal/store/valkeystore/ -race
//	docker stop gowait-test-valkey

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/valkey-io/valkey-go"

	"github.com/davidaparicio/gowait/internal/store"
)

var (
	ctx       = context.Background()
	t0        = time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	activeTTL = 60 * time.Second
	queueTTL  = 30 * time.Second
)

// newTestStore returns a Store with a unique key prefix per test, or skips
// if no Valkey is reachable.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	addr := os.Getenv("GOWAIT_TEST_VALKEY_URL")
	if addr == "" {
		addr = "127.0.0.1:6390"
	}
	client, err := valkey.NewClient(valkey.ClientOption{InitAddress: []string{addr}})
	if err != nil {
		t.Skipf("no Valkey at %s: %v (set GOWAIT_TEST_VALKEY_URL or start one)", addr, err)
	}
	if err := client.Do(ctx, client.B().Ping().Build()).Error(); err != nil {
		client.Close()
		t.Skipf("Valkey at %s not answering: %v", addr, err)
	}

	prefix := fmt.Sprintf("gowait-test:%s:%d:", t.Name(), time.Now().UnixNano())
	s := NewWithClient(client, prefix)
	t.Cleanup(func() {
		for _, key := range []string{s.order, s.seen, s.active, s.admitted, s.avg, s.seq} {
			_ = client.Do(ctx, client.B().Del().Key(key).Build()).Error()
		}
		client.Close()
	})
	return s
}

func TestTryAdmitUpToCapacity(t *testing.T) {
	s := newTestStore(t)
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
	s := newTestStore(t)
	_, _ = s.TryAdmit(ctx, "a", 1, t0)
	ok, err := s.TryAdmit(ctx, "a", 1, t0.Add(time.Second))
	if err != nil || !ok {
		t.Fatalf("re-admit of active user failed: ok=%v err=%v", ok, err)
	}
	stats, _ := s.Stats(ctx)
	if stats.ActiveCount != 1 {
		t.Fatalf("ActiveCount = %d, want 1", stats.ActiveCount)
	}
}

func TestTryAdmitFIFOFairness(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.TryAdmit(ctx, "a", 1, t0)
	_, _ = s.Enqueue(ctx, "waiting", t0)

	if ok, _ := s.TryAdmit(ctx, "newcomer", 2, t0); ok {
		t.Fatal("newcomer jumped a non-empty queue despite free slot")
	}
}

func TestEnqueueFIFOPositions(t *testing.T) {
	s := newTestStore(t)
	for i := 1; i <= 3; i++ {
		snap, err := s.Enqueue(ctx, fmt.Sprintf("q%d", i), t0)
		if err != nil {
			t.Fatal(err)
		}
		if snap.Position != i {
			t.Fatalf("q%d Position = %d, want %d", i, snap.Position, i)
		}
	}
	snap, _ := s.Enqueue(ctx, "q1", t0.Add(time.Second))
	if snap.Position != 1 {
		t.Fatalf("re-enqueued q1 Position = %d, want 1", snap.Position)
	}
}

func TestReconcileExpiresAndPromotes(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.TryAdmit(ctx, "a", 1, t0)
	_, _ = s.Enqueue(ctx, "b", t0)

	later := t0.Add(activeTTL + time.Second)
	_, _ = s.Lookup(ctx, "b", later)
	promoted, err := s.Reconcile(ctx, 1, activeTTL, queueTTL, later)
	if err != nil {
		t.Fatal(err)
	}
	if promoted != 1 {
		t.Fatalf("promoted = %d, want 1", promoted)
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
	s := newTestStore(t)
	_, _ = s.TryAdmit(ctx, "a", 1, t0)
	_, _ = s.Enqueue(ctx, "ghost", t0)
	_, _ = s.Enqueue(ctx, "alive", t0)

	mid := t0.Add(queueTTL + time.Second)
	_, _ = s.Touch(ctx, "a", mid)
	_, _ = s.Lookup(ctx, "alive", mid)
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
	s := newTestStore(t)
	_, _ = s.TryAdmit(ctx, "a", 1, t0)
	_, _ = s.Touch(ctx, "a", t0.Add(40*time.Second))
	_, _ = s.Reconcile(ctx, 1, activeTTL, queueTTL, t0.Add(40*time.Second).Add(activeTTL+time.Second))

	stats, _ := s.Stats(ctx)
	if stats.AvgSessionSecs != 40 {
		t.Fatalf("AvgSessionSecs = %v, want 40 (first sample seeds EMA)", stats.AvgSessionSecs)
	}
}

func TestConcurrentAccess(t *testing.T) {
	s := newTestStore(t)
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
