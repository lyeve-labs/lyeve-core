import http from "k6/http";
import { check, sleep } from "k6";
import { Rate, Trend } from "k6/metrics";

const BASE_URL = __ENV.LYEVE_ADMIN_URL || "http://localhost:3001";
const EMAIL = __ENV.LYEVE_ADMIN_EMAIL || "admin@loadtest.local";
const PASSWORD=__ENV.LYEVE_ADMIN_PASS || "LoadTest123!";

const loginFailed = new Rate("login_failed");
const loginDuration = new Trend("login_duration", true);
const meDuration = new Trend("me_duration", true);

export const options = {
  stages: [
    { duration: "30s", target: 50 },
    { duration: "4m", target: 100 },
    { duration: "30s", target: 0 },
  ],
  thresholds: {
    http_req_duration: ["p(95)<500", "p(99)<1000"],
    http_req_failed: ["rate<0.05"],
    login_failed: ["rate<0.05"],
    login_duration: ["p(95)<300"],
    me_duration: ["p(95)<200"],
  },
};

export function setup() {
  const setupRes = http.post(
    `${BASE_URL}/api/admin/setup`,
    JSON.stringify({ email: EMAIL, password: PASSWORD }),
    { headers: { "Content-Type": "application/json" } }
  );
  if (setupRes.status !== 201 && setupRes.status !== 409) {
    console.warn(`setup returned ${setupRes.status}: ${setupRes.body}`);
  }
}

export default function () {
  const loginStart = Date.now();
  const loginRes = http.post(
    `${BASE_URL}/api/admin/auth/login`,
    JSON.stringify({ email: EMAIL, password: PASSWORD }),
    { headers: { "Content-Type": "application/json" }, tags: { name: "login" } }
  );
  loginDuration.add(Date.now() - loginStart);

  const loginOk = check(loginRes, {
    "login status 200": (r) => r.status === 200,
    "login has token": (r) => {
      try {
        return r.json("token") !== undefined;
      } catch {
        return false;
      }
    },
  });
  loginFailed.add(!loginOk);

  if (!loginOk) {
    sleep(1);
    return;
  }

  const token = loginRes.json("token");

  const meStart = Date.now();
  const meRes = http.get(`${BASE_URL}/api/admin/auth/me`, {
    headers: { Authorization: `Bearer ${token}` },
    tags: { name: "me" },
  });
  meDuration.add(Date.now() - meStart);

  check(meRes, {
    "me status 200": (r) => r.status === 200,
    "me has email": (r) => {
      try {
        return r.json("email") === EMAIL;
      } catch {
        return false;
      }
    },
  });

  const logoutRes = http.post(
    `${BASE_URL}/api/admin/auth/logout`,
    null,
    {
      headers: { Authorization: `Bearer ${token}` },
      tags: { name: "logout" },
    }
  );
  check(logoutRes, {
    "logout accepted": (r) => r.status === 200 || r.status === 204 || r.status === 404,
  });

  sleep(1);
}
