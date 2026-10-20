// Package ingest turns provider files into domain records. Each settlement
// format has its own adapter (column layout, delimiter, date format, number
// convention, timezone) and a matching writer used by the synthetic data
// generator, so the formats are exercised in both directions.
//
// Parsing is row-tolerant and file-strict: a malformed row becomes a Reject
// (stored, never silently dropped) while a file with missing required columns
// fails as a whole.
package ingest

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Reject is a row that could not be ingested.
type Reject struct {
	Line   int    `json:"line"`
	Reason string `json:"reason"`
	Raw    string `json:"raw"`
}

// Options carries per-provider context into an adapter.
type Options struct {
	Provider string
	// Location is the provider's local timezone for formats that print local
	// wall-clock times without an offset.
	Location *time.Location
}

// Emit receives one parsed record and a fingerprint of its content. The
// fingerprint lets storage tell a harmless re-delivery (same key, same
// content) from a conflicting one (same key, different content).
type Emit[T any] func(rec T, contentHash string) error

// ErrHeader means a file lacks required columns; the whole file is refused.
var ErrHeader = errors.New("missing required column")

// RowHash fingerprints a row's fields.
func RowHash(fields ...string) string {
	h := sha256.New()
	for _, f := range fields {
		h.Write([]byte(f))
		h.Write([]byte{0x1f})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// columns maps lower-cased header names to their index and checks that every
// required column is present. A UTF-8 BOM (spreadsheet exports) is stripped.
func columns(header []string, required ...string) (map[string]int, error) {
	idx := make(map[string]int, len(header))
	for i, h := range header {
		if i == 0 {
			h = strings.TrimPrefix(h, "\ufeff")
		}
		idx[strings.ToLower(strings.TrimSpace(h))] = i
	}
	var missing []string
	for _, r := range required {
		if _, ok := idx[r]; !ok {
			missing = append(missing, r)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrHeader, strings.Join(missing, ", "))
	}
	return idx, nil
}

func field(rec []string, idx map[string]int, name string) string {
	i, ok := idx[name]
	if !ok || i >= len(rec) {
		return ""
	}
	return strings.TrimSpace(rec[i])
}

func newCSVReader(r io.Reader, comma rune) *csv.Reader {
	cr := csv.NewReader(r)
	cr.Comma = comma
	cr.FieldsPerRecord = -1 // validated per row so one short row is a Reject, not a fatal error
	cr.ReuseRecord = true
	return cr
}

// eachRow drives a CSV reader: header first, then fn per data row. Rows the
// csv package itself cannot parse (bad quoting) become rejects.
func eachRow(cr *csv.Reader, comma rune, onHeader func([]string) error, fn func(line int, rec []string, raw string) error, reject func(Reject)) error {
	header, err := cr.Read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%w: empty file", ErrHeader)
		}
		return fmt.Errorf("read header: %w", err)
	}
	if err := onHeader(header); err != nil {
		return err
	}
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		var pe *csv.ParseError
		if errors.As(err, &pe) {
			reject(Reject{Line: pe.Line, Reason: pe.Err.Error()})
			continue
		}
		if err != nil {
			return err
		}
		line, _ := cr.FieldPos(0)
		if len(rec) == 1 && strings.TrimSpace(rec[0]) == "" {
			continue // blank line
		}
		if err := fn(line, rec, joinRaw(rec, comma)); err != nil {
			return err
		}
	}
}

// joinRaw re-serialises a record (quoting only where needed) so evidence shows
// the row as the provider sent it.
func joinRaw(rec []string, comma rune) string {
	special := string(comma) + "\"\r\n"
	var b strings.Builder
	for i, f := range rec {
		if i > 0 {
			b.WriteRune(comma)
		}
		if strings.ContainsAny(f, special) {
			b.WriteByte('"')
			b.WriteString(strings.ReplaceAll(f, `"`, `""`))
			b.WriteByte('"')
			continue
		}
		b.WriteString(f)
	}
	return b.String()
}
