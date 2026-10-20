package httpapi

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

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
	}
	reg.MustRegister(m.requests, m.latency, m.webhooks, m.ingested, m.runs, m.runSeconds, m.outcomes, m.open)
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

// RunFinished records a completed run and its outcome counts.
func (m *Metrics) RunFinished(d time.Duration, counts map[domain.Class]int) {
	m.runs.Inc()
	m.runSeconds.Observe(d.Seconds())
	for _, c := range domain.Classes {
		m.outcomes.WithLabelValues(string(c)).Set(float64(counts[c]))
	}
}
