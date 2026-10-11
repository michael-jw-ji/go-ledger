//go:build integration

// Package integration runs the ledger against real Postgres (and later Redis)
// started with Testcontainers.
package integration

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/michael-jw-ji/go-ledger/internal/ledger"
	"github.com/michael-jw-ji/go-ledger/migrations"
)

var (
	pool *pgxpool.Pool
	svc  *ledger.Service
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	ctx := context.Background()
	pg, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("ledger"),
		postgres.WithUsername("ledger"),
		postgres.WithPassword("ledger"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		log.Printf("start postgres: %v", err)
		return 1
	}
	defer testcontainers.TerminateContainer(pg) //nolint:errcheck

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Printf("dsn: %v", err)
		return 1
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		log.Printf("parse dsn: %v", err)
		return 1
	}
	cfg.MaxConns = 32
	pool, err = pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		log.Printf("pool: %v", err)
		return 1
	}
	defer pool.Close()

	db := stdlib.OpenDBFromPool(pool)
	err = migrations.Up(ctx, db)
	_ = db.Close()
	if err != nil {
		log.Printf("migrate: %v", err)
		return 1
	}

	svc = ledger.NewService(pool)
	return m.Run()
}

var seq atomic.Int64

// uniq returns a string unique within the test run, for names and keys.
func uniq(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, seq.Add(1))
}

func newAccount(t *testing.T, allowNegative bool) ledger.Account {
	t.Helper()
	return newAccountIn(t, "USD", allowNegative)
}

func newAccountIn(t *testing.T, currency string, allowNegative bool) ledger.Account {
	t.Helper()
	a, err := svc.CreateAccount(context.Background(), ledger.NewAccount{
		Name:          uniq(t.Name()),
		Currency:      currency,
		NormalBalance: ledger.Credit,
		AllowNegative: allowNegative,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	return a
}

func move(from, to int64, amount int64) []ledger.Leg {
	return []ledger.Leg{
		{AccountID: from, Direction: ledger.Debit, Amount: amount},
		{AccountID: to, Direction: ledger.Credit, Amount: amount},
	}
}

func transfer(t *testing.T, key string, legs []ledger.Leg) ledger.Transfer {
	t.Helper()
	tr, err := svc.CreateTransfer(context.Background(), ledger.TransferRequest{IdempotencyKey: key, Legs: legs})
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}
	return tr
}

func balance(t *testing.T, id int64) int64 {
	t.Helper()
	a, err := svc.GetAccount(context.Background(), id)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	return a.Balance
}

func wantBalance(t *testing.T, id, want int64) {
	t.Helper()
	if got := balance(t, id); got != want {
		t.Errorf("account %d balance = %d, want %d", id, got, want)
	}
}
