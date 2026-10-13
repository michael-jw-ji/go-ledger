package ledger

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Service is the ledger's write and read API over Postgres.
type Service struct {
	pool  *pgxpool.Pool
	hooks Hooks
}

// Hooks let a caller observe the service without the ledger package
// depending on a metrics library. Any hook may be nil.
type Hooks struct {
	// OnTransfer is called once per transfer attempt that reached the
	// database, with its outcome ("posted", "rejected", "replayed",
	// "conflict" or "error") and how long it took.
	OnTransfer func(outcome string, took time.Duration)
	// OnLockWait is called with the time spent acquiring the account row
	// locks of one transfer.
	OnLockWait func(took time.Duration)
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// WithHooks returns a copy of the service that reports to h.
func (s *Service) WithHooks(h Hooks) *Service {
	return &Service{pool: s.pool, hooks: h}
}

// selectAccounts reads effective balances: the cached balance plus any hot
// entries not rolled up yet, in one statement so both come from the same
// snapshot.
const selectAccounts = `
	SELECT a.id, a.name, a.currency, a.normal_balance, a.allow_negative, a.hot,
	       a.balance + COALESCE(p.total, 0), a.lock_version + COALESCE(p.n, 0), COALESCE(p.n, 0),
	       a.created_at
	FROM accounts a
	LEFT JOIN LATERAL (
	    SELECT SUM(signed_amount) AS total, COUNT(*) AS n
	    FROM hot_pending WHERE account_id = a.id
	) p ON a.hot`

func scanAccount(row pgx.Row) (Account, error) {
	var a Account
	err := row.Scan(&a.ID, &a.Name, &a.Currency, &a.NormalBalance, &a.AllowNegative, &a.Hot,
		&a.Balance, &a.LockVersion, &a.PendingEntries, &a.CreatedAt)
	return a, err
}

func (s *Service) CreateAccount(ctx context.Context, in NewAccount) (Account, error) {
	if err := in.Validate(); err != nil {
		return Account{}, err
	}
	return scanAccount(s.pool.QueryRow(ctx, `
		INSERT INTO accounts (name, currency, normal_balance, allow_negative, hot)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, name, currency, normal_balance, allow_negative, hot,
		          balance, lock_version, 0::bigint, created_at`,
		in.Name, in.Currency, string(in.NormalBalance), in.AllowNegative, in.Hot))
}

func (s *Service) GetAccount(ctx context.Context, id int64) (Account, error) {
	a, err := scanAccount(s.pool.QueryRow(ctx, selectAccounts+` WHERE a.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, fmt.Errorf("account %d: %w", id, ErrNotFound)
	}
	return a, err
}

func (s *Service) ListAccounts(ctx context.Context) ([]Account, error) {
	rows, err := s.pool.Query(ctx, selectAccounts+` ORDER BY a.id LIMIT 1000`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Account, error) { return scanAccount(r) })
}

// CreateTransfer posts a balanced set of entries atomically.
//
// Business-rule failures (insufficient funds, currency mismatch, unknown
// account) are not Go errors: they come back as a Transfer with
// StatusRejected, recorded under the idempotency key so a retry with the same
// key returns the same rejection even if Redis has forgotten it.
func (s *Service) CreateTransfer(ctx context.Context, req TransferRequest) (Transfer, error) {
	if err := req.Validate(); err != nil {
		return Transfer{}, err
	}
	fp := req.Fingerprint
	if len(fp) == 0 {
		fp = RequestHash(req.Legs)
	}
	return s.post(ctx, req.IdempotencyKey, fp, req.Legs, nil)
}

// ReverseTransfer posts the mirror image of a posted transfer. The key is
// scoped to the transfer, so the same client key can be reused across
// different reversals.
//
// fingerprint identifies the request, as in TransferRequest. If empty, it
// defaults to the fingerprint of an empty HTTP body, so direct callers and
// body-less HTTP requests agree.
func (s *Service) ReverseTransfer(ctx context.Context, id uuid.UUID, idempotencyKey string, fingerprint []byte) (Transfer, error) {
	if idempotencyKey == "" || len(idempotencyKey) > maxKeyLen {
		return Transfer{}, invalid("idempotency key must be 1 to %d characters", maxKeyLen)
	}
	orig, err := s.GetTransfer(ctx, id)
	if err != nil {
		return Transfer{}, err
	}
	if orig.Status != StatusPosted {
		return Transfer{}, fmt.Errorf("%w: transfer is %s", ErrNotReversible, orig.Status)
	}
	if orig.ReversesTransferID != nil {
		return Transfer{}, fmt.Errorf("%w: transfer is itself a reversal", ErrNotReversible)
	}
	legs := make([]Leg, len(orig.Entries))
	for i, e := range orig.Entries {
		legs[i] = Leg{AccountID: e.AccountID, Direction: e.Direction.Opposite(), Amount: e.Amount}
	}
	key := "reverse:" + id.String() + ":" + idempotencyKey
	if len(fingerprint) == 0 {
		empty := sha256.Sum256(nil)
		fingerprint = empty[:]
	}
	return s.post(ctx, key, fingerprint, legs, &id)
}

// post runs the whole transfer in one transaction:
//
//  1. Insert the transfer row as pending. The UNIQUE idempotency_key makes a
//     concurrent duplicate wait here until we commit, then replay our result.
//  2. Under a savepoint, lock every account in ascending id order (so two
//     transfers can never deadlock), check the business rules, insert the
//     entries and update the cached balances.
//  3. On a business-rule failure, roll back to the savepoint and record the
//     transfer as rejected instead, so the rejection is idempotent too.
func (s *Service) post(ctx context.Context, key string, hash []byte, legs []Leg, reverses *uuid.UUID) (Transfer, error) {
	start := time.Now()
	t, err := s.postTx(ctx, key, hash, legs, reverses)
	if s.hooks.OnTransfer != nil {
		outcome := string(t.Status)
		switch {
		case errors.Is(err, ErrAlreadyReversed), errors.Is(err, ErrIdempotencyMismatch):
			outcome = "conflict"
		case err != nil:
			outcome = "error"
		case t.Replayed:
			outcome = "replayed"
		}
		s.hooks.OnTransfer(outcome, time.Since(start))
	}
	return t, err
}

func (s *Service) postTx(ctx context.Context, key string, hash []byte, legs []Leg, reverses *uuid.UUID) (Transfer, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Transfer{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	t := Transfer{ID: uuid.New(), Status: StatusPending, ReversesTransferID: reverses}
	err = tx.QueryRow(ctx, `
		INSERT INTO transfers (id, idempotency_key, request_hash, status, reverses_transfer_id)
		VALUES ($1, $2, $3, 'pending', $4)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING created_at`,
		t.ID, key, hash, reverses).Scan(&t.CreatedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The key is already recorded (and committed: ON CONFLICT waited for it).
		_ = tx.Rollback(ctx)
		return s.replay(ctx, key, hash)
	case isUniqueViolation(err, "transfers_one_reversal"):
		return Transfer{}, ErrAlreadyReversed
	case err != nil:
		return Transfer{}, err
	}

	sp, err := tx.Begin(ctx) // SAVEPOINT
	if err != nil {
		return Transfer{}, err
	}
	entries, code, err := s.applyLegs(ctx, sp, t.ID, legs)
	if err != nil {
		return Transfer{}, err
	}
	if code != "" {
		if err := sp.Rollback(ctx); err != nil { // ROLLBACK TO SAVEPOINT
			return Transfer{}, err
		}
		t.Status, t.ErrorCode, t.Entries = StatusRejected, code, []Entry{}
	} else {
		if err := sp.Commit(ctx); err != nil { // RELEASE SAVEPOINT
			return Transfer{}, err
		}
		t.Status, t.Entries = StatusPosted, entries
	}

	var errCode *string
	if t.ErrorCode != "" {
		c := string(t.ErrorCode)
		errCode = &c
	}
	// The event commits or rolls back with the entries, so other systems
	// hear about exactly the transfers that moved money. Rejections move
	// nothing and publish nothing; replays never reach this point. It shares
	// one statement with the status update: the account row locks are still
	// held here, and every extra round trip lengthens how long they are.
	var eventType *string
	var payload []byte
	if t.Status == StatusPosted {
		et := EventTransferPosted
		if reverses != nil {
			et = EventTransferReversed
		}
		eventType = &et
		if payload, err = json.Marshal(t); err != nil {
			return Transfer{}, err
		}
	}
	if _, err := tx.Exec(ctx, `
		WITH u AS (
		    UPDATE transfers SET status = $2, error_code = $3 WHERE id = $1
		)
		INSERT INTO outbox (event_type, aggregate_id, payload)
		SELECT $4, $1, $5 WHERE $4::text IS NOT NULL`,
		t.ID, string(t.Status), errCode, eventType, payload); err != nil {
		return Transfer{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Transfer{}, err
	}
	return t, nil
}

// applyLegs checks the business rules and writes the entries. Accounts take
// one of two paths:
//
//   - Locked: ordinary accounts, and hot accounts this transfer would
//     decrease without an overdraft allowance. Their rows are locked in
//     ascending id order (so transfers can't deadlock), checked, and updated.
//   - Hot fast path: hot accounts this transfer increases (or that may go
//     negative). No row lock and no balance update; each of their entries
//     gets a hot_pending row instead, committed with the transfer, and the
//     rollup worker applies it to the cached balance shortly after.
//
// The fast path can't break an overdraft check: unlocked writers only ever
// add to a no-overdraft hot account, so a check made under its lock stays
// true as their entries land.
func (s *Service) applyLegs(ctx context.Context, tx pgx.Tx, transferID uuid.UUID, legs []Leg) ([]Entry, ErrorCode, error) {
	ids := make([]int64, 0, len(legs))
	for _, l := range legs {
		ids = append(ids, l.AccountID)
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)

	type acct struct {
		currency      string
		normal        Side
		allowNegative bool
		hot           bool
		locked        bool
		cached        int64 // accounts.balance, read under the lock
		folded, nFold int64 // pending hot entries rolled up by this transfer
		delta, n      int64 // this transfer's effect
	}
	// Currency, normal side and the flags never change after creation, so
	// they can be read before deciding what to lock.
	rows, err := tx.Query(ctx, `
		SELECT id, currency, normal_balance, allow_negative, hot
		FROM accounts WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, "", err
	}
	accts := make(map[int64]*acct, len(ids))
	for rows.Next() {
		var id int64
		a := &acct{}
		if err := rows.Scan(&id, &a.currency, &a.normal, &a.allowNegative, &a.hot); err != nil {
			rows.Close()
			return nil, "", err
		}
		accts[id] = a
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(accts) != len(ids) {
		return nil, CodeAccountNotFound, nil
	}

	var currency string
	for _, a := range accts {
		if currency == "" {
			currency = a.currency
		} else if a.currency != currency {
			return nil, CodeCurrencyMismatch, nil
		}
	}

	for _, l := range legs {
		a := accts[l.AccountID]
		a.delta += l.delta(a.normal)
		a.n++
	}
	var lockIDs, hotLockIDs []int64
	for _, id := range ids {
		a := accts[id]
		a.locked = !a.hot || (a.delta < 0 && !a.allowNegative)
		if a.locked {
			lockIDs = append(lockIDs, id)
			if a.hot {
				hotLockIDs = append(hotLockIDs, id)
			}
		}
	}

	if len(lockIDs) > 0 {
		// FOR NO KEY UPDATE, not FOR UPDATE: it still serializes balance
		// writers, but doesn't conflict with the FOR KEY SHARE lock that
		// inserting an entry (a foreign key to accounts) takes. With FOR
		// UPDATE, a debit or rollup of a hot account would queue behind
		// every in-flight credit to it, and new credits behind that.
		lockStart := time.Now()
		rows, err = tx.Query(ctx, `
			SELECT id, balance FROM accounts WHERE id = ANY($1)
			ORDER BY id
			FOR NO KEY UPDATE`, lockIDs)
		if err != nil {
			return nil, "", err
		}
		for rows.Next() {
			var id, bal int64
			if err := rows.Scan(&id, &bal); err != nil {
				rows.Close()
				return nil, "", err
			}
			accts[id].cached = bal
		}
		if err := rows.Err(); err != nil {
			return nil, "", err
		}
		if s.hooks.OnLockWait != nil {
			s.hooks.OnLockWait(time.Since(lockStart))
		}
	}

	// A hot account on the locked path is being debited, and its pending
	// entries count toward what it can spend. Holding its row lock, roll
	// them up here: that is the lock the rollup worker would take anyway,
	// and it keeps the cached balance from dipping below zero.
	if len(hotLockIDs) > 0 {
		rows, err = tx.Query(ctx, `
			WITH d AS (
			    DELETE FROM hot_pending WHERE account_id = ANY($1)
			    RETURNING account_id, signed_amount
			)
			SELECT account_id, SUM(signed_amount)::bigint, COUNT(*) FROM d GROUP BY account_id`, hotLockIDs)
		if err != nil {
			return nil, "", err
		}
		for rows.Next() {
			var id, total, n int64
			if err := rows.Scan(&id, &total, &n); err != nil {
				rows.Close()
				return nil, "", err
			}
			accts[id].folded, accts[id].nFold = total, n
		}
		if err := rows.Err(); err != nil {
			return nil, "", err
		}
	}

	deltas := make([]int64, len(lockIDs))
	counts := make([]int64, len(lockIDs))
	for i, id := range lockIDs {
		a := accts[id]
		if !a.allowNegative && a.cached+a.folded+a.delta < 0 {
			return nil, CodeInsufficientFunds, nil
		}
		deltas[i], counts[i] = a.folded+a.delta, a.nFold+a.n
	}

	accountIDs := make([]int64, len(legs))
	directions := make([]string, len(legs))
	amounts := make([]int64, len(legs))
	for i, l := range legs {
		accountIDs[i], directions[i], amounts[i] = l.AccountID, string(l.Direction), l.Amount
	}
	rows, err = tx.Query(ctx, `
		INSERT INTO entries (transfer_id, account_id, direction, amount)
		SELECT $1, l.account_id, l.direction::side, l.amount
		FROM unnest($2::bigint[], $3::text[], $4::bigint[]) WITH ORDINALITY
		     AS l(account_id, direction, amount, ord)
		ORDER BY l.ord
		RETURNING id, transfer_id, account_id, direction, amount, created_at`,
		transferID, accountIDs, directions, amounts)
	if err != nil {
		return nil, "", err
	}
	entries, err := pgx.CollectRows(rows, scanEntry)
	if err != nil {
		return nil, "", err
	}
	slices.SortFunc(entries, func(a, b Entry) int { return cmp.Compare(a.ID, b.ID) })

	var pendingEntries, pendingAccounts, pendingAmounts []int64
	for _, e := range entries {
		if a := accts[e.AccountID]; !a.locked {
			pendingEntries = append(pendingEntries, e.ID)
			pendingAccounts = append(pendingAccounts, e.AccountID)
			pendingAmounts = append(pendingAmounts, Leg{Direction: e.Direction, Amount: e.Amount}.delta(a.normal))
		}
	}
	if len(pendingEntries) > 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO hot_pending (entry_id, account_id, signed_amount)
			SELECT * FROM unnest($1::bigint[], $2::bigint[], $3::bigint[])`,
			pendingEntries, pendingAccounts, pendingAmounts); err != nil {
			return nil, "", err
		}
	}

	if len(lockIDs) > 0 {
		if _, err := tx.Exec(ctx, `
			UPDATE accounts a
			SET balance = a.balance + d.delta, lock_version = a.lock_version + d.n
			FROM unnest($1::bigint[], $2::bigint[], $3::bigint[]) AS d(id, delta, n)
			WHERE a.id = d.id`,
			lockIDs, deltas, counts); err != nil {
			return nil, "", err
		}
	}
	return entries, "", nil
}

func scanEntry(r pgx.CollectableRow) (Entry, error) {
	var e Entry
	err := r.Scan(&e.ID, &e.TransferID, &e.AccountID, &e.Direction, &e.Amount, &e.CreatedAt)
	return e, err
}

func (s *Service) replay(ctx context.Context, key string, hash []byte) (Transfer, error) {
	var stored []byte
	t, err := s.loadTransfer(ctx, `idempotency_key = $1`, key, &stored)
	if err != nil {
		return Transfer{}, err
	}
	if !bytes.Equal(stored, hash) {
		return Transfer{}, ErrIdempotencyMismatch
	}
	t.Replayed = true
	return t, nil
}

func (s *Service) GetTransfer(ctx context.Context, id uuid.UUID) (Transfer, error) {
	var stored []byte
	return s.loadTransfer(ctx, `id = $1`, id, &stored)
}

func (s *Service) loadTransfer(ctx context.Context, where string, arg any, hash *[]byte) (Transfer, error) {
	var t Transfer
	var code *string
	err := s.pool.QueryRow(ctx, `
		SELECT id, status, error_code, reverses_transfer_id, created_at, request_hash
		FROM transfers WHERE `+where, arg).
		Scan(&t.ID, &t.Status, &code, &t.ReversesTransferID, &t.CreatedAt, hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return Transfer{}, fmt.Errorf("transfer: %w", ErrNotFound)
	}
	if err != nil {
		return Transfer{}, err
	}
	if code != nil {
		t.ErrorCode = ErrorCode(*code)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, transfer_id, account_id, direction, amount, created_at
		FROM entries WHERE transfer_id = $1 ORDER BY id`, t.ID)
	if err != nil {
		return Transfer{}, err
	}
	t.Entries, err = pgx.CollectRows(rows, scanEntry)
	return t, err
}

// ListEntries returns an account's entries newest first. Pass the returned
// cursor back to get the next page; it is 0 when there are no more.
func (s *Service) ListEntries(ctx context.Context, accountID, before int64, limit int) ([]Entry, int64, error) {
	if _, err := s.GetAccount(ctx, accountID); err != nil {
		return nil, 0, err
	}
	if before <= 0 {
		before = 1<<63 - 1
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, transfer_id, account_id, direction, amount, created_at
		FROM entries
		WHERE account_id = $1 AND id < $2
		ORDER BY id DESC
		LIMIT $3`, accountID, before, limit+1)
	if err != nil {
		return nil, 0, err
	}
	entries, err := pgx.CollectRows(rows, scanEntry)
	if err != nil {
		return nil, 0, err
	}
	var next int64
	if len(entries) > limit {
		entries = entries[:limit]
		next = entries[limit-1].ID
	}
	return entries, next, nil
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}
