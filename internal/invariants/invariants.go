// Package invariants reports, for one sandbox, the properties the ledger
// promises to keep: money is conserved, every balance matches its entries,
// and no key posts twice. The console polls it, so visitors can watch the
// numbers hold while they try to break things.
package invariants

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/michael-jw-ji/go-ledger/internal/recon"
	"github.com/michael-jw-ji/go-ledger/internal/sandbox"
)

// Trial is one currency's trial balance: debits minus credits must be zero.
type Trial struct {
	Currency   string `json:"currency"`
	Debits     int64  `json:"debits"`
	Credits    int64  `json:"credits"`
	Difference int64  `json:"difference"`
}

// Held is the effective balance of the accounts that can't go negative, per
// currency: the money the scope's customers hold. The overdraft accounts
// (funding sources) are left out.
type Held struct {
	Currency string `json:"currency"`
	Amount   int64  `json:"amount"`
}

// Report is one snapshot. Every figure comes from the same REPEATABLE READ
// transaction, so the checks agree with each other.
type Report struct {
	CheckedAt time.Time `json:"checked_at"`
	Accounts  int64     `json:"accounts"`
	Trial     []Trial   `json:"trial_balance"`
	Held      []Held    `json:"held"`
	// Recon is a scoped reconciliation. It doesn't write to
	// reconciliation_runs, so polling it leaves the global history alone.
	Recon recon.Result `json:"recon"`
	// KeysWithMultipleTransfers is the number of idempotency keys that posted
	// more than one transfer. The UNIQUE index on transfers makes this zero by
	// construction. It's reported to show the guarantee, not to measure it.
	KeysWithMultipleTransfers int64 `json:"keys_with_multiple_transfers"`
}

// Snapshot builds a report for scope.
func Snapshot(ctx context.Context, pool *pgxpool.Pool, scope recon.Scope) (Report, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Report{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only

	rep := Report{CheckedAt: time.Now().UTC(), Trial: []Trial{}, Held: []Held{}}
	args := scope.Args()

	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM accounts a
		WHERE `+scope.Filter("a.sandbox_id")+``, args...).Scan(&rep.Accounts); err != nil {
		return Report{}, err
	}

	rows, err := tx.Query(ctx, `
		SELECT a.currency,
		       COALESCE(SUM(e.amount) FILTER (WHERE e.direction = 'debit'), 0)::bigint,
		       COALESCE(SUM(e.amount) FILTER (WHERE e.direction = 'credit'), 0)::bigint
		FROM entries e JOIN accounts a ON a.id = e.account_id
		WHERE `+scope.Filter("a.sandbox_id")+`
		GROUP BY a.currency
		ORDER BY a.currency`, args...)
	if err != nil {
		return Report{}, err
	}
	for rows.Next() {
		var t Trial
		if err := rows.Scan(&t.Currency, &t.Debits, &t.Credits); err != nil {
			rows.Close()
			return Report{}, err
		}
		t.Difference = t.Debits - t.Credits
		rep.Trial = append(rep.Trial, t)
	}
	if err := rows.Err(); err != nil {
		return Report{}, err
	}

	rows, err = tx.Query(ctx, `
		SELECT a.currency, SUM(a.balance + COALESCE(p.total, 0))::bigint
		FROM accounts a
		LEFT JOIN (
		    SELECT account_id, SUM(signed_amount) AS total FROM hot_pending GROUP BY account_id
		) p ON p.account_id = a.id
		WHERE NOT a.allow_negative AND `+scope.Filter("a.sandbox_id")+`
		GROUP BY a.currency
		ORDER BY a.currency`, args...)
	if err != nil {
		return Report{}, err
	}
	for rows.Next() {
		var h Held
		if err := rows.Scan(&h.Currency, &h.Amount); err != nil {
			rows.Close()
			return Report{}, err
		}
		rep.Held = append(rep.Held, h)
	}
	if err := rows.Err(); err != nil {
		return Report{}, err
	}

	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM (
		    SELECT t.idempotency_key FROM transfers t
		    WHERE `+scope.Filter("t.sandbox_id")+`
		    GROUP BY t.idempotency_key HAVING COUNT(*) > 1
		) d`, args...).Scan(&rep.KeysWithMultipleTransfers); err != nil {
		return Report{}, err
	}

	rep.Recon, err = recon.CheckScope(ctx, tx, scope)
	if err != nil {
		return Report{}, err
	}
	return rep, nil
}

// Handler serves GET /invariants for the request's sandbox. A request without
// the sandbox header gets the unscoped data.
func Handler(pool *pgxpool.Pool, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope := recon.Scope{Sandbox: sandbox.ID(r.Context())}
		rep, err := Snapshot(r.Context(), pool, scope)
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			log.ErrorContext(r.Context(), "invariants", "err", err)
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"code":"internal","message":"internal error"}}` + "\n"))
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(rep)
	}
}
