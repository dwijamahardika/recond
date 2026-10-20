package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/dwijamahardika/recond/internal/adapters/sqlite"
	"github.com/dwijamahardika/recond/internal/app"
	"github.com/dwijamahardika/recond/internal/config"
	"github.com/dwijamahardika/recond/internal/domain"
)

// ---- ingest ----

type planned struct{ kind, provider, path string }

func cmdIngest(ctx context.Context, args []string, out io.Writer) error {
	var c common
	fs := newFlags("ingest", &c, true)
	kind := fs.String("kind", "", "ledger | settlement | webhooks")
	provider := fs.String("provider", "", "settlement provider; for ledger files, the default provider for rows without one")
	format := fs.String("format", "", "ledger format: csv | json (default: detect)")
	dir := fs.String("dir", "", "ingest a data directory laid out like cmd/gen output (ledger.*, settlements/<provider>_*.csv, webhooks.jsonl)")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(fs.Output(), "usage: recond ingest -kind ledger|settlement|webhooks [-provider P] FILE...\n       recond ingest -dir DIR")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	svc, store, err := open(ctx, c)
	if err != nil {
		return err
	}
	defer closeStore(store)

	var plan []planned
	switch {
	case *dir != "":
		if plan, err = planDir(*dir, svc.Rules().Names()); err != nil {
			return err
		}
	case *kind == "" || fs.NArg() == 0:
		fs.Usage()
		return errors.New("need -kind and at least one file, or -dir")
	default:
		for _, f := range fs.Args() {
			plan = append(plan, planned{kind: *kind, provider: *provider, path: f})
		}
	}

	results := make([]app.IngestResult, 0, len(plan))
	tw := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
	tp := &printer{w: tw}
	if !c.json {
		tp.ln("FILE\tKIND\tROWS\tINSERTED\tDUPLICATE\tCONFLICT\tREJECTED\tNOTE")
	}
	for _, p := range plan {
		var r app.IngestResult
		src := app.FileSource(p.path)
		switch p.kind {
		case "ledger":
			r, err = svc.IngestLedger(ctx, src, *format, p.provider)
		case "settlement":
			if p.provider == "" {
				return fmt.Errorf("%s: -provider required for settlement files", p.path)
			}
			r, err = svc.IngestSettlement(ctx, p.provider, src)
		case "webhooks":
			r, err = svc.IngestWebhooks(ctx, src)
		default:
			return fmt.Errorf("unknown -kind %q", p.kind)
		}
		if err != nil {
			return err
		}
		results = append(results, r)
		if c.json {
			continue
		}
		label := p.kind
		if p.kind == "settlement" {
			label = p.provider
		}
		note := ""
		switch {
		case r.AlreadyIngested:
			note = fmt.Sprintf("already ingested as file #%d", r.Stats.FileID)
		case r.Stats.OutOfOrder > 0 || r.Stats.RejectedTransitions > 0:
			note = fmt.Sprintf("%d out of order, %d illegal transitions", r.Stats.OutOfOrder, r.Stats.RejectedTransitions)
		}
		st := r.Stats
		tp.f("%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.File, label, num(st.Rows), num(st.Inserted), num(st.Duplicates), num(st.Conflicts), num(st.Rejected), note)
	}
	if c.json {
		return json.NewEncoder(out).Encode(results)
	}
	return tp.flush(tw)
}

// planDir lays out a generated data directory in dependency order: ledger,
// settlement files (provider from the file-name prefix), then webhooks.
func planDir(dir string, providers []string) ([]planned, error) {
	plan := make([]planned, 0, 8)
	for _, name := range []string{"ledger.csv", "ledger.json", "ledger.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			plan = append(plan, planned{kind: "ledger", path: filepath.Join(dir, name)})
		}
	}
	files, err := filepath.Glob(filepath.Join(dir, "settlements", "*.csv"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	sort.Slice(providers, func(i, j int) bool { return len(providers[i]) > len(providers[j]) })
	for _, f := range files {
		base := filepath.Base(f)
		matched := ""
		for _, p := range providers {
			if strings.HasPrefix(base, p+"_") {
				matched = p
				break
			}
		}
		if matched == "" {
			return nil, fmt.Errorf("%s: cannot tell the provider from the file name (want <provider>_*.csv)", f)
		}
		plan = append(plan, planned{kind: "settlement", provider: matched, path: f})
	}
	if _, err := os.Stat(filepath.Join(dir, "webhooks.jsonl")); err == nil {
		plan = append(plan, planned{kind: "webhooks", path: filepath.Join(dir, "webhooks.jsonl")})
	}
	if len(plan) == 0 {
		return nil, fmt.Errorf("%s: nothing to ingest", dir)
	}
	return plan, nil
}

// ---- run / verify / runs ----

func parseAsOf(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("-as-of %q: want RFC 3339 or YYYY-MM-DD", s)
}

func cmdRun(ctx context.Context, args []string, out io.Writer) error {
	var c common
	fs := newFlags("run", &c, true)
	asOf := fs.String("as-of", "", "reconcile as of this time (RFC 3339 or YYYY-MM-DD, UTC); default now")
	if err := fs.Parse(args); err != nil {
		return err
	}
	at, err := parseAsOf(*asOf)
	if err != nil {
		return err
	}
	svc, store, err := open(ctx, c)
	if err != nil {
		return err
	}
	defer closeStore(store)
	sum, err := svc.Run(ctx, app.RunOptions{AsOf: at})
	if err != nil {
		return err
	}
	if c.json {
		return json.NewEncoder(out).Encode(sum)
	}
	return printSummary(out, sum)
}

func printSummary(out io.Writer, s *app.Summary) error {
	op := &printer{w: out}
	op.f("run #%d  as of %s  engine v%s  rules %s\n", s.RunID, s.AsOf.Format(time.RFC3339), s.EngineVersion, s.ConfigSHA256[:12])
	op.f("inputs: %s ledger txns, %s settlement lines, %s payments (files <= #%d, events <= #%d)\n\n",
		num(s.LedgerTxns), num(s.Lines), num(s.Payments), s.MaxFileID, s.MaxEventSeq)
	tw := tabwriter.NewWriter(out, 2, 4, 2, ' ', tabwriter.AlignRight)
	tp := &printer{w: tw}
	tp.ln("CLASS\tGROUPS\tLEDGER\tLINES\t|NET DIFF|\t")
	for _, c := range s.Classes {
		var deltas []string
		for cur, v := range c.AbsNetDelta {
			deltas = append(deltas, domain.FormatMoney(v, cur))
		}
		sort.Strings(deltas)
		tp.f("%s\t%s\t%s\t%s\t%s\t\n", c.Class, num(c.Outcomes), num(c.LedgerTxns), num(c.Lines), strings.Join(deltas, " "))
	}
	if err := tp.flush(tw); err != nil {
		return err
	}
	op.ln()
	for _, p := range s.Providers {
		op.f("%-8s %s of %s ledger txns matched (%.2f%%), %s flagged\n", p.Provider, num(p.Matched), num(p.LedgerTxns), 100*p.MatchRate, num(p.Flagged))
	}
	e := s.Exceptions
	op.f("\nexceptions: %d opened, %d reopened, %d auto-closed, %d still open, %d already resolved\n", e.Opened, e.Reopened, e.AutoClosed, e.StillOpen, e.Resolved)
	if len(s.Timings) > 0 {
		op.f("timings: load %dms, match %dms, evidence %dms, save %dms, total %dms\n",
			s.Timings["load"], s.Timings["match"], s.Timings["evidence"], s.Timings["save"], s.Timings["total"])
	}
	op.f("result digest %s\n", s.ResultDigest)
	return op.err
}

// runArg reads an optional run id given before or after the flags.
func runArg(fs *flag.FlagSet, args []string) (int64, error) {
	raw, rest := idArg(args)
	if err := fs.Parse(rest); err != nil {
		return 0, err
	}
	if raw == "" && fs.NArg() > 0 {
		raw = fs.Arg(0)
	}
	if raw == "" || raw == "latest" {
		return 0, nil
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("run id %q: want a positive integer or latest", raw)
	}
	return id, nil
}

func cmdVerify(ctx context.Context, args []string, out io.Writer) error {
	var c common
	fs := newFlags("verify", &c, true)
	id, err := runArg(fs, args)
	if err != nil {
		return err
	}
	svc, store, err := open(ctx, c)
	if err != nil {
		return err
	}
	defer closeStore(store)
	if id == 0 {
		_, run, err := svc.Summary(ctx, 0)
		if err != nil {
			return err
		}
		id = run.ID
	}
	v, err := svc.Verify(ctx, id)
	if err != nil {
		return err
	}
	if c.json {
		if err := json.NewEncoder(out).Encode(v); err != nil {
			return err
		}
	} else {
		op := &printer{w: out}
		op.f("run #%d (engine v%s, now v%s)\n", v.RunID, v.EngineVersion, v.CurrentEngine)
		op.f("  inputs  %s  %s\n", v.InputDigest[:16], okStr(v.InputMatches))
		op.f("  result  %s  replay %s  %s\n", v.ResultDigest[:16], v.ReplayDigest[:16], okStr(v.ReplayDigest == v.ResultDigest))
		op.f("  replayed in %dms from the event log and stored rules\n", v.ReplayDuration)
		if op.err != nil {
			return op.err
		}
	}
	if !v.Reproduced {
		return fmt.Errorf("run #%d did not reproduce", v.RunID)
	}
	return nil
}

func okStr(ok bool) string {
	if ok {
		return "match"
	}
	return "MISMATCH"
}

func cmdRuns(ctx context.Context, args []string, out io.Writer) error {
	var c common
	fs := newFlags("runs", &c, true)
	limit := fs.Int("limit", 20, "how many runs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	svc, store, err := open(ctx, c)
	if err != nil {
		return err
	}
	defer closeStore(store)
	runs, err := svc.Runs(ctx, *limit)
	if err != nil {
		return err
	}
	if c.json {
		return json.NewEncoder(out).Encode(runs)
	}
	tw := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
	tp := &printer{w: tw}
	tp.ln("RUN\tCREATED\tAS OF\tLEDGER\tLINES\tDISCREPANCIES\tMS\tDIGEST")
	for _, r := range runs {
		var s app.Summary
		_ = json.Unmarshal([]byte(r.SummaryJSON), &s)
		tp.f("%d\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n", r.ID, r.CreatedAt.Format(time.RFC3339), r.AsOf.Format(time.RFC3339),
			num(r.LedgerCount), num(r.LineCount), num(s.Discrepancies), r.DurationMS, r.ResultDigest[:12])
	}
	return tp.flush(tw)
}

// ---- report ----

func cmdReport(ctx context.Context, args []string, out io.Writer) error {
	var c common
	fs := newFlags("report", &c, true)
	dir := fs.String("out", "reports", "output directory (writes <out>/run-<id>/items.csv and summary.json)")
	id, err := runArg(fs, args)
	if err != nil {
		return err
	}
	svc, store, err := open(ctx, c)
	if err != nil {
		return err
	}
	defer closeStore(store)
	sum, files, err := svc.WriteReport(ctx, id, *dir)
	if err != nil {
		return err
	}
	if c.json {
		return json.NewEncoder(out).Encode(map[string]any{"summary": sum, "files": files})
	}
	if err := printSummary(out, sum); err != nil {
		return err
	}
	op := &printer{w: out}
	op.f("\nwrote %s (%s rows)\nwrote %s\n", files.Items, num(files.Rows), files.Summary)
	return op.err
}

// ---- exceptions ----

func cmdExceptions(ctx context.Context, args []string, out io.Writer) error {
	if len(args) > 0 && args[0] == "show" {
		return cmdShowException(ctx, args[1:], out)
	}
	var c common
	fs := newFlags("exceptions", &c, true)
	var f sqlite.ExceptionFilter
	fs.StringVar(&f.Status, "status", "open", "open | resolved | auto_closed | all")
	fs.StringVar(&f.Class, "class", "", "filter by class")
	fs.StringVar(&f.Provider, "provider", "", "filter by provider")
	fs.IntVar(&f.Limit, "limit", 50, "page size (max 1000)")
	fs.Int64Var(&f.AfterID, "after", 0, "keyset cursor: list ids after this one")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if f.Status == "all" {
		f.Status = ""
	}
	svc, store, err := open(ctx, c)
	if err != nil {
		return err
	}
	defer closeStore(store)
	list, err := svc.Exceptions(ctx, f)
	if err != nil {
		return err
	}
	if c.json {
		return json.NewEncoder(out).Encode(list)
	}
	tw := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
	tp := &printer{w: tw}
	tp.ln("ID\tSTATUS\tCLASS\tPROVIDER\tREFS\tNET DIFF\tRUNS\tDETAIL")
	for _, e := range list {
		tp.f("%d\t%s\t%s\t%s\t%s\t%s\t%d-%d\t%s\n", e.ID, e.Status, e.Class, e.Provider, clip(e.Refs, 34),
			domain.FormatMoney(e.Delta, e.Currency), e.FirstRun, e.LastRun, clip(e.Detail, 70))
	}
	if err := tp.flush(tw); err != nil {
		return err
	}
	op := &printer{w: out}
	if len(list) == f.Limit && len(list) > 0 {
		op.f("\nmore: recond exceptions -after %d\n", list[len(list)-1].ID)
	}
	return op.err
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func idArg(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

func cmdShowException(ctx context.Context, args []string, out io.Writer) error {
	var c common
	fs := newFlags("exceptions show", &c, true)
	raw, rest := idArg(args)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if raw == "" && fs.NArg() > 0 {
		raw = fs.Arg(0)
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return errors.New("usage: recond exceptions show ID")
	}
	svc, store, err := open(ctx, c)
	if err != nil {
		return err
	}
	defer closeStore(store)
	d, err := svc.Exception(ctx, id)
	if err != nil {
		return err
	}
	if c.json {
		return json.NewEncoder(out).Encode(d)
	}
	op := &printer{w: out}
	op.f("exception #%d  %s  %s  %s\n", d.ID, d.Status, d.Class, d.Provider)
	op.f("refs:   %s\nnet diff: %s\ndetail: %s\nseen in runs %d..%d (evidence from run #%d, outcome %d)\n\n",
		d.Refs, domain.FormatMoney(d.Delta, d.Currency), d.Detail, d.FirstRun, d.LastRun, d.RunID, d.OutcomeID)
	if len(d.Evidence) > 0 {
		var pretty map[string]any
		_ = json.Unmarshal(d.Evidence, &pretty)
		b, _ := json.MarshalIndent(pretty, "", "  ")
		op.f("evidence:\n%s\n\n", b)
	}
	op.ln("audit trail:")
	for _, h := range d.History {
		op.f("  %s  %-11s by %-14s %s\n", time.Unix(0, h.At).UTC().Format(time.RFC3339), h.Action, h.Actor, h.Reason)
	}
	return op.err
}

func cmdDecide(ctx context.Context, args []string, out io.Writer, resolve bool) error {
	name := map[bool]string{true: "resolve", false: "reopen"}[resolve]
	var c common
	fs := newFlags(name, &c, true)
	by := fs.String("by", "", "who is deciding (required; recorded in the audit trail)")
	reason := fs.String("reason", "", "why (required)")
	raw, rest := idArg(args)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if raw == "" && fs.NArg() > 0 {
		raw = fs.Arg(0)
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fmt.Errorf("usage: recond %s ID -by NAME -reason TEXT", name)
	}
	svc, store, err := open(ctx, c)
	if err != nil {
		return err
	}
	defer closeStore(store)
	fn := svc.Reopen
	if resolve {
		fn = svc.Resolve
	}
	e, err := fn(ctx, id, *by, *reason)
	if err != nil {
		return err
	}
	if c.json {
		return json.NewEncoder(out).Encode(e)
	}
	op := &printer{w: out}
	op.f("exception #%d is now %s (by %s)\n", e.ID, e.Status, strings.TrimSpace(*by))
	return op.err
}

// ---- payment / config ----

func cmdPayment(ctx context.Context, args []string, out io.Writer) error {
	var c common
	fs := newFlags("payment", &c, false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("usage: recond payment PROVIDER REF")
	}
	svc, store, err := open(ctx, c)
	if err != nil {
		return err
	}
	defer closeStore(store)
	p, hist, err := svc.Payment(ctx, fs.Arg(0), fs.Arg(1))
	if err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{"status": p.Status(), "state": p, "events_in_arrival_order": hist})
}

func cmdConfig(args []string, out io.Writer) error {
	var c common
	fs := newFlags("config", &c, false) // -db accepted for uniformity; rules do not live in the database
	if err := fs.Parse(args); err != nil {
		return err
	}
	rules, err := config.LoadRules(os.Getenv("RECOND_PROVIDERS_FILE"))
	if err != nil {
		return err
	}
	b, err := rules.Canonical()
	if err != nil {
		return err
	}
	var v any
	_ = json.Unmarshal(b, &v)
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// num formats an integer with thousands separators.
func num(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		return "-" + s
	}
	return s
}

// printer writes CLI output and remembers the first write error, so a
// command checks once at the end instead of after every line.
type printer struct {
	w   io.Writer
	err error
}

func (p *printer) f(format string, a ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprintf(p.w, format, a...)
	}
}

func (p *printer) ln(a ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprintln(p.w, a...)
	}
}

// flush flushes a tabwriter that p wrote to.
func (p *printer) flush(tw *tabwriter.Writer) error {
	if err := tw.Flush(); err != nil {
		return err
	}
	return p.err
}

// closeStore closes the database; by then every write has committed, so a
// close error can only be reported, not acted on.
func closeStore(s *sqlite.Store) {
	if err := s.Close(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "recond: close database:", err)
	}
}
