// Command ledger runs the ledger HTTP API.
//
//	ledger         serve the API, with the reconciliation, hot-account
//	               rollup and outbox workers in the background
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
	"strings"
	"sync"
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
	"github.com/michael-jw-ji/go-ledger/internal/outbox"
	"github.com/michael-jw-ji/go-ledger/internal/recon"
	"github.com/michael-jw-ji/go-ledger/internal/rollup"
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
	rollupInterval, err := time.ParseDuration(env("ROLLUP_INTERVAL", "100ms"))
	if err != nil {
		return fmt.Errorf("ROLLUP_INTERVAL: %w", err)
	}
	outboxInterval, err := time.ParseDuration(env("OUTBOX_INTERVAL", "200ms"))
	if err != nil {
		return fmt.Errorf("OUTBOX_INTERVAL: %w", err)
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

	var workers sync.WaitGroup
	reconWorker := &recon.Worker{Pool: pool, Interval: reconInterval, Log: log, OnResult: m.OnRecon}
	rollupWorker := &rollup.Worker{Pool: pool, Interval: rollupInterval, Log: log, OnTick: m.OnRollup}
	workers.Go(func() { reconWorker.Run(ctx) })
	workers.Go(func() { rollupWorker.Run(ctx) })
	outboxWorker := &outbox.Worker{
		Publisher: &outbox.Publisher{Pool: pool, Redis: rdb, Stream: env("OUTBOX_STREAM", outbox.DefaultStream)},
		Interval:  outboxInterval,
		Log:       log,
		OnTick:    m.OnOutbox,
	}
	workers.Go(func() { outboxWorker.Run(ctx) })

	srv := &http.Server{
		Addr:              listenAddr(),
		Handler:           withConsole(m.Instrument(mux), os.Getenv("WEB_DIR")),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", srv.Addr,
			"recon_interval", reconInterval.String(), "rollup_interval", rollupInterval.String(),
			"outbox_interval", outboxInterval.String())
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
	workers.Wait()
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

// listenAddr is ADDR, else :$PORT (set by hosts like Render), else :8088.
func listenAddr() string {
	if a := os.Getenv("ADDR"); a != "" {
		return a
	}
	if p := os.Getenv("PORT"); p != "" {
		return ":" + p
	}
	return ":8088"
}

// withConsole serves the built web console from dir and mounts the API
// under /api, the same layout the Vite dev proxy gives the console locally.
// With no dir, the API is served at the root as usual.
func withConsole(api http.Handler, dir string) http.Handler {
	if dir == "" {
		return api
	}
	files := http.FileServer(http.Dir(dir))
	root := http.NewServeMux()
	root.Handle("/api/", http.StripPrefix("/api", api))
	root.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hashed asset names never change, so they can be cached for good;
		// index.html must be revalidated so a deploy shows up.
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	}))
	return root
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
