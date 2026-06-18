import http from "k6/http";
import { check, sleep } from "k6";
import { Rate, Trend } from "k6/metrics";
import { randomString } from "https://jslib.k6.io/k6-utils/1.4.0/index.js";

const ADMIN_URL = __ENV.LYEVE_ADMIN_URL || "http://localhost:3001";
const API_URL = __ENV.LYEVE_API_URL || "http://localhost:3002";
const EMAIL = __ENV.LYEVE_ADMIN_EMAIL || "admin@loadtest.local";
const PASSWORD=__ENV.LYEVE_ADMIN_PASS || "LoadTest123!";
const SCHEMA = __ENV.LYEVE_SCHEMA || "loadtest_tenant_items";
const NUM_TENANTS = parseInt(__ENV.TENANT_COUNT) || 10;

const crossTenantLeak = new Rate("cross_tenant_leak");
const tenantReqFailed = new Rate("tenant_req_failed");
const tenantReqDuration = new Trend("tenant_req_duration", true);

export const options = {
  scenarios: {
    ...Object.fromEntries(
      Array.from({ length: NUM_TENANTS }, (_, i) => [
        `tenant_${i}`,
        {
          executor: "constant-vus",
          vus: 20,
          duration: "5m",
          exec: "tenantScenario",
          env: { TENANT_ID: `tenant_${i}` },
        },
      ])
    ),
  },
  thresholds: {
    http_req_duration: ["p(95)<500", "p(99)<1000"],
    http_req_failed: ["rate<0.05"],
    cross_tenant_leak: ["rate<0"],
    tenant_req_failed: ["rate<0.05"],
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
    throw new Error(`login failed: ${loginRes.status}`);
  }
  const adminToken = loginRes.json("token");

  const schemaRes = http.post(
    `${ADMIN_URL}/api/admin/schemas`,
    JSON.stringify({
      name: SCHEMA,
      fields: [
        { name: "label", field_type: "text", required: true },
        { name: "tenant_tag", field_type: "text" },
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
    console.warn(`schema create: ${schemaRes.status}: ${schemaRes.body}`);
  }

  const tokenRes = http.post(
    `${API_URL}/api/v1/auth/token`,
    JSON.stringify({ email: EMAIL, password: PASSWORD }),
    { headers: { "Content-Type": "application/json" } }
  );
  if (tokenRes.status !== 200) {
    throw new Error(`token failed: ${tokenRes.status}`);
  }

  return { token: tokenRes.json("token") };
}

export function tenantScenario(data) {
  const tenantId = __ENV.TENANT_ID || "tenant_0";
  const headers = {
    "Content-Type": "application/json",
    Authorization: `Bearer ${data.token}`,
    "X-Tenant-ID": tenantId,
  };

  const tag = `[${tenantId}]`;

  const label = `${tenantId}_item_${randomString(6)}`;
  const start = Date.now();
  const createRes = http.post(
    `${API_URL}/api/v1/content/${SCHEMA}`,
    JSON.stringify({ data: { label, tenant_tag: tenantId } }),
    { headers, tags: { name: "tenant_create" } }
  );
  tenantReqDuration.add(Date.now() - start);

  const created = check(createRes, {
    [`${tag} create status 201`]: (r) => r.status === 201,
  });
  tenantReqFailed.add(!created);

  const listStart = Date.now();
  const listRes = http.get(
    `${API_URL}/api/v1/content/${SCHEMA}?limit=50`,
    { headers, tags: { name: "tenant_list" } }
  );
  tenantReqDuration.add(Date.now() - listStart);

  const listOk = check(listRes, {
    [`${tag} list status 200`]: (r) => r.status === 200,
  });
  tenantReqFailed.add(!listOk);

  if (listOk && listRes.status === 200) {
    try {
      const body = listRes.json();
      const items = body.data || body.items || [];
      for (const item of items) {
        const itemTag = item.tenant_tag || (item.data && item.data.tenant_tag);
        if (itemTag && itemTag !== tenantId) {
          crossTenantLeak.add(1);
          console.error(`LEAK: ${tenantId} saw item from ${itemTag}`);
          break;
        }
      }
    } catch (e) {
      // Response format may vary
    }
  }

  sleep(0.5);
}
