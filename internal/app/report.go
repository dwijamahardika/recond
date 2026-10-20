package app

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/dwijamahardika/recond/internal/adapters/sqlite"
)

// ItemColumns is the report CSV header: one row per record per outcome, so
// a batch payout and each transaction it covers are separate rows.
var ItemColumns = []string{
	"run_id", "outcome_id", "class", "rule", "provider", "currency", "role", "entity_id", "reference", "amount_minor",
	"expected_gross", "expected_fee", "expected_net", "actual_gross", "actual_fee", "actual_net", "deadline", "settled_on", "detail",
}

// WriteItemsCSV streams a run's report rows.
func (s *Service) WriteItemsCSV(ctx context.Context, runID int64, w io.Writer) (int, error) {
	run, err := s.store.Run(ctx, runID)
	if err != nil {
		return 0, err
	}
	cw := csv.NewWriter(w)
	if err := cw.Write(ItemColumns); err != nil {
		return 0, err
	}
	rid := strconv.FormatInt(run.ID, 10)
	n := 0
	i64 := func(v int64) string { return strconv.FormatInt(v, 10) }
	err = s.store.RunItems(ctx, run.ID, func(it sqlite.Item) error {
		n++
		return cw.Write([]string{
			rid, strconv.Itoa(it.OutcomeID), string(it.Class), string(it.Rule), it.Provider, string(it.Currency),
			it.Role, cell(it.EntityID), cell(it.Reference), i64(it.Amount),
			i64(it.Expected.Gross), i64(it.Expected.Fee), i64(it.Expected.Net), i64(it.Actual.Gross), i64(it.Actual.Fee), i64(it.Actual.Net),
			it.Deadline, it.SettledOn, cell(it.Detail),
		})
	})
	if err != nil {
		return n, err
	}
	cw.Flush()
	return n, cw.Error()
}

// cell neutralises spreadsheet formula injection: references and details
// come from provider files, and finance opens reports in spreadsheets.
func cell(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}

// ReportFiles are the paths a report wrote.
type ReportFiles struct {
	Items   string `json:"items_csv"`
	Summary string `json:"summary_json"`
	Rows    int    `json:"rows"`
}

// WriteReport writes <dir>/run-<id>/items.csv and summary.json.
func (s *Service) WriteReport(ctx context.Context, runID int64, dir string) (*Summary, ReportFiles, error) {
	sum, run, err := s.Summary(ctx, runID)
	if err != nil {
		return nil, ReportFiles{}, err
	}
	out := filepath.Join(dir, fmt.Sprintf("run-%d", run.ID))
	if err := os.MkdirAll(out, 0o750); err != nil {
		return nil, ReportFiles{}, err
	}
	files := ReportFiles{Items: filepath.Join(out, "items.csv"), Summary: filepath.Join(out, "summary.json")}
	f, err := os.Create(files.Items)
	if err != nil {
		return nil, files, err
	}
	if files.Rows, err = s.WriteItemsCSV(ctx, run.ID, f); err != nil {
		_ = f.Close()
		return nil, files, err
	}
	if err := f.Close(); err != nil {
		return nil, files, err
	}
	b, err := json.MarshalIndent(sum, "", "  ")
	if err != nil {
		return nil, files, err
	}
	return sum, files, os.WriteFile(files.Summary, append(b, '\n'), 0o600)
}
