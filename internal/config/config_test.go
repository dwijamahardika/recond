package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != "127.0.0.1:8080" || c.AdminAddr != "" || c.WebhookTolerance != 5*time.Minute {
		t.Fatalf("listeners must default to loopback / disabled: %+v", c)
	}
	if got := c.Rules.Names(); strings.Join(got, ",") != "bank_b,psp_a" {
		t.Fatalf("providers %v", got)
	}
	if len(c.WebhookSecrets) != 0 || len(c.APIKeys) != 0 {
		t.Fatal("no secrets or keys may be configured by default")
	}
}

func TestSecretsAndKeys(t *testing.T) {
	t.Setenv("RECOND_WEBHOOK_SECRET_PSP_A", strings.Repeat("s", 32))
	t.Setenv("RECOND_API_KEYS", strings.Repeat("k", 24)+":ops")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.WebhookSecrets["psp_a"]) != 32 || c.WebhookSecrets["bank_b"] != nil {
		t.Fatalf("secrets %v", c.WebhookSecrets)
	}
	if c.APIKeys[strings.Repeat("k", 24)] != "ops" {
		t.Fatalf("keys %v", c.APIKeys)
	}

	t.Setenv("RECOND_WEBHOOK_SECRET_BANK_B", "short")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "RECOND_WEBHOOK_SECRET_BANK_B") {
		t.Fatalf("short secret must be rejected, got %v", err)
	}
}

func TestRulesFile(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "p.json")
	if err := os.WriteFile(bad, []byte(`{"providers":[{"name":"x","format":"psp_a","settlement_days":-1,"fees":{"IDR":{"rate_bps":20000}}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadRules(bad)
	if err == nil || !strings.Contains(err.Error(), "settlement_days") || !strings.Contains(err.Error(), "rate_bps") {
		t.Fatalf("want all validation errors at once, got %v", err)
	}
	unknown := filepath.Join(dir, "u.json")
	_ = os.WriteFile(unknown, []byte(`{"providers":[],"typo_field":1}`), 0o600)
	if _, err := LoadRules(unknown); err == nil {
		t.Fatal("unknown fields must be rejected")
	}
}
