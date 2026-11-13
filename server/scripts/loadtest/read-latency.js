// Read-path latency probe for the API (k6, https://k6.io).
//
//   k6 run -e API=https://api.example.test -e EMAIL=loadtest@example.test -e ORG=<org id> \
//          -e VUS=32 -e DURATION=10m --summary-export=read-latency.json scripts/loadtest/read-latency.js
//
// The password comes from the PASSWORD environment variable (k6 passes the
// process environment through __ENV), never from a flag. One sign-in is
// shared by every virtual user, so the probe measures reads, not Argon2id.
// Each iteration lists the catalog (first page, then the next one), the
// datasets and the ingestion jobs, and reads one job's progress when JOB is
// set. Thresholds are the pass/fail lines from docs/load-testing.md.
import http from 'k6/http';
import { check, fail } from 'k6';

const api = (__ENV.API || 'http://127.0.0.1:8080').replace(/\/$/, '') + '/api/v1';

export const options = {
  vus: Number(__ENV.VUS || 16),
  duration: __ENV.DURATION || '2m',
  thresholds: {
    http_req_failed: ['rate<0.001'],
    'http_req_duration{read:catalog_first_page}': ['p(95)<150', 'p(99)<400'],
    'http_req_duration{read:catalog_next_page}': ['p(95)<150', 'p(99)<400'],
    'http_req_duration{read:datasets}': ['p(95)<150', 'p(99)<400'],
    'http_req_duration{read:ingestion_jobs}': ['p(95)<150', 'p(99)<400'],
    'http_req_duration{read:ingestion_job}': ['p(95)<100', 'p(99)<300'],
  },
};

export function setup() {
  if (!__ENV.EMAIL || !__ENV.PASSWORD) {
    fail('set EMAIL and the PASSWORD environment variable');
  }
  const res = http.post(`${api}/auth/login`, JSON.stringify({ email: __ENV.EMAIL, password: __ENV.PASSWORD }), {
    headers: { 'Content-Type': 'application/json' },
  });
  if (res.status !== 200) {
    fail(`sign-in failed: ${res.status}`);
  }
  return { token: res.json('access_token') };
}

function get(path, read, token) {
  const headers = { Authorization: `Bearer ${token}` };
  if (__ENV.ORG) {
    headers['X-Organization-ID'] = __ENV.ORG;
  }
  const res = http.get(`${api}${path}`, { headers, tags: { read } });
  check(res, { [`${read} 200`]: (r) => r.status === 200 });
  return res;
}

export default function (data) {
  const first = get('/catalog/objects?limit=100', 'catalog_first_page', data.token);
  const cursor = first.status === 200 ? first.json('next_cursor') : '';
  if (cursor) {
    get(`/catalog/objects?limit=100&cursor=${encodeURIComponent(cursor)}`, 'catalog_next_page', data.token);
  }
  get('/datasets?limit=50', 'datasets', data.token);
  get('/ingestion/jobs?limit=50', 'ingestion_jobs', data.token);
  if (__ENV.JOB) {
    get(`/ingestion/jobs/${__ENV.JOB}`, 'ingestion_job', data.token);
  }
}
