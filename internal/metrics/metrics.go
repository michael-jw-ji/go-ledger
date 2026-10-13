// Package metrics holds the Prometheus collectors and adapts them to the
// hooks exposed by the ledger, idempotency and recon packages.
package metrics

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/michael-jw-ji/go-ledger/internal/idempotency"
	"github.com/michael-jw-ji/go-ledger/internal/ledger"
	"github.com/michael-jw-ji/go-ledger/internal/recon"
)

// Buckets from 0.5ms to ~4s: transfers are single-digit milliseconds when
// healthy and lock waits on a hot account climb into the hundreds.
var latencyBuckets = prometheus.ExponentialBuckets(0.0005, 2, 14)

type Metrics struct {
	reg *prometheus.Registry

	httpDuration      *prometheus.HistogramVec
	transfers         *prometheus.CounterVec
	txDuration        *prometheus.HistogramVec
	lockWait          prometheus.Histogram
	idemLookups       *prometheus.CounterVec
	reconMismatches   *prometheus.GaugeVec
	reconLastSuccess  prometheus.Gauge
	reconDuration     prometheus.Histogram
	reconFailures     prometheus.Counter
	reconAccountsSeen prometheus.Gauge
	rollupDuration    prometheus.Histogram
	rollupEntries     prometheus.Counter
	rollupFailures    prometheus.Counter
}

