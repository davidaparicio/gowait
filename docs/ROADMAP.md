# gowait Roadmap

v1 shipped the core: a drop-in reverse-proxy waiting room with FIFO admission,
sliding-TTL sessions, signed-cookie tickets, admin bypass, an embedded
self-updating waiting page, and pluggable state (in-memory or shared
[Valkey](https://github.com/valkey-io/valkey)).

This roadmap tracks what comes next. Phases are ordered by dependency; each
ships independently. Phase 2 (dynamic capacity) intentionally comes early —
it underpins both the admin API (4) and the health prober (7).

```
1 → 2 → {3, 4} → 5 → 6 (anytime) → 7 (needs 2) → 8 last
```

## Phase 1 — OSS release ✅ (this phase)

CI (test with a real Valkey service container, lint, docker build smoke),
goreleaser releases (multi-platform binaries + multi-arch images on
`ghcr.io/davidaparicio/gowait`), MIT license, README badges.

## Phase 2 — Dynamic capacity core ✅

Make capacity runtime-changeable with the store as the shared source of truth:

- `Store.SetCapacity` / `Store.GetCapacity` (memory: fields under the mutex;
  Valkey: a `capacity` key with plain GET/SET).
- `queue.Controller` caches capacity in an `atomic.Int64`; the janitor
  refreshes it from the store every tick, giving ≤1s cross-replica
  propagation.
- A stored override persists across restarts in Valkey mode and wins over the
  `-capacity` flag.

## Phase 3 — Prometheus metrics (`/gowait/metrics`) ✅

Hand-rolled text exposition, zero new dependencies:

- `Reconcile` returns a `ReconcileResult{Promoted, Expired, Evicted,
  WaitedSecs}` so events are counted exactly once cluster-wide (Valkey store
  gains an `enqueued` hash for wait durations).
- Counters: admissions, expirations, evictions, requests by decision.
  Gauges: queue length, active users, capacity, average session. Histogram:
  time waited before admission.
- Open by default, `-metrics=false` to disable.

## Phase 4 — Admin REST API (`/gowait/admin/*`)

Protected by the existing admin key/cookie (404 when no key is configured):

- `GET /gowait/admin/stats` — live queue/session stats.
- `GET|PUT /gowait/admin/capacity` — change capacity without restart (Phase 2).
- `POST /gowait/admin/flush` — empty the queue (new `Store.Flush`; never
  boots active sessions).

## Phase 5 — Multi-replica demo + Kubernetes manifests

- `docker-compose.multi.yml`: 2+ gowait replicas behind traefik sharing the
  Valkey store — one queue, replica failures don't lose positions.
- `deploy/k8s/`: plain manifests with kustomize (Deployment with
  readiness/liveness on `/gowait/healthz`, Service, Ingress,
  ConfigMap/Secret, demo Valkey).

## Phase 6 — Waiting page customization

- Config for title, brand, message, and language (English/French to start).
- `-wait-template` to override the embedded page with your own file.
- Constraints kept: rendered once at startup, zero external assets.

## Phase 7 — Backend health prober (adaptive capacity)

Optional, off by default. Probes a backend health URL and adjusts capacity
between configured min/max bounds with AIMD (halve on failure, +1 after 3
consecutive successes) via the Phase 2 mechanism. In Valkey mode a `SET NX PX`
lock ensures one adjuster per interval across replicas.

## Phase 8 — Load test + tuning

- k6 scenario (per-VU cookie jars = thousands of distinct waiters): measure
  time-to-admission, status-endpoint latency, and FIFO fairness.
- Data-driven fixes only: the memory store's O(n) position lookup (sequence
  counters), and the Valkey store's per-request round trips (skip per-request
  reconcile, cache ETA stats one poll interval).

## Cross-cutting: `store.Store` interface changes

| Phase | Change | memory | valkeystore |
|---|---|---|---|
| 2 | + `SetCapacity` / `GetCapacity` | 2 fields under mutex | `capacity` key, GET/SET |
| 3 | `Reconcile` → `ReconcileResult` | trivial | `enqueued` hash + Lua edits |
| 4 | + `Flush` | clear queue | 3-line Lua |
| 7 | optional `Locker` (not on `Store`) | — | `SET NX PX` |
