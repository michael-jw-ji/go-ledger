// Command ledger runs the ledger HTTP API.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"

	"github.com/michael-jw-ji/go-ledger/internal/httpapi"
	"github.com/michael-jw-ji/go-ledger/internal/idempotency"
	"github.com/michael-jw-ji/go-ledger/internal/ledger"
	"github.com/michael-jw-ji/go-ledger/migrations"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, env("DATABASE_URL", "postgres://ledger:ledger@localhost:5433/ledger?sslmode=disable"))
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return err
	}

	db := stdlib.OpenDBFromPool(pool)
	err = migrations.Up(ctx, db)
	_ = db.Close()
	if err != nil {
		return err
	}

	redisOpts, err := redis.ParseURL(env("REDIS_URL", "redis://localhost:6379/0"))
	if err != nil {
		return err
	}
	rdb := redis.NewClient(redisOpts)
	defer rdb.Close() //nolint:errcheck
	// Redis is an optimization, so the server starts without it.
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Warn("redis unavailable, idempotency falls back to postgres", "err", err)
	}
	idem := idempotency.NewMiddleware(idempotency.NewStore(rdb), log)

	srv := &http.Server{
		Addr:              env("ADDR", ":8088"),
		Handler:           httpapi.New(ledger.NewService(pool), log, idem),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", srv.Addr)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
