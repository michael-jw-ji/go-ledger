// Steady load: random transfers between 1,000 accounts at a constant rate.
// Contention is low, so this measures the per-transfer cost.
//
//   RATE (transfers/s, default 300)  DURATION (default 60s)
import { setupAccounts, transfer, pick } from './lib.js';

const RATE = Number(__ENV.RATE || 300);

export const options = {
  setupTimeout: '120s',
  scenarios: {
    steady: {
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
    'http_req_duration{name:transfer}': ['p(99)<250'],
  },
  summaryTrendStats: ['avg', 'p(50)', 'p(95)', 'p(99)', 'max'],
};

export function setup() {
  return setupAccounts(1000, 1_000_000);
}

export default function (data) {
  const from = pick(data.users);
  let to = pick(data.users);
  while (to === from) to = pick(data.users);
  transfer(from, to, 1 + Math.floor(Math.random() * 1000), 'transfer');
}
