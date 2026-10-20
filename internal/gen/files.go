package gen

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/dwijamahardika/recond/internal/domain"
	"github.com/dwijamahardika/recond/internal/ingest"
)

// File names written by Render.
const (
	LedgerFile   = "ledger.csv"
	EventsFile   = "webhooks.jsonl"
	TruthFile    = "truth.json"
	SettleDir    = "settlements"
	pspaPattern  = "psp_a_settlement_%s.csv"
	bankbPattern = "bank_b_statement_%s.csv"
)

// Sink opens a named output; Render closes what it opens.
type Sink func(name string) (io.WriteCloser, error)

// SettlementFile names one rendered settlement file and its provider.
type SettlementFile struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
}

// Render writes the dataset in recond's input formats: a ledger export,
// one settlement file per provider per date, the webhook stream in delivery
// order, and the ground truth.
func (ds *Dataset) Render(open Sink) ([]SettlementFile, error) {
	files := make([]SettlementFile, 0, len(ds.PSPA)+len(ds.BankB))
	write := func(name string, fn func(io.Writer) error) error {
		w, err := open(name)
		if err != nil {
			return err
		}
		if err := fn(w); err != nil {
			_ = w.Close()
			return fmt.Errorf("%s: %w", name, err)
		}
		return w.Close()
	}

	if err := write(LedgerFile, func(w io.Writer) error {
		lw, err := ingest.NewLedgerCSVWriter(w)
		if err != nil {
			return err
		}
		for _, t := range ds.Ledger {
			if err := lw.Write(t); err != nil {
				return err
			}
		}
		return lw.Flush()
	}); err != nil {
		return nil, err
	}

	for _, d := range sortedDates(ds.PSPA) {
		name := SettleDir + "/" + fmt.Sprintf(pspaPattern, d)
		files = append(files, SettlementFile{Provider: ProviderA, Name: name})
		items := ds.PSPA[d]
		sort.Slice(items, func(i, j int) bool { return items[i].SettlementID < items[j].SettlementID })
		if err := write(name, func(w io.Writer) error {
			pw, err := ingest.NewPSPAWriter(w, ProviderA)
			if err != nil {
				return err
			}
			for _, it := range items {
				if _, err := pw.Write(ingest.PSPARow{SettlementID: it.SettlementID, MerchantRef: it.Ref, TxnTime: it.TxnTime,
					SettlementDate: it.Date, Currency: Currency, Gross: it.Gross, Fee: it.Fee}); err != nil {
					return err
				}
			}
			return pw.Flush()
		}); err != nil {
			return nil, err
		}
	}

	rb, _ := ds.Rules.Provider(ProviderB)
	for _, d := range sortedDates(ds.BankB) {
		name := SettleDir + "/" + fmt.Sprintf(bankbPattern, d)
		files = append(files, SettlementFile{Provider: ProviderB, Name: name})
		if err := write(name, func(w io.Writer) error {
			bw, err := ingest.NewBankBWriter(w, ProviderB, rb.Location())
			if err != nil {
				return err
			}
			for _, e := range ds.BankB[d] { // already in RefNo order (IDs assigned in this order)
				id, err := bw.Write(e.row())
				if err != nil {
					return err
				}
				if id != e.ID {
					return fmt.Errorf("bank_b line id drift: %s != %s", id, e.ID)
				}
			}
			return bw.Flush()
		}); err != nil {
			return nil, err
		}
	}

	if err := write(EventsFile, func(w io.Writer) error {
		ew := ingest.NewEventWriter(w)
		for _, e := range ds.Events {
			if err := ew.Write(e); err != nil {
				return err
			}
		}
		return ew.Flush()
	}); err != nil {
		return nil, err
	}

	if err := write(TruthFile, func(w io.Writer) error {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(ds.Truth)
	}); err != nil {
		return nil, err
	}
	return files, nil
}

func sortedDates[T any](m map[domain.Date]T) []domain.Date {
	out := make([]domain.Date, 0, len(m))
	for d := range m {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// WriteDir renders the dataset into dir (settlement files under
// dir/settlements) and returns the settlement files written.
func (ds *Dataset) WriteDir(dir string) ([]SettlementFile, error) {
	if err := os.MkdirAll(filepath.Join(dir, SettleDir), 0o750); err != nil {
		return nil, err
	}
	return ds.Render(func(name string) (io.WriteCloser, error) {
		f, err := os.Create(filepath.Join(dir, filepath.FromSlash(name))) //nolint:gosec // generator output path
		if err != nil {
			return nil, err
		}
		return &bufferedFile{f: f, w: bufio.NewWriterSize(f, 1<<20)}, nil
	})
}

type bufferedFile struct {
	f *os.File
	w *bufio.Writer
}

func (b *bufferedFile) Write(p []byte) (int, error) { return b.w.Write(p) }

func (b *bufferedFile) Close() error {
	if err := b.w.Flush(); err != nil {
		_ = b.f.Close()
		return err
	}
	return b.f.Close()
}
