# gowait

[![CI](https://github.com/davidaparicio/gowait/actions/workflows/ci.yml/badge.svg)](https://github.com/davidaparicio/gowait/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/davidaparicio/gowait)](https://github.com/davidaparicio/gowait/releases)
[![ghcr.io](https://img.shields.io/badge/ghcr.io-davidaparicio%2Fgowait-blue)](https://github.com/davidaparicio/gowait/pkgs/container/gowait)
[![Go Report Card](https://goreportcard.com/badge/github.com/davidaparicio/gowait)](https://goreportcard.com/report/github.com/davidaparicio/gowait)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

A virtual waiting room for saturated backends, as a drop-in reverse proxy.
Single static Go binary; the only dependency is the Valkey client, used when
you opt into shared state.

When your backend can only handle N concurrent users, gowait admits the first
N and puts everyone else in a Doctolib-style waiting room: position in line,
estimated wait time, automatic entry when it's their turn. Refreshing the page
never loses your place.

```
User ──▶ gowait ──▶ Backend
            │
            ├─ active or admin?  → transparent reverse proxy
            └─ room full?        → waiting page, served in place
                                   (JS polls /gowait/status, auto-enters)
```

## Features

- **Drop-in**: point your DNS/ingress at gowait, set `GOWAIT_BACKEND_URL`.
  Zero backend changes; WebSockets and SSE pass through.
- **Refresh-safe**: the queue ticket lives in an HMAC-signed, HttpOnly cookie;
  position is kept server-side. Tampering sends you to the back of the line.
- **Admin bypass**: present the admin key once (header or query param) and
  skip the queue for 12 hours.
- **Sliding sessions**: admitted users keep their slot while they browse;
  after a configurable idle period the slot frees and the next user enters.
- **Ghost eviction**: queued users who close their tab stop polling and are
  removed, so the line keeps moving.
- **Honest to machines**: non-HTML clients get `503` + `Retry-After` and a
  JSON body instead of an HTML page.

## Install

```sh
# Container image (multi-arch)
docker pull ghcr.io/davidaparicio/gowait:latest

# From source
go install github.com/davidaparicio/gowait/cmd/gowait@latest
```

Prebuilt binaries for Linux, macOS and Windows are on the
[releases page](https://github.com/davidaparicio/gowait/releases).

## Quickstart

```sh
docker compose up --build
```

This starts gowait (capacity **2**, 30 s idle TTL) in front of a
[whoami](https://hub.docker.com/r/traefik/whoami) backend on
<http://localhost:8080>. Open it in three browser windows (use incognito /
another profile for distinct users): the third gets the waiting room and
enters automatically once a slot frees.

Same thing with curl (each cookie jar is one user):

```sh
curl -sc jarA -b jarA localhost:8080/            # slot 1 → whoami output
curl -sc jarB -b jarB localhost:8080/            # slot 2 → whoami output
curl -sc jarC -b jarC localhost:8080/            # full   → waiting-room HTML
curl -s  -b jarC localhost:8080/gowait/status    # {"status":"queued","position":1,...}
sleep 31                                         # A and B idle out
curl -s  -b jarC localhost:8080/gowait/status    # {"status":"active",...}
curl -s  -b jarC localhost:8080/                 # → whoami output

# Admin bypass while the room is full:
curl -s -H 'X-Gowait-Admin: letmein' -c jarAdm -b jarAdm localhost:8080/
```

Or run the binary directly:

```sh
go build -o bin/gowait ./cmd/gowait
./bin/gowait -backend http://localhost:9000 -capacity 100
```

## Configuration

Flags win over environment variables, which win over defaults.

| Flag | Env | Default | Description |
|---|---|---|---|
| `-listen` | `GOWAIT_LISTEN` | `:8080` | Address to listen on |
| `-backend` | `GOWAIT_BACKEND_URL` | — | **Required.** Backend URL to proxy to |
| `-capacity` | `GOWAIT_CAPACITY` | `100` | Max concurrent active users |
| `-inactivity-ttl` | `GOWAIT_INACTIVITY_TTL` | `60s` | Idle time before an admitted user's slot frees |
| `-queue-ttl` | `GOWAIT_QUEUE_TTL` | `30s` | Idle time before a queued user is evicted (must be ≥ 2× poll interval) |
| `-poll-interval` | `GOWAIT_POLL_INTERVAL` | `3s` | Poll cadence advertised to the waiting page |
| `-cookie-secret` | `GOWAIT_COOKIE_SECRET` | random | HMAC key for cookies. Set it, or cookies die on restart |
| `-admin-key` | `GOWAIT_ADMIN_KEY` | — | Queue-bypass secret. Empty disables bypass |
| `-cookie-secure` | `GOWAIT_COOKIE_SECURE` | `false` | Set the `Secure` cookie attribute (enable behind TLS) |
| `-preserve-host` | `GOWAIT_PRESERVE_HOST` | `false` | Forward the original `Host` header to the backend |
| `-store` | `GOWAIT_STORE` | `memory` | State store: `memory` (single instance) or `valkey` (shared) |
| `-valkey-url` | `GOWAIT_VALKEY_URL` | — | Valkey/Redis URL (`valkey://host:6379`), required with `-store=valkey` |
| `-valkey-prefix` | `GOWAIT_VALKEY_PREFIX` | `gowait:` | Key namespace; use a `{hash-tag}:` prefix on Valkey Cluster |
| `-metrics` | `GOWAIT_METRICS` | `true` | Expose Prometheus metrics at `/gowait/metrics` |
| `-wait-lang` | `GOWAIT_WAIT_LANG` | `en` | Waiting page language: `en` or `fr` |
| `-wait-title` | `GOWAIT_WAIT_TITLE` | localized | Waiting page title/heading |
| `-wait-brand` | `GOWAIT_WAIT_BRAND` | — | Brand name shown above the heading (hidden if empty) |
| `-wait-message` | `GOWAIT_WAIT_MESSAGE` | localized | Waiting page explanation paragraph |
| `-wait-template` | `GOWAIT_WAIT_TEMPLATE` | embedded | Path to a custom waiting page template |
| `-probe-url` | `GOWAIT_PROBE_URL` | — | Backend health URL enabling the adaptive-capacity prober |
| `-probe-interval` | `GOWAIT_PROBE_INTERVAL` | `10s` | Health probe cadence (also the probe timeout and lock lease) |
| `-probe-min` | `GOWAIT_PROBE_MIN` | `1` | Capacity floor for the prober |
| `-probe-max` | `GOWAIT_PROBE_MAX` | `-capacity` | Capacity ceiling for the prober |

## Waiting page customization

The embedded page ships in English and French (`-wait-lang fr`), and the
title, brand, and message are configurable without touching HTML:

```sh
gowait -backend http://backend:8080 \
  -wait-lang fr -wait-brand "ACME Billetterie" -wait-title "Forte affluence"
```

For full control, `-wait-template` replaces the embedded page with your own
Go [`html/template`](https://pkg.go.dev/html/template). It receives:

| Field | Meaning |
|---|---|
| `{{.Lang}}` | Page language (`en`, `fr`) |
| `{{.Title}}` | Title (custom or localized default) |
| `{{.Brand}}` | Brand name (empty if unset) |
| `{{.Message}}` | Explanation paragraph (custom or localized default) |
| `{{.PollMs}}` | Poll interval in milliseconds |
| `{{.L}}` | Localized strings: `.EtaLabel`, `.Notice`, `.LessMinute`, `.Minute`, `.Minutes`, `.Position`, `.Updated` |

Use [the embedded page](internal/waitpage/wait.html) as a starting point —
its script polls `/gowait/status` and reloads once admitted, which any custom
page should keep doing. The template is rendered **once at startup** (bad
templates fail fast, and per-request cost stays zero) and must stay
self-contained: no external assets, so it renders even when everything else
is on fire.

## Endpoints

gowait reserves the `/gowait/` prefix; everything else is proxied or queued.

- `GET /gowait/status` — JSON used by the waiting page's poller. Doubles as
  the queued user's heartbeat.

  ```json
  {"status":"queued","position":12,"queue_length":40,"active_users":100,
   "eta_seconds":180,"poll_seconds":3,"server_time":"2026-07-02T14:05:00Z"}
  ```

- `GET /gowait/healthz` — liveness probe for gowait itself, never queued.

- `GET /gowait/metrics` — Prometheus metrics (disable with `-metrics=false`):
  gauges `gowait_queue_length`, `gowait_active_users`, `gowait_capacity`,
  `gowait_avg_session_seconds`; counters `gowait_admissions_total`,
  `gowait_expirations_total`, `gowait_evictions_total`,
  `gowait_requests_total{decision}`; histogram `gowait_wait_seconds` (time
  spent queued before admission). Counters are fed exactly once per event, so
  with multiple replicas `sum()` them; gauges reflect shared store state, so
  use `max()`.

## Admin API

With `GOWAIT_ADMIN_KEY` set, a small REST API is available under
`/gowait/admin/` (it returns 404 when no key is configured). Authenticate
with the `X-Gowait-Admin` header or an admin cookie:

```sh
K='X-Gowait-Admin: <key>'
curl -H "$K" localhost:8080/gowait/admin/stats
# {"active_users":2,"avg_session_seconds":41.3,"capacity":2,"queue_length":7}

curl -X PUT -H "$K" -d '{"capacity":50}' localhost:8080/gowait/admin/capacity
# {"capacity":50} — effective immediately, propagates to all replicas,
# persists across restarts in Valkey mode

curl -X POST -H "$K" localhost:8080/gowait/admin/flush
# {"flushed":7} — empties the queue; active sessions are never touched,
# flushed users re-enter the line on their next request
```

Lowering capacity never kicks active users; the room shrinks as their
sessions expire naturally.

## Adaptive capacity (health prober)

Off by default. With `-probe-url` set, gowait probes the backend's health
endpoint every `-probe-interval` and adjusts capacity with AIMD:

- **Probe fails** (connection error, timeout, or HTTP ≥ 400): capacity is
  **halved** immediately — a struggling backend sheds load fast.
- **Three consecutive successes** (HTTP 200–399): capacity grows by **+1** —
  recovery is deliberately gradual.
- Capacity always stays within `[-probe-min, -probe-max]`.

```sh
./bin/gowait -backend http://backend:8080 -capacity 100 \
  -probe-url http://backend:8080/healthz -probe-interval 10s -probe-min 10
```

Adjustments go through the same store-backed channel as
`PUT /gowait/admin/capacity`, so every replica adopts them within about a
second, and lowering capacity never kicks active users. With the Valkey
store, a `SET NX PX` lease ensures only one replica probes and adjusts per
interval, whichever grabs it first.

## Admin bypass

With `GOWAIT_ADMIN_KEY` set, either of these skips the queue and sets a
signed 12-hour admin cookie:

```
curl -H 'X-Gowait-Admin: <key>' https://example.com/
open 'https://example.com/?gowait_admin=<key>'   # redirects, key stripped from URL
```

## Valkey store (multiple replicas)

By default state lives in memory, which ties the waiting room to one gowait
process. With the [Valkey](https://github.com/valkey-io/valkey) store, all
replicas share one queue and gowait itself becomes stateless:

```sh
./bin/gowait -backend http://localhost:9000 \
  -store valkey -valkey-url valkey://localhost:6379 \
  -cookie-secret <same-on-every-replica>
```

Or try the demo with Valkey 9:

```sh
docker compose -f docker-compose.yml -f docker-compose.valkey.yml up --build
```

Every store operation runs as a single server-side Lua script, so admissions,
promotions and evictions stay atomic across replicas — no user can be
double-admitted and FIFO order holds. Any Redis-compatible server works.
Notes for production:

- Set the **same `GOWAIT_COOKIE_SECRET` on all replicas**, or they will
  reject each other's tickets.
- Capacity can be **changed at runtime** without a restart: the store holds a
  shared override (`<prefix>capacity` key) that every replica adopts within
  about a second. In Valkey mode the override also survives restarts and wins
  over the `-capacity` flag. Change it via `PUT /gowait/admin/capacity` (see
  [Admin API](#admin-api)).
- On Valkey Cluster, set a prefix containing a hash tag (e.g.
  `-valkey-prefix '{gowait}:'`) so all keys share one slot, which Lua
  scripting requires.

To run the store's conformance tests against a real Valkey:

```sh
make test-valkey
```

### Multi-replica demo

```sh
make demo-multi
```

runs **two gowait replicas** behind an nginx load balancer
(`deploy/nginx-lb.conf`), sharing the Valkey store. Kill one
(`docker kill gowait-gowait-1`) — waiters keep their exact place in line,
served by the survivor.

### Kubernetes

`deploy/k8s/` has plain manifests (Deployment ×2 with health probes,
Service, Ingress, ConfigMap, Secret, demo-grade Valkey), applied with
kustomize:

```sh
# Edit configmap.yaml (backend URL, capacity) and secret.yaml first!
kubectl apply -k deploy/k8s
```

## How it works

- One ticket per browser, stored as `gowait_ticket` (HMAC-SHA256-signed,
  HttpOnly). All state is server-side; the cookie only carries the ID.
- Admission is FIFO: a free slot goes to the queue head, never to a newcomer
  jumping the line.
- Promotion happens on every request/poll *and* via a background janitor, so
  the queue drains even with zero traffic.
- The estimated wait is `position × average-session-duration / capacity`,
  using an exponential moving average of completed sessions (falls back to
  the inactivity TTL before any data exists). It is an estimate, not a promise.
- The waiting page is served **in place** at the URL the user asked for — no
  redirect — so deep links and refreshes just work.

## Development

```sh
make test        # go test ./... -race
make vet
make demo        # docker compose up --build
make loadtest    # k6 rush scenario, see docs/LOADTEST.md
```

## Roadmap

All eight phases of [docs/ROADMAP.md](docs/ROADMAP.md) have shipped:
Prometheus metrics, admin API (live capacity changes, queue flush),
multi-replica + Kubernetes manifests, waiting page branding/i18n, adaptive
capacity via backend health probing, and k6 load testing with the tuning it
motivated ([docs/LOADTEST.md](docs/LOADTEST.md)).
