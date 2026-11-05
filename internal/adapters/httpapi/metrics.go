package httpapi

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/dwijamahardika/recond/internal/app"
	"github.com/dwijamahardika/recond/internal/domain"
)

// Metrics are RED signals for the API plus reconciliation counters. They
// register against an injected registry so tests never collide on the
// global one. Metrics also satisfies app.Metrics.
type Metrics struct {
	requests   *prometheus.CounterVec
	latency    *prometheus.HistogramVec
	webhooks   *prometheus.CounterVec
	ingested   *prometheus.CounterVec
	runs       prometheus.Counter
	runSeconds prometheus.Histogram
	outcomes   *prometheus.GaugeVec
	open       prometheus.Gauge

	// Windowed runs (docs/lifecycle/windowed-runs/06-runbook.md).
	windowedRuns   prometheus.Counter
	windowDays     prometheus.Gauge
	windowRecords  *prometheus.GaugeVec
	windowNotOwned prometheus.Gauge
	outsideTotal   *prometheus.CounterVec
	lastOutside    prometheus.Gauge
}

// NewMetrics creates and registers the metrics.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "recond", Subsystem: "http", Name: "requests_total"},
			[]string{"route", "method", "status"}),
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: "recond", Subsystem: "http", Name: "request_duration_seconds",
			Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 5, 30}}, []string{"route", "method"}),
		webhooks: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "recond", Name: "webhook_events_total",
			Help: "Webhook deliveries by result: applied, duplicate, out_of_order, rejected_transition, malformed, bad_signature, stale_timestamp."},
			[]string{"provider", "result"}),
		ingested: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "recond", Name: "ingested_rows_total"}, []string{"kind"}),
		runs:     prometheus.NewCounter(prometheus.CounterOpts{Namespace: "recond", Name: "runs_total"}),
		runSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{Namespace: "recond", Name: "run_duration_seconds",
			Buckets: prometheus.ExponentialBuckets(0.05, 2, 14)}),
		outcomes: prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: "recond", Name: "last_run_outcomes",
			Help: "Outcomes of the most recent run, by class."}, []string{"class"}),
		open: prometheus.NewGauge(prometheus.GaugeOpts{Namespace: "recond", Name: "exceptions_open"}),

		windowedRuns: prometheus.NewCounter(prometheus.CounterOpts{Namespace: "recond", Name: "windowed_runs_total",
			Help: "Windowed runs completed."}),
		windowDays: prometheus.NewGauge(prometheus.GaugeOpts{Namespace: "recond", Name: "last_run_window_days",
			Help: "Business days in the most recent run's window; 0 for a full-history run."}),
		windowRecords: prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: "recond", Name: "last_run_window_records",
			Help: "Records the most recent windowed run read, by record (ledger|line) and scope: window (dated inside it) or margin (read only to find counterparts, not reported)."},
			[]string{"record", "scope"}),
		windowNotOwned: prometheus.NewGauge(prometheus.GaugeOpts{Namespace: "recond", Name: "last_run_window_groups_not_owned",
			Help: "Groups the most recent windowed run computed for boundary records but left to the neighbouring window that owns them."}),
		outsideTotal: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: "recond", Name: "outside_window_total",
			Help: "Groups deferred as outside_window: a counterpart lies beyond the run's margins. Stays 0 with adequate margins."},
			[]string{"provider"}),
		lastOutside: prometheus.NewGauge(prometheus.GaugeOpts{Namespace: "recond", Name: "last_run_outside_window",
			Help: "outside_window groups in the most recent run."}),
	}
	reg.MustRegister(m.requests, m.latency, m.webhooks, m.ingested, m.runs, m.runSeconds, m.outcomes, m.open,
		m.windowedRuns, m.windowDays, m.windowRecords, m.windowNotOwned, m.outsideTotal, m.lastOutside)
	return m
}

// Observe records one HTTP request.
func (m *Metrics) Observe(route, method string, status int, d time.Duration) {
	m.requests.WithLabelValues(route, method, strconv.Itoa(status)).Inc()
	m.latency.WithLabelValues(route, method).Observe(d.Seconds())
}

// Webhook counts one webhook delivery by result.
func (m *Metrics) Webhook(provider, result string) {
	m.webhooks.WithLabelValues(provider, result).Inc()
}

// Ingested counts rows inserted by file ingestion.
func (m *Metrics) Ingested(kind string, rows int) {
	m.ingested.WithLabelValues(kind).Add(float64(rows))
}

// ExceptionsOpen sets the open exception gauge.
func (m *Metrics) ExceptionsOpen(n int) { m.open.Set(float64(n)) }

// RunWindow records how a window shaped the most recent run (nil: a
// full-history run, which resets the last-run window gauges).
func (m *Metrics) RunWindow(w *app.WindowSummary, outside map[string]int) {
	if w == nil {
		m.windowDays.Set(0)
		m.windowNotOwned.Set(0)
		m.lastOutside.Set(0)
		m.windowRecords.Reset()
		return
	}
	m.windowedRuns.Inc()
	m.windowDays.Set(float64(w.Days))
	m.windowRecords.WithLabelValues("ledger", "window").Set(float64(w.LedgerInWindow))
	m.windowRecords.WithLabelValues("ledger", "margin").Set(float64(w.LedgerInMargin))
	m.windowRecords.WithLabelValues("line", "window").Set(float64(w.LinesInWindow))
	m.windowRecords.WithLabelValues("line", "margin").Set(float64(w.LinesInMargin))
	m.windowNotOwned.Set(float64(w.GroupsNotOwned))
	m.lastOutside.Set(float64(w.OutsideWindow))
	for p, n := range outside {
		m.outsideTotal.WithLabelValues(p).Add(float64(n))
	}
}

// RunFinished records a completed run and its outcome counts.
func (m *Metrics) RunFinished(d time.Duration, counts map[domain.Class]int) {
	m.runs.Inc()
	m.runSeconds.Observe(d.Seconds())
	for _, c := range domain.Classes {
		m.outcomes.WithLabelValues(string(c)).Set(float64(counts[c]))
	}
}
