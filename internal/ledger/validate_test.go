package ledger

import (
	"bytes"
	"errors"
	"testing"
)

func TestTransferRequestValidate(t *testing.T) {
	ok := []Leg{
		{AccountID: 1, Direction: Debit, Amount: 100},
		{AccountID: 2, Direction: Credit, Amount: 100},
	}
	tests := []struct {
		name string
		req  TransferRequest
		want error
	}{
		{"balanced two-leg", TransferRequest{IdempotencyKey: "k", Legs: ok}, nil},
		{"balanced multi-leg", TransferRequest{IdempotencyKey: "k", Legs: []Leg{
			{AccountID: 1, Direction: Debit, Amount: 100},
			{AccountID: 2, Direction: Credit, Amount: 60},
			{AccountID: 3, Direction: Credit, Amount: 40},
		}}, nil},
		{"unbalanced", TransferRequest{IdempotencyKey: "k", Legs: []Leg{
			{AccountID: 1, Direction: Debit, Amount: 100},
			{AccountID: 2, Direction: Credit, Amount: 99},
		}}, ErrUnbalanced},
		{"all debits", TransferRequest{IdempotencyKey: "k", Legs: []Leg{
			{AccountID: 1, Direction: Debit, Amount: 100},
			{AccountID: 2, Direction: Debit, Amount: 100},
		}}, ErrUnbalanced},
		{"missing key", TransferRequest{IdempotencyKey: "", Legs: ok}, ErrInvalidRequest},
		{"single leg", TransferRequest{IdempotencyKey: "k", Legs: ok[:1]}, ErrInvalidRequest},
		{"zero amount", TransferRequest{IdempotencyKey: "k", Legs: []Leg{
			{AccountID: 1, Direction: Debit, Amount: 0},
			{AccountID: 2, Direction: Credit, Amount: 0},
		}}, ErrInvalidRequest},
		{"negative amount", TransferRequest{IdempotencyKey: "k", Legs: []Leg{
			{AccountID: 1, Direction: Debit, Amount: -5},
			{AccountID: 2, Direction: Credit, Amount: -5},
		}}, ErrInvalidRequest},
		{"amount over max", TransferRequest{IdempotencyKey: "k", Legs: []Leg{
			{AccountID: 1, Direction: Debit, Amount: MaxAmount + 1},
			{AccountID: 2, Direction: Credit, Amount: MaxAmount + 1},
		}}, ErrInvalidRequest},
		{"bad direction", TransferRequest{IdempotencyKey: "k", Legs: []Leg{
			{AccountID: 1, Direction: "sideways", Amount: 100},
			{AccountID: 2, Direction: Credit, Amount: 100},
		}}, ErrInvalidRequest},
		{"same account both sides", TransferRequest{IdempotencyKey: "k", Legs: []Leg{
			{AccountID: 1, Direction: Debit, Amount: 100},
			{AccountID: 1, Direction: Credit, Amount: 100},
		}}, ErrInvalidRequest},
		{"missing account", TransferRequest{IdempotencyKey: "k", Legs: []Leg{
			{Direction: Debit, Amount: 100},
			{AccountID: 2, Direction: Credit, Amount: 100},
		}}, ErrInvalidRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate()
			if tt.want == nil && err != nil {
				t.Fatalf("want ok, got %v", err)
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("want %v, got %v", tt.want, err)
			}
		})
	}
}

func TestNewAccountValidate(t *testing.T) {
	good := NewAccount{Name: "alice", Currency: "USD", NormalBalance: Credit}
	if err := good.Validate(); err != nil {
		t.Fatalf("want ok, got %v", err)
	}
	for _, bad := range []NewAccount{
		{Name: "", Currency: "USD", NormalBalance: Credit},
		{Name: "a", Currency: "usd", NormalBalance: Credit},
		{Name: "a", Currency: "US", NormalBalance: Credit},
		{Name: "a", Currency: "USD", NormalBalance: "asset"},
	} {
		if err := bad.Validate(); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%+v: want ErrInvalidRequest, got %v", bad, err)
		}
	}
}

func TestLegDelta(t *testing.T) {
	l := Leg{AccountID: 1, Direction: Debit, Amount: 100}
	if got := l.delta(Debit); got != 100 {
		t.Errorf("debit on debit-normal: got %d, want 100", got)
	}
	if got := l.delta(Credit); got != -100 {
		t.Errorf("debit on credit-normal: got %d, want -100", got)
	}
}

func TestRequestHash(t *testing.T) {
	a := []Leg{{AccountID: 1, Direction: Debit, Amount: 100}, {AccountID: 2, Direction: Credit, Amount: 100}}
	b := []Leg{{AccountID: 1, Direction: Debit, Amount: 101}, {AccountID: 2, Direction: Credit, Amount: 101}}
	if !bytes.Equal(RequestHash(a), RequestHash(a)) {
		t.Error("hash is not deterministic")
	}
	if bytes.Equal(RequestHash(a), RequestHash(b)) {
		t.Error("different requests hash the same")
	}
}
