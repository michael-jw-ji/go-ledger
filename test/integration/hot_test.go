//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/michael-jw-ji/go-ledger/internal/ledger"
	"github.com/michael-jw-ji/go-ledger/internal/rollup"
)

func newHotAccount(t *testing.T) ledger.Account {
	t.Helper()
	a, err := svc.CreateAccount(context.Background(), ledger.NewAccount{
		Name: uniq("merchant"), Currency: "USD", NormalBalance: ledger.Credit, Hot: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// raw returns the cached balance, lock_version and pending entry count,
// bypassing the service's effective-balance read.
func raw(t *testing.T, id int64) (cached, version, pending int64) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `
		SELECT balance, lock_version, (SELECT count(*) FROM hot_pending WHERE account_id = $1)
		FROM accounts WHERE id = $1`, id).Scan(&cached, &version, &pending)
	if err != nil {
		t.Fatal(err)
	}
	return
}

// holdRowLock locks an account's row from another transaction, the way a
// slow concurrent transfer or the rollup worker would, until the returned
// release func runs.
func holdRowLock(t *testing.T, id int64) (release func()) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM accounts WHERE id = $1 FOR NO KEY UPDATE`, id); err != nil {
		t.Fatal(err)
	}
	done := false
	release = func() {
		if !done {
			done = true
			_ = tx.Rollback(ctx)
		}
	}
	t.Cleanup(release)
	return release
}

func TestHotCreditSkipsRowLock(t *testing.T) {
	src, merchant := newAccount(t, true), newHotAccount(t)
	release := holdRowLock(t, merchant.ID)

	// With the merchant's row locked elsewhere, an ordinary account would
	// block here. A credit to a hot account must not.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	tr, err := svc.CreateTransfer(ctx, ledger.TransferRequest{IdempotencyKey: uniq("k"), Legs: move(src.ID, merchant.ID, 250)})
	if err != nil {
		t.Fatalf("credit to a hot account blocked or failed: %v", err)
	}
	if tr.Status != ledger.StatusPosted {
		t.Fatalf("status = %s", tr.Status)
	}
	release()

	// Reads see the credit immediately; the cached balance doesn't yet.
	a, err := svc.GetAccount(context.Background(), merchant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a.Balance != 250 || a.LockVersion != 1 || a.PendingEntries != 1 {
		t.Fatalf("effective = %d v%d pending %d, want 250 v1 pending 1", a.Balance, a.LockVersion, a.PendingEntries)
	}
	if cached, version, pending := raw(t, merchant.ID); cached != 0 || version != 0 || pending != 1 {
		t.Fatalf("raw = %d v%d pending %d, want 0 v0 pending 1", cached, version, pending)
	}

	n, err := rollup.Account(context.Background(), pool, merchant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("rolled up %d entries, want 1", n)
	}
	if cached, version, pending := raw(t, merchant.ID); cached != 250 || version != 1 || pending != 0 {
		t.Fatalf("after rollup raw = %d v%d pending %d, want 250 v1 pending 0", cached, version, pending)
	}
	wantBalance(t, merchant.ID, 250)
	assertReconClean(t)
}

// Spending from a hot account takes its row lock, counts pending credits as
// available, and folds them into the cached balance on the way.
func TestHotDebitLocksAndFoldsPending(t *testing.T) {
	src, dst, merchant := newAccount(t, true), newAccount(t, false), newHotAccount(t)
	transfer(t, uniq("k"), move(src.ID, merchant.ID, 100))
	transfer(t, uniq("k"), move(src.ID, merchant.ID, 100))

	release := holdRowLock(t, merchant.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := svc.CreateTransfer(ctx, ledger.TransferRequest{IdempotencyKey: uniq("k"), Legs: move(merchant.ID, dst.ID, 150)})
	if err == nil {
		t.Fatal("a debit of a hot account went through while its row was locked")
	}
	if !errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		t.Fatalf("unexpected error: %v", err)
	}
	release()

	// Cached balance is 0 but 200 is pending, so 150 is affordable.
	tr := transfer(t, uniq("k"), move(merchant.ID, dst.ID, 150))
	if tr.Status != ledger.StatusPosted {
		t.Fatalf("debit against pending credits: %s %s", tr.Status, tr.ErrorCode)
	}
	if cached, version, pending := raw(t, merchant.ID); cached != 50 || version != 3 || pending != 0 {
		t.Fatalf("raw = %d v%d pending %d, want 50 v3 pending 0", cached, version, pending)
	}

	if tr := transfer(t, uniq("k"), move(merchant.ID, dst.ID, 51)); tr.ErrorCode != ledger.CodeInsufficientFunds {
		t.Fatalf("overdraft of a hot account: %s %s", tr.Status, tr.ErrorCode)
	}
	wantBalance(t, merchant.ID, 50)
	assertReconClean(t)
}

// A rejected debit rolls back to its savepoint, which must also undo the
// inline rollup: the pending rows come back untouched.
func TestRejectedHotDebitKeepsPendingRows(t *testing.T) {
	src, dst, merchant := newAccount(t, true), newAccount(t, false), newHotAccount(t)
	transfer(t, uniq("k"), move(src.ID, merchant.ID, 100))

	if tr := transfer(t, uniq("k"), move(merchant.ID, dst.ID, 500)); tr.ErrorCode != ledger.CodeInsufficientFunds {
		t.Fatalf("got %s %s, want insufficient funds", tr.Status, tr.ErrorCode)
	}
	if cached, version, pending := raw(t, merchant.ID); cached != 0 || version != 0 || pending != 1 {
		t.Fatalf("raw = %d v%d pending %d, want 0 v0 pending 1", cached, version, pending)
	}
	wantBalance(t, merchant.ID, 100)
	assertReconClean(t)
}

// A hot account that may go negative never needs its lock, in either
// direction.
func TestHotAllowNegativeDebitSkipsRowLock(t *testing.T) {
	ctx := context.Background()
	fees, err := svc.CreateAccount(ctx, ledger.NewAccount{
		Name: uniq("fees"), Currency: "USD", NormalBalance: ledger.Credit, AllowNegative: true, Hot: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	dst := newAccount(t, false)
	holdRowLock(t, fees.ID)

	tctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := svc.CreateTransfer(tctx, ledger.TransferRequest{IdempotencyKey: uniq("k"), Legs: move(fees.ID, dst.ID, 75)}); err != nil {
		t.Fatalf("debit of an allow-negative hot account blocked or failed: %v", err)
	}
	wantBalance(t, fees.ID, -75)
	wantBalance(t, dst.ID, 75)
}

// Reads of a hot account come from one snapshot, so a rollup committing
// between two halves of the read can't double-count or drop entries.
func TestHotBalanceReadIsConsistentDuringRollup(t *testing.T) {
	ctx := context.Background()
	src, merchant := newAccount(t, true), newHotAccount(t)
	var want int64
	for i := range 50 {
		amt := int64(i + 1)
		transfer(t, uniq("k"), move(src.ID, merchant.ID, amt))
		want += amt
		if i%7 == 0 {
			if _, err := rollup.Account(ctx, pool, merchant.ID); err != nil {
				t.Fatal(err)
			}
		}
		wantBalance(t, merchant.ID, want)
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var before int64
	if err := tx.QueryRow(ctx, `
		SELECT a.balance + COALESCE((SELECT sum(signed_amount) FROM hot_pending WHERE account_id = a.id), 0)
		FROM accounts a WHERE a.id = $1`, merchant.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := rollup.Account(ctx, pool, merchant.ID); err != nil {
		t.Fatal(err)
	}
	if before != want {
		t.Fatalf("snapshot balance = %d, want %d", before, want)
	}
	wantBalance(t, merchant.ID, want)
}
