// Package app holds recond's use cases: ingest files and webhooks, run a
// reconciliation, verify a past run, report, and work the exception queue.
// It wires the pure parts (ingest adapters, the match engine) to storage.
//
// There is deliberately no repository interface between app and storage:
// SQLite is embedded, so tests run the real store in a temp directory, and an
// interface with a single implementation would only add indirection.
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dwijamahardika/recond/internal/adapters/sqlite"
	"github.com/dwijamahardika/recond/internal/domain"
	"github.com/dwijamahardika/recond/internal/ingest"
	"github.com/dwijamahardika/recond/internal/match"
	"github.com/dwijamahardika/recond/internal/platform/clock"
)

// Metrics receives domain signals; nil-safe via noMetrics.
type Metrics interface {
	Webhook(provider, result string)
	Ingested(kind string, rows int)
	RunFinished(d time.Duration, counts map[domain.Class]int)
	ExceptionsOpen(n int)
}

type noMetrics struct{}

func (noMetrics) Webhook(string, string)                          {}
func (noMetrics) Ingested(string, int)                            {}
func (noMetrics) RunFinished(time.Duration, map[domain.Class]int) {}
func (noMetrics) ExceptionsOpen(int)                              {}

// Service is the application layer used by the CLI and the HTTP API.
type Service struct {
	store   *sqlite.Store
	rules   *match.Config
	clock   clock.Clock
	log     *slog.Logger
	metrics Metrics
}

