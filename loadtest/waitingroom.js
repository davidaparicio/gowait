// k6 waiting-room scenario: a rush of distinct visitors hits gowait at once.
//
// Each VU is one visitor with its own cookie jar (k6 default): enter the
// site, poll /gowait/status at the advertised cadence until admitted, browse
// briefly, leave. Measures:
//
//   gowait_time_to_admission          how long each visitor queued (ms)
//   http_req_duration{endpoint:status} status-endpoint latency
//   gowait_fifo_violations            times a visitor's position went UP
//
// Usage (against a locally running gowait, see `make loadtest` for the
// recommended server flags):
//
//   k6 run loadtest/waitingroom.js
//   k6 run -e VUS=5000 -e GOWAIT_URL=http://localhost:8080 loadtest/waitingroom.js

import http from "k6/http";
import { check, sleep } from "k6";
import { Counter, Trend } from "k6/metrics";

const timeToAdmission = new Trend("gowait_time_to_admission", true);
const fifoViolations = new Counter("gowait_fifo_violations");

const BASE = __ENV.GOWAIT_URL || "http://localhost:8080";
const SESSION_TOUCHES = 2; // proxied requests made once admitted

export const options = {
  scenarios: {
    rush: {
      executor: "per-vu-iterations", // every VU runs one full visitor journey
      vus: Number(__ENV.VUS || 2000),
      iterations: 1,
      maxDuration: __ENV.MAX_DURATION || "5m",
    },
  },
  thresholds: {
    "http_req_duration{endpoint:status}": ["p(95)<250"],
    "http_req_duration{endpoint:enter}": ["p(95)<1000"],
    gowait_fifo_violations: ["count==0"],
    checks: ["rate>0.99"],
  },
};

export default function () {
  const t0 = Date.now();
  const enter = http.get(`${BASE}/`, {
    headers: { Accept: "text/html" },
    tags: { endpoint: "enter" },
  });
  check(enter, { "entered (200)": (r) => r.status === 200 });

  let lastPosition = Infinity;
  for (;;) {
    const res = http.get(`${BASE}/gowait/status`, {
      tags: { endpoint: "status" },
    });
    if (!check(res, { "status poll ok": (r) => r.status === 200 })) {
      sleep(1);
      continue;
    }
    const s = res.json();
    if (s.status === "active") {
      timeToAdmission.add(Date.now() - t0);
      break;
    }
    // FIFO fairness: a waiting visitor must never lose ground. (Position may
    // stay equal between polls; going up means someone jumped the line or we
    // were evicted and re-queued at the tail.)
    if (s.position > lastPosition) {
      fifoViolations.add(1);
    }
    lastPosition = s.position;
    sleep(s.poll_seconds || 2);
  }

  // Admitted: browse a little so the slot is visibly held, then leave. The
  // server frees the slot -inactivity-ttl after the last touch.
  for (let i = 0; i < SESSION_TOUCHES; i++) {
    http.get(`${BASE}/`, {
      headers: { Accept: "text/html" },
      tags: { endpoint: "session" },
    });
    sleep(1);
  }
}
