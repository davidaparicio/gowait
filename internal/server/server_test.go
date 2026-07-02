package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/davidaparicio/gowait/internal/config"
	"github.com/davidaparicio/gowait/internal/metrics"
	"github.com/davidaparicio/gowait/internal/queue"
	"github.com/davidaparicio/gowait/internal/store/memory"
	"github.com/davidaparicio/gowait/internal/ticket"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

type testEnv struct {
	gowait     *httptest.Server
	clock      *fakeClock
	backendHit *atomic.Int64
}

func newTestEnv(t *testing.T, capacity int) *testEnv {
	return newTestEnvAdminKey(t, capacity, "letmein")
}

func newTestEnvAdminKey(t *testing.T, capacity int, adminKey string) *testEnv {
	t.Helper()

	var hits atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("hello from backend"))
	}))
	t.Cleanup(backend.Close)

	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		BackendURL:    backendURL,
		Capacity:      capacity,
		InactivityTTL: 60 * time.Second,
		QueueTTL:      30 * time.Second,
		PollInterval:  3 * time.Second,
		AdminKey:      adminKey,
		Metrics:       true,
	}
	clk := &fakeClock{t: time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)}
	ctrl := queue.New(memory.New(), queue.Config{
		Capacity:  cfg.Capacity,
		ActiveTTL: cfg.InactivityTTL,
		QueueTTL:  cfg.QueueTTL,
	}, clk.now)

	reg := metrics.New("test")
	ctrl.SetMetrics(reg)
	srv, err := New(cfg, ctrl, ticket.NewSigner("test-secret"), clk.now, reg)
	if err != nil {
		t.Fatal(err)
	}
	gowait := httptest.NewServer(srv.Handler())
	t.Cleanup(gowait.Close)

	return &testEnv{gowait: gowait, clock: clk, backendHit: &hits}
}

// client returns an HTTP client with its own cookie jar (one distinct user).
func (e *testEnv) client(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar}
}

func (e *testEnv) get(t *testing.T, c *http.Client, path string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.gowait.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(body)
}

func (e *testEnv) status(t *testing.T, c *http.Client) statusResponse {
	t.Helper()
	resp, body := e.get(t, c, "/gowait/status", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/gowait/status = %d, body %s", resp.StatusCode, body)
	}
	var s statusResponse
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatalf("bad status JSON %q: %v", body, err)
	}
	return s
}

func TestEndToEndQueueFlow(t *testing.T) {
	env := newTestEnv(t, 2)
	a, b, c := env.client(t), env.client(t), env.client(t)

	// A and B fill the two slots and reach the backend.
	for name, cl := range map[string]*http.Client{"A": a, "B": b} {
		resp, body := env.get(t, cl, "/", nil)
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, "hello from backend") {
			t.Fatalf("%s: expected backend response, got %d %q", name, resp.StatusCode, body)
		}
	}
	if env.backendHit.Load() != 2 {
		t.Fatalf("backend hits = %d, want 2", env.backendHit.Load())
	}

	// C gets the waiting page, not the backend.
	resp, body := env.get(t, c, "/some/deep/link", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "waiting room") {
		t.Fatalf("C: expected waiting page, got %d %q", resp.StatusCode, body[:min(80, len(body))])
	}
	if env.backendHit.Load() != 2 {
		t.Fatalf("backend hits = %d after queued user, want still 2", env.backendHit.Load())
	}

	// C polls: queued at position 1.
	st := env.status(t, c)
	if st.Status != "queued" || st.Position != 1 {
		t.Fatalf("C status = %+v, want queued/1", st)
	}
	if st.ETASeconds <= 0 {
		t.Fatalf("C eta_seconds = %d, want > 0", st.ETASeconds)
	}

	// Refresh must not lose the position (same cookie jar).
	st = env.status(t, c)
	if st.Position != 1 {
		t.Fatalf("C position after refresh = %d, want 1", st.Position)
	}

	// A and B go idle; C keeps polling (each poll refreshes its heartbeat,
	// like the real waiting page). Once A and B pass the TTL, C is promoted.
	env.clock.advance(25 * time.Second)
	env.status(t, c)
	env.clock.advance(25 * time.Second)
	env.status(t, c)
	env.clock.advance(25 * time.Second)
	st = env.status(t, c)
	if st.Status != "active" {
		t.Fatalf("C status after TTL = %q, want active", st.Status)
	}

	// And C's request now reaches the backend.
	resp, body = env.get(t, c, "/some/deep/link", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "hello from backend") {
		t.Fatalf("C after promotion: got %d %q", resp.StatusCode, body)
	}
}

func TestTamperedCookieGetsNewTicket(t *testing.T) {
	env := newTestEnv(t, 1)
	a := env.client(t)
	env.get(t, a, "/", nil) // admitted

	// Corrupt the ticket cookie.
	u, _ := url.Parse(env.gowait.URL)
	for _, ck := range a.Jar.Cookies(u) {
		if ck.Name == ticketCookie {
			ck.Value = ck.Value + "tamper"
			a.Jar.SetCookies(u, []*http.Cookie{ck})
		}
	}

	// Slot is taken by the original (untampered) ticket → tampered client is
	// treated as a new user and queued.
	resp, body := env.get(t, a, "/", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "waiting room") {
		t.Fatalf("tampered client: expected waiting page, got %d", resp.StatusCode)
	}
}

