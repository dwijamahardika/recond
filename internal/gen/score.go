package gen

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/dwijamahardika/recond/internal/domain"
)

// Scored is the view of an outcome the scorer needs; it can be built from an
// in-memory result or from a report CSV.
type Scored struct {
	ID     int
	Class  domain.Class
	Rule   domain.Rule
	Ledger []string
	Lines  []string
}

// Score compares outcomes with the ground truth.
type Score struct {
	Faults      int      `json:"faults"`
	Detected    int      `json:"detected"`
	Missed      []string `json:"missed,omitempty"`
	Flagged     int      `json:"flagged"`
	Unexplained []string `json:"unexplained,omitempty"`
	Paired      int      `json:"pairs_checked"`
	Mispaired   []string `json:"mispaired,omitempty"`
}

// Perfect is the ground-truth property: every fault classified exactly, every
// flag explained by a fault, every match pairing the right records.
func (s Score) Perfect() bool {
	return s.Detected == s.Faults && len(s.Missed) == 0 && len(s.Unexplained) == 0 && len(s.Mispaired) == 0
}

func (s Score) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "faults %d, detected %d; flagged outcomes %d, unexplained %d; pairs checked %d, mispaired %d",
		s.Faults, s.Detected, s.Flagged, len(s.Unexplained), s.Paired, len(s.Mispaired))
	for _, list := range [][]string{s.Missed, s.Unexplained, s.Mispaired} {
		for i, m := range list {
			if i == 10 {
				fmt.Fprintf(&b, "\n  ... %d more", len(list)-10)
				break
			}
			b.WriteString("\n  " + m)
		}
	}
	return b.String()
}

// ScoreOutcomes checks outcomes against the truth.
func ScoreOutcomes(t *Truth, outs []Scored) Score {
	s := Score{Faults: len(t.Faults)}
	byKey := map[string][]int{}
	for i, o := range outs {
		for _, id := range o.Ledger {
			byKey[id] = append(byKey[id], i)
		}
		for _, id := range o.Lines {
			byKey[id] = append(byKey[id], i)
		}
	}
	faultsByKey := map[string][]Fault{}
	for _, f := range t.Faults {
		faultsByKey[f.Key] = append(faultsByKey[f.Key], f)
		found := false
		var got []string
		for _, i := range byKey[f.Key] {
			o := outs[i]
			got = append(got, string(o.Class)+"/"+string(o.Rule))
			if o.Class != f.Class {
				continue
			}
			if f.Kind == FaultCorruptReference && (!contains(o.Ledger, f.LedgerID) || !contains(o.Lines, f.LineID)) {
				continue
			}
			found = true
		}
		if found {
			s.Detected++
		} else {
			s.Missed = append(s.Missed, fmt.Sprintf("%s %s: want %s, got %v", f.Kind, f.Key, f.Class, got))
		}
	}
	for _, o := range outs {
		if o.Class == domain.ClassMatched {
			continue
		}
		s.Flagged++
		explained := false
		for _, id := range append(append([]string(nil), o.Ledger...), o.Lines...) {
			for _, f := range faultsByKey[id] {
				if f.Class == o.Class {
					explained = true
				}
			}
		}
		if !explained {
			s.Unexplained = append(s.Unexplained, fmt.Sprintf("outcome %d %s/%s ledger=%v lines=%v", o.ID, o.Class, o.Rule, o.Ledger, o.Lines))
		}
	}
	if len(t.Settles) > 0 {
		for _, o := range outs {
			if o.Class == domain.ClassDuplicateSettlement || len(o.Lines) == 0 {
				continue
			}
			for _, l := range o.Ledger {
				s.Paired++
				if want := t.Settles[l]; !contains(o.Lines, want) {
					s.Mispaired = append(s.Mispaired, fmt.Sprintf("ledger %s grouped with %v, settled by %q", l, o.Lines, want))
				}
			}
		}
	}
	sort.Strings(s.Missed)
	return s
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// ScoredFromItemsCSV rebuilds outcomes from a recond items report (one row
// per record per outcome), so a run can be scored offline against truth.json.
func ScoredFromItemsCSV(r io.Reader) ([]Scored, error) {
	cr := csv.NewReader(r)
	header, err := cr.Read()
	if err != nil {
		return nil, err
	}
	col := map[string]int{}
	for i, h := range header {
		col[h] = i
	}
	for _, need := range []string{"outcome_id", "class", "rule", "role", "entity_id"} {
		if _, ok := col[need]; !ok {
			return nil, fmt.Errorf("items csv: missing column %s", need)
		}
	}
	var out []Scored
	index := map[int]int{}
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		id, err := strconv.Atoi(rec[col["outcome_id"]])
		if err != nil {
			return nil, fmt.Errorf("items csv: outcome_id %q", rec[col["outcome_id"]])
		}
		i, ok := index[id]
		if !ok {
			i = len(out)
			index[id] = i
			out = append(out, Scored{ID: id, Class: domain.Class(rec[col["class"]]), Rule: domain.Rule(rec[col["rule"]])})
		}
		entity := strings.TrimPrefix(rec[col["entity_id"]], "'")
		switch rec[col["role"]] {
		case "ledger":
			out[i].Ledger = append(out[i].Ledger, entity)
		case "line":
			out[i].Lines = append(out[i].Lines, entity)
		}
	}
}
