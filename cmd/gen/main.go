// gen writes a synthetic reconciliation world - ledger export, provider
// settlement files, webhook stream - with injected faults and their ground
// truth, and scores a recond report against that truth.
//
//	gen -out data -n 10000 -seed 1
//	gen score -truth data/truth.json -items reports/run-1/items.csv
//
// All data is synthetic: fictional providers (psp_a, bank_b), random IDs and
// order references, no real people, merchants or accounts.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/dwijamahardika/recond/internal/config"
	"github.com/dwijamahardika/recond/internal/domain"
	"github.com/dwijamahardika/recond/internal/gen"
)

func main() {
	var err error
	if len(os.Args) > 1 && os.Args[1] == "score" {
		err = score(os.Args[2:])
	} else {
		err = generate(os.Args[1:])
	}
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}

func generate(args []string) error {
	fs := flag.NewFlagSet("gen", flag.ContinueOnError)
	out := fs.String("out", "data", "output directory")
	n := fs.Int("n", 10_000, "payment attempts")
	seed := fs.Int64("seed", 1, "random seed (same seed, same world)")
	start := fs.String("start", "2026-10-01", "first day (UTC)")
	days := fs.Int("days", 7, "days of payments")
	faults := fs.String("faults", "all", `fault kinds: "all", "none", or a comma list of `+strings.Join(append(append([]string(nil), gen.ItemFaults...), gen.BatchFaults...), ","))
	faultRate := fs.Float64("fault-rate", 0.03, "fraction of psp_a transactions given an item fault")
	batchRate := fs.Float64("batch-fault-rate", 0.25, "fraction of bank_b payouts given a batch fault")
	dupRate := fs.Float64("dup-rate", 0.15, "fraction of webhook events delivered twice")
	if err := fs.Parse(args); err != nil {
		return err
	}
	day, err := time.Parse("2006-01-02", *start)
	if err != nil {
		return fmt.Errorf("-start: %w", err)
	}
	rules, err := config.LoadRules(os.Getenv("RECOND_PROVIDERS_FILE"))
	if err != nil {
		return err
	}
	cfg := gen.Config{Seed: *seed, Payments: *n, Start: day, Days: *days, Rules: rules,
		FaultRate: *faultRate, BatchFaultRate: *batchRate, DupEventRate: *dupRate}
	switch *faults {
	case "all":
	case "none":
		cfg.FaultRate, cfg.BatchFaultRate = -1, -1
	default:
		cfg.Faults = map[string]bool{}
		known := map[string]bool{}
		for _, k := range append(append([]string(nil), gen.ItemFaults...), gen.BatchFaults...) {
			known[k] = true
		}
		for _, k := range strings.Split(*faults, ",") {
			k = strings.TrimSpace(k)
			if !known[k] {
				return fmt.Errorf("unknown fault kind %q", k)
			}
			cfg.Faults[k] = true
		}
	}
	if *faultRate <= 0 {
		cfg.FaultRate = -1
	}
	if *batchRate <= 0 {
		cfg.BatchFaultRate = -1
	}
	if *dupRate <= 0 {
		cfg.DupEventRate = -1
	}

	t0 := time.Now()
	ds, err := gen.Generate(cfg)
	if err != nil {
		return err
	}
	files, err := ds.WriteDir(*out)
	if err != nil {
		return err
	}
	lines := 0
	for _, items := range ds.PSPA {
		lines += len(items)
	}
	for _, e := range ds.BankB {
		lines += len(e)
	}
	fmt.Printf("wrote %s in %s: %d ledger txns, %d settlement lines in %d files, %d webhook deliveries\n",
		*out, time.Since(t0).Round(time.Millisecond), len(ds.Ledger), lines, len(files), len(ds.Events))
	kinds := map[string]int{}
	for _, f := range ds.Truth.Faults {
		kinds[f.Kind]++
	}
	fmt.Printf("injected %d faults: %s\n", len(ds.Truth.Faults), sortedCounts(kinds))
	exp := map[string]int{}
	for c, v := range ds.Truth.Expected {
		exp[string(c)] = v
	}
	fmt.Printf("expected discrepancies: %s\n", sortedCounts(exp))
	fmt.Printf("reconcile as of %s\n", ds.Truth.AsOf.Format(time.RFC3339))
	return nil
}

func sortedCounts(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " ")
}

func score(args []string) error {
	fs := flag.NewFlagSet("gen score", flag.ContinueOnError)
	truthPath := fs.String("truth", "data/truth.json", "truth.json written by gen")
	itemsPath := fs.String("items", "", "items.csv written by recond report")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *itemsPath == "" {
		return errors.New("-items required")
	}
	b, err := os.ReadFile(*truthPath)
	if err != nil {
		return err
	}
	var truth gen.Truth
	if err := json.Unmarshal(b, &truth); err != nil {
		return fmt.Errorf("%s: %w", *truthPath, err)
	}
	f, err := os.Open(*itemsPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	outs, err := gen.ScoredFromItemsCSV(f)
	if err != nil {
		return err
	}
	s := gen.ScoreOutcomes(&truth, outs)
	got := map[domain.Class]int{}
	for _, o := range outs {
		if o.Class.IsDiscrepancy() {
			got[o.Class]++
		}
	}
	fmt.Printf("%-22s %8s %8s\n", "class", "truth", "recond")
	for _, c := range domain.Classes {
		if c.IsDiscrepancy() {
			fmt.Printf("%-22s %8d %8d\n", c, truth.Expected[c], got[c])
		}
	}
	fmt.Println(s.String())
	if !s.Perfect() {
		return errors.New("report does not match ground truth")
	}
	fmt.Println("every injected fault classified exactly; nothing clean flagged")
	return nil
}
