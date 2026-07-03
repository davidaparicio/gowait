package prober

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

var ctx = context.Background()

type fakeCtrl struct {
	capacity int
	setErr   error
	sets     int
}

func (f *fakeCtrl) Capacity() int { return f.capacity }

func (f *fakeCtrl) SetCapacity(_ context.Context, n int) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.capacity = n
	f.sets++
	return nil
}

type fakeLocker struct {
	ok    bool
	err   error
	calls int
}

func (f *fakeLocker) TryLock(context.Context, string, time.Duration) (bool, error) {
	f.calls++
	return f.ok, f.err
}

// serve returns a Prober against a test server answering with the given
// status, plus the controller and a hit counter.
func serve(t *testing.T, status int, cfg Config) (*Prober, *fakeCtrl, *int) {
	t.Helper()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	cfg.URL = srv.URL
	if cfg.Interval == 0 {
		cfg.Interval = time.Second
	}
	ctrl := &fakeCtrl{capacity: 10}
	return New(cfg, ctrl, nil), ctrl, &hits
}

func TestAdditiveIncrease(t *testing.T) {
	p, ctrl, _ := serve(t, http.StatusOK, Config{Min: 1, Max: 12})
	for i := 0; i < 5; i++ {
		p.tick(ctx)
	}
	if ctrl.capacity != 11 {
		t.Errorf("capacity after 5 healthy ticks = %d, want 11 (+1 per 3 successes)", ctrl.capacity)
	}
	p.tick(ctx) // 6th healthy tick completes the second streak
	if ctrl.capacity != 12 {
		t.Errorf("capacity after 6 healthy ticks = %d, want 12", ctrl.capacity)
	}
}

func TestIncreaseStopsAtMax(t *testing.T) {
	p, ctrl, _ := serve(t, http.StatusOK, Config{Min: 1, Max: 10})
	for i := 0; i < 9; i++ {
		p.tick(ctx)
	}
	if ctrl.capacity != 10 || ctrl.sets != 0 {
		t.Errorf("capacity = %d (sets = %d), want 10 untouched at the ceiling", ctrl.capacity, ctrl.sets)
	}
}

func TestMultiplicativeDecrease(t *testing.T) {
	p, ctrl, _ := serve(t, http.StatusInternalServerError, Config{Min: 2, Max: 100})
	want := []int{5, 2, 2} // 10 → 5 → 2 (clamped) → 2
	for i, w := range want {
		p.tick(ctx)
		if ctrl.capacity != w {
			t.Errorf("capacity after failure %d = %d, want %d", i+1, ctrl.capacity, w)
		}
	}
}

func TestFailureResetsStreak(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
	defer srv.Close()
	ctrl := &fakeCtrl{capacity: 10}
	p := New(Config{URL: srv.URL, Interval: time.Second, Min: 1, Max: 100}, ctrl, nil)

	p.tick(ctx)
	p.tick(ctx) // 2 successes banked
	status = http.StatusServiceUnavailable
	p.tick(ctx) // failure: halve and reset the streak
	if ctrl.capacity != 5 {
		t.Fatalf("capacity after failure = %d, want 5", ctrl.capacity)
	}
	status = http.StatusOK
	p.tick(ctx)
	p.tick(ctx)
	if ctrl.capacity != 5 {
		t.Errorf("capacity after 2 post-failure successes = %d, want 5 (streak must restart)", ctrl.capacity)
	}
	p.tick(ctx)
	if ctrl.capacity != 6 {
		t.Errorf("capacity after 3 post-failure successes = %d, want 6", ctrl.capacity)
	}
}

func TestRedirectCountsHealthy(t *testing.T) {
	p, ctrl, _ := serve(t, http.StatusFound, Config{Min: 1, Max: 100})
	p.tick(ctx)
	if ctrl.capacity != 10 {
		t.Errorf("capacity = %d, want 10 (3xx is healthy, no decrease)", ctrl.capacity)
	}
}

func TestClientErrorCountsUnhealthy(t *testing.T) {
	p, ctrl, _ := serve(t, http.StatusNotFound, Config{Min: 1, Max: 100})
	p.tick(ctx)
	if ctrl.capacity != 5 {
		t.Errorf("capacity = %d, want 5 (404 is unhealthy)", ctrl.capacity)
	}
}

func TestUnreachableBackendCountsUnhealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close() // now nothing listens there
	ctrl := &fakeCtrl{capacity: 10}
	p := New(Config{URL: srv.URL, Interval: time.Second, Min: 1, Max: 100}, ctrl, nil)
	p.tick(ctx)
	if ctrl.capacity != 5 {
		t.Errorf("capacity = %d, want 5 (connection refused is unhealthy)", ctrl.capacity)
	}
}

func TestLockDeniedSkipsRound(t *testing.T) {
	p, ctrl, hits := serve(t, http.StatusOK, Config{Min: 1, Max: 100})
	lk := &fakeLocker{ok: false}
	p.locker = lk
	for i := 0; i < 3; i++ {
		p.tick(ctx)
	}
	if lk.calls != 3 || *hits != 0 || ctrl.sets != 0 {
		t.Errorf("locks = %d, probes = %d, sets = %d; want 3 lock attempts and nothing else",
			lk.calls, *hits, ctrl.sets)
	}
}

func TestLockErrorSkipsRound(t *testing.T) {
	p, _, hits := serve(t, http.StatusOK, Config{Min: 1, Max: 100})
	p.locker = &fakeLocker{err: errors.New("valkey down")}
	p.tick(ctx)
	if *hits != 0 {
		t.Errorf("probes = %d, want 0 when the lock errors", *hits)
	}
}

func TestLockAcquiredProbes(t *testing.T) {
	p, ctrl, hits := serve(t, http.StatusBadGateway, Config{Min: 1, Max: 100})
	p.locker = &fakeLocker{ok: true}
	p.tick(ctx)
	if *hits != 1 || ctrl.capacity != 5 {
		t.Errorf("probes = %d, capacity = %d; want 1 probe and a halve", *hits, ctrl.capacity)
	}
}

func TestSetCapacityErrorKeepsGoing(t *testing.T) {
	p, ctrl, _ := serve(t, http.StatusInternalServerError, Config{Min: 1, Max: 100})
	ctrl.setErr = errors.New("store down")
	p.tick(ctx) // must not panic; capacity stays
	if ctrl.capacity != 10 {
		t.Errorf("capacity = %d, want 10 when the store write fails", ctrl.capacity)
	}
}
