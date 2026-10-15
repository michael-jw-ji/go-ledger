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
	// KindPending: a hot_pending row disagrees with the entry it stands for.
	KindPending = "pending"
)

// Kinds lists every kind, so metrics can report zeros for the clean ones.
var Kinds = []string{KindAccount, KindTransfer, KindTrialBalance, KindPending}

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

// Scope selects which accounts a check covers. All covers the whole ledger.
// Otherwise the check covers one sandbox, where an empty Sandbox means the
// unscoped rows (NULL sandbox_id).
type Scope struct {
	All     bool
	Sandbox string
	// ExcludeChaos skips sandboxes a chaos endpoint has deliberately
	// corrupted, so a visitor's experiment can't fire the real alert.
	ExcludeChaos bool
}

var (
	// AllScope checks the whole ledger, chaos sandboxes included.
	AllScope = Scope{All: true}
	// GlobalScope is what the reconciliation worker checks.
	GlobalScope = Scope{All: true, ExcludeChaos: true}
)

// Args are the parameters of every query that uses Filter.
func (s Scope) Args() []any {
	var sb any
	if s.Sandbox != "" {
		sb = s.Sandbox
	}
	return []any{s.All, sb, s.ExcludeChaos}
}

// Filter is the SQL condition keeping rows whose sandbox column col is in
// scope. EXISTS, not IN, so unscoped rows (NULL col) are never dropped by
// the chaos exclusion.
func (s Scope) Filter(col string) string {
	return "($1 OR " + col + " IS NOT DISTINCT FROM $2::text)" +
		" AND NOT ($3 AND EXISTS (SELECT 1 FROM sandboxes sb WHERE sb.id = " + col + " AND sb.chaos_active))"
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

	if err := check(ctx, tx, GlobalScope, &res); err != nil {
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
	return CheckScope(ctx, tx, AllScope)
}

// CheckScope is Check limited to one scope. The sandbox console uses it for
// live invariants: it runs in the caller's snapshot and records nothing, so
// polling it doesn't fill reconciliation_runs.
func CheckScope(ctx context.Context, tx pgx.Tx, scope Scope) (Result, error) {
	res := Result{StartedAt: time.Now().UTC(), Mismatches: []Mismatch{}}
	err := check(ctx, tx, scope, &res)
	res.FinishedAt = time.Now().UTC()
	return res, err
}

func check(ctx context.Context, tx pgx.Tx, scope Scope, res *Result) error {
	if err := checkAccounts(ctx, tx, scope, res); err != nil {
		return fmt.Errorf("check accounts: %w", err)
	}
	if err := checkTransfers(ctx, tx, scope, res); err != nil {
		return fmt.Errorf("check transfers: %w", err)
	}
	if err := checkTrialBalance(ctx, tx, scope, res); err != nil {
		return fmt.Errorf("check trial balance: %w", err)
	}
	if err := checkPending(ctx, tx, scope, res); err != nil {
		return fmt.Errorf("check pending: %w", err)
	}
	return nil
}

// signed is an entry's effect on its account's balance, which grows on the
// account's normal side. Cached balances are compared as effective balances:
// accounts.balance plus the account's hot_pending rows.
const signed = `CASE WHEN e.direction = a.normal_balance THEN e.amount ELSE -e.amount END`

func checkAccounts(ctx context.Context, tx pgx.Tx, scope Scope, res *Result) error {
	rows, err := tx.Query(ctx, `
		SELECT a.id,
		       a.balance + COALESCE(p.total, 0), a.lock_version + COALESCE(p.n, 0),
		       COALESCE(s.total, 0), COALESCE(s.n, 0)
		FROM accounts a
		LEFT JOIN (
		    SELECT e.account_id, SUM(`+signed+`) AS total, COUNT(*) AS n
		    FROM entries e JOIN accounts a ON a.id = e.account_id
		    WHERE `+scope.Filter("a.sandbox_id")+`
		    GROUP BY e.account_id
		) s ON s.account_id = a.id
		LEFT JOIN (
		    SELECT p.account_id, SUM(p.signed_amount) AS total, COUNT(*) AS n
		    FROM hot_pending p JOIN accounts a ON a.id = p.account_id
		    WHERE `+scope.Filter("a.sandbox_id")+`
		    GROUP BY p.account_id
		) p ON p.account_id = a.id
		WHERE `+scope.Filter("a.sandbox_id")+`
		ORDER BY a.id`, scope.Args()...)
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

func checkTransfers(ctx context.Context, tx pgx.Tx, scope Scope, res *Result) error {
	rows, err := tx.Query(ctx, `
		SELECT e.transfer_id::text,
		       SUM(e.amount) FILTER (WHERE e.direction = 'debit'),
		       SUM(e.amount) FILTER (WHERE e.direction = 'credit')
		FROM entries e JOIN transfers t ON t.id = e.transfer_id
		WHERE `+scope.Filter("t.sandbox_id")+`
		GROUP BY e.transfer_id
		HAVING SUM(CASE WHEN e.direction = 'debit' THEN e.amount ELSE -e.amount END) <> 0`, scope.Args()...)
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

func checkTrialBalance(ctx context.Context, tx pgx.Tx, scope Scope, res *Result) error {
	rows, err := tx.Query(ctx, `
		SELECT a.currency,
		       COALESCE(SUM(e.amount) FILTER (WHERE e.direction = 'debit'), 0),
		       COALESCE(SUM(e.amount) FILTER (WHERE e.direction = 'credit'), 0)
		FROM entries e JOIN accounts a ON a.id = e.account_id
		WHERE `+scope.Filter("a.sandbox_id")+`
		GROUP BY a.currency
		ORDER BY a.currency`, scope.Args()...)
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

// checkPending catches a pending row filed under the wrong account or with
// the wrong sign or amount: the account check alone would see such a row as
// two accounts drifting in opposite directions.
func checkPending(ctx context.Context, tx pgx.Tx, scope Scope, res *Result) error {
	rows, err := tx.Query(ctx, `
		SELECT p.account_id, p.signed_amount, `+signed+`
		FROM hot_pending p
		JOIN entries e ON e.id = p.entry_id
		JOIN accounts a ON a.id = e.account_id
		WHERE `+scope.Filter("a.sandbox_id")+`
		  AND (p.account_id <> e.account_id OR p.signed_amount <> `+signed+`)`, scope.Args()...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		m := Mismatch{Kind: KindPending}
		if err := rows.Scan(&m.AccountID, &m.Cached, &m.Computed); err != nil {
			return err
		}
		res.Mismatches = append(res.Mismatches, m)
	}
	return rows.Err()
}

// Repair rebuilds the cached balance and lock_version of every account in
// scope from its entries, which are the source of truth, and returns the ids
// it changed. It is the fix for an account finding.
//
// It locks the accounts first, in id order like a posting, so no locked-path
// posting or rollup can change them mid-repair. Hot credits skip that lock,
// but each adds an entry and a hot_pending row in one transaction, and the
// single UPDATE below sees both or neither.
func Repair(ctx context.Context, pool *pgxpool.Pool, scope Scope) ([]int64, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if _, err := tx.Exec(ctx, `
		SELECT a.id FROM accounts a WHERE `+scope.Filter("a.sandbox_id")+`
		ORDER BY a.id FOR NO KEY UPDATE`, scope.Args()...); err != nil {
		return nil, fmt.Errorf("lock accounts: %w", err)
	}
	rows, err := tx.Query(ctx, `
		UPDATE accounts a
		SET balance = s.total - COALESCE(p.total, 0), lock_version = s.n - COALESCE(p.n, 0)
		FROM (
		    SELECT a.id, COALESCE(SUM(`+signed+`), 0) AS total, COUNT(e.id) AS n
		    FROM accounts a LEFT JOIN entries e ON e.account_id = a.id
		    WHERE `+scope.Filter("a.sandbox_id")+`
		    GROUP BY a.id
		) s
		LEFT JOIN (
		    SELECT account_id, SUM(signed_amount) AS total, COUNT(*) AS n
		    FROM hot_pending GROUP BY account_id
		) p ON p.account_id = s.id
		WHERE a.id = s.id
		  AND (a.balance, a.lock_version) IS DISTINCT FROM (s.total - COALESCE(p.total, 0), s.n - COALESCE(p.n, 0))
		RETURNING a.id`, scope.Args()...)
	if err != nil {
		return nil, fmt.Errorf("repair: %w", err)
	}
	fixed, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, err
	}
	return fixed, tx.Commit(ctx)
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
