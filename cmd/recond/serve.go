package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"golang.org/x/sync/errgroup"

	"github.com/dwijamahardika/recond/internal/adapters/httpapi"
	"github.com/dwijamahardika/recond/internal/adapters/sqlite"
	"github.com/dwijamahardika/recond/internal/app"
	"github.com/dwijamahardika/recond/internal/config"
	"github.com/dwijamahardika/recond/internal/platform/clock"
)

func cmdServe(ctx context.Context, args []string) error {
	var c common
	fs := newFlags("serve", &c, false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if isSet(fs, "db") {
		cfg.DBPath = c.db
	}
	log := newLogger(cfg.LogLevel)

	store, err := sqlite.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer closeStore(store)

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	metrics := httpapi.NewMetrics(reg)
	svc := app.NewService(store, cfg.Rules, clock.System{}, log, metrics)
	if counts, err := store.ExceptionCounts(ctx); err == nil {
		metrics.ExceptionsOpen(counts[sqlite.StatusOpen])
	}

	if len(cfg.APIKeys) == 0 {
		log.Warn("RECOND_API_KEYS not set: the /v1 management API will refuse every request")
	}
	for _, p := range cfg.Rules.Names() {
		if _, ok := cfg.WebhookSecrets[p]; !ok {
			log.Warn("no webhook secret configured; webhooks for this provider are refused", "provider", p)
		}
	}
	for _, addr := range []string{cfg.Addr, cfg.AdminAddr} {
		if addr != "" && !loopback(addr) {
			log.Warn("listening beyond loopback; put TLS and a firewall in front", "addr", addr)
		}
	}

	handler := httpapi.NewHandler(httpapi.Deps{
		Service: svc, Metrics: metrics, Registry: reg, Logger: log, Clock: clock.System{},
		APIKeys: cfg.APIKeys, WebhookSecrets: cfg.WebhookSecrets, WebhookTolerance: cfg.WebhookTolerance,
		Ready: store.Ping,
	})
	servers := []*http.Server{httpapi.NewServer(cfg.Addr, handler)}
	if cfg.AdminAddr != "" {
		servers = append(servers, httpapi.NewServer(cfg.AdminAddr, httpapi.NewAdminHandler(reg)))
	}

	g, gctx := errgroup.WithContext(ctx)
	for _, srv := range servers {
		g.Go(func() error {
			log.Info("http listening", "addr", srv.Addr)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("http %s: %w", srv.Addr, err)
			}
			return nil
		})
	}
	g.Go(func() error {
		<-gctx.Done()
		log.Info("shutting down", "timeout", cfg.ShutdownTimeout)
		shCtx, cancel := context.WithTimeout(context.WithoutCancel(gctx), cfg.ShutdownTimeout) // gctx is already canceled
		defer cancel()
		var errs []error
		for _, srv := range servers {
			errs = append(errs, srv.Shutdown(shCtx))
		}
		return errors.Join(errs...)
	})
	err = g.Wait()
	log.Info("bye")
	return err
}

func isSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