func TestAdminBypassWhileFull(t *testing.T) {
	env := newTestEnv(t, 1)
	env.get(t, env.client(t), "/", nil) // fill the only slot

	admin := env.client(t)

	// Wrong key is rejected.
	resp, _ := env.get(t, admin, "/", map[string]string{"X-Gowait-Admin": "wrong"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong admin key: got %d, want 403", resp.StatusCode)
	}

	// Correct key: redirect sets the admin cookie, then straight to backend.
	resp, body := env.get(t, admin, "/", map[string]string{"X-Gowait-Admin": "letmein"})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "hello from backend") {
		t.Fatalf("admin: expected backend via bypass, got %d %q", resp.StatusCode, body)
	}

	// Cookie persists: subsequent plain requests still bypass the full room.
	resp, body = env.get(t, admin, "/again", nil)
	if !strings.Contains(body, "hello from backend") {
		t.Fatalf("admin cookie not honored: got %d %q", resp.StatusCode, body)
	}
}

func TestQueuedAPIClientGets503(t *testing.T) {
	env := newTestEnv(t, 1)
	env.get(t, env.client(t), "/", nil) // fill the slot

	apiClient := env.client(t)
	req, _ := http.NewRequest(http.MethodGet, env.gowait.URL+"/api/thing", nil)
	req.Header.Set("Accept", "application/json")
	resp, err := apiClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("API client while queued: got %d, want 503", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("missing Retry-After header")
	}
	var s statusResponse
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatalf("503 body is not status JSON: %v", err)
	}
	if s.Status != "queued" {
		t.Fatalf("503 status = %q, want queued", s.Status)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	env := newTestEnv(t, 1)
	env.get(t, env.client(t), "/", nil) // one proxied request
	env.get(t, env.client(t), "/", nil) // one queued request

	resp, body := env.get(t, env.client(t), "/gowait/metrics", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics: got %d", resp.StatusCode)
	}
	for _, want := range []string{
		"gowait_active_users 1",
		"gowait_queue_length 1",
		"gowait_capacity 1",
		"gowait_admissions_total 1",
		`gowait_requests_total{decision="proxied"} 1`,
		`gowait_requests_total{decision="queued"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q\noutput:\n%s", want, body)
		}
	}
}

func (e *testEnv) adminReq(t *testing.T, method, path, key, body string) (*http.Response, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, e.gowait.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("X-Gowait-Admin", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(b)
}

func TestAdminAPIHiddenWithoutKey(t *testing.T) {
	env := newTestEnvAdminKey(t, 1, "") // admin bypass disabled
	resp, _ := env.adminReq(t, "GET", "/gowait/admin/stats", "", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("stats without configured key: got %d, want 404", resp.StatusCode)
	}
}

func TestAdminAPIAuth(t *testing.T) {
	env := newTestEnv(t, 2)

	resp, _ := env.adminReq(t, "GET", "/gowait/admin/stats", "wrong", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong key: got %d, want 403", resp.StatusCode)
	}
	resp, _ = env.adminReq(t, "GET", "/gowait/admin/stats", "", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no credentials: got %d, want 403", resp.StatusCode)
	}

	resp, body := env.adminReq(t, "GET", "/gowait/admin/stats", "letmein", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stats: got %d %s", resp.StatusCode, body)
	}
	var stats struct {
		Capacity int `json:"capacity"`
	}
	if err := json.Unmarshal([]byte(body), &stats); err != nil || stats.Capacity != 2 {
		t.Fatalf("stats body %q: capacity = %d, want 2", body, stats.Capacity)
	}
}

func TestAdminCapacityPut(t *testing.T) {
	env := newTestEnv(t, 2)
	env.get(t, env.client(t), "/", nil) // one active user

	resp, body := env.adminReq(t, "PUT", "/gowait/admin/capacity", "letmein", `{"capacity":0}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("capacity 0: got %d %s, want 400", resp.StatusCode, body)
	}

	resp, body = env.adminReq(t, "PUT", "/gowait/admin/capacity", "letmein", `{"capacity":1}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"capacity":1`) {
		t.Fatalf("capacity put: got %d %s", resp.StatusCode, body)
	}

	// Effective immediately: with capacity 1 and one active user, the next
	// new user queues even though the configured capacity was 2.
	resp, b := env.get(t, env.client(t), "/", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(b, "waiting room") {
		t.Fatalf("after capacity 1: expected waiting page, got %d", resp.StatusCode)
	}
}

func TestAdminFlush(t *testing.T) {
	env := newTestEnv(t, 1)
	env.get(t, env.client(t), "/", nil) // fill the slot
	c := env.client(t)
	env.get(t, c, "/", nil) // C queues

	resp, body := env.adminReq(t, "POST", "/gowait/admin/flush", "letmein", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"flushed":1`) {
		t.Fatalf("flush: got %d %s, want flushed:1", resp.StatusCode, body)
	}

	// GET on flush must not work (state-changing operations are POST-only).
	resp, _ = env.adminReq(t, "GET", "/gowait/admin/flush", "letmein", "")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET flush: got %d, want 405", resp.StatusCode)
	}

	// The flushed user re-enters the (now empty) queue on their next request.
	resp, b := env.get(t, c, "/", nil)
	if !strings.Contains(b, "waiting room") {
		t.Fatalf("flushed C: expected waiting page again, got %d", resp.StatusCode)
	}
	st := env.status(t, c)
	if st.Status != "queued" || st.Position != 1 {
		t.Fatalf("flushed C status = %+v, want queued/1", st)
	}
}

func TestHealthzNeverQueued(t *testing.T) {
	env := newTestEnv(t, 1)
	env.get(t, env.client(t), "/", nil) // fill the slot

	resp, body := env.get(t, env.client(t), "/gowait/healthz", nil)
	if resp.StatusCode != http.StatusOK || body != "ok" {
		t.Fatalf("healthz: got %d %q", resp.StatusCode, body)
	}
}
