package idempotency

import (
	"bytes"
	"context"
	"testing"
)

func TestFingerprintIgnoresFormatting(t *testing.T) {
	a := Fingerprint([]byte(`{"debit_account_id":1,"credit_account_id":2,"amount":100}`))
	same := [][]byte{
		[]byte(`{ "amount": 100, "credit_account_id": 2, "debit_account_id": 1 }`),
		[]byte("{\n  \"debit_account_id\": 1,\n  \"credit_account_id\": 2,\n  \"amount\": 100\n}\n"),
		[]byte(`{"debit_account_id":1,"credit_account_id":2,"amount":100}`),
	}
	for _, b := range same {
		if !bytes.Equal(a, Fingerprint(b)) {
			t.Errorf("%s: fingerprint differs from the compact form", b)
		}
	}
}

func TestFingerprintSeesRealDifferences(t *testing.T) {
	base := Fingerprint([]byte(`{"debit_account_id":1,"credit_account_id":2,"amount":100}`))
	for _, b := range []string{
		`{"debit_account_id":1,"credit_account_id":2,"amount":101}`,
		`{"debit_account_id":2,"credit_account_id":1,"amount":100}`,
		`{"debit_account_id":1,"credit_account_id":2,"amount":100,"extra":true}`,
		// Leg order is meaningful, so arrays are not reordered.
		`{"entries":[{"account_id":1},{"account_id":2}]}`,
	} {
		if bytes.Equal(base, Fingerprint([]byte(b))) {
			t.Errorf("%s: same fingerprint as a different request", b)
		}
	}
	x := Fingerprint([]byte(`{"entries":[{"account_id":1},{"account_id":2}]}`))
	y := Fingerprint([]byte(`{"entries":[{"account_id":2},{"account_id":1}]}`))
	if bytes.Equal(x, y) {
		t.Error("reordered legs got the same fingerprint")
	}
}

func TestFingerprintNonJSON(t *testing.T) {
	// Not JSON (or trailing garbage): hashed as raw bytes, so still stable.
	for _, b := range []string{"", "not json", `{"a":1} {"b":2}`, `{"a":1`} {
		if !bytes.Equal(Fingerprint([]byte(b)), Fingerprint([]byte(b))) {
			t.Errorf("%q: unstable fingerprint", b)
		}
	}
	if bytes.Equal(Fingerprint([]byte(`{"a":1} {"b":2}`)), Fingerprint([]byte(`{"a":1}`))) {
		t.Error("trailing data was ignored")
	}
}

func TestMarkRecordedWithoutClaimIsNoop(t *testing.T) {
	MarkRecorded(context.Background()) // must not panic
}
