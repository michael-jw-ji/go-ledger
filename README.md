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

## Run

```sh
make up          # Postgres on :5433, Redis on :6379
make run         # API on :8088
make test        # unit tests
make test-integration   # Testcontainers (needs Docker)
make lint
```

## API

| Method | Path | Notes |
|---|---|---|
| `POST` | `/accounts` | `{name, currency, normal_balance: "debit"\|"credit", allow_negative}` |
| `GET` | `/accounts`, `/accounts/{id}` | |
| `GET` | `/accounts/{id}/entries?cursor=&limit=` | newest first |
| `POST` | `/transfers` | `Idempotency-Key` header required. Body: `{entries: [{account_id, direction, amount}]}` or `{debit_account_id, credit_account_id, amount}` |
| `GET` | `/transfers/{id}` | |
| `POST` | `/transfers/{id}/reverse` | `Idempotency-Key` header required |

Amounts are integer minor units (cents).
