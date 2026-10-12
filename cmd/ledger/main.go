// Command ledger runs the ledger HTTP API.
//
//	ledger         serve the API (and run reconciliation in the background)
//	ledger recon   reconcile once, print the result, exit 1 on mismatches
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"

	"github.com/michael-jw-ji/go-ledger/internal/httpapi"
	"github.com/michael-jw-ji/go-ledger/internal/idempotency"
	"github.com/michael-jw-ji/go-ledger/internal/ledger"
	"github.com/michael-jw-ji/go-ledger/internal/metrics"
	"github.com/michael-jw-ji/go-ledger/internal/recon"
	"github.com/michael-jw-ji/go-ledger/migrations"
)

// errMismatches makes `ledger recon` exit non-zero without logging a fatal.
var errMismatches = errors.New("reconciliation found mismatches")

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	var err error
	if len(os.Args) > 1 && os.Args[1] == "recon" {
		err = runRecon(log)
	} else {
		err = serve(log)
	}
	if errors.Is(err, errMismatches) {
		os.Exit(1)
	}
	if err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func openDB(ctx context.Context) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, env("DATABASE_URL", "postgres://ledger:ledger@localhost:5433/ledger?sslmode=disable"))
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	db := stdlib.OpenDBFromPool(pool)
	err = migrations.Up(ctx, db)
	_ = db.Close()
	if err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

func runRecon(log *slog.Logger) error {
	ctx := context.Background()
	pool, err := openDB(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	res, err := recon.Run(ctx, pool)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(res); err != nil {
		return err
	}
	if res.Skipped {
		log.Warn("another instance is reconciling, nothing checked")
	}
	if len(res.Mismatches) > 0 {
		return errMismatches
	}
	return nil
}

func serve(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := openDB(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()

	reconInterval, err := time.ParseDuration(env("RECON_INTERVAL", "60s"))
	if err != nil {
		return fmt.Errorf("RECON_INTERVAL: %w", err)
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

	m := metrics.New(pool)
	idem := idempotency.NewMiddleware(idempotency.NewStore(rdb), log)
	idem.OnLookup = m.OnIdempotencyLookup
	svc := ledger.NewService(pool).WithHooks(m.LedgerHooks())

	mux := httpapi.New(svc, log, idem)
	mux.Handle("GET /metrics", m.Handler())
	mux.HandleFunc("GET /reconciliation", latestRecon(pool, log))

	worker := &recon.Worker{Pool: pool, Interval: reconInterval, Log: log, OnResult: m.OnRecon}
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		worker.Run(ctx)
	}()

	srv := &http.Server{
		Addr:              env("ADDR", ":8088"),
		Handler:           m.Instrument(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", srv.Addr, "recon_interval", reconInterval.String())
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
	<-workerDone
	return nil
}

// latestRecon serves the most recent reconciliation run, or 404 before the
// first one finishes.
func latestRecon(pool *pgxpool.Pool, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		res, err := recon.Latest(r.Context(), pool)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"no reconciliation run yet"}}` + "\n"))
		case err != nil:
			log.ErrorContext(r.Context(), "load reconciliation", "err", err)
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"code":"internal","message":"internal error"}}` + "\n"))
		default:
			_ = json.NewEncoder(w).Encode(res)
		}
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
