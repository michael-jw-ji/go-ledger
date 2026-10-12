// Package recon checks the ledger's derived state against its source of
// truth. Entries are the truth; cached balances and lock versions are derived
// from them and could drift through a bug or a manual edit. Recon is a
// detective control: it cannot prevent drift, only make it visible quickly.
package recon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Kinds of finding.
const (
	// KindAccount: an account's cached balance or lock_version disagrees
	// with its entries.
	KindAccount = "account"
	// KindTransfer: a posted transfer's entries don't net to zero.
	KindTransfer = "transfer"
	// KindTrialBalance: total debits and credits differ within a currency.
	KindTrialBalance = "trial_balance"
)

// Kinds lists every kind, so metrics can report zeros for the clean ones.
var Kinds = []string{KindAccount, KindTransfer, KindTrialBalance}

// Mismatch is one finding. Only the fields relevant to its kind are set.
type Mismatch struct {
	Kind        string `json:"kind"`
	AccountID   int64  `json:"account_id,omitempty"`
	TransferID  string `json:"transfer_id,omitempty"`
	Currency    string `json:"currency,omitempty"`
	Cached      int64  `json:"cached"`
	Computed    int64  `json:"computed"`
	CachedCount int64  `json:"cached_count,omitempty"`
	EntryCount  int64  `json:"entry_count,omitempty"`
}

type Result struct {
	RunID           int64      `json:"run_id"`
	StartedAt       time.Time  `json:"started_at"`
	FinishedAt      time.Time  `json:"finished_at"`
	AccountsChecked int        `json:"accounts_checked"`
	Mismatches      []Mismatch `json:"mismatches"`
	// Skipped is true when another process held the recon lock.
	Skipped bool `json:"skipped,omitempty"`
}

// Count returns the number of findings of one kind.
func (r Result) Count(kind string) int {
	n := 0
	for _, m := range r.Mismatches {
		if m.Kind == kind {
			n++
		}
	}
	return n
}

// lockKey is an arbitrary constant for pg_try_advisory_xact_lock, so only
// one instance reconciles at a time.
const lockKey = 0x6c65646765720001

// Run reconciles once and records the run in reconciliation_runs.
//
// All checks read one REPEATABLE READ snapshot, so a transfer committing
// mid-run is either fully visible or not at all. Without that, the balance
// read and the entry sum could straddle a commit and report a false
// mismatch.
func Run(ctx context.Context, pool *pgxpool.Pool) (Result, error) {
	res := Result{StartedAt: time.Now().UTC(), Mismatches: []Mismatch{}}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return res, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only

	var locked bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, int64(lockKey)).Scan(&locked); err != nil {
		return res, err
	}
	if !locked {
		res.Skipped = true
		return res, nil
	}

	if err := check(ctx, tx, &res); err != nil {
		return res, err
	}
	if err := tx.Commit(ctx); err != nil {
		return res, err
	}
	res.FinishedAt = time.Now().UTC()

	details, err := json.Marshal(res.Mismatches)
	if err != nil {
		return res, err
	}
	err = pool.QueryRow(ctx, `
		INSERT INTO reconciliation_runs (started_at, finished_at, accounts_checked, mismatches, details)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id`,
		res.StartedAt, res.FinishedAt, res.AccountsChecked, len(res.Mismatches), details).Scan(&res.RunID)
	return res, err
}

// Check runs every check inside tx without recording a run. Run is the
// normal entry point; Check exists so tests can reconcile uncommitted state.
func Check(ctx context.Context, tx pgx.Tx) (Result, error) {
	res := Result{StartedAt: time.Now().UTC(), Mismatches: []Mismatch{}}
	err := check(ctx, tx, &res)
	res.FinishedAt = time.Now().UTC()
	return res, err
}

func check(ctx context.Context, tx pgx.Tx, res *Result) error {
	if err := checkAccounts(ctx, tx, res); err != nil {
		return fmt.Errorf("check accounts: %w", err)
	}
	if err := checkTransfers(ctx, tx, res); err != nil {
		return fmt.Errorf("check transfers: %w", err)
	}
	if err := checkTrialBalance(ctx, tx, res); err != nil {
		return fmt.Errorf("check trial balance: %w", err)
	}
	return nil
}

// signed is an entry's effect on its account's balance, which grows on the
// account's normal side.
const signed = `CASE WHEN e.direction = a.normal_balance THEN e.amount ELSE -e.amount END`