func New(pool *pgxpool.Pool) *Metrics {
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request latency by route pattern, method and status code.",
			Buckets: latencyBuckets,
		}, []string{"route", "method", "code"}),
		transfers: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ledger_transfers_total",
			Help: "Transfer attempts that reached Postgres, by outcome (posted, rejected, replayed, conflict, error).",
		}, []string{"status"}),
		txDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "ledger_tx_duration_seconds",
			Help:    "Duration of the transfer transaction, by outcome.",
			Buckets: latencyBuckets,
		}, []string{"status"}),
		lockWait: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "ledger_lock_wait_seconds",
			Help:    "Time spent acquiring a transfer's account row locks (SELECT ... FOR NO KEY UPDATE).",
			Buckets: latencyBuckets,
		}),
		idemLookups: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "idempotency_lookups_total",
			Help: "Redis idempotency lookups by result (miss, hit, in_progress, mismatch, redis_error).",
		}, []string{"result"}),
		reconMismatches: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ledger_reconciliation_mismatches",
			Help: "Findings in the last reconciliation run, by kind. Anything above 0 needs investigating.",
		}, []string{"kind"}),
		reconLastSuccess: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ledger_reconciliation_last_success_timestamp",
			Help: "Unix time of the last reconciliation run that completed.",
		}),
		reconDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "ledger_reconciliation_duration_seconds",
			Help:    "Duration of reconciliation runs.",
			Buckets: prometheus.ExponentialBuckets(0.005, 2, 14),
		}),
		reconFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ledger_reconciliation_failures_total",
			Help: "Reconciliation runs that errored before finishing.",
		}),
		reconAccountsSeen: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ledger_reconciliation_accounts_checked",
			Help: "Accounts checked by the last reconciliation run.",
		}),
	}
	m.rollupDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "ledger_rollup_duration_seconds",
		Help:    "Duration of one hot-account rollup tick.",
		Buckets: latencyBuckets,
	})
	m.rollupEntries = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "ledger_rollup_entries_total",
		Help: "Pending hot-account entries folded into cached balances by the rollup worker.",
	})
	m.rollupFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "ledger_rollup_failures_total",
		Help: "Rollup ticks that errored.",
	})
	m.reg.MustRegister(m.rollupDuration, m.rollupEntries, m.rollupFailures)
	m.reg.MustRegister(
		m.httpDuration, m.transfers, m.txDuration, m.lockWait, m.idemLookups,
		m.reconMismatches, m.reconLastSuccess, m.reconDuration, m.reconFailures, m.reconAccountsSeen,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	if pool != nil {
		m.reg.MustRegister(newPoolCollector(pool))
		// Counted at scrape time; the table holds at most ~one rollup
		// interval of hot entries, so this stays cheap.
		m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "ledger_hot_pending_entries",
			Help: "Hot-account entries committed but not yet in the cached balance.",
		}, func() float64 {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var n int64
			if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM hot_pending`).Scan(&n); err != nil {
				return -1
			}
			return float64(n)
		}))
	}
	// Pre-create the series so dashboards show 0 instead of "no data".
	for _, s := range []string{"posted", "rejected", "replayed", "conflict", "error"} {
		m.transfers.WithLabelValues(s)
	}
	for _, r := range []string{idempotency.ResultMiss, idempotency.ResultHit, idempotency.ResultInProgress,
		idempotency.ResultMismatch, idempotency.ResultRedisError} {
		m.idemLookups.WithLabelValues(r)
	}
	return m
}

// Handler serves the registry in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{Registry: m.reg})
}

// LedgerHooks reports transfer outcomes and lock waits.
func (m *Metrics) LedgerHooks() ledger.Hooks {
	return ledger.Hooks{
		OnTransfer: func(outcome string, took time.Duration) {
			m.transfers.WithLabelValues(outcome).Inc()
			m.txDuration.WithLabelValues(outcome).Observe(took.Seconds())
		},
		OnLockWait: func(took time.Duration) { m.lockWait.Observe(took.Seconds()) },
	}
}

// OnIdempotencyLookup is an idempotency.Middleware.OnLookup hook.
func (m *Metrics) OnIdempotencyLookup(result string) {
	m.idemLookups.WithLabelValues(result).Inc()
}

// OnRecon is a recon.Worker.OnResult hook.
func (m *Metrics) OnRecon(res recon.Result, took time.Duration, err error) {
	if err != nil {
		m.reconFailures.Inc()
		return
	}
	if res.Skipped {
		return
	}
	for _, k := range recon.Kinds {
		m.reconMismatches.WithLabelValues(k).Set(float64(res.Count(k)))
	}
	m.reconAccountsSeen.Set(float64(res.AccountsChecked))
	m.reconDuration.Observe(took.Seconds())
	m.reconLastSuccess.Set(float64(res.FinishedAt.Unix()))
}

// OnRollup is a rollup.Worker.OnTick hook.
func (m *Metrics) OnRollup(applied int64, took time.Duration, err error) {
	if err != nil {
		m.rollupFailures.Inc()
		return
	}
	m.rollupEntries.Add(float64(applied))
	m.rollupDuration.Observe(took.Seconds())
}

// Router is the subset of *http.ServeMux used to resolve a request's route.
type Router interface {
	http.Handler
	Handler(r *http.Request) (h http.Handler, pattern string)
}

// Instrument records http_request_duration_seconds for every request. The
// route label is the mux pattern (e.g. "/accounts/{id}"), never the raw
// path, so label cardinality stays bounded.
func (m *Metrics) Instrument(mux Router) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pattern := mux.Handler(r)
		route := "unmatched"
		if pattern != "" {
			// Patterns carry their method ("POST /transfers"); it has its own label.
			_, route, _ = strings.Cut(pattern, " ")
			if route == "" {
				route = pattern
			}
		}
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		mux.ServeHTTP(sw, r)
		m.httpDuration.WithLabelValues(route, r.Method, strconv.Itoa(sw.status)).
			Observe(time.Since(start).Seconds())
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// poolCollector exports pgxpool.Stat() at scrape time.
type poolCollector struct {
	pool                                             *pgxpool.Pool
	acquired, idle, total, max, constructing         *prometheus.Desc
	acquireCount, emptyAcquire, acquireDur, canceled *prometheus.Desc
}

func newPoolCollector(pool *pgxpool.Pool) *poolCollector {
	d := func(name, help string) *prometheus.Desc {
		return prometheus.NewDesc("pgxpool_"+name, help, nil, nil)
	}
	return &poolCollector{
		pool:         pool,
		acquired:     d("acquired_conns", "Connections currently checked out."),
		idle:         d("idle_conns", "Idle connections in the pool."),
		total:        d("total_conns", "Total connections in the pool."),
		max:          d("max_conns", "Maximum pool size."),
		constructing: d("constructing_conns", "Connections being established."),
		acquireCount: d("acquire_total", "Successful acquires from the pool."),
		emptyAcquire: d("empty_acquire_total", "Acquires that had to wait because the pool was empty."),
		acquireDur:   d("acquire_duration_seconds_total", "Total time spent waiting to acquire connections."),
		canceled:     d("canceled_acquire_total", "Acquires canceled by their context."),
	}
}

func (c *poolCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.acquired, c.idle, c.total, c.max, c.constructing,
		c.acquireCount, c.emptyAcquire, c.acquireDur, c.canceled} {
		ch <- d
	}
}

func (c *poolCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.pool.Stat()
	g := func(d *prometheus.Desc, v float64) { ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v) }
	ct := func(d *prometheus.Desc, v float64) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, v)
	}
	g(c.acquired, float64(s.AcquiredConns()))
	g(c.idle, float64(s.IdleConns()))
	g(c.total, float64(s.TotalConns()))
	g(c.max, float64(s.MaxConns()))
	g(c.constructing, float64(s.ConstructingConns()))
	ct(c.acquireCount, float64(s.AcquireCount()))
	ct(c.emptyAcquire, float64(s.EmptyAcquireCount()))
	ct(c.acquireDur, s.AcquireDuration().Seconds())
	ct(c.canceled, float64(s.CanceledAcquireCount()))
}
