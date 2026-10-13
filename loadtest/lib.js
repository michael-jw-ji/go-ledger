// Shared setup for the k6 scripts: create and fund accounts in batches.
import http from 'k6/http';
import { check, fail } from 'k6';

export const BASE = __ENV.BASE_URL || 'http://host.docker.internal:8088';
const JSON_HEADERS = { 'Content-Type': 'application/json' };

// A rejected transfer (422) is a correct answer, not a failed request.
http.setResponseCallback(http.expectedStatuses(200, 201, 422));

let seq = 0;
export function key(prefix) {
  seq++;
  // __VU is 0 in setup(); the random part keeps keys unique across VUs.
  return `${prefix}-${__VU}-${seq}-${Date.now()}-${Math.floor(Math.random() * 1e12)}`;
}

function createAccounts(specs) {
  const out = [];
  for (let i = 0; i < specs.length; i += 100) {
    const reqs = specs.slice(i, i + 100).map((s) => ['POST', `${BASE}/accounts`, JSON.stringify(s), { headers: JSON_HEADERS }]);
    for (const r of http.batch(reqs)) {
      if (r.status !== 201) fail(`create account: ${r.status} ${r.body}`);
      out.push(r.json().id);
    }
  }
  return out;
}

// Creates a funding account (allowed to go negative) and n user accounts,
// each funded with `fund` cents, plus extra accounts given as
// { name, hot } specs.
export function setupAccounts(n, fund, extra = []) {
  const run = Date.now();
  const [funding] = createAccounts([
    { name: `k6 funding ${run}`, currency: 'USD', normal_balance: 'credit', allow_negative: true },
  ]);
  const users = createAccounts(
    Array.from({ length: n }, (_, i) => ({ name: `k6 user ${run}-${i}`, currency: 'USD', normal_balance: 'credit' })),
  );
  const extras = createAccounts(
    extra.map((e) => ({ name: `k6 ${e.name} ${run}`, currency: 'USD', normal_balance: 'credit', hot: !!e.hot })),
  );
  for (let i = 0; i < users.length; i += 100) {
    const reqs = users.slice(i, i + 100).map((id) => [
      'POST',
      `${BASE}/transfers`,
      JSON.stringify({ debit_account_id: funding, credit_account_id: id, amount: fund }),
      { headers: { ...JSON_HEADERS, 'Idempotency-Key': key('fund') } },
    ]);
    for (const r of http.batch(reqs)) {
      if (r.status !== 201) fail(`fund account: ${r.status} ${r.body}`);
    }
  }
  return { funding, users, extras };
}

export function transfer(from, to, amount, tag) {
  const res = http.post(
    `${BASE}/transfers`,
    JSON.stringify({ debit_account_id: from, credit_account_id: to, amount }),
    { headers: { ...JSON_HEADERS, 'Idempotency-Key': key('t') }, tags: { name: tag } },
  );
  check(res, { 'posted or rejected': (r) => r.status === 201 || r.status === 422 });
  return res;
}

export function pick(arr) {
  return arr[Math.floor(Math.random() * arr.length)];
}
