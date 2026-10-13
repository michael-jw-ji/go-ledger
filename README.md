# go-ledger

A double-entry payments ledger in Go and Postgres. Work in progress.

**Done (milestone 1):**
- Multi-leg balanced transfers in one transaction.
- Overdraft checks.
- Row locks taken in ascending id order.
- Append-only entries.
- Reversals.
- Cursor pagination.
- An idempotency backstop in Postgres: the key is unique, and rejected attempts are recorded too.

**Done (milestone 2):**
- Redis idempotency layer: `SET NX` claims, a 409 while a request is in flight, replay of stored responses, and a 422 when a key is reused with a different body. A 5xx frees the key for a retry. If Redis is unavailable, requests fall through to Postgres.
- Property test: 64 goroutines send random 2-to-4-leg transfers across 10 accounts, with 20% of requests resent under the same key (half of those at the same time).
  - After the run it checks four things: money is conserved, every cached balance and `lock_version` matches the entries, every key maps to exactly one transfer, and every response for a key agrees.
  - It passes with 10,000 transfers. A second variant runs `FLUSHALL` on Redis every 100ms and also passes.

Set the number of transfers with `LEDGER_PROP_ITERS=10000 make test-integration`.

**Done (milestone 3):**
- Prometheus metrics at `GET /metrics`:
  - request latency by route
  - transfers by outcome
  - idempotency lookups by result
  - row-lock wait and transaction time
  - pgxpool stats
- Reconciliation job (every 60s; `RECON_INTERVAL` changes it) and a `make recon` one-off.
  - It reads one REPEATABLE READ snapshot and checks three things: every cached balance and `lock_version` against the entries, that every transfer nets to zero, and that debits equal credits per currency.
  - Results go to `ledger_reconciliation_mismatches` (target 0), the `reconciliation_runs` table and `GET /reconciliation`.
  - A test corrupts one balance with raw SQL and requires exactly one finding.
- `make obs` starts Prometheus (:9090, with alert rules) and Grafana (:3000, provisioned dashboard).
- k6 baselines (`make load-steady`, `make load-hot`). [docs/results.md](docs/results.md) has the numbers. The steady test runs 300 transfers/s with a 32 ms p99. Sending 90% of those transfers to one merchant account pushes the p99 to 882 ms on that row lock.
- CI on GitHub Actions: gofmt, golangci-lint, unit tests, and integration tests with `-race`, plus the web typecheck, tests and build.

**Done (milestone 5): hot-account rollup.**
- Accounts created with `"hot": true` (a merchant, a fee account) take credits without locking their row.
  - The entry commits with its transfer, together with a `hot_pending` row.
  - A worker folds pending rows into the cached balance every 100ms (`ROLLUP_INTERVAL`).
  - Nothing is buffered in memory, so a crash loses nothing that was acknowledged.
- Reads return the effective balance: the cached balance plus pending entries, read in one statement. It equals the sum of the entries at all times, not only once the worker catches up.
- Debiting a hot account still takes its lock and rolls up its pending rows on the spot, so the overdraft check counts them.
- Test coverage:
  - The property test makes two of its accounts hot, runs the rollup worker throughout, and reconciles a fresh snapshot every 25ms during the run.
  - The focused tests hold the merchant's row lock from another transaction to show that credits don't wait for it and debits do.
- Load test at 300/s with 90% of transfers paying one merchant: merchant p99 drops from 1.67 s to 31 ms. At 1,000/s the hot test hits the same ceiling as the spread-out test. See [docs/results.md](docs/results.md).

**Test console (`web/`):** a React + TypeScript dashboard for trying the API by hand. It covers accounts, two-leg and multi-leg transfers with a balance preview, entry history with a load-more cursor, reversals, and a request log that shows each idempotency key and replay. Its idempotency lab can resend a request with the same key or fire the same request twice at once.

```sh
make web-install   # once
make web           # http://localhost:5173, with make run in another terminal
make web-test      # Vitest + Testing Library, including a double-click-posts-once test
```

## Run

```sh
make up          # Postgres on :5433, Redis on :6379
make run         # API on :8088
make test        # unit tests
make test-integration   # Testcontainers (needs Docker)
make lint
make recon       # one-off reconciliation, exits 1 on mismatches
make obs         # Prometheus :9090, Grafana :3000
make load-steady # k6 via Docker; RATE=500 DURATION=2m to override
make load-hot
```

## API

| Method | Path | Notes |
|---|---|---|
| `POST` | `/accounts` | `{name, currency, normal_balance: "debit"\|"credit", allow_negative, hot}` |
| `GET` | `/accounts`, `/accounts/{id}` | |
| `GET` | `/accounts/{id}/entries?cursor=&limit=` | newest first |
| `POST` | `/transfers` | `Idempotency-Key` header required. Body: `{entries: [{account_id, direction, amount}]}` or `{debit_account_id, credit_account_id, amount}` |
| `GET` | `/transfers/{id}` | |
| `POST` | `/transfers/{id}/reverse` | `Idempotency-Key` header required |
| `GET` | `/reconciliation` | latest reconciliation run |
| `GET` | `/metrics` | Prometheus |

Amounts are integer minor units (cents).
