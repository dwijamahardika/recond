// Package config reads 12-factor environment variables and the provider rules
// file, validating everything up front so a misconfiguration fails at start,
// not halfway through a run.
package config

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dwijamahardika/recond/internal/ingest"
	"github.com/dwijamahardika/recond/internal/match"
)

// defaultProviders are the example rules for the fictional providers psp_a
// and bank_b, used when RECOND_PROVIDERS_FILE is unset.
//
//go:embed providers.json
var defaultProviders []byte

// Config is the process configuration for `recond serve`.
type Config struct {
	DBPath        string
	Addr          string // API + webhooks
	AdminAddr     string // pprof + metrics; empty disables
	ProvidersFile string
	LogLevel      string

	ShutdownTimeout time.Duration
	// APIKeys maps key -> principal for the /v1 management API. The principal
	// is what the exception audit trail records as the resolver.
	APIKeys map[string]string
	// WebhookSecrets maps provider -> HMAC secret, from
	// RECOND_WEBHOOK_SECRET_<PROVIDER>. A provider without a secret cannot
	// deliver webhooks (fail closed).
	WebhookSecrets   map[string][]byte
	WebhookTolerance time.Duration

	Rules *match.Config
}

// MinSecretLen rejects secrets too short to be worth the HMAC.
const MinSecretLen = 16

// Load reads the environment and the provider rules, reporting every problem at once.
func Load() (*Config, error) {
	c := &Config{
		DBPath:           env("RECOND_DB", "recond.db"),
		Addr:             env("RECOND_ADDR", "127.0.0.1:8080"),
		AdminAddr:        env("RECOND_ADMIN_ADDR", ""),
		ProvidersFile:    env("RECOND_PROVIDERS_FILE", ""),
		LogLevel:         env("RECOND_LOG_LEVEL", "info"),
		ShutdownTimeout:  envDur("RECOND_SHUTDOWN_TIMEOUT", 20*time.Second),
		WebhookTolerance: envDur("RECOND_WEBHOOK_TOLERANCE", 5*time.Minute),
		APIKeys:          map[string]string{},
		WebhookSecrets:   map[string][]byte{},
	}
	var errs []error

	rules, err := LoadRules(c.ProvidersFile)
	if err != nil {
		errs = append(errs, err)
	}
	c.Rules = rules

	for i, pair := range strings.Split(env("RECOND_API_KEYS", ""), ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		key, name, found := strings.Cut(pair, ":")
		if !found || name == "" {
			name = fmt.Sprintf("client-%d", i+1)
		}
		if len(key) < MinSecretLen {
			errs = append(errs, fmt.Errorf("RECOND_API_KEYS entry %d: key shorter than %d chars", i+1, MinSecretLen))
			continue
		}
		c.APIKeys[key] = name
	}
	if rules != nil {
		for _, name := range rules.Names() {
			k := "RECOND_WEBHOOK_SECRET_" + strings.ToUpper(name)
			v := os.Getenv(k)
			if v == "" {
				continue
			}
			if len(v) < MinSecretLen {
				errs = append(errs, fmt.Errorf("%s shorter than %d chars", k, MinSecretLen))
				continue
			}
			c.WebhookSecrets[name] = []byte(v)
		}
	}
	if c.WebhookTolerance <= 0 || c.WebhookTolerance > time.Hour {
		errs = append(errs, errors.New("RECOND_WEBHOOK_TOLERANCE must be in (0, 1h]"))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}

// LoadRules reads provider rules from path, or the embedded defaults if path
// is empty.
func LoadRules(path string) (*match.Config, error) {
	b := defaultProviders
	if path != "" {
		var err error
		if b, err = os.ReadFile(path); err != nil { //nolint:gosec // operator-supplied config path
			return nil, fmt.Errorf("RECOND_PROVIDERS_FILE: %w", err)
		}
	}
	rules, err := match.ParseConfig(b)
	if err != nil {
		return nil, err
	}
	var errs []error
	for _, p := range rules.Providers {
		if _, ok := ingest.AdapterFor(p.Format); !ok {
			errs = append(errs, fmt.Errorf("provider %q: unknown format %q (have %s)", p.Name, p.Format, strings.Join(ingest.Formats(), ", ")))
		}
	}
	return rules, errors.Join(errs...)
}

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(env(k, "")); err == nil {
		return v
	}
	return def
}
