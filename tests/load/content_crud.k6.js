import http from "k6/http";
import { check, sleep } from "k6";
import { Rate, Trend } from "k6/metrics";

const ADMIN_URL = __ENV.LYEVE_ADMIN_URL || "http://localhost:3001";
const API_URL = __ENV.LYEVE_API_URL || "http://localhost:3002";
const EMAIL = __ENV.LYEVE_ADMIN_EMAIL || "admin@loadtest.local";
const PASSWORD=__ENV.LYEVE_ADMIN_PASS || "LoadTest123!";
const SCHEMA = __ENV.LYEVE_SCHEMA || "loadtest_posts";

const createFailed = new Rate("create_failed");
const readFailed = new Rate("read_failed");
const updateFailed = new Rate("update_failed");
const deleteFailed = new Rate("delete_failed");

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
    read_failed: ["rate<0.05"],
    update_failed: ["rate<0.05"],
    delete_failed: ["rate<0.05"],
  },
};

export function setup() {
  const setupRes = http.post(
    `${ADMIN_URL}/api/admin/setup`,
    JSON.stringify({ email: EMAIL, password: PASSWORD }),
    { headers: { "Content-Type": "application/json" } }
  );
  if (setupRes.status !== 201 && setupRes.status !== 409) {
    console.warn(`setup returned ${setupRes.status}: ${setupRes.body}`);
  }

  const loginRes = http.post(
    `${ADMIN_URL}/api/admin/auth/login`,
    JSON.stringify({ email: EMAIL, password: PASSWORD }),
    { headers: { "Content-Type": "application/json" } }
  );
  if (loginRes.status !== 200) {
    throw new Error(`login failed: ${loginRes.status} ${loginRes.body}`);
  }
  const adminToken = loginRes.json("token");

  const schemaRes = http.post(
    `${ADMIN_URL}/api/admin/schemas`,
    JSON.stringify({
      name: SCHEMA,
      fields: [
        { name: "title", field_type: "text", required: true },
        { name: "body", field_type: "rich_text" },
      ],
    }),
    {
      headers: {
        "Content-Type": "application/json",
        Authorization: `Bearer ${adminToken}`,
      },
    }
  );
  if (schemaRes.status !== 200 && schemaRes.status !== 409) {
    console.warn(`schema create returned ${schemaRes.status}: ${schemaRes.body}`);
  }

  const tokenRes = http.post(
    `${API_URL}/api/v1/auth/token`,
    JSON.stringify({ email: EMAIL, password: PASSWORD }),
    { headers: { "Content-Type": "application/json" } }
  );
  if (tokenRes.status !== 200) {
    throw new Error(`token failed: ${tokenRes.status} ${tokenRes.body}`);
  }

  return { token: tokenRes.json("token") };
}

export default function (data) {
  const headers = {
    "Content-Type": "application/json",
    Authorization: `Bearer ${data.token}`,
  };

  const createRes = http.post(
    `${API_URL}/api/v1/content/${SCHEMA}`,
    JSON.stringify({ data: { title: `Load test item ${Date.now()}` } }),
    { headers, tags: { name: "content_create" } }
  );
  const created = check(createRes, {
    "create status 201": (r) => r.status === 201,
    "create has id": (r) => {
      try { return r.json("id") !== undefined; } catch { return false; }
    },
  });
  createFailed.add(!created);

  if (!created) {
    sleep(1);
    return;
  }

  const id = createRes.json("id");

  const readRes = http.get(
    `${API_URL}/api/v1/content/${SCHEMA}/${id}`,
    { headers, tags: { name: "content_read" } }
  );
  const readOk = check(readRes, {
    "read status 200": (r) => r.status === 200,
    "read has data": (r) => {
      try { return r.json("data") !== undefined; } catch { return false; }
    },
  });
  readFailed.add(!readOk);

  const listRes = http.get(
    `${API_URL}/api/v1/content/${SCHEMA}?limit=10`,
    { headers, tags: { name: "content_list" } }
  );
  check(listRes, {
    "list status 200": (r) => r.status === 200,
  });

  const updateRes = http.put(
    `${API_URL}/api/v1/content/${SCHEMA}/${id}`,
    JSON.stringify({ data: { title: `Updated ${Date.now()}` } }),
    { headers, tags: { name: "content_update" } }
  );
  const updateOk = check(updateRes, {
    "update status 200": (r) => r.status === 200,
  });
  updateFailed.add(!updateOk);

  const deleteRes = http.del(
    `${API_URL}/api/v1/content/${SCHEMA}/${id}`,
    null,
    { headers, tags: { name: "content_delete" } }
  );
  const deleteOk = check(deleteRes, {
    "delete status 204": (r) => r.status === 204,
  });
  deleteFailed.add(!deleteOk);

  sleep(1);
}
