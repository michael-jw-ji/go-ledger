//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/michael-jw-ji/go-ledger/internal/recon"
)

func assertReconClean(t *testing.T) {
	t.Helper()
	res, err := recon.Run(context.Background(), pool)
	if err != nil {
		t.Fatalf("recon: %v", err)
	}
	if res.Skipped {
		t.Fatal("recon skipped: lock held")
	}
	if len(res.Mismatches) != 0 {
		t.Fatalf("recon found %d mismatches: %+v", len(res.Mismatches), res.Mismatches)
	}
}

func TestReconCleanLedgerHasNoMismatches(t *testing.T) {
	ctx := context.Background()
	src, dst := newAccount(t, true), newAccount(t, false)
	transfer(t, uniq("k"), move(src.ID, dst.ID, 500))
	transfer(t, uniq("k"), move(dst.ID, src.ID, 9999)) // rejected: no entries

	res, err := recon.Run(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Mismatches) != 0 {
		t.Fatalf("mismatches on a clean ledger: %+v", res.Mismatches)
	}
	if res.AccountsChecked < 2 || res.RunID == 0 {
		t.Fatalf("run not recorded properly: %+v", res)
	}

	latest, err := recon.Latest(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if latest.RunID != res.RunID || len(latest.Mismatches) != 0 {
		t.Fatalf("latest = %+v, want run %d", latest, res.RunID)
	}
}

// Corrupting one cached balance behind the service's back must produce
// exactly one finding, naming that account and the size of the drift.
func TestReconDetectsCorruptedBalance(t *testing.T) {
	ctx := context.Background()
	src, dst := newAccount(t, true), newAccount(t, false)
	transfer(t, uniq("k"), move(src.ID, dst.ID, 1000))

	if _, err := pool.Exec(ctx, `UPDATE accounts SET balance = balance + 7 WHERE id = $1`, dst.ID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `UPDATE accounts SET balance = balance - 7 WHERE id = $1`, dst.ID)
	})

	res, err := recon.Run(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Mismatches) != 1 {
		t.Fatalf("got %d mismatches, want 1: %+v", len(res.Mismatches), res.Mismatches)
	}
	m := res.Mismatches[0]
	if m.Kind != recon.KindAccount || m.AccountID != dst.ID || m.Cached-m.Computed != 7 {
		t.Fatalf("mismatch = %+v, want account %d off by 7", m, dst.ID)
	}

	var recorded int
	if err := pool.QueryRow(ctx, `SELECT mismatches FROM reconciliation_runs WHERE id = $1`, res.RunID).
		Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded != 1 {
		t.Fatalf("recorded %d mismatches, want 1", recorded)
	}
}

// A lone entry (a transfer with only one side) breaks every invariant at
// once. Entries are append-only, so this runs in a transaction that is
// rolled back instead of committed.
func TestReconDetectsUnbalancedTransfer(t *testing.T) {
	ctx := context.Background()
	acct := newAccount(t, false)

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	id := uuid.New()
	if _, err := tx.Exec(ctx, `
		INSERT INTO transfers (id, idempotency_key, request_hash, status)
		VALUES ($1, $2, '\x00', 'posted')`, id, uniq("k")); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO entries (transfer_id, account_id, direction, amount)
		VALUES ($1, $2, 'credit', 300)`, id, acct.ID); err != nil {
		t.Fatal(err)
	}

	res, err := recon.Check(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Count(recon.KindAccount); got != 1 {
		t.Errorf("account findings = %d, want 1", got)
	}
	if got := res.Count(recon.KindTransfer); got != 1 {
		t.Errorf("transfer findings = %d, want 1", got)
	}
	if got := res.Count(recon.KindTrialBalance); got != 1 {
		t.Errorf("trial balance findings = %d, want 1", got)
	}
	for _, m := range res.Mismatches {
		if m.Kind == recon.KindTransfer && m.TransferID != id.String() {
			t.Errorf("transfer finding names %s, want %s", m.TransferID, id)
		}
	}
}
