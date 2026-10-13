// Package rollup folds hot accounts' pending entries into their cached
// balances.
//
// Credits to a hot account commit with their transfer (entry plus a
// hot_pending row) but skip the account row. This worker takes the row lock
// once per tick per account instead of once per transfer. Nothing is held in
// memory: if the worker stops, pending rows simply wait, reads still see the
// right effective balance, and the next tick picks them up.
package rollup

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Account applies every committed pending entry of one account and returns
// how many it applied.
func Account(ctx context.Context, pool *pgxpool.Pool, accountID int64) (int64, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// Lock first, then read the pending rows in a later statement, so the
	// DELETE sees everything committed up to the moment we got the lock.
	// A debit of the account folds pending rows under the same lock, so the
	// two never apply a row twice.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM accounts WHERE id = $1 FOR NO KEY UPDATE`, accountID); err != nil {
		return 0, err
	}
	var n int64
	err = tx.QueryRow(ctx, `
		WITH d AS (
		    DELETE FROM hot_pending WHERE account_id = $1
		    RETURNING signed_amount
		), s AS (
		    SELECT COALESCE(SUM(signed_amount), 0)::bigint AS total, COUNT(*) AS n FROM d
		), u AS (
		    UPDATE accounts a
		    SET balance = a.balance + s.total, lock_version = a.lock_version + s.n
		    FROM s
		    WHERE a.id = $1 AND s.n > 0
		)
		SELECT n FROM s`, accountID).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, tx.Commit(ctx)
}

// Once rolls up every account with pending entries and returns the number
// of entries applied.
func Once(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	rows, err := pool.Query(ctx, `SELECT DISTINCT account_id FROM hot_pending`)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var total int64
	for _, id := range ids {
		n, err := Account(ctx, pool, id)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// Worker runs Once on an interval until its context ends.
type Worker struct {
	Pool     *pgxpool.Pool
	Interval time.Duration
	Log      *slog.Logger
	// OnTick, if set, receives each tick's applied count and duration.
	OnTick func(applied int64, took time.Duration, err error)
}

func (w *Worker) Run(ctx context.Context) {
	t := time.NewTicker(w.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		start := time.Now()
		n, err := Once(ctx, w.Pool)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			w.Log.Error("hot account rollup failed", "err", err)
		}
		if w.OnTick != nil {
			w.OnTick(n, time.Since(start), err)
		}
	}
}
