# Load test results

Measured numbers only. Each row is one k6 run (`make load-steady` / `make load-hot`) against the API from `make run`, with Prometheus scraping it. Lock wait and pool figures come from Prometheus over the same window.

## Setup

| | |
|---|---|
| Machine | Intel Core 7 240H laptop (16 logical CPUs, 16 GB RAM), Windows 11 |
| API | `go build`, run natively on Windows, default pgxpool size (16 connections) |
| Postgres / Redis | `postgres:16-alpine`, `redis:7-alpine` in Docker Desktop (WSL2), default config |
| Load generator | `grafana/k6:1.2.0` in Docker on the same machine |
| Accounts | 1,000 funded user accounts per run (+1 merchant for the hot test) |

Everything shares one laptop, so these are relative numbers for comparing designs, not capacity figures.

## Baseline (milestone 3, before the hot-account rollup)

### Steady: random pairs across 1,000 accounts

| Target rate | Achieved | p50 | p95 | p99 | Failed | Notes |
|---|---|---|---|---|---|---|
| 300/s, 60s | 300/s | 6.6 ms | 11.9 ms | 32.4 ms | 0% | |
| 1,000/s, 30s | 895/s | 23.9 ms | 419 ms | 579 ms | 0% | saturated; 1,034 iterations dropped |
| 2,000/s, 30s | 1,269/s | 278 ms | 409 ms | 460 ms | 0% | well past saturation |

With 1,000 accounts, two transfers rarely touch the same row. Throughput is bounded by the connection pool: at 2,000/s nearly every acquire waited for a free connection.

### Hot account: 90% of transfers credit one merchant

| Target rate | Achieved | Merchant p50 / p99 | User→user p50 / p99 | Lock wait p50 / p99 | Pool acquires that waited | Failed |
|---|---|---|---|---|---|---|
| 200/s, 60s | 202/s | 5.8 / 14.3 ms | 5.8 / 10.8 ms | 0.7 / 4.4 ms | 0 | 0% |
| 300/s, 60s | 301/s | 24.0 / 882 ms | 8.2 / 443 ms | 4.4 / 719 ms | 9,342 of 19,498 (48%) | 0% |

The steady test runs 300/s with a 32 ms p99. Point 90% of that traffic at one account and p99 climbs to 882 ms. Every merchant credit runs `SELECT ... FOR UPDATE` on the same row, so those transactions run one at a time. The ceiling is roughly 1 / (time the lock is held).

The user→user column shows the second-order effect. Those transfers never touch the merchant, yet their p99 rises from 11 ms to 443 ms. Transactions queued on the merchant lock hold pool connections while they wait. Unrelated work then queues for a connection: head-of-line blocking at the pool.

Reconciliation ran every 15 seconds through every run above and reported 0 mismatches each time.

## After the hot-account rollup (milestone 5)

The merchant is created with `hot: true`. Credits to it insert their entry plus a `hot_pending` row and skip the account's row lock. The rollup worker folds pending rows into the cached balance every 100 ms. `HOT=false` creates an ordinary merchant on the same build, so both sides of each comparison ran back to back on identical code.

### Hot account, before and after

| Run | Achieved | Merchant p50 / p99 | User→user p50 / p99 | Lock wait p50 / p99 | Pool acquires that waited | Failed |
|---|---|---|---|---|---|---|
| 300/s, ordinary merchant (`HOT=false`) | 275/s (383 dropped) | 285 ms / 1.67 s | 246 ms / 1.39 s | 5.5 / 846 ms | 11,463 of 17,228 (67%) | 0% |
| 300/s, hot merchant | 283/s (0 dropped) | 6.4 / 30.9 ms | 6.0 / 26.5 ms | 0.6 / 1.8 ms | 55 of 19,223 (0.3%) | 0% |
| 1,000/s, hot merchant, 30s | 879/s (350 dropped) | 10.7 / 232 ms | 10.2 / 235 ms | 0.8 / 4.2 ms | 11,608 of 34,640 | 0% |

At 300/s, merchant p99 drops from 1.67 s to 31 ms and lock-wait p99 from 846 ms to 1.8 ms. Unrelated user→user transfers recover too (1.39 s to 27 ms), because connections no longer sit queued on the merchant's lock.

At 1,000/s the hot test saturates at 879/s, essentially the steady test's ceiling (below). The hot account is no longer the bottleneck; the connection pool is, as it is for the spread-out workload. Merchant and user→user latencies are the same, which says the remaining queueing has nothing to do with the merchant.

Today's `HOT=false` rerun was slower than the milestone 3 baseline at the same rate (merchant p99 1.67 s vs 882 ms). Both are well past the knee, where small changes in lock hold time swing the queue a lot. The comparison that matters is the back-to-back pair above.

### Steady load on the milestone 5 build

The transfer path now reads account attributes before deciding which rows to lock: one extra round trip per transfer.

| Target rate | Achieved | p50 | p95 | p99 | Failed |
|---|---|---|---|---|---|
| 300/s, 60s | 284/s | 6.7 ms | 13.0 ms | 51.7 ms | 0% |
| 1,000/s, 30s | 896/s | 8.9 ms | 148 ms | 276 ms | 0% |

p50 matches milestone 3 (6.6 ms). p99 at 300/s is 52 ms against 32 ms before, but tail latency on this shared laptop varies by that much between runs (at 1,000/s it came out lower than before). A cache of each account's immutable attributes would remove the extra query if it ever shows up in a profile.

### Rollup worker

Over all the runs above, the worker applied 42,962 pending entries in 2,922 ticks; idle ticks are cheap. `ledger_hot_pending_entries` read 0 after each run drained. Reconciliation ran every 15 seconds throughout and reported 0 mismatches every time, with effective balances (cached plus pending) compared against the entries.
