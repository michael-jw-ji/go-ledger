// Hot account: 90% of transfers credit one merchant account, the rest move
// money between random users. Every merchant credit takes the merchant's row
// lock, so these transfers serialize; watch ledger_lock_wait_seconds.
//
//   RATE (transfers/s, default 300)  DURATION (default 60s)
import { setupAccounts, transfer, pick } from './lib.js';

const RATE = Number(__ENV.RATE || 300);

export const options = {
  setupTimeout: '120s',
  scenarios: {
    hot: {
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: __ENV.DURATION || '60s',
      preAllocatedVUs: 50,
      maxVUs: 400,
    },
  },
  thresholds: {
    http_req_failed: ['rate<0.01'],
    // Always pass; declared so k6 reports each transfer type separately.
    'http_req_duration{name:to_merchant}': ['max>=0'],
    'http_req_duration{name:user_to_user}': ['max>=0'],
  },
  summaryTrendStats: ['avg', 'p(50)', 'p(95)', 'p(99)', 'max'],
};

export function setup() {
  const { users, extras } = setupAccounts(1000, 1_000_000, ['merchant']);
  return { users, merchant: extras[0] };
}

export default function (data) {
  const from = pick(data.users);
  if (Math.random() < 0.9) {
    transfer(from, data.merchant, 1 + Math.floor(Math.random() * 1000), 'to_merchant');
    return;
  }
  let to = pick(data.users);
  while (to === from) to = pick(data.users);
  transfer(from, to, 1 + Math.floor(Math.random() * 1000), 'user_to_user');
}
