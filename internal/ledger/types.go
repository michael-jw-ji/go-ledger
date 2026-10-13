// Package ledger implements a double-entry ledger on Postgres.
//
// Money is always an int64 count of minor units (cents). A transfer is a set
// of entries whose debits equal its credits; entries are never edited, and
// mistakes are corrected with a reversal transfer.
package ledger

import (
	"time"

	"github.com/google/uuid"
)

// Side is the direction of an entry, or the side an account's balance
// normally sits on.
type Side string

const (
	Debit  Side = "debit"
	Credit Side = "credit"
)

func (s Side) valid() bool { return s == Debit || s == Credit }

func (s Side) Opposite() Side {
	if s == Debit {
		return Credit
	}
	return Debit
}

// Account is a ledger account. Balance is signed relative to NormalBalance:
// a debit-normal account goes up on debits, a credit-normal one on credits.
//
// Balance and LockVersion are effective values. For a hot account they
// include entries the rollup worker has not folded into the cached balance
// yet (PendingEntries of them), so they always match the account's entries.
type Account struct {
	ID             int64     `json:"id"`
	Name           string    `json:"name"`
	Currency       string    `json:"currency"`
	NormalBalance  Side      `json:"normal_balance"`
	AllowNegative  bool      `json:"allow_negative"`
	Hot            bool      `json:"hot"`
	Balance        int64     `json:"balance"`
	LockVersion    int64     `json:"lock_version"`
	PendingEntries int64     `json:"pending_entries"`
	CreatedAt      time.Time `json:"created_at"`
}

// NewAccount is the input to CreateAccount.
type NewAccount struct {
	Name          string `json:"name"`
	Currency      string `json:"currency"`
	NormalBalance Side   `json:"normal_balance"`
	AllowNegative bool   `json:"allow_negative"`
	// Hot marks an account that many transfers credit at once, such as a
	// merchant or fee account. Increases to it skip its row lock; see
	// hot_pending.
	Hot bool `json:"hot"`
}

// Leg is one requested entry of a transfer.
type Leg struct {
	AccountID int64 `json:"account_id"`
	Direction Side  `json:"direction"`
	Amount    int64 `json:"amount"`
}

// delta is the leg's effect on the balance of an account with the given
// normal side.
func (l Leg) delta(normal Side) int64 {
	if l.Direction == normal {
		return l.Amount
	}
	return -l.Amount
}

// TransferRequest is the input to CreateTransfer.
type TransferRequest struct {
	IdempotencyKey string
	Legs           []Leg
}

// Status is the outcome of a transfer. Pending only exists inside the
// posting transaction and is never visible to readers.
type Status string

const (
	StatusPending  Status = "pending"
	StatusPosted   Status = "posted"
	StatusRejected Status = "rejected"
)

// Entry is an append-only posting to one account.
type Entry struct {
	ID         int64     `json:"id"`
	TransferID uuid.UUID `json:"transfer_id"`
	AccountID  int64     `json:"account_id"`
	Direction  Side      `json:"direction"`
	Amount     int64     `json:"amount"`
	CreatedAt  time.Time `json:"created_at"`
}

// Transfer is a recorded transfer attempt. A rejected transfer has an
// ErrorCode and no entries.
type Transfer struct {
	ID                 uuid.UUID  `json:"id"`
	Status             Status     `json:"status"`
	ErrorCode          ErrorCode  `json:"error_code,omitempty"`
	ReversesTransferID *uuid.UUID `json:"reverses_transfer_id,omitempty"`
	Entries            []Entry    `json:"entries"`
	CreatedAt          time.Time  `json:"created_at"`

	// Replayed is true when the transfer was already recorded under the
	// request's idempotency key and this call returned it without posting.
	Replayed bool `json:"-"`
}
