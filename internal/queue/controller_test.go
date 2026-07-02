package queue

import (
	"context"
	"testing"
	"time"

	"github.com/davidaparicio/gowait/internal/store"
	"github.com/davidaparicio/gowait/internal/store/memory"
)

// fakeClock is an adjustable clock for driving the controller in tests.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestController(capacity int) (*Controller, *fakeClock) {
	clk := &fakeClock{t: time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)}
	ctrl := New(memory.New(), Config{
		Capacity:  capacity,
		ActiveTTL: 60 * time.Second,
		QueueTTL:  30 * time.Second,
	}, clk.now)
	return ctrl, clk
}

func TestLifecycle(t *testing.T) {
	ctx := context.Background()
	ctrl, clk := newTestController(1)

	// First user is admitted.
	res, err := ctrl.Check(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != DecisionProxy {
		t.Fatalf("a: Decision = %v, want Proxy", res.Decision)
	}

	// Second user queues at position 1.
	res, _ = ctrl.Check(ctx, "b")
	if res.Decision != DecisionWait || res.Position != 1 {
		t.Fatalf("b: Decision=%v Position=%d, want Wait/1", res.Decision, res.Position)
	}
	if res.ETA <= 0 {
		t.Fatalf("b: ETA = %s, want > 0", res.ETA)
	}

	// a keeps browsing and b keeps polling: the sliding TTL holds b out even
	// well past a's original admission time.
	for i := 0; i < 2; i++ {
		clk.advance(25 * time.Second)
		ctrl.Check(ctx, "a")
		res, _ = ctrl.StatusOf(ctx, "b")
		if res.Decision != DecisionWait {
			t.Fatalf("b promoted while a still active (sliding TTL broken)")
		}
	}

	// a stops browsing; b keeps polling. Once a is idle past ActiveTTL, b's
	// next poll promotes it.
	clk.advance(25 * time.Second)
	ctrl.StatusOf(ctx, "b") // heartbeat, a idle 25s
	clk.advance(25 * time.Second)
	ctrl.StatusOf(ctx, "b") // heartbeat, a idle 50s
	clk.advance(25 * time.Second)
	res, _ = ctrl.StatusOf(ctx, "b") // a idle 75s > 60s TTL
	if res.Decision != DecisionProxy {
		t.Fatalf("b: Decision = %v after a expired, want Proxy", res.Decision)
	}

	// a comes back: its ticket expired, so it re-joins at the tail.
	res, _ = ctrl.Check(ctx, "a")
	if res.Decision != DecisionWait || res.Position != 1 {
		t.Fatalf("returning a: Decision=%v Position=%d, want Wait/1 (tail)", res.Decision, res.Position)
	}
}

func TestStatusOfDoesNotEnqueue(t *testing.T) {
	ctx := context.Background()
	ctrl, _ := newTestController(1)
	ctrl.Check(ctx, "a") // fill the slot

	res, err := ctrl.StatusOf(ctx, "stranger")
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != DecisionWait {
		t.Fatalf("stranger: Decision = %v, want Wait", res.Decision)
	}
	// A real request right after must find position 1 free: StatusOf must not
	// have enqueued "stranger".
	res, _ = ctrl.Check(ctx, "b")
	if res.Position != 1 {
		t.Fatalf("b Position = %d, want 1 (StatusOf must not enqueue)", res.Position)
	}
}

func TestStatusOfUnknownTicket(t *testing.T) {
	ctx := context.Background()
	ctrl, clk := newTestController(1)
	ctrl.Check(ctx, "a") // fill the slot

	// Room full: an unknown ticket (expired/evicted) is told the tail
	// position it would get on its next real request.
	res, _ := ctrl.StatusOf(ctx, "ghost")
	if res.Decision != DecisionWait || res.Position != 1 {
		t.Fatalf("ghost while full: Decision=%v Position=%d, want Wait/1", res.Decision, res.Position)
	}

	// Room empty: the unknown ticket must be told "active" so the waiting
	// page reloads and gets admitted, instead of polling forever.
	clk.advance(61 * time.Second)
	res, _ = ctrl.StatusOf(ctx, "ghost")
	if res.Decision != DecisionProxy {
		t.Fatalf("ghost with empty room: Decision = %v, want Proxy", res.Decision)
	}
}

func TestETAFallbackAndEMA(t *testing.T) {
	ctx := context.Background()
	ctrl, clk := newTestController(2)
	ctrl.Check(ctx, "a")
	ctrl.Check(ctx, "b")

	// No completed session yet → fallback: position × ActiveTTL / capacity.
	res, _ := ctrl.Check(ctx, "c")
	if want := 60 * time.Second / 2; res.ETA != want {
		t.Fatalf("fallback ETA = %s, want %s", res.ETA, want)
	}

	// a and b browse ~10s then go idle; c keeps polling until both expire.
	clk.advance(10 * time.Second)
	ctrl.Check(ctx, "a")
	ctrl.Check(ctx, "b")
	ctrl.StatusOf(ctx, "c")
	for i := 0; i < 3; i++ {
		clk.advance(25 * time.Second)
		ctrl.StatusOf(ctx, "c") // heartbeat; last iteration promotes c
	}

	// Sessions of ~10s completed → EMA kicks in. d takes the free slot, e
	// queues with an EMA-based ETA (10s/2 = 5s), far below the 30s fallback.
	res, _ = ctrl.Check(ctx, "d")
	if res.Decision != DecisionProxy {
		t.Fatalf("d: Decision = %v, want Proxy", res.Decision)
	}
	res, _ = ctrl.Check(ctx, "e")
	if res.Decision != DecisionWait {
		t.Fatalf("e: Decision = %v, want Wait", res.Decision)
	}
	if res.ETA >= 30*time.Second {
		t.Fatalf("ETA = %s, want < 30s (EMA of ~10s sessions should apply)", res.ETA)
	}
}

func TestJanitorPromotesWithoutTraffic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clk := &fakeClock{t: time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)}
	st := memory.New()
	ctrl := New(st, Config{Capacity: 1, ActiveTTL: 60 * time.Second, QueueTTL: 300 * time.Second}, clk.now)

	ctrl.Check(ctx, "a")
	ctrl.Check(ctx, "b") // queued

	// a idles out. Nobody sends requests; only the janitor runs.
	clk.advance(61 * time.Second)
	go ctrl.Run(ctx)

	deadline := time.After(3 * time.Second)
	for {
		snap, _ := st.Lookup(ctx, "b", clk.now())
		if snap.Status == store.StatusActive {
			return // janitor promoted b
		}
		select {
		case <-deadline:
			t.Fatal("janitor did not promote b within 3s")
		case <-time.After(50 * time.Millisecond):
		}
	}
}
