package ledger

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Service is the ledger's write and read API over Postgres.
type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

const accountCols = `id, name, currency, normal_balance, allow_negative, balance, lock_version, created_at`

func scanAccount(row pgx.Row) (Account, error) {
	var a Account
	err := row.Scan(&a.ID, &a.Name, &a.Currency, &a.NormalBalance, &a.AllowNegative,
		&a.Balance, &a.LockVersion, &a.CreatedAt)
	return a, err
}

func (s *Service) CreateAccount(ctx context.Context, in NewAccount) (Account, error) {
	if err := in.Validate(); err != nil {
		return Account{}, err
	}
	return scanAccount(s.pool.QueryRow(ctx, `
		INSERT INTO accounts (name, currency, normal_balance, allow_negative)
		VALUES ($1, $2, $3, $4)
		RETURNING `+accountCols,
		in.Name, in.Currency, string(in.NormalBalance), in.AllowNegative))
}

func (s *Service) GetAccount(ctx context.Context, id int64) (Account, error) {
	a, err := scanAccount(s.pool.QueryRow(ctx,
		`SELECT `+accountCols+` FROM accounts WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, fmt.Errorf("account %d: %w", id, ErrNotFound)
	}
	return a, err
}

func (s *Service) ListAccounts(ctx context.Context) ([]Account, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+accountCols+` FROM accounts ORDER BY id LIMIT 1000`)
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
	return s.post(ctx, req.IdempotencyKey, RequestHash(req.Legs), req.Legs, nil)
}

// ReverseTransfer posts the mirror image of a posted transfer. The key is
// scoped to the transfer, so the same client key can be reused across
// different reversals.
func (s *Service) ReverseTransfer(ctx context.Context, id uuid.UUID, idempotencyKey string) (Transfer, error) {
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
	hash := sha256.Sum256([]byte("reverse:" + id.String()))
	return s.post(ctx, key, hash[:], legs, &id)
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
	entries, code, err := applyLegs(ctx, sp, t.ID, legs)
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
	if _, err := tx.Exec(ctx, `UPDATE transfers SET status = $2, error_code = $3 WHERE id = $1`,
		t.ID, string(t.Status), errCode); err != nil {
		return Transfer{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Transfer{}, err
	}
	return t, nil
}

func applyLegs(ctx context.Context, tx pgx.Tx, transferID uuid.UUID, legs []Leg) ([]Entry, ErrorCode, error) {
	ids := make([]int64, 0, len(legs))
	for _, l := range legs {
		ids = append(ids, l.AccountID)
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)

	type locked struct {
		currency      string
		normal        Side
		allowNegative bool
		balance       int64
		delta, n      int64
	}
	rows, err := tx.Query(ctx, `
		SELECT id, currency, normal_balance, allow_negative, balance
		FROM accounts WHERE id = ANY($1)
		ORDER BY id
		FOR UPDATE`, ids)
	if err != nil {
		return nil, "", err
	}
	accts := make(map[int64]*locked, len(ids))
	for rows.Next() {
		var id int64
		a := &locked{}
		if err := rows.Scan(&id, &a.currency, &a.normal, &a.allowNegative, &a.balance); err != nil {
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
	deltas := make([]int64, len(ids))
	counts := make([]int64, len(ids))
	for i, id := range ids {
		a := accts[id]
		if !a.allowNegative && a.balance+a.delta < 0 {
			return nil, CodeInsufficientFunds, nil
		}
		deltas[i], counts[i] = a.delta, a.n
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

	if _, err := tx.Exec(ctx, `
		UPDATE accounts a
		SET balance = a.balance + d.delta, lock_version = a.lock_version + d.n
		FROM unnest($1::bigint[], $2::bigint[], $3::bigint[]) AS d(id, delta, n)
		WHERE a.id = d.id`,
		ids, deltas, counts); err != nil {
		return nil, "", err
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
