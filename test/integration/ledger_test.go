//go:build integration

package integration

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/michael-jw-ji/go-ledger/internal/ledger"
)

func TestTransferPostsAndUpdatesBalances(t *testing.T) {
	funding, alice, bob := newAccount(t, true), newAccount(t, false), newAccount(t, false)

	transfer(t, uniq("k"), move(funding.ID, alice.ID, 1000))
	tr := transfer(t, uniq("k"), []ledger.Leg{
		{AccountID: alice.ID, Direction: ledger.Debit, Amount: 600},
		{AccountID: bob.ID, Direction: ledger.Credit, Amount: 450},
		{AccountID: funding.ID, Direction: ledger.Credit, Amount: 150},
	})

	if tr.Status != ledger.StatusPosted || len(tr.Entries) != 3 {
		t.Fatalf("got status %s with %d entries, want posted with 3", tr.Status, len(tr.Entries))
	}
	wantBalance(t, funding.ID, -850)
	wantBalance(t, alice.ID, 400)
	wantBalance(t, bob.ID, 450)

	a, _ := svc.GetAccount(context.Background(), alice.ID)
	if a.LockVersion != 2 {
		t.Errorf("alice lock_version = %d, want 2 (one per entry)", a.LockVersion)
	}
}

func TestOverdraftRejected(t *testing.T) {
	funding, alice, bob := newAccount(t, true), newAccount(t, false), newAccount(t, false)
	transfer(t, uniq("k"), move(funding.ID, alice.ID, 100))

	tr := transfer(t, uniq("k"), move(alice.ID, bob.ID, 101))

	if tr.Status != ledger.StatusRejected || tr.ErrorCode != ledger.CodeInsufficientFunds {
		t.Fatalf("got %s/%s, want rejected/insufficient_funds", tr.Status, tr.ErrorCode)
	}
	if len(tr.Entries) != 0 {
		t.Errorf("rejected transfer has %d entries, want 0", len(tr.Entries))
	}
	wantBalance(t, alice.ID, 100)
	wantBalance(t, bob.ID, 0)

	// The rejection is recorded, with no entries, so it can be replayed.
	got, err := svc.GetTransfer(context.Background(), tr.ID)
	if err != nil || got.Status != ledger.StatusRejected || len(got.Entries) != 0 {
		t.Fatalf("stored transfer = %+v, %v", got, err)
	}
}

func TestRejectedKeyReplaysAfterFundsArrive(t *testing.T) {
	funding, alice, bob := newAccount(t, true), newAccount(t, false), newAccount(t, false)
	key := uniq("k")

	first := transfer(t, key, move(alice.ID, bob.ID, 500))
	if first.Status != ledger.StatusRejected {
		t.Fatalf("first attempt: %s, want rejected", first.Status)
	}
	transfer(t, uniq("k"), move(funding.ID, alice.ID, 1000))

	// Even with enough funds now (and no Redis), the same key must not post.
	retry := transfer(t, key, move(alice.ID, bob.ID, 500))
	if retry.ID != first.ID || retry.Status != ledger.StatusRejected || !retry.Replayed {
		t.Fatalf("retry = %s %s replayed=%v, want replay of %s", retry.ID, retry.Status, retry.Replayed, first.ID)
	}
	wantBalance(t, alice.ID, 1000)
	wantBalance(t, bob.ID, 0)
}

func TestDuplicateKeyReturnsSameTransfer(t *testing.T) {
	funding, alice := newAccount(t, true), newAccount(t, false)
	key := uniq("k")

	first := transfer(t, key, move(funding.ID, alice.ID, 250))
	second := transfer(t, key, move(funding.ID, alice.ID, 250))

	if second.ID != first.ID || !second.Replayed || first.Replayed {
		t.Fatalf("first=%s replayed=%v, second=%s replayed=%v", first.ID, first.Replayed, second.ID, second.Replayed)
	}
	if len(second.Entries) != 2 {
		t.Errorf("replay has %d entries, want 2", len(second.Entries))
	}
	wantBalance(t, alice.ID, 250)

	_, err := svc.CreateTransfer(context.Background(), ledger.TransferRequest{
		IdempotencyKey: key, Legs: move(funding.ID, alice.ID, 999),
	})
	if !errors.Is(err, ledger.ErrIdempotencyMismatch) {
		t.Fatalf("same key, different body: got %v, want ErrIdempotencyMismatch", err)
	}
}

// Without Redis in front, concurrent duplicates still post once: the unique
// index makes every duplicate wait for the first insert to commit.
func TestConcurrentDuplicateKeyPostsOnce(t *testing.T) {
	funding, alice := newAccount(t, true), newAccount(t, false)
	key := uniq("k")

	const n = 20
	ids := make([]uuid.UUID, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr, err := svc.CreateTransfer(context.Background(), ledger.TransferRequest{
				IdempotencyKey: key, Legs: move(funding.ID, alice.ID, 10),
			})
			if err != nil {
				t.Errorf("transfer: %v", err)
				return
			}
			ids[i] = tr.ID
		}()
	}
	wg.Wait()

	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("got different transfer ids for one key: %v", ids)
		}
	}
	wantBalance(t, alice.ID, 10)
}

