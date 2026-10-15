package ledger

import "errors"

// Errors returned before anything is recorded. Wrapped errors carry detail;
// match them with errors.Is.
var (
	ErrInvalidRequest      = errors.New("invalid request")
	ErrUnbalanced          = errors.New("debits do not equal credits")
	ErrNotFound            = errors.New("not found")
	ErrIdempotencyMismatch = errors.New("idempotency key was already used with a different request")
	ErrAlreadyReversed     = errors.New("transfer has already been reversed")
	ErrNotReversible       = errors.New("transfer cannot be reversed")
	// ErrSandboxLimit: the sandbox already holds as many accounts or
	// transfers as one visitor may create. Nothing is recorded, so the key
	// stays free.
	ErrSandboxLimit = errors.New("sandbox limit reached")
)

// ErrorCode explains why a transfer was rejected. Rejections are recorded
// under the idempotency key, so a retry gets the same answer.
type ErrorCode string

const (
	CodeInsufficientFunds ErrorCode = "insufficient_funds"
	CodeCurrencyMismatch  ErrorCode = "currency_mismatch"
	CodeAccountNotFound   ErrorCode = "account_not_found"
)

// Message is a human-readable description of the code.
func (c ErrorCode) Message() string {
	switch c {
	case CodeInsufficientFunds:
		return "an account would go below zero and does not allow negative balances"
	case CodeCurrencyMismatch:
		return "all accounts in a transfer must share one currency"
	case CodeAccountNotFound:
		return "one or more accounts do not exist"
	}
	return string(c)
}
