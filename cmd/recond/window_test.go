package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dwijamahardika/recond/internal/config"
	"github.com/dwijamahardika/recond/internal/gen"
)

// TestCLIWindowedRun drives a windowed close the way an operator would,
// with the feature flag off and then on.
func TestCLIWindowedRun(t *testing.T) {
	dir := t.TempDir()
	rules, err := config.LoadRules("")
	if err != nil {
		t.Fatal(err)
	}
	ds, err := gen.Generate(gen.Config{Seed: 9, Payments: 600, FaultRate: 0.1, Rules: rules})
	if err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "data")
	if _, err := ds.WriteDir(data); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "r.db")
	ctx := context.Background()
	sh := func(args ...string) (string, error) {
		var out bytes.Buffer
		err := dispatch(ctx, args[0], append(args[1:], "-db", db), &out)
		return out.String(), err
	}
	must := func(args ...string) string {
		t.Helper()
		out, err := sh(args...)
		if err != nil {
			t.Fatalf("recond %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	asOf := ds.Truth.AsOf.Format(time.RFC3339)
	must("ingest", "-dir", data)
	must("run", "-as-of", asOf)
	if out := must("runs"); strings.Contains(out, "WINDOW") {
		t.Fatalf("full-history runs list exactly as before:\n%s", out)
	}
	t.Setenv("RECOND_WINDOWED_RUNS", "off")
	if _, err := sh("run", "-from", "2026-10-01", "-to", "2026-10-02"); err == nil || !strings.Contains(err.Error(), "RECOND_WINDOWED_RUNS") {
		t.Fatalf("flag off: %v", err)
	}
	t.Setenv("RECOND_WINDOWED_RUNS", "")
	if out := must("migrations"); !strings.Contains(out, map[bool]string{true: "windowed runs: enabled", false: "windowed runs: disabled"}[config.DefaultWindowedRuns]) {
		t.Fatalf("unset must follow config.DefaultWindowedRuns:\n%s", out)
	}
	t.Setenv("RECOND_WINDOWED_RUNS", "on")
	for args, want := range map[string]string{
		"-from 2026-10-01":                              "-to",
		"-from 2026-10-02 -to 2026-10-01":               "before from",
		"-from 1/10/2026 -to 2026-10-02":                "YYYY-MM-DD",
		"-lookahead 2":                                  "need -from and -to",
		"-from 2026-10-01 -to 2026-10-02 -lookbehind 0": "lookbehind must be 1..31",
	} {
		if _, err := sh(append([]string{"run"}, strings.Fields(args)...)...); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("run %s: %v, want error containing %q", args, err, want)
		}
	}
	out := must("run", "-as-of", asOf, "-from", "2026-10-01", "-to", "2026-10-02", "-lookahead", "2")
	for _, want := range []string{"run #2", "window 2026-10-01..2026-10-02 (lookbehind 1d, lookahead 2d): 2 business day(s)",
		"bank_b   +07:00  2026-09-30T17:00:00Z .. 2026-10-02T17:00:00Z", "dated in the window:", "reported (members of groups this window owns)", "outside_window (counterpart beyond the margin): 0", "outside_window"} {
		if !strings.Contains(out, want) {
			t.Errorf("run output lacks %q:\n%s", want, out)
		}
	}
	if out := must("runs"); !strings.Contains(out, "WINDOW") || !strings.Contains(out, "2026-10-01..2026-10-02 -1/+2") || !strings.Contains(out, "  all  ") {
		t.Fatalf("runs:\n%s", out)
	}
	t.Setenv("RECOND_WINDOWED_RUNS", "off") // verify never needs the flag
	if out := must("verify", "2"); !strings.Contains(out, "window  2026-10-01..2026-10-02 (lookbehind 1d, lookahead 2d)") || strings.Contains(out, "MISMATCH") {
		t.Fatalf("verify:\n%s", out)
	}
	if out := must("migrations"); !strings.Contains(out, "0002_windowed_runs") || !strings.Contains(out, "migrate phase: complete") ||
		!strings.Contains(out, "windowed runs: disabled") {
		t.Fatalf("migrations:\n%s", out)
	}
	t.Setenv("RECOND_WINDOWED_RUNS", "maybe")
	if _, err := sh("runs"); err == nil || !strings.Contains(err.Error(), "RECOND_WINDOWED_RUNS") {
		t.Fatalf("a bad flag value must fail every command: %v", err)
	}
}
