import http from "k6/http";
import { check, sleep } from "k6";
import { Rate, Trend } from "k6/metrics";
import { randomString } from "https://jslib.k6.io/k6-utils/1.4.0/index.js";

const BASE_URL = __ENV.LYEVE_ADMIN_URL || "http://localhost:3001";
const EMAIL = __ENV.LYEVE_ADMIN_EMAIL || "admin@loadtest.local";
const PASSWORD=__ENV.LYEVE_ADMIN_PASS || "LoadTest123!";

const createFailed = new Rate("schema_create_failed");
const getFailed = new Rate("schema_get_failed");
const listDuration = new Trend("schema_list_duration", true);
const getDuration = new Trend("schema_get_duration", true);

export const options = {
  stages: [
    { duration: "30s", target: 25 },
    { duration: "4m", target: 50 },
    { duration: "30s", target: 0 },
  ],
  thresholds: {
    http_req_duration: ["p(95)<500", "p(99)<1000"],
    http_req_failed: ["rate<0.05"],
    create_failed: ["rate<0.05"],
    get_failed: ["rate<0.05"],
    schema_list_duration: ["p(95)<300"],
    schema_get_duration: ["p(95)<200"],
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

  const loginRes = http.post(
    `${BASE_URL}/api/admin/auth/login`,
    JSON.stringify({ email: EMAIL, password: PASSWORD }),
    { headers: { "Content-Type": "application/json" } }
  );
  if (loginRes.status !== 200) {
    throw new Error(`login failed: ${loginRes.status}`);
  }
  return { token: loginRes.json("token") };
}

export default function (data) {
  const headers = {
    "Content-Type": "application/json",
    Authorization: `Bearer ${data.token}`,
  };

  const schemaName = `load_schema_${randomString(8)}`;

  const createRes = http.post(
    `${BASE_URL}/api/admin/schemas`,
    JSON.stringify({
      name: schemaName,
      fields: [
        { name: "title", field_type: "text", required: true },
        { name: "body", field_type: "rich_text" },
      ],
    }),
    { headers, tags: { name: "schema_create" } }
  );
  const created = check(createRes, {
    "schema create status 200": (r) => r.status === 200,
  });
  createFailed.add(!created);

  const listStart = Date.now();
  const listRes = http.get(`${BASE_URL}/api/admin/schemas`, {
    headers,
    tags: { name: "schema_list" },
  });
  listDuration.add(Date.now() - listStart);
  check(listRes, {
    "schema list status 200": (r) => r.status === 200,
  });

  if (created) {
    const getStart = Date.now();
    const getRes = http.get(`${BASE_URL}/api/admin/schemas/${schemaName}`, {
      headers,
      tags: { name: "schema_get" },
    });
    getDuration.add(Date.now() - getStart);
    const got = check(getRes, {
      "schema get status 200": (r) => r.status === 200,
      "schema get has name": (r) => {
        try { return r.json("name") === schemaName; } catch { return false; }
      },
    });
    getFailed.add(!got);

    http.del(`${BASE_URL}/api/admin/schemas/${schemaName}`, null, { headers });
  }

  sleep(1);
}