func TestReversalRestoresBalances(t *testing.T) {
	ctx := context.Background()
	funding, alice, bob := newAccount(t, true), newAccount(t, false), newAccount(t, false)
	transfer(t, uniq("k"), move(funding.ID, alice.ID, 1000))
	pay := transfer(t, uniq("k"), move(alice.ID, bob.ID, 300))

	key := uniq("k")
	rev, err := svc.ReverseTransfer(ctx, pay.ID, key)
	if err != nil {
		t.Fatalf("reverse: %v", err)
	}
	if rev.Status != ledger.StatusPosted || rev.ReversesTransferID == nil || *rev.ReversesTransferID != pay.ID {
		t.Fatalf("reversal = %+v", rev)
	}
	wantBalance(t, alice.ID, 1000)
	wantBalance(t, bob.ID, 0)

	again, err := svc.ReverseTransfer(ctx, pay.ID, key)
	if err != nil || again.ID != rev.ID || !again.Replayed {
		t.Fatalf("same key replay = %+v, %v", again, err)
	}
	if _, err := svc.ReverseTransfer(ctx, pay.ID, uniq("k")); !errors.Is(err, ledger.ErrAlreadyReversed) {
		t.Fatalf("second reversal with a new key: got %v, want ErrAlreadyReversed", err)
	}
	if _, err := svc.ReverseTransfer(ctx, rev.ID, uniq("k")); !errors.Is(err, ledger.ErrNotReversible) {
		t.Fatalf("reversing a reversal: got %v, want ErrNotReversible", err)
	}
	wantBalance(t, alice.ID, 1000)
}

// If the money has already moved on, the reversal is rejected, and a later
// reversal (after funds return) is still allowed.
func TestRejectedReversalCanBeRetried(t *testing.T) {
	ctx := context.Background()
	funding, alice, bob := newAccount(t, true), newAccount(t, false), newAccount(t, false)
	transfer(t, uniq("k"), move(funding.ID, alice.ID, 300))
	pay := transfer(t, uniq("k"), move(alice.ID, bob.ID, 300))
	out := transfer(t, uniq("k"), move(bob.ID, funding.ID, 300))

	rev, err := svc.ReverseTransfer(ctx, pay.ID, uniq("k"))
	if err != nil || rev.Status != ledger.StatusRejected || rev.ErrorCode != ledger.CodeInsufficientFunds {
		t.Fatalf("reversal with bob empty = %+v, %v", rev, err)
	}
	if _, err := svc.ReverseTransfer(ctx, out.ID, uniq("k")); err != nil {
		t.Fatalf("reverse bob's payout: %v", err)
	}
	rev, err = svc.ReverseTransfer(ctx, pay.ID, uniq("k"))
	if err != nil || rev.Status != ledger.StatusPosted {
		t.Fatalf("retried reversal = %+v, %v", rev, err)
	}
	wantBalance(t, alice.ID, 300)
	wantBalance(t, bob.ID, 0)
}

func TestBusinessRuleRejections(t *testing.T) {
	usd, eur := newAccount(t, true), newAccountIn(t, "EUR", true)

	tr := transfer(t, uniq("k"), move(usd.ID, eur.ID, 100))
	if tr.ErrorCode != ledger.CodeCurrencyMismatch {
		t.Errorf("cross-currency: got %q, want currency_mismatch", tr.ErrorCode)
	}
	tr = transfer(t, uniq("k"), move(usd.ID, 1<<40, 100))
	if tr.ErrorCode != ledger.CodeAccountNotFound {
		t.Errorf("unknown account: got %q, want account_not_found", tr.ErrorCode)
	}
	wantBalance(t, usd.ID, 0)
}

func TestEntriesAreAppendOnly(t *testing.T) {
	ctx := context.Background()
	funding, alice := newAccount(t, true), newAccount(t, false)
	tr := transfer(t, uniq("k"), move(funding.ID, alice.ID, 100))

	if _, err := pool.Exec(ctx, `UPDATE entries SET amount = 1 WHERE id = $1`, tr.Entries[0].ID); err == nil {
		t.Error("UPDATE on entries succeeded, want error")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM entries WHERE id = $1`, tr.Entries[0].ID); err == nil {
		t.Error("DELETE on entries succeeded, want error")
	}
}

func TestListEntriesPaginates(t *testing.T) {
	ctx := context.Background()
	funding, alice := newAccount(t, true), newAccount(t, false)
	for i := 1; i <= 5; i++ {
		transfer(t, uniq("k"), move(funding.ID, alice.ID, int64(i)))
	}

	var amounts []int64
	var cursor int64
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("pagination did not terminate")
		}
		page, next, err := svc.ListEntries(ctx, alice.ID, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range page {
			amounts = append(amounts, e.Amount)
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	want := []int64{5, 4, 3, 2, 1}
	if len(amounts) != len(want) {
		t.Fatalf("got %v, want %v", amounts, want)
	}
	for i := range want {
		if amounts[i] != want[i] {
			t.Fatalf("got %v, want %v", amounts, want)
		}
	}
}
