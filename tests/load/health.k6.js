/**
 * k6 Load Test: Health Endpoints
 *
 * Tests /healthz and /readyz under high concurrency (200 VUs).
 * These are the lightest endpoints: they should sustain high throughput
 * without degradation.
 *
 * Environment variables:
 *   LYEVE_ADMIN_URL - Admin server (default: http://localhost:3001)
 *   LYEVE_API_URL - API server (default: http://localhost:3002)
 *
 * Usage:
 *   k6 run tests/load/health.k6.js
 */

import http from "k6/http";
import { check, sleep } from "k6";
import { Rate, Trend } from "k6/metrics";

const ADMIN_URL = __ENV.LYEVE_ADMIN_URL || "http://localhost:3001";
const API_URL = __ENV.LYEVE_API_URL || "http://localhost:3002";

const healthzFailed = new Rate("healthz_failed");
const readyzFailed = new Rate("readyz_failed");
const healthzDuration = new Trend("healthz_duration", true);
const readyzDuration = new Trend("readyz_duration", true);

export const options = {
  stages: [
    { duration: "15s", target: 100 },   // ramp up
    { duration: "1m30s", target: 200 }, // sustain 200 VUs
    { duration: "15s", target: 0 },     // ramp down
  ],
  thresholds: {
    http_req_duration: ["p(95)<100", "p(99)<200"],
    http_req_failed: ["rate<0.01"],
    healthz_failed: ["rate<0.01"],
    readyz_failed: ["rate<0.01"],
    healthz_duration: ["p(95)<50"],
    readyz_duration: ["p(95)<50"],
  },
};

export default function () {
  // Try common health endpoint paths on both admin and API servers.
  const targets = [
    { url: `${ADMIN_URL}/healthz`, name: "admin_healthz" },
    { url: `${ADMIN_URL}/readyz`, name: "admin_readyz" },
    { url: `${ADMIN_URL}/api/admin/healthz`, name: "admin_api_healthz" },
    { url: `${API_URL}/healthz`, name: "api_healthz" },
    { url: `${API_URL}/readyz`, name: "api_readyz" },
    { url: `${API_URL}/api/v1/healthz`, name: "api_v1_healthz" },
  ];

  for (const target of targets) {
    const start = Date.now();
    const res = http.get(target.url, {
      tags: { name: target.name },
      timeout: "5s",
    });
    const dur = Date.now() - start;

    if (target.name.includes("healthz")) {
      healthzDuration.add(dur);
      const ok = check(res, {
        [`${target.name} reachable`]: (r) => r.status === 200 || r.status === 404,
      });
      healthzFailed.add(!ok);
    } else {
      readyzDuration.add(dur);
      const ok = check(res, {
        [`${target.name} reachable`]: (r) => r.status === 200 || r.status === 404,
      });
      readyzFailed.add(!ok);
    }
  }

  sleep(0.1); // Minimal sleep: health endpoints should be fast.
}
