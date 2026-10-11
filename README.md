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
