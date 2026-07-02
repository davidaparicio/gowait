# gowait

A virtual waiting room for saturated backends, as a drop-in reverse proxy.
Single static Go binary, standard library only.

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

## Endpoints

gowait reserves the `/gowait/` prefix; everything else is proxied or queued.

- `GET /gowait/status` — JSON used by the waiting page's poller. Doubles as
  the queued user's heartbeat.

  ```json
  {"status":"queued","position":12,"queue_length":40,"active_users":100,
   "eta_seconds":180,"poll_seconds":3,"server_time":"2026-07-02T14:05:00Z"}
  ```

- `GET /gowait/healthz` — liveness probe for gowait itself, never queued.

## Admin bypass

With `GOWAIT_ADMIN_KEY` set, either of these skips the queue and sets a
signed 12-hour admin cookie:

```
curl -H 'X-Gowait-Admin: <key>' https://example.com/
open 'https://example.com/?gowait_admin=<key>'   # redirects, key stripped from URL
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
make test    # go test ./... -race
make vet
make demo    # docker compose up --build
```

## Future work

The seams are already in place:

- **Redis store** for multi-replica gowait: implement the `Store` interface
  (`internal/store/store.go`) — each method maps to one atomic Lua script.
- **Prometheus metrics**: wrap `Store.Stats()` under `/gowait/metrics`.
- **Admin API** (live capacity changes, queue flush) under the reserved
  `/gowait/` prefix.
