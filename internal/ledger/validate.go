package ledger

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
)

const (
	// MaxLegs and MaxAmount bound a transfer so that sums can't overflow int64.
	MaxLegs   = 64
	MaxAmount = 1_000_000_000_000_000 // 10 trillion in major units

	maxKeyLen  = 255
	maxNameLen = 200
)

var currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidRequest, fmt.Sprintf(format, args...))
}

// Validate checks everything that can be checked without the database.
func (a NewAccount) Validate() error {
	if a.Name == "" || len(a.Name) > maxNameLen {
		return invalid("name must be 1 to %d characters", maxNameLen)
	}
	if !currencyRe.MatchString(a.Currency) {
		return invalid("currency must be a 3-letter uppercase ISO code")
	}
	if !a.NormalBalance.valid() {
		return invalid("normal_balance must be %q or %q", Debit, Credit)
	}
	return nil
}

// Validate checks everything that can be checked without the database,
// including that debits equal credits.
func (r TransferRequest) Validate() error {
	if r.IdempotencyKey == "" || len(r.IdempotencyKey) > maxKeyLen {
		return invalid("idempotency key must be 1 to %d characters", maxKeyLen)
	}
	return validateLegs(r.Legs)
}

func validateLegs(legs []Leg) error {
	if len(legs) < 2 || len(legs) > MaxLegs {
		return invalid("a transfer needs 2 to %d entries", MaxLegs)
	}
	var debits, credits int64
	accounts := make(map[int64]struct{}, len(legs))
	for i, l := range legs {
		if l.AccountID <= 0 {
			return invalid("entries[%d]: account_id is required", i)
		}
		if !l.Direction.valid() {
			return invalid("entries[%d]: direction must be %q or %q", i, Debit, Credit)
		}
		if l.Amount <= 0 || l.Amount > MaxAmount {
			return invalid("entries[%d]: amount must be between 1 and %d minor units", i, int64(MaxAmount))
		}
		if l.Direction == Debit {
			debits += l.Amount
		} else {
			credits += l.Amount
		}
		accounts[l.AccountID] = struct{}{}
	}
	if len(accounts) < 2 {
		return invalid("a transfer must touch at least 2 accounts")
	}
	if debits != credits {
		return fmt.Errorf("%w: debits %d, credits %d", ErrUnbalanced, debits, credits)
	}
	return nil
}

// RequestHash fingerprints the legs so that reusing an idempotency key with a
// different request can be detected.
func RequestHash(legs []Leg) []byte {
	b, err := json.Marshal(legs)
	if err != nil {
		panic(err) // Leg contains only ints and strings
	}
	sum := sha256.Sum256(b)
	return sum[:]
}
