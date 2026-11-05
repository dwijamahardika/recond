package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dwijamahardika/recond/internal/adapters/httpapi"
	"github.com/dwijamahardika/recond/internal/app"
	"github.com/dwijamahardika/recond/internal/config"
	"github.com/dwijamahardika/recond/internal/gen"
)

// load ingests a small generated world and returns its as-of.
func load(t *testing.T, e *env) time.Time {
	t.Helper()
	ctx := context.Background()
	rules, _ := config.LoadRules("")
	ds, err := gen.Generate(gen.Config{Seed: 2, Payments: 400, FaultRate: 0.2, Rules: rules})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	files, err := ds.WriteDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.IngestLedger(ctx, app.FileSource(filepath.Join(dir, gen.LedgerFile)), "", ""); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if _, err := e.svc.IngestSettlement(ctx, f.Provider, app.FileSource(filepath.Join(dir, f.Name))); err != nil {
			t.Fatal(err)
		}
	}
	return ds.Truth.AsOf
}

func TestWindowedRunsOverHTTP(t *testing.T) {
	auth := map[string]string{httpapi.HeaderAPIKey: apiKey}
	week := `"window":{"from":"2026-10-01","to":"2026-10-02"}`

	t.Run("flag off", func(t *testing.T) {
		e := setup(t)
		asOf := load(t, e).Format(time.RFC3339)
		resp, body := e.do(t, "POST", "/v1/runs", fmt.Sprintf(`{"as_of":%q,%s}`, asOf, week), auth)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "Windowed runs disabled") || !strings.Contains(body, "RECOND_WINDOWED_RUNS=on") {
			t.Fatalf("%d %s", resp.StatusCode, body)
		}
		if resp, body := e.do(t, "POST", "/v1/runs", fmt.Sprintf(`{"as_of":%q}`, asOf), auth); resp.StatusCode != http.StatusCreated || strings.Contains(body, `"window"`) {
			t.Fatalf("a full run is unchanged: %d %s", resp.StatusCode, body)
		}
	})

	t.Run("flag on", func(t *testing.T) {
		e := setup(t, app.WithWindowedRuns(true))
		asOf := load(t, e).Format(time.RFC3339)
		for req, want := range map[string]string{
			`{"window":{"from":"2026-10-02","to":"2026-10-01"}}`:                     "Invalid window",
			`{"window":{"from":"2026-10-01","to":"2026-10-02","lookahead_days":40}}`: "lookahead must be 1..31",
			`{"window":{}}`: "from and to are required",
			`{"window":{"from":"2026-13-01","to":"2026-10-02"}}`:               "Malformed JSON",
			`{"window":{"from":"2026-10-01","to":"2026-10-02","lookahead":2}}`: "Malformed JSON",
		} {
			if resp, body := e.do(t, "POST", "/v1/runs", req, auth); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, want) {
				t.Errorf("%s: %d %s, want 400 with %q", req, resp.StatusCode, body, want)
			}
		}
		resp, body := e.do(t, "POST", "/v1/runs", fmt.Sprintf(`{"as_of":%q,"window":{"from":"2026-10-01","to":"2026-10-02","lookahead_days":2}}`, asOf), auth)
		if resp.StatusCode != http.StatusCreated ||
			!strings.Contains(body, `"window":{"from":"2026-10-01","to":"2026-10-02","lookbehind_days":1,"lookahead_days":2,"days":2,"providers":[{"provider":"bank_b","timezone":"+07:00","from":"2026-09-30T17:00:00Z"`) ||
			!strings.Contains(body, `"outside_window":0`) || !strings.Contains(body, `"class":"outside_window"`) {
			t.Fatalf("windowed run: %d %s", resp.StatusCode, body)
		}
		if resp, body := e.do(t, "GET", "/v1/runs", "", auth); resp.StatusCode != 200 ||
			!strings.Contains(body, `"window":{"from":"2026-10-01","to":"2026-10-02","lookbehind_days":1,"lookahead_days":2}`) {
			t.Fatalf("list: %d %s", resp.StatusCode, body)
		}
		if resp, body := e.do(t, "POST", "/v1/runs/1/verify", "", auth); resp.StatusCode != 200 ||
			!strings.Contains(body, `"reproduced":true`) || !strings.Contains(body, `"window":{"from":"2026-10-01"`) {
			t.Fatalf("verify: %d %s", resp.StatusCode, body)
		}
		_, metrics := e.do(t, "GET", "/metrics", "", nil)
		for _, want := range []string{"recond_windowed_runs_total 1", "recond_last_run_window_days 2", "recond_last_run_outside_window 0",
			`recond_last_run_window_records{record="ledger",scope="window"}`, `recond_last_run_window_records{record="line",scope="margin"}`,
			"recond_last_run_window_groups_not_owned "} {
			if !strings.Contains(metrics, want) {
				t.Errorf("metrics lack %q", want)
			}
		}
		// A full run resets the last-run window gauges.
		if resp, _ := e.do(t, "POST", "/v1/runs", fmt.Sprintf(`{"as_of":%q}`, asOf), auth); resp.StatusCode != http.StatusCreated {
			t.Fatal(resp.StatusCode)
		}
		_, metrics = e.do(t, "GET", "/metrics", "", nil)
		if !strings.Contains(metrics, "recond_last_run_window_days 0") || strings.Contains(metrics, "recond_last_run_window_records{") ||
			!strings.Contains(metrics, "recond_windowed_runs_total 1") {
			t.Fatal("full run must reset the last-run window gauges")
		}
	})
}
