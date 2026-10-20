package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/dwijamahardika/recond/internal/adapters/httpapi"
	"github.com/dwijamahardika/recond/internal/adapters/sqlite"
	"github.com/dwijamahardika/recond/internal/app"
	"github.com/dwijamahardika/recond/internal/config"
	"github.com/dwijamahardika/recond/internal/gen"
	"github.com/dwijamahardika/recond/internal/platform/clock"
)

var (
	now    = time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	secret = []byte(strings.Repeat("test-only-secret-", 2))
	apiKey = strings.Repeat("k", 24)
)

type env struct {
	srv *httptest.Server
	svc *app.Service
	clk *clock.Fake
}

func setup(t *testing.T) *env {
	t.Helper()
	rules, err := config.LoadRules("")
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	reg := prometheus.NewRegistry()
	m := httpapi.NewMetrics(reg)
	clk := clock.NewFake(now)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := app.NewService(store, rules, clk, log, m)
	h := httpapi.NewHandler(httpapi.Deps{
		Service: svc, Metrics: m, Registry: reg, Logger: log, Clock: clk,
		APIKeys:        map[string]string{apiKey: "ops-team"},
		WebhookSecrets: map[string][]byte{"psp_a": secret}, WebhookTolerance: 5 * time.Minute,
		Ready: store.Ping,
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &env{srv: srv, svc: svc, clk: clk}
}

func (e *env) do(t *testing.T, method, path, body string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, e.srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func signed(body string, ts time.Time, key []byte) map[string]string {
	t := strconv.FormatInt(ts.Unix(), 10)
	return map[string]string{httpapi.HeaderTimestamp: t, httpapi.HeaderSignature: httpapi.Sign(key, t, []byte(body)), "Content-Type": "application/json"}
}

const paidBody = `{"event_id":"evt_1","payment_ref":"ORD-1","status":"paid","amount":150000,"currency":"IDR","sequence":2,"occurred_at":"2026-10-10T08:59:00Z"}`

func TestWebhookSignature(t *testing.T) {
	e := setup(t)
	tampered := strings.Replace(paidBody, "150000", "999999", 1)
	rotated := signed(paidBody, now, secret)
	rotated[httpapi.HeaderSignature] = httpapi.Sign([]byte("old-secret-being-rotated-out"), rotated[httpapi.HeaderTimestamp], []byte(paidBody)) + ", " + rotated[httpapi.HeaderSignature]
	cases := []struct {
		name     string
		provider string
		body     string
		hdr      map[string]string
		status   int
		contains string
	}{
		{"valid", "psp_a", paidBody, signed(paidBody, now, secret), 200, `"verdict":"applied"`},
		{"replayed delivery is idempotent", "psp_a", paidBody, signed(paidBody, now, secret), 200, `"duplicate":true`},
		{"rotation: any listed signature", "psp_a", paidBody, rotated, 200, `"duplicate":true`},
		{"tampered body", "psp_a", tampered, signed(paidBody, now, secret), 401, ""},
		{"wrong secret", "psp_a", paidBody, signed(paidBody, now, []byte("not-the-secret-at-all")), 401, ""},
		{"stale timestamp (replay)", "psp_a", paidBody, signed(paidBody, now.Add(-6*time.Minute), secret), 401, ""},
		{"future timestamp", "psp_a", paidBody, signed(paidBody, now.Add(6*time.Minute), secret), 401, ""},
		{"missing headers", "psp_a", paidBody, nil, 401, ""},
		{"provider without secret fails closed", "bank_b", paidBody, signed(paidBody, now, secret), 401, ""},
		{"unknown provider", "psp_zz", paidBody, signed(paidBody, now, secret), 401, ""},
		{"valid signature, invalid event", "psp_a", `{"event_id":"x"}`, signed(`{"event_id":"x"}`, now, secret), 400, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := e.do(t, "POST", "/v1/webhooks/"+tc.provider, tc.body, tc.hdr)
			if resp.StatusCode != tc.status || !strings.Contains(body, tc.contains) {
				t.Fatalf("status %d body %s", resp.StatusCode, body)
			}
		})
	}
	big := `{"event_id":"` + strings.Repeat("a", 70<<10) + `"}`
	if resp, _ := e.do(t, "POST", "/v1/webhooks/psp_a", big, signed(big, now, secret)); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: %d", resp.StatusCode)
	}
	_, metrics := e.do(t, "GET", "/metrics", "", nil)
	for _, want := range []string{`recond_webhook_events_total{provider="psp_a",result="applied"} 1`, `result="duplicate"} 2`, `result="stale_timestamp"} 2`} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics missing %s", want)
		}
	}
}

func TestVerifyUnit(t *testing.T) {
	ts := strconv.FormatInt(now.Unix(), 10)
	sig := httpapi.Sign(secret, ts, []byte("b"))
	if err := httpapi.Verify(secret, ts, sig, []byte("b"), time.Minute, now); err != nil {
		t.Fatal(err)
	}
	if err := httpapi.Verify(secret, "abc", sig, []byte("b"), time.Minute, now); err == nil {
		t.Fatal("non-numeric timestamp accepted")
	}
	if err := httpapi.Verify(secret, ts, strings.ToUpper(sig), []byte("b"), time.Minute, now); err == nil {
		t.Fatal("signature compare must be exact")
	}
}

