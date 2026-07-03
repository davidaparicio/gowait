# Load test results (Phase 8)

Scenario: `loadtest/waitingroom.js` — 2000 distinct visitors (per-VU cookie
jars) rush gowait at once, poll `/gowait/status` at the advertised cadence
until admitted, browse briefly, leave.

Server flags: `-capacity 200 -inactivity-ttl 6s -queue-ttl 8s
-poll-interval 2s`, trivial Go backend, local Valkey in Docker.
Hardware: Apple M1 Max, k6 v2.1.0, ~590 req/s sustained, 44k requests per run.

## Results

| metric | memory | valkey (before) | valkey (after) |
|---|---|---|---|
| status p95 | 8.3 ms | 44.6 ms | **24.1 ms** |
| status median | 0.7 ms | 20.8 ms | **6.0 ms** |
| enter p95 | 21.4 ms | 99.4 ms | **65.7 ms** |
| time to admission (avg / max) | 36.3 s / 73 s | 36.6 s / 73 s | 36.2 s / 72 s |
| FIFO violations | 0 | 0 | 0 |
| failed requests | 0 | 0 | 0 |

Time-to-admission is dominated by slot turnover (capacity × TTL), identical
across stores and unchanged by the tuning — as it should be.

## What the data justified

**Memory store: O(n) position walk → sequence counters.** At 2000 waiters
the walk was invisible (status p95 8.3 ms), but a Go benchmark showed the
per-lookup cost is linear and runs under the store's single mutex:

| queued | before | after |
|---|---|---|
| 1k | 540 ns | 23 ns |
| 10k | 9.6 µs | 25 ns |
| 100k | 88.5 µs | **37 ns** |

At the 1e5-waiter target, 88 µs × one poll per waiter per interval saturates
the mutex. Fix: each queued entry gets a monotonic sequence number; position
is its seq distance to the queue head minus the evicted "holes" in between
(sorted slice, binary search). Exact positions, O(log holes) per lookup.
Reproduce with: `go test ./internal/store/memory/ -bench BenchmarkLookup -run '^$'`

**Valkey store: 3 round trips per status poll → 1.** Every poll ran
Reconcile (Lua scan) + Lookup + Stats (for the ETA). Two controller-level
changes, both no-ops unless configured, both set automatically in valkey
mode:

- `MinReconcileGap` (250 ms): request-triggered reconciles are throttled to
  one per gap per instance; the janitor still guarantees one per second, so
  promotions lag ≤250 ms and FIFO order is untouched.
- `StatsCacheTTL` (= poll interval): ETA math reuses the session-duration
  EMA for one poll interval instead of re-fetching per queued poller. The
  admin/metrics `Stats` stays uncached.

Result: status median 20.8 → 6.0 ms, p95 44.6 → 24.1 ms, and reconcile
executions on the Valkey server dropped from ~590/s (one per request) to ~5/s.

## Re-running

```sh
go build -o bin/gowait ./cmd/gowait
./bin/gowait -backend http://localhost:9001 -capacity 200 \
  -inactivity-ttl 6s -queue-ttl 8s -poll-interval 2s
make loadtest                 # or: k6 run -e VUS=5000 loadtest/waitingroom.js
```

A run finishes in `(VUS - capacity) / capacity × ~7.5s` — about 75 s for the
default 2000 VUs. Raise `ulimit -n` for VUS above ~2000.
