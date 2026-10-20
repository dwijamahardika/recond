// Package httpapi is recond's HTTP adapter, on net/http alone (Go 1.22
// method/path patterns): signed provider webhooks, an API-key-protected
// management API for runs and the exception queue, and operational endpoints.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/dwijamahardika/recond/internal/app"
	"github.com/dwijamahardika/recond/internal/platform/clock"
)

// Deps are the handler's collaborators and settings.
type Deps struct {
	Service          *app.Service
	Metrics          *Metrics
	Registry         prometheus.Gatherer
	Logger           *slog.Logger
	Clock            clock.Clock
	APIKeys          map[string]string // key -> principal
	WebhookSecrets   map[string][]byte // provider -> secret
	WebhookTolerance time.Duration
	Ready            func(ctx context.Context) error
}

type handlers struct {
	Deps
	runMu sync.Mutex // one run at a time per process
}

// NewHandler builds the public listener's handler.
func NewHandler(d Deps) http.Handler {
	if d.Clock == nil {
		d.Clock = clock.System{}
	}
	h := &handlers{Deps: d}
	mux := http.NewServeMux()

	// ---- operational: no auth ----
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := d.Ready(ctx); err != nil {
			writeProblem(w, r, http.StatusServiceUnavailable, "Not ready", "storage unavailable")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(d.Registry, promhttp.HandlerOpts{}))

	// ---- provider webhooks: HMAC per provider ----
	mux.Handle("POST /v1/webhooks/{provider}", chain(http.HandlerFunc(h.webhook), maxBody(64<<10)))

	// ---- management API: API key ----
	api := func(fn http.HandlerFunc) http.Handler { return chain(fn, apiKeyAuth(d.APIKeys), maxBody(1<<20)) }
	mux.Handle("GET /v1/payments/{provider}/{ref}", api(h.payment))
	mux.Handle("POST /v1/runs", api(h.createRun))
	mux.Handle("GET /v1/runs", api(h.listRuns))
	mux.Handle("GET /v1/runs/{id}", api(h.getRun))
	mux.Handle("GET /v1/runs/{id}/items.csv", api(h.runItems))
	mux.Handle("POST /v1/runs/{id}/verify", api(h.verifyRun))
	mux.Handle("GET /v1/exceptions", api(h.listExceptions))
	mux.Handle("GET /v1/exceptions/{id}", api(h.getException))
	mux.Handle("POST /v1/exceptions/{id}/resolve", api(h.resolve))
	mux.Handle("POST /v1/exceptions/{id}/reopen", api(h.reopen))

	return chain(mux, recoverer, requestID, logging(d.Logger, d.Metrics))
}

// NewAdminHandler serves pprof and metrics on a separate listener, so
// profiling is never reachable through the public port.
func NewAdminHandler(reg prometheus.Gatherer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	return mux
}

// NewServer applies the timeouts every server needs; without them a slow
// client holds a connection forever. Long runs extend their own deadline.
func NewServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}
}
