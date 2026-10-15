# Design

This document covers how go-ledger keeps money correct. It explains the guarantees, the mechanisms behind each one, and what happens when parts fail. The README covers running it, and [docs/results.md](docs/results.md) has the load-test numbers.

## Guarantees

1. **Every posted transfer balances.** Its debits equal its credits, within one currency.
2. **A request posts at most once per idempotency key.** This holds even when Redis is down, has been flushed, or the client retries concurrently. Every response for a key describes the same outcome.
3. **No account without an overdraft allowance goes negative.**
4. **History is immutable.** Entries are append-only, and mistakes are corrected with a reversal transfer.
5. **Derived state matches the source of truth.** An account's effective balance equals the signed sum of its entries, and its `lock_version` equals its entry count. This holds at every instant, and a reconciliation job checks it continuously.

## Data model

| Table | Role |
|---|---|
| `accounts` | Name, currency, `normal_balance` (the side that increases it), `allow_negative`, `hot`, and the **cached** `balance` and `lock_version`. |
| `transfers` | One row per idempotency key: `request_hash`, `status` (`pending` only inside its own transaction, then `posted` or `rejected`), `error_code`, `reverses_transfer_id`. |
| `entries` | The source of truth. Each row is one leg: `(transfer_id, account_id, direction, amount > 0)`. A trigger rejects `UPDATE` and `DELETE`. |
| `hot_pending` | Entries of hot accounts not yet folded into the cached balance (see [Hot accounts](#hot-accounts)). |
| `reconciliation_runs` | Audit trail of every reconciliation run and its findings. |
| `outbox` | Events for posted transfers, written in the same transaction and published to a Redis stream (see [Events](#events-transactional-outbox)). |

Money is an `int64` count of minor units. Floats appear nowhere: the API, the database and the web console's parser all use integers. Validation caps legs at 64 and each amount at 10^15, so no sum can overflow.

Balances are signed relative to `normal_balance`. A credit-normal wallet grows when credited, and a debit-normal cash account grows when debited.

## Posting a transfer

`ledger.Service.post` runs the whole transfer in one Postgres transaction:

```
BEGIN
  INSERT INTO transfers (... status 'pending') ON CONFLICT (idempotency_key) DO NOTHING RETURNING ...
    no row returned  → the key already exists: ROLLBACK and replay the stored outcome
  SAVEPOINT
    read account attributes (currency, normal side, flags)
    SELECT ... FROM accounts WHERE id = ANY(locked ids) ORDER BY id FOR NO KEY UPDATE
    check currency, check overdraft against the post-transfer balance
    INSERT entries; INSERT hot_pending rows; UPDATE cached balances
  on a business-rule failure: ROLLBACK TO SAVEPOINT, mark the transfer rejected
  otherwise: RELEASE SAVEPOINT, mark it posted
COMMIT
```

**Deadlock freedom.** Every transaction locks its accounts in ascending id order, so no two transfers can wait on each other in a cycle.

**`FOR NO KEY UPDATE`, not `FOR UPDATE`.** Inserting an entry checks its foreign key to `accounts`, and that check takes `FOR KEY SHARE` on the account row. `FOR UPDATE` conflicts with `FOR KEY SHARE`; `FOR NO KEY UPDATE` does not. The weaker lock still serializes every writer of a balance. With the stronger one, a lock on a hot account would wait for every in-flight credit to it, and new credits would queue behind that lock.

**Defense in depth.** A `CHECK (allow_negative OR balance >= 0)` on `accounts` catches any overdraft bug the service-level check misses, and `entries.amount > 0` is also a constraint.

**Reversals** post the mirror image of a posted transfer. Their key is derived as `reverse:{transfer id}:{client key}`, and their fingerprint is that of the (normally empty) request body. A partial unique index on `reverses_transfer_id` (ignoring rejected attempts) allows at most one successful reversal per transfer. A reversal that failed for insufficient funds can be retried. Reversing a reversal is refused.

## Idempotency

Clients send an `Idempotency-Key` header on every `POST /transfers` and `POST /transfers/{id}/reverse`. Two layers enforce it. Postgres is the source of truth. Redis is a cache in front of it that answers replays and in-flight duplicates quickly.

Two rules keep the layers from ever disagreeing:

1. **One fingerprint.** `idempotency.Fingerprint` hashes the request body after re-encoding the JSON (object keys sorted, whitespace dropped, numbers as written). The middleware compares it in Redis, and the handler passes the same value to the ledger, which stores it as `transfers.request_hash`. Formatting differences don't count as a different request in either layer. A different value, or the same legs in a different shape (the two-leg shorthand vs explicit `entries`, or reordered legs), counts as different in both. A retry must resend the request it sent.
2. **Redis caches only what Postgres recorded.** The handler marks a response as recorded when the ledger returned a transfer: posted, rejected or replayed. Anything else releases the Redis claim instead of storing it: a 5xx, or a request rejected before it reached the ledger, such as malformed JSON or unbalanced legs. Stripe follows the same rule: requests that fail validation never started executing, so they don't use up the key.

Together these mean a retry gets the same answer whether Redis still remembers the key, has forgotten it, or is down. `TestReformattedRetryGetsSameAnswerFromBothLayers`, `TestRejectedBeforeLedgerDoesNotBurnKey` and `TestReversalFingerprintMatchesAcrossLayers` check each case against both layers by deleting the Redis key between attempts. Mutation checks confirmed the tests fail if either rule is broken.

### Postgres backstop

`transfers.idempotency_key` is `UNIQUE`. The insert of the pending transfer row uses `ON CONFLICT DO NOTHING`.

- **Sequential retry.** The key already exists, no row comes back, and the service returns the stored transfer with `Idempotent-Replayed: true`.
- **Concurrent duplicate.** The second insert blocks on the unique index until the first transaction commits, then returns no row and replays the result. Two posts for one key are impossible, whatever the timing.
- **Different request, same key.** The stored `request_hash` (the fingerprint) differs, so the request gets 422 `idempotency_key_reused`.

**Rejections are recorded, not rolled back.** The obvious design rolls back the whole transaction on insufficient funds. That leaves no trace of the key, so a retry with the same key after funds arrive would post. The client would see a 422 and later a 201 for one key. Instead, business-rule failures (insufficient funds, currency mismatch, unknown account) roll back only to the savepoint taken after the transfer row was inserted. The transfer is then committed as `rejected` with its `error_code` and no entries.

The key is held by the unique index the whole time, so a retry can't slip in between the rejection and the commit. Retries replay the same 422 indefinitely, with or without Redis. `TestRejectedKeyReplaysAfterFundsArrive` covers this.

Validation failures (malformed JSON, unbalanced legs, bad amounts) are rejected before the transaction starts and are not recorded, in either layer. Retrying the same bad body fails the same way. Correcting it and retrying under the same key posts normally.

### Redis layer

`internal/idempotency` wraps the two POST routes:

| Redis state for the key | Response |
|---|---|
| absent | `SET NX` an in-progress claim (30s TTL), run the handler, store the response for 24h if Postgres recorded it, otherwise release the claim |
| done, same fingerprint | replay the stored status and body, `Idempotent-Replayed: true` |
| in progress | 409 `request_in_progress` with `Retry-After: 1` |
| either state, different fingerprint | 422 `idempotency_key_reused` |

Some details:
- **Endpoint scoping.** Keys are scoped by method and path (`idem:POST /transfers:{key}`), so one client key can be reused across different endpoints.
- **Business rejections are stored** (a 422 for insufficient funds is a recorded outcome). Validation failures and 5xx responses release the claim, so the client can retry.
- **Claims are token-checked.** Each claim carries a random token. Lua scripts make both `Release` and `Complete` act only on the request's own claim, so a slow request whose claim expired can't delete or overwrite a newer request's claim. `Complete` still writes to a key that has expired with nobody holding it, because the outcome it stores is already in Postgres.
- **Disconnects.** The store calls use `context.WithoutCancel`, so the result is still recorded when the client disconnects mid-request. The transfer may have committed.

### Failure cases

**The process crashes after the Postgres commit, before the Redis write.** The key stays `in_progress` in Redis until its 30-second TTL expires, and clients get 409 until then. The first retry after expiry claims the key, reaches Postgres, finds the committed transfer and replays it. The client sees a delay but never a double post. The 30-second TTL is the trade-off: a shorter one recovers faster but risks expiring under a slow-but-alive request.

**A request outlives its 30-second claim.** A second request can then claim the key and run. Postgres serializes the two on the unique index: one posts, and the other waits and replays. Both clients get the same transfer. The second request's claim is the one in Redis, so the slow request's token-checked `Complete` leaves it alone, and the second request stores the result. Both responses describe the same Postgres outcome either way.

**Redis is down.** Lookups fail open: the handler runs directly, `idempotency_lookups_total{result="redis_error"}` counts it, and Postgres alone enforces idempotency. Latency rises, because duplicates now reach the database, but correctness is unchanged. `TestRedisDownFallsBackToPostgres` covers this.

**Redis is flushed mid-traffic.** The property test runs a variant that calls `FLUSHALL` every 100ms while 64 workers post and retry. It asserts the same invariants as the normal run: every key maps to one transfer, every response for a key agrees (including rejections), and money is conserved.

As a mutation check, the Postgres key was briefly made unique per attempt so the backstop couldn't match, and the flush variant failed as it should.

**The same key is reused with a different body.** Both layers compare the same fingerprint, so both return 422 `idempotency_key_reused`. That holds within Redis's 24-hour window, after it, and with Redis down.

## Hot accounts

**The problem.** A merchant account that receives most transfers serializes them on its row lock. In the milestone 3 baseline, at 300 transfers/s with 90% paying one merchant, merchant p99 reached 882 ms. Unrelated transfers suffered too, because transactions queued on the lock held pool connections. The full measurements are in [docs/results.md](docs/results.md).

**The design.** An account created with `hot: true` takes the fast path for any transfer that *increases* it (or any transfer at all, if it may go negative):

- No row lock and no balance update for that account.
- Each of its entries gets a `hot_pending (entry_id, account_id, signed_amount)` row, written **in the same transaction** as the entry.
- The rollup worker (`internal/rollup`, every 100ms) locks each account with pending rows once. It deletes them with `RETURNING`, adds their sum to `balance` and their count to `lock_version`, and commits. That is one lock acquisition per tick instead of one per transfer.

**Reads stay exact.** The effective balance is `balance + SUM(hot_pending.signed_amount)` and the effective `lock_version` is `lock_version + COUNT(*)`, both read in one statement. Because pending rows commit atomically with their entries, the effective balance equals the sum of the entries at every instant, not just after the worker drains. `GET /accounts`, reconciliation and the property test all use effective values.

**Spending from a hot account** (a refund, a payout) takes the locked path. Under the row lock, the transaction folds the account's pending rows into the balance itself, which is the lock the worker would take anyway. Pending credits therefore count toward the overdraft check, and the cached balance never dips below zero. This is safe against concurrent fast-path writers because they can only *add* to a no-overdraft hot account: a balance check made under the lock can only become more true as their entries land.

**Why no in-memory buffer.** Batching credits in the API process and writing them later would make throughput even higher. It would also mean acknowledging transfers that exist only in memory, so a crash would lose money that was reported as moved. Here every acknowledged entry is committed before the response.

**Why a pending table, not a watermark.** The obvious alternative is a per-account "rolled up through entry id N" watermark, which avoids the extra table. But `bigserial` ids are assigned at insert and commit in any order. Entry 1001 can commit before entry 1000, a rollup to 1001 would advance the watermark, and entry 1000 would be skipped forever. Explicit pending rows have no ordering assumption.

**If the rollup worker stops.** Nothing is lost and reads stay correct. Pending rows accumulate, and the next tick, on any instance, folds them. Workers on several instances are safe together, because the row lock serializes them. The `LedgerRollupBacklog` alert fires if more than 10,000 entries wait for 2 minutes.

**Costs and limits.**
- **Extra round trip.** Every transfer now reads account attributes before choosing which rows to lock. That's one extra round trip, at a cost within run-to-run noise in the load tests.
- **Debits still serialize.** Debits of a hot account still take its lock. That suits merchants and fee accounts, where payouts are rare next to incoming payments.
- **`hot` is fixed at creation.** Reads only add pending rows for accounts flagged hot. Making the flag mutable would need a rollup under the row lock before clearing it.

**The result.** At 300 transfers/s with 90% paying one merchant, merchant p99 fell from 1.67 s to 31 ms, measured back to back on the same build. The hot test now saturates at the same rate as the spread-out workload; the connection pool, not the account row, is the limit.

## Events: transactional outbox

Other systems need to hear when money moves. Examples: a notification service, a search index, a data warehouse. The obvious approach publishes to a queue after the transaction commits. If the process dies between the commit and the publish, the money moved and nobody heard about it. Publishing before the commit has the opposite failure: an event goes out for a transfer that then rolls back.

**The design.** A posted transfer writes its event, `transfer.posted` or `transfer.reversed` with the transfer as JSON, into an `outbox` table in the same statement that marks the transfer posted. The event exists if and only if the money moved. Rejections and replays write nothing.

A worker (`internal/outbox`, every 200 ms, `OUTBOX_INTERVAL`) publishes the backlog in one transaction:
1. It claims up to 100 unpublished rows with `FOR UPDATE SKIP LOCKED`.
2. It appends each to the Redis stream `ledger.events` with `XADD`.
3. It marks the rows published and commits.

**At-least-once delivery.** If the process dies after `XADD` and before the commit, the rows are still unpublished, and the next run sends them again. Events are never lost, but a crash can duplicate a batch. Each stream message carries `event_id` (the outbox row id), so consumers dedupe on it. Exactly-once delivery to an external system isn't achievable without the consumer's cooperation. Being explicit about at-least-once is the honest version.

**Several publishers** can run at once (one per API instance). `SKIP LOCKED` hands each a disjoint batch, so without a crash nothing is sent twice.

**Ordering.** Each batch is published in id order, but there is no global order guarantee. Ids are assigned at insert and commit in any order, and two publishers work on different batches concurrently. Events for one transfer never conflict (each transfer has one event), so consumers that need an order can use `created_at` or the transfer's entry ids.

**If Redis is down,** publishing fails, events wait in Postgres, and the worker retries every tick. `outbox_lag_seconds` (the age of the oldest unpublished event) shows how stale consumers are, and `LedgerOutboxLag` alerts after 2 minutes over 60 seconds. The stream is capped at about 1,000,000 messages, so a consumer that far behind must resync from the database.

**Tests.**
- **Atomicity:** exactly one event per posted transfer or reversal, none for a rejection or a replay.
- **Crash between publishing and marking:** the batch is delivered twice, and deduping by `event_id` yields each event exactly once.
- **Redis down:** events wait and publish once Redis returns.
- **Concurrent publishers:** four publishers draining together send 200 events exactly once.
- **Property test:** under concurrent retries and Redis flushes, every transfer a client saw posted has exactly one event.

**Not built:** pruning published rows (a periodic `DELETE ... WHERE published_at < now() - interval '7 days'` would do), and consumer groups on the stream.

## Reconciliation

Reconciliation is a **detective** control: it can't prevent drift, but it makes drift visible within a minute. `internal/recon` runs every 60 seconds (`RECON_INTERVAL`), on demand with `make recon` (exit code 1 on findings), and at the end of the property test.

All checks read one `REPEATABLE READ` snapshot. Otherwise the balance read and the entry sum could straddle a commit and report a mismatch that doesn't exist. The checks:

| Kind | Finding |
|---|---|
| `account` | Effective balance or `lock_version` differs from the account's entries. |
| `transfer` | A transfer's entries don't net to zero. |
| `trial_balance` | Total debits differ from total credits within a currency. |
| `pending` | A `hot_pending` row disagrees with the entry it stands for. |

Each run is recorded in `reconciliation_runs` with its findings as JSON. Clean runs are recorded too, so "no mismatches" is evidence rather than silence. The gauge `ledger_reconciliation_mismatches{kind}` (target 0) drives an alert, and `ledger_reconciliation_last_success_timestamp` drives a second alert if recon stops running. A transaction-scoped advisory lock keeps concurrent instances from running it twice.

`TestReconDetectsCorruptedBalance` edits one cached balance with raw SQL, bypassing the service, and requires exactly one finding naming that account and the size of the drift. `TestReconDetectsUnbalancedTransfer` inserts a one-legged transfer inside a transaction that is never committed, so the append-only table stays clean. It requires one finding of each relevant kind.

**What it does not catch.** Recon proves the books are internally consistent. It can't tell whether they're *right*. Some examples:
- A transfer posted to the wrong account, balanced and recorded consistently.
- A payment sent twice under two different idempotency keys.
- An amount entered wrong.

Those need external reconciliation against a bank or processor statement, which is out of scope here.

**Cost.** Each run aggregates every entry, which is O(entries). That's fine at this scale. A large ledger would checkpoint per-account totals at a known entry boundary and reconcile only entries since then.

## Sandboxes and the live console

The public demo has no login, so every visitor gets a sandbox: a random UUID the console sends in `X-Sandbox-Id`. Accounts and transfers carry a `sandbox_id`, and requests see only their own sandbox. Rows with no sandbox are the unscoped data that curl, the tests and local development have always used, so requests without the header behave as before.

- **Isolation.** A transfer that names an account from another sandbox is rejected as `account_not_found`, and recorded like any other rejection. Reads of another sandbox's account or transfer return 404.
- **Idempotency keys.** The Postgres key becomes `sb:{sandbox}:{key}` and the Redis key `idem:{sandbox}:{METHOD path}:{key}`. Two visitors can use the same key without colliding.
- **Scoped checks.** `GET /invariants` runs the reconciliation checks for one sandbox, in the same REPEATABLE READ snapshot as the trial balance and the held totals. It records nothing in `reconciliation_runs`, so the console can poll it every second without filling the history. A corrupted balance in one sandbox shows up there and nowhere else, and the global worker still checks everything.
- **Keys with multiple transfers** is reported as zero because the `transfers` table has a unique key. It shows the guarantee; it doesn't measure it.

The invariants panel polls that endpoint. Every break-it scenario creates its own accounts, so it can't disturb another visitor's numbers.

**Cost.** Each poll runs the scoped checks, which scan the sandbox's entries. That is cheap for one visitor. It is the reason the demo will need caps on sandbox size before it's opened to more traffic.

## Observability

`GET /metrics` exposes:
- `http_request_duration_seconds{route,method,code}`. `route` is the mux pattern (`/accounts/{id}`), never the raw path, so label cardinality is bounded.
- `ledger_transfers_total{status}` and `ledger_tx_duration_seconds{status}`, where status is `posted`, `rejected`, `replayed`, `conflict` or `error`.
- `ledger_lock_wait_seconds`: time spent acquiring row locks.
- `idempotency_lookups_total{result}`.
- The reconciliation and rollup metrics.
- `pgxpool_*` connection pool statistics.

The ledger package depends on no metrics library. It exposes plain hook functions, and `internal/metrics` adapts them.

`make obs` starts Prometheus (with the alert rules in `deploy/prometheus/alerts.yml`) and Grafana with a provisioned dashboard. The dashboard has rows for correctness, throughput, latency, idempotency and Postgres, and the hot-account rollup.

## Testing

| Layer | What it shows |
|---|---|
| Unit (`internal/ledger`) | Validation: balance, limits, leg shapes. |
| Integration (Testcontainers: real Postgres 16 and Redis 7) | Overdraft, reversal, rejection replay, concurrent duplicate keys, append-only enforcement, pagination, the Redis layer's states and failure modes, metrics wiring, recon detection, and the hot-account lock behavior (holding the merchant's row lock from another transaction to prove credits don't wait for it and debits do). |
| Property test | 64 workers send random 2-to-4-leg transfers across 10 accounts (two of them hot, half debit-normal). 20% are retried with the same key, half of those concurrently. The rollup worker runs throughout, and a sampler reconciles a fresh snapshot every 25ms. Afterwards the test checks that every key has one transfer, every response for a key agrees, money is conserved, every balance and `lock_version` matches the entries before and after a final rollup, and recon is clean. A second variant runs `FLUSHALL` on Redis every 100ms. |
| Web (Vitest + Testing Library) | A double click posts once, retries reuse the key, a 409 is retried automatically with the same key, and errors render. |
| CI | gofmt, golangci-lint, unit tests and integration tests with `-race`, plus the web typecheck, tests and build. |

Mutation checks were used to confirm the tests can fail. Disabling the Postgres backstop failed the flush variant. Skipping `hot_pending` inserts failed four hot-account tests. Caching responses Postgres never recorded failed two fingerprint tests, and going back to a legs-only hash in Postgres failed the reformatted-retry test.