func checkAccounts(ctx context.Context, tx pgx.Tx, res *Result) error {
	rows, err := tx.Query(ctx, `
		SELECT a.id, a.balance, a.lock_version,
		       COALESCE(s.total, 0), COALESCE(s.n, 0)
		FROM accounts a
		LEFT JOIN (
		    SELECT e.account_id, SUM(`+signed+`) AS total, COUNT(*) AS n
		    FROM entries e JOIN accounts a ON a.id = e.account_id
		    GROUP BY e.account_id
		) s ON s.account_id = a.id
		ORDER BY a.id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var m Mismatch
		if err := rows.Scan(&m.AccountID, &m.Cached, &m.CachedCount, &m.Computed, &m.EntryCount); err != nil {
			return err
		}
		res.AccountsChecked++
		if m.Cached != m.Computed || m.CachedCount != m.EntryCount {
			m.Kind = KindAccount
			res.Mismatches = append(res.Mismatches, m)
		}
	}
	return rows.Err()
}

func checkTransfers(ctx context.Context, tx pgx.Tx, res *Result) error {
	rows, err := tx.Query(ctx, `
		SELECT e.transfer_id::text,
		       SUM(e.amount) FILTER (WHERE e.direction = 'debit'),
		       SUM(e.amount) FILTER (WHERE e.direction = 'credit')
		FROM entries e
		GROUP BY e.transfer_id
		HAVING SUM(CASE WHEN e.direction = 'debit' THEN e.amount ELSE -e.amount END) <> 0`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var debits, credits *int64
		if err := rows.Scan(&id, &debits, &credits); err != nil {
			return err
		}
		res.Mismatches = append(res.Mismatches, Mismatch{
			Kind: KindTransfer, TransferID: id, Cached: deref(debits), Computed: deref(credits),
		})
	}
	return rows.Err()
}

func checkTrialBalance(ctx context.Context, tx pgx.Tx, res *Result) error {
	rows, err := tx.Query(ctx, `
		SELECT a.currency,
		       COALESCE(SUM(e.amount) FILTER (WHERE e.direction = 'debit'), 0),
		       COALESCE(SUM(e.amount) FILTER (WHERE e.direction = 'credit'), 0)
		FROM entries e JOIN accounts a ON a.id = e.account_id
		GROUP BY a.currency
		ORDER BY a.currency`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var m Mismatch
		if err := rows.Scan(&m.Currency, &m.Cached, &m.Computed); err != nil {
			return err
		}
		if m.Cached != m.Computed {
			m.Kind = KindTrialBalance
			res.Mismatches = append(res.Mismatches, m)
		}
	}
	return rows.Err()
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// Latest returns the most recent recorded run.
func Latest(ctx context.Context, pool *pgxpool.Pool) (Result, error) {
	var res Result
	var details []byte
	err := pool.QueryRow(ctx, `
		SELECT id, started_at, finished_at, accounts_checked, details
		FROM reconciliation_runs ORDER BY started_at DESC, id DESC LIMIT 1`).
		Scan(&res.RunID, &res.StartedAt, &res.FinishedAt, &res.AccountsChecked, &details)
	if err != nil {
		return res, err
	}
	err = json.Unmarshal(details, &res.Mismatches)
	return res, err
}

// Worker runs reconciliation on an interval until its context ends.
type Worker struct {
	Pool     *pgxpool.Pool
	Interval time.Duration
	Log      *slog.Logger
	// OnResult, if set, receives every run's outcome; err is non-nil when
	// the run itself failed.
	OnResult func(res Result, took time.Duration, err error)
}

func (w *Worker) Run(ctx context.Context) {
	t := time.NewTicker(w.Interval)
	defer t.Stop()
	for {
		w.once(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (w *Worker) once(ctx context.Context) {
	start := time.Now()
	res, err := Run(ctx, w.Pool)
	took := time.Since(start)
	switch {
	case ctx.Err() != nil:
		return
	case err != nil:
		w.Log.Error("reconciliation failed", "err", err)
	case res.Skipped:
		w.Log.Info("reconciliation skipped, another instance holds the lock")
	case len(res.Mismatches) > 0:
		for _, m := range res.Mismatches {
			w.Log.Error("reconciliation mismatch", "run_id", res.RunID, "kind", m.Kind,
				"account_id", m.AccountID, "transfer_id", m.TransferID, "currency", m.Currency,
				"cached", m.Cached, "computed", m.Computed, "delta", m.Cached-m.Computed,
				"cached_count", m.CachedCount, "entry_count", m.EntryCount)
		}
	default:
		w.Log.Info("reconciliation clean", "run_id", res.RunID,
			"accounts_checked", res.AccountsChecked, "took", took)
	}
	if w.OnResult != nil {
		w.OnResult(res, took, err)
	}
}
