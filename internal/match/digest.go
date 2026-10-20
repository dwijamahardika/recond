package match

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"strconv"

	"github.com/dwijamahardika/recond/internal/domain"
)

// Digest is a SHA-256 over a canonical rendering of every outcome, in order.
// Two runs over the same inputs and config must produce the same digest; the
// replay check compares exactly this.
func (r *Result) Digest() string {
	h := sha256.New()
	w := bufio.NewWriterSize(h, 1<<16)
	buf := make([]byte, 0, 256)
	for i := range r.Outcomes {
		o := &r.Outcomes[i]
		buf = buf[:0]
		buf = append(buf, o.Class...)
		buf = append(buf, '|')
		buf = append(buf, o.Rule...)
		buf = append(buf, '|')
		buf = append(buf, o.Provider...)
		buf = append(buf, '|')
		buf = append(buf, o.Currency...)
		for _, a := range []int64{o.Expected.Gross, o.Expected.Fee, o.Expected.Net, o.Actual.Gross, o.Actual.Fee, o.Actual.Net, int64(o.Deadline), int64(o.SettledOn)} {
			buf = append(buf, '|')
			buf = strconv.AppendInt(buf, a, 10)
		}
		buf = append(buf, '|')
		buf = append(buf, o.Detail...)
		_, _ = w.Write(buf)
		for _, i := range o.Ledger {
			_, _ = w.WriteString("|L:")
			_, _ = w.WriteString(r.In.Ledger[i].ID)
		}
		for _, i := range o.Lines {
			_, _ = w.WriteString("|S:")
			_, _ = w.WriteString(r.In.Lines[i].ID)
		}
		for _, i := range o.Context {
			_, _ = w.WriteString("|C:")
			_, _ = w.WriteString(r.In.Lines[i].ID)
		}
		_ = w.WriteByte('\n')
	}
	_ = w.Flush()
	return hex.EncodeToString(h.Sum(nil))
}

// Counts tallies outcomes by class.
func (r *Result) Counts() map[domain.Class]int {
	c := make(map[domain.Class]int, len(domain.Classes))
	for i := range r.Outcomes {
		c[r.Outcomes[i].Class]++
	}
	return c
}
