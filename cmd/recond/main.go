// recond: a payment reconciliation engine.
//
// Composition root and CLI. Every subcommand opens the same embedded SQLite
// database; `serve` additionally runs the HTTP API with errgroup +
// signal.NotifyContext so SIGTERM drains in-flight requests before closing
// the store.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime/pprof"
	"strings"
	"syscall"
	"time"

	_ "time/tzdata" // IANA zones in provider rules work even in a scratch image

	"github.com/dwijamahardika/recond/internal/adapters/sqlite"
	"github.com/dwijamahardika/recond/internal/app"
	"github.com/dwijamahardika/recond/internal/config"
)

var version = "dev"

const usage = `recond - payment reconciliation engine

Usage:
  recond <command> [flags]

Commands:
  ingest      ingest ledger exports, settlement files and webhook event files
  run         reconcile everything ingested so far and store a versioned run
  verify      replay a stored run from its recorded inputs and rules
  runs        list runs
  report      write a run's items.csv and summary.json
  exceptions  list the exception queue, or "exceptions show ID"
  resolve     resolve an exception (append-only audit trail)
  reopen      reopen a resolved or auto-closed exception
  payment     show a payment's webhook-derived state and events
  serve       run the HTTP API (webhooks, runs, exceptions, metrics)
  config      print the provider rules in effect
  version     print the version

Every command accepts -db (default $RECOND_DB or ./recond.db).
Run "recond <command> -h" for a command's flags.
`

func main() {
	if len(os.Args) < 2 {
		_, _ = fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	start := time.Now()
	stopProfile := startProfile(os.Getenv("RECOND_CPUPROFILE"))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	err := dispatch(ctx, os.Args[1], os.Args[2:], os.Stdout)
	stop()
	stopProfile()
	if os.Getenv("RECOND_STATS") != "" {
		reportStats(os.Stderr, os.Args[1], time.Since(start))
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		_, _ = fmt.Fprintln(os.Stderr, "recond:", err)
		os.Exit(1)
	}
}

func dispatch(ctx context.Context, cmd string, args []string, out io.Writer) error {
	switch cmd {
	case "ingest":
		return cmdIngest(ctx, args, out)
	case "run":
		return cmdRun(ctx, args, out)
	case "verify":
		return cmdVerify(ctx, args, out)
	case "runs":
		return cmdRuns(ctx, args, out)
	case "report":
		return cmdReport(ctx, args, out)
	case "exceptions":
		return cmdExceptions(ctx, args, out)
	case "resolve":
		return cmdDecide(ctx, args, out, true)
	case "reopen":
		return cmdDecide(ctx, args, out, false)
	case "payment":
		return cmdPayment(ctx, args, out)
	case "serve":
		return cmdServe(ctx, args)
	case "config":
		return cmdConfig(args, out)
	case "version", "-v", "--version":
		_, err := fmt.Fprintln(out, "recond", version)
		return err
	case "help", "-h", "--help":
		_, err := fmt.Fprint(out, usage)
		return err
	}
	_, _ = fmt.Fprint(os.Stderr, usage)
	return fmt.Errorf("unknown command %q", cmd)
}

// common holds flags every command shares.
type common struct {
	db      string
	verbose bool
	json    bool
}

func newFlags(name string, c *common, withJSON bool) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.StringVar(&c.db, "db", envOr("RECOND_DB", "recond.db"), "SQLite database path")
	fs.BoolVar(&c.verbose, "v", false, "log progress to stderr")
	if withJSON {
		fs.BoolVar(&c.json, "json", false, "machine-readable JSON output")
	}
	return fs
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// open loads config and the store and builds the service.
func open(ctx context.Context, c common) (*app.Service, *sqlite.Store, error) {
	rules, err := config.LoadRules(os.Getenv("RECOND_PROVIDERS_FILE"))
	if err != nil {
		return nil, nil, err
	}
	store, err := sqlite.Open(ctx, c.db)
	if err != nil {
		return nil, nil, err
	}
	level := slog.LevelWarn
	if c.verbose {
		level = slog.LevelInfo
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	return app.NewService(store, rules, nil, log, nil), store, nil
}

// startProfile writes a CPU profile of one CLI command when
// RECOND_CPUPROFILE names a file (for `go tool pprof`).
func startProfile(path string) func() {
	if path == "" {
		return func() {}
	}
	f, err := os.Create(path) //nolint:gosec // operator-chosen profile path
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "recond: cpu profile:", err)
		return func() {}
	}
	if err := pprof.StartCPUProfile(f); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "recond: cpu profile:", err)
		_ = f.Close()
		return func() {}
	}
	return func() {
		pprof.StopCPUProfile()
		_ = f.Close()
	}
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}