// NewService wires a service; clk, log and m may be nil.
func NewService(store *sqlite.Store, rules *match.Config, clk clock.Clock, log *slog.Logger, m Metrics) *Service {
	if clk == nil {
		clk = clock.System{}
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if m == nil {
		m = noMetrics{}
	}
	return &Service{store: store, rules: rules, clock: clk, log: log, metrics: m}
}

// Rules returns the provider rules in effect.
func (s *Service) Rules() *match.Config { return s.rules }

// Errors adapters map to user-facing responses.
var (
	ErrUnknownProvider = errors.New("unknown provider")
	ErrValidation      = errors.New("validation")
)

// Source is an input file. Open may be called twice (hash, then parse).
type Source struct {
	Name string
	Open func() (io.ReadCloser, error)
}

// FileSource reads a file from disk.
func FileSource(path string) Source {
	return Source{Name: path, Open: func() (io.ReadCloser, error) { return os.Open(path) }} //nolint:gosec // operator-supplied input file
}

func (src Source) digest() (string, int64, error) {
	r, err := src.Open()
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = r.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, r)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// IngestResult reports one file.
type IngestResult struct {
	File            string             `json:"file"`
	SHA256          string             `json:"sha256"`
	AlreadyIngested bool               `json:"already_ingested"`
	Stats           sqlite.IngestStats `json:"stats"`
}

func (s *Service) ingest(ctx context.Context, kind, source string, src Source, parse func(io.Reader, *sqlite.Ingester, func(ingest.Reject)) error) (IngestResult, error) {
	sum, size, err := src.digest()
	if err != nil {
		return IngestResult{}, fmt.Errorf("read %s: %w", src.Name, err)
	}
	res := IngestResult{File: filepath.Base(src.Name), SHA256: sum}
	meta := sqlite.FileMeta{Kind: kind, Source: source, Name: filepath.Base(src.Name), SHA256: sum, Size: size}
	stats, err := s.store.Ingest(ctx, meta, func(in *sqlite.Ingester) error {
		r, err := src.Open()
		if err != nil {
			return err
		}
		defer func() { _ = r.Close() }()
		var rejErr error
		reject := func(rj ingest.Reject) {
			if err := in.Reject(rj); err != nil && rejErr == nil {
				rejErr = err
			}
		}
		if err := parse(r, in, reject); err != nil {
			return err
		}
		return rejErr
	})
	if errors.Is(err, sqlite.ErrAlreadyIngested) {
		res.AlreadyIngested, res.Stats.FileID = true, stats.FileID
		return res, nil
	}
	if err != nil {
		return res, fmt.Errorf("ingest %s: %w", res.File, err)
	}
	res.Stats = stats
	s.metrics.Ingested(kind, stats.Inserted)
	s.log.Info("ingested", "kind", kind, "source", source, "file", res.File, "rows", stats.Rows, "inserted", stats.Inserted,
		"duplicates", stats.Duplicates, "conflicts", stats.Conflicts, "rejected", stats.Rejected)
	return res, nil
}

// IngestLedger ingests a ledger export (csv, json, jsonl or "" to detect).
// defaultProvider applies to rows without a provider column.
func (s *Service) IngestLedger(ctx context.Context, src Source, format, defaultProvider string) (IngestResult, error) {
	return s.ingest(ctx, "ledger", "ledger", src, func(r io.Reader, in *sqlite.Ingester, reject func(ingest.Reject)) error {
		return ingest.ReadLedger(r, format, ingest.Options{Provider: defaultProvider}, func(t domain.LedgerTxn, h string) error {
			if _, ok := s.rules.Provider(t.Provider); !ok {
				reject(ingest.Reject{Reason: fmt.Sprintf("unknown provider %q", t.Provider), Raw: t.ID})
				return nil
			}
			return in.Ledger(t, h)
		}, reject)
	})
}

// IngestSettlement ingests one provider settlement file with its adapter.
func (s *Service) IngestSettlement(ctx context.Context, provider string, src Source) (IngestResult, error) {
	rules, ok := s.rules.Provider(provider)
	if !ok {
		return IngestResult{}, fmt.Errorf("%w %q (configured: %s)", ErrUnknownProvider, provider, strings.Join(s.rules.Names(), ", "))
	}
	adapter, ok := ingest.AdapterFor(rules.Format)
	if !ok {
		return IngestResult{}, fmt.Errorf("provider %s: no adapter for format %q", provider, rules.Format)
	}
	opt := ingest.Options{Provider: provider, Location: rules.Location()}
	return s.ingest(ctx, "settlement", provider, src, func(r io.Reader, in *sqlite.Ingester, reject func(ingest.Reject)) error {
		return adapter.Read(r, opt, in.Line, reject)
	})
}

// IngestWebhooks ingests a JSON Lines file of webhook events in delivery
// order (for backfills and the demo; live events arrive over HTTP).
func (s *Service) IngestWebhooks(ctx context.Context, src Source) (IngestResult, error) {
	return s.ingest(ctx, "webhooks", "webhooks", src, func(r io.Reader, in *sqlite.Ingester, reject func(ingest.Reject)) error {
		return ingest.ReadEvents(r, ingest.Options{}, func(e domain.PaymentEvent, h string) error {
			if _, ok := s.rules.Provider(e.Provider); !ok {
				reject(ingest.Reject{Reason: fmt.Sprintf("unknown provider %q", e.Provider), Raw: e.EventID})
				return nil
			}
			return in.Event(e, h)
		}, reject)
	})
}

// ApplyWebhook decodes, validates and applies one verified webhook body.
func (s *Service) ApplyWebhook(ctx context.Context, provider string, body []byte) (sqlite.EventResult, error) {
	if _, ok := s.rules.Provider(provider); !ok {
		return sqlite.EventResult{}, ErrUnknownProvider
	}
	e, err := ingest.DecodeEvent(body, provider)
	if err != nil {
		s.metrics.Webhook(provider, "malformed")
		return sqlite.EventResult{}, fmt.Errorf("%w: %w", ErrValidation, err)
	}
	res, err := s.store.ApplyEvent(ctx, e, ingest.EventHash(e))
	if err != nil {
		return res, err
	}
	switch {
	case res.Duplicate:
		s.metrics.Webhook(provider, "duplicate")
	case res.Verdict == domain.Rejected:
		s.metrics.Webhook(provider, "rejected_transition")
		s.log.Warn("webhook transition rejected", "provider", provider, "payment_ref", e.PaymentRef, "event_id", e.EventID, "reason", res.Reason)
	case res.OutOfOrder:
		s.metrics.Webhook(provider, "out_of_order")
	default:
		s.metrics.Webhook(provider, "applied")
	}
	return res, nil
}

// Payment returns a payment's state and event history.
func (s *Service) Payment(ctx context.Context, provider, ref string) (domain.Payment, []domain.PaymentEvent, error) {
	return s.store.Payment(ctx, provider, ref)
}
