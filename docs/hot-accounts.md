# One merchant, 300 payments a second: fixing a hot row in a Postgres ledger

Most transfers in a payments ledger touch two unrelated accounts and never compete. Then a single merchant starts receiving most of the payments, and every one of them needs that merchant's row lock. This is how go-ledger hit that wall, and the fix that removed it without giving up any of the ledger's guarantees. Every number below was measured. The setup and raw results are in [results.md](results.md).

## The baseline

Every transfer runs in one Postgres transaction. It locks the accounts involved (`SELECT ... FOR UPDATE`, in ascending id order so two transfers can never deadlock), checks the overdraft rules, appends the entries, and updates each account's cached balance.

With transfers spread across 1,000 accounts, that's cheap. At 300 transfers/s, p99 latency was **32 ms**, and the system saturated at about 900/s, limited by its 16 database connections.

Then I sent 90% of transfers to one merchant account. At 200/s it held up; at 300/s, the same rate the spread-out workload handled easily, it fell over:

| 90% of transfers to one merchant | 200/s | 300/s |
|---|---|---|
| Merchant p99 | 14 ms | **882 ms** |
| Unrelated user→user p99 | 11 ms | **443 ms** |
| Row-lock wait p99 | 4.4 ms | **719 ms** |
| Pool acquires that had to wait | 0 | 48% |

The first row is expected. Every payment to the merchant takes the same row lock, so those transactions run one at a time, and throughput to that account is capped at roughly one transfer per lock hold time.

The second row is the interesting one. **Transfers between two ordinary users, which never touch the merchant, got 40 times slower.** Transactions waiting on the merchant's lock are still holding a database connection while they wait. Once enough of them pile up, the pool is empty, and every other request queues for a connection. One hot row degraded the whole service.

## What not to do

The tempting fix is to buffer: keep merchant credits in memory and flush a sum every 100 ms. It's fast, and it's wrong for money. Between the response and the flush, a transfer the client was told succeeded exists only in a process that can crash. Any fix had to keep the rule that **everything acknowledged is committed**.

## The fix: commit the entry, defer the balance

What makes the row hot isn't the entry; entries are append-only inserts and don't conflict. It's the read-modify-write of the merchant's **cached balance**. So the fix is to commit everything except that:

1. An account can be created with `hot: true`.
2. When a transfer *increases* a hot account, the transaction skips that account's row lock and balance update. It inserts the entry as usual, plus a row in a `hot_pending` table holding the entry's signed amount. Both commit with the transfer.
3. A rollup worker runs every 100 ms. For each hot account with pending rows, it takes the row lock **once**, deletes the pending rows with `DELETE ... RETURNING`, adds their sum to the balance, and commits. That's one lock acquisition per tick instead of one per payment.

Reads stay exact. The balance the API returns is `balance + SUM(hot_pending)`, computed in one statement, so it always equals the sum of the account's entries, before or after the worker runs. If the worker dies, nothing is lost: pending rows wait, reads are still right, and the next tick on any instance catches up.

**Spending** from a hot account (a refund, a payout) still takes the row lock. While holding it, the transaction folds the account's pending rows into the balance itself, so pending credits count toward the overdraft check. This is safe even though credits keep arriving without the lock. For an account that can't go negative, unlocked writers can only *add* money, so a balance check made under the lock can only become more true while they land.

## Two traps on the way

**A watermark looks simpler than a pending table, and it's broken.** The obvious design records a high-water mark per account, "rolled up through entry 1,000", and sums newer entries on read. But `bigserial` ids are handed out when a row is inserted, not when it commits. Entry 1,001 can commit before entry 1,000. A rollup that sees 1,001 moves the mark past 1,000, and when 1,000 commits a moment later, it's below the mark and never counted. Explicit pending rows assume nothing about commit order.

**`FOR UPDATE` would have reintroduced the bottleneck.** Inserting an entry checks its foreign key to `accounts`, and Postgres implements that check by taking a `FOR KEY SHARE` lock on the account row. `FOR UPDATE` conflicts with `FOR KEY SHARE`. A rollup or debit that took `FOR UPDATE` on the merchant would wait for every in-flight credit to commit, and every new credit would queue behind it: a brief stall every 100 ms. `FOR NO KEY UPDATE` still serializes everyone who writes the balance, but doesn't conflict with foreign-key checks, so the ledger now uses it everywhere.

## Results

Same build, same machine, back to back, with only the merchant's `hot` flag changed:

| 300 transfers/s, 90% to one merchant | Ordinary merchant | Hot merchant |
|---|---|---|
| Merchant p50 / p99 | 285 ms / 1.67 s | 6.4 ms / **31 ms** |
| User→user p99 | 1.39 s | **27 ms** |
| Row-lock wait p99 | 846 ms | **1.8 ms** |
| Pool acquires that had to wait | 67% | 0.3% |
| Transfers dropped by the load generator | 383 | 0 |

(The ordinary-merchant run here was slower than the earlier baseline at the same rate. Past saturation, small differences in lock hold time swing the queue a lot, which is why the comparison is back to back.)

At 1,000/s, the hot-merchant test saturates at **879/s**, essentially the 896/s ceiling of the spread-out workload on the same build. The merchant is no longer the bottleneck; the connection pool is, exactly as when nobody is hot.

## How I know it's still correct

Speed was the easy part to measure. The checks that matter are the ones that would catch lost or double-counted money:

- **A property test** runs 64 concurrent workers over 10 accounts, two of them hot. It retries 20% of requests with the same idempotency key, and runs the rollup worker the whole time.
  - **During the run**, a sampler reconciles a fresh snapshot every 25 ms. Effective balances must match the entries at every instant, not just at the end.
  - **Afterwards**, it checks conservation of money and balances against entries, before and after a final rollup.
- **Lock-behavior tests** hold the merchant's row lock from another transaction. A credit must still complete within the timeout. A debit must block, and once released, must count pending credits as spendable.
- **A mutation check** made the service silently skip writing `hot_pending` rows. Four tests failed, as they should.
- **Reconciliation** ran every 15 seconds through every load test above and found 0 mismatches.

## What it costs

- **One extra round trip per transfer.** The transaction reads account flags before deciding what to lock. The cost was within run-to-run noise in these tests.
- **Debits of a hot account still serialize.** That suits merchants and fee accounts, where money mostly flows in.
- **`hot` is fixed at account creation.** Turning it off safely would need a rollup under the row lock first.

The general lesson: when one row is hot, find the specific piece of state that forces serialization, which here was a cached sum, not the record of what happened. Defer only that piece, and keep everything you acknowledge durable.
