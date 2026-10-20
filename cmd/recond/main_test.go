package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dwijamahardika/recond/internal/adapters/sqlite"
	"github.com/dwijamahardika/recond/internal/config"
	"github.com/dwijamahardika/recond/internal/gen"
)

// TestCLIWorkflow drives the commands an operator would type, in order.
func TestCLIWorkflow(t *testing.T) {
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
	sh := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		if err := dispatch(ctx, args[0], append(args[1:], "-db", db), &out); err != nil {
			t.Fatalf("recond %s: %v\n%s", strings.Join(args, " "), err, out.String())
		}
		return out.String()
	}
	out := sh("ingest", "-dir", data)
	if !strings.Contains(out, "ledger.csv") || !strings.Contains(out, "webhooks.jsonl") {
		t.Fatalf("ingest output:\n%s", out)
	}
	if out := sh("ingest", "-dir", data); strings.Count(out, "already ingested") < 3 {
		t.Fatalf("re-ingest should be a no-op:\n%s", out)
	}
	out = sh("run", "-as-of", ds.Truth.AsOf.Format(time.RFC3339))
	if !strings.Contains(out, "run #1") || !strings.Contains(out, "missing_in_ledger") {
		t.Fatalf("run output:\n%s", out)
	}
	if out := sh("verify", "1"); !strings.Contains(out, "match") || strings.Contains(out, "MISMATCH") {
		t.Fatalf("verify output:\n%s", out)
	}
	report := filepath.Join(dir, "reports")
	if out := sh("report", "-out", report); !strings.Contains(out, "items.csv") {
		t.Fatalf("report output:\n%s", out)
	}
	var list []sqlite.Exception
	if err := json.Unmarshal([]byte(sh("exceptions", "-json")), &list); err != nil || len(list) == 0 {
		t.Fatalf("exceptions: %v %d", err, len(list))
	}
	id := strconv.FormatInt(list[0].ID, 10)
	if out := sh("resolve", id, "-by", "ops-1", "-reason", "confirmed with provider"); !strings.Contains(out, "resolved") {
		t.Fatalf("resolve output: %s", out)
	}
	out = sh("exceptions", "show", id)
	if !strings.Contains(out, "resolved    by ops-1") || !strings.Contains(out, `"raw"`) && !strings.Contains(out, `"ledger"`) {
		t.Fatalf("show output:\n%s", out)
	}
	if out := sh("reopen", id, "-by", "ops-2", "-reason", "adjustment never arrived"); !strings.Contains(out, "open") {
		t.Fatalf("reopen output: %s", out)
	}
	if out := sh("runs"); !strings.Contains(out, "DIGEST") {
		t.Fatalf("runs output:\n%s", out)
	}
	if out := sh("config"); !strings.Contains(out, `"psp_a"`) {
		t.Fatalf("config output:\n%s", out)
	}
	var stdout bytes.Buffer
	if err := dispatch(ctx, "nope", nil, &stdout); err == nil {
		t.Fatal("unknown command must fail")
	}
}

func TestPlanDirNeedsProviderPrefix(t *testing.T) {
	dir := t.TempDir()
	if _, err := planDir(dir, []string{"psp_a"}); err == nil {
		t.Fatal("empty dir must fail")
	}
}

func TestNum(t *testing.T) {
	for in, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", -1234567: "-1,234,567"} {
		if got := num(in); got != want {
			t.Errorf("num(%d) = %q, want %q", in, got, want)
		}
	}
}
