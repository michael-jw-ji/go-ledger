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

Not measured yet.