func TestOutOfOrderOverHTTP(t *testing.T) {
	e := setup(t)
	pending := `{"event_id":"evt_0","payment_ref":"ORD-1","status":"pending","amount":150000,"currency":"IDR","sequence":1,"occurred_at":"2026-10-10T08:58:00Z"}`
	e.do(t, "POST", "/v1/webhooks/psp_a", paidBody, signed(paidBody, now, secret))
	resp, body := e.do(t, "POST", "/v1/webhooks/psp_a", pending, signed(pending, now, secret))
	if resp.StatusCode != 200 || !strings.Contains(body, `"out_of_order":true`) || !strings.Contains(body, `"status_after":"paid"`) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	resp, body = e.do(t, "GET", "/v1/payments/psp_a/ORD-1", "", map[string]string{httpapi.HeaderAPIKey: apiKey})
	if resp.StatusCode != 200 || !strings.Contains(body, `"status":"paid"`) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

func TestManagementAPI(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	auth := map[string]string{httpapi.HeaderAPIKey: apiKey}

	for _, path := range []string{"/v1/runs", "/v1/exceptions", "/v1/payments/psp_a/x"} {
		if resp, _ := e.do(t, "GET", path, "", nil); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s without key: %d", path, resp.StatusCode)
		}
		if resp, _ := e.do(t, "GET", path, "", map[string]string{httpapi.HeaderAPIKey: apiKey + "x"}); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s with wrong key: %d", path, resp.StatusCode)
		}
	}
	if resp, _ := e.do(t, "GET", "/healthz", "", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatal("healthz")
	}
	if resp, _ := e.do(t, "GET", "/readyz", "", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatal("readyz")
	}

	// Load a small generated world through the service, then drive the API.
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
	resp, body := e.do(t, "POST", "/v1/runs", fmt.Sprintf(`{"as_of":%q}`, ds.Truth.AsOf.Format(time.RFC3339)), auth)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create run: %d %s", resp.StatusCode, body)
	}
	var sum app.Summary
	if err := json.Unmarshal([]byte(body), &sum); err != nil || sum.RunID != 1 || sum.Discrepancies == 0 {
		t.Fatalf("%+v %v", sum, err)
	}
	if resp, body := e.do(t, "GET", "/v1/runs/latest", "", auth); resp.StatusCode != 200 || !strings.Contains(body, sum.ResultDigest) {
		t.Fatalf("latest: %d", resp.StatusCode)
	}
	if resp, body := e.do(t, "GET", "/v1/runs/1/items.csv", "", auth); resp.StatusCode != 200 || !strings.HasPrefix(body, "run_id,outcome_id,class") {
		t.Fatalf("items: %d %.80s", resp.StatusCode, body)
	}
	if resp, body := e.do(t, "POST", "/v1/runs/1/verify", "", auth); resp.StatusCode != 200 || !strings.Contains(body, `"reproduced":true`) {
		t.Fatalf("verify: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.do(t, "GET", "/v1/runs/99", "", auth); resp.StatusCode != http.StatusNotFound {
		t.Fatal("missing run should 404")
	}

	resp, body = e.do(t, "GET", "/v1/exceptions?status=open&limit=2", "", auth)
	var list struct {
		Data      []sqlite.Exception `json:"data"`
		NextAfter int64              `json:"next_after"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil || resp.StatusCode != 200 || len(list.Data) != 2 || list.NextAfter != list.Data[1].ID {
		t.Fatalf("list: %d %s", resp.StatusCode, body)
	}
	id := strconv.FormatInt(list.Data[0].ID, 10)
	if resp, body := e.do(t, "GET", "/v1/exceptions/"+id, "", auth); resp.StatusCode != 200 || !strings.Contains(body, `"evidence"`) {
		t.Fatalf("detail: %d", resp.StatusCode)
	}
	// The resolver is the authenticated principal; a client-supplied actor is refused.
	if resp, _ := e.do(t, "POST", "/v1/exceptions/"+id+"/resolve", `{"reason":"x","actor":"someone-else"}`, auth); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown fields must be rejected: %d", resp.StatusCode)
	}
	if resp, body := e.do(t, "POST", "/v1/exceptions/"+id+"/resolve", `{"reason":"refund confirmed by provider"}`, auth); resp.StatusCode != 200 || !strings.Contains(body, `"status":"resolved"`) {
		t.Fatalf("resolve: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.do(t, "POST", "/v1/exceptions/"+id+"/resolve", `{"reason":"again"}`, auth); resp.StatusCode != http.StatusConflict {
		t.Fatalf("double resolve: %d", resp.StatusCode)
	}
	_, body = e.do(t, "GET", "/v1/exceptions/"+id, "", auth)
	if !strings.Contains(body, `"actor":"ops-team"`) {
		t.Fatalf("audit trail must name the principal: %s", body)
	}
}
