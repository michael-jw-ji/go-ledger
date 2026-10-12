//go:build integration

// Package integration runs the ledger against real Postgres (and later Redis)
// started with Testcontainers.
package integration

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/michael-jw-ji/go-ledger/internal/httpapi"
	"github.com/michael-jw-ji/go-ledger/internal/idempotency"
	"github.com/michael-jw-ji/go-ledger/internal/ledger"
	"github.com/michael-jw-ji/go-ledger/migrations"
)

var (
	pool *pgxpool.Pool
	svc  *ledger.Service
	rdb  *redis.Client
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

	rc, err := tcredis.Run(ctx, "redis:7-alpine")
	if err != nil {
		log.Printf("start redis: %v", err)
		return 1
	}
	defer testcontainers.TerminateContainer(rc) //nolint:errcheck
	redisURL, err := rc.ConnectionString(ctx)
	if err != nil {
		log.Printf("redis url: %v", err)
		return 1
	}
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		log.Printf("parse redis url: %v", err)
		return 1
	}
	rdb = redis.NewClient(opts)
	defer rdb.Close() //nolint:errcheck

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

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newServer serves the full API. With a nil client it runs without the
// Redis layer, relying on the Postgres backstop alone.
func newServer(t *testing.T, client *redis.Client) (*httptest.Server, *idempotency.Middleware) {
	t.Helper()
	var idem *idempotency.Middleware
	if client != nil {
		idem = idempotency.NewMiddleware(idempotency.NewStore(client), quietLog())
	}
	srv := httptest.NewServer(httpapi.New(svc, quietLog(), idem))
	t.Cleanup(srv.Close)
	return srv, idem
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
