package match

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dwijamahardika/recond/internal/domain"
)

// EngineVersion is recorded with every run. Bump it whenever matching
// semantics change so a replay of an old run reports the difference instead
// of silently "verifying" against new rules.
const EngineVersion = "1"

// Config is everything that determines a run's result besides its inputs.
// It is stored verbatim with each run; replays use the stored copy.
type Config struct {
	Note      string          `json:"_note,omitempty"`
	Providers []ProviderRules `json:"providers"`
	// BatchSearchBudget caps the subset-search nodes per payout.
	BatchSearchBudget int `json:"batch_search_budget,omitempty"`

	byName map[string]*ProviderRules
}

// ProviderRules are the settlement terms recond expects from one provider.
// The example values shipped with recond describe fictional providers.
type ProviderRules struct {
	Name   string `json:"name"`
	Format string `json:"format"` // settlement file adapter
	// Timezone the provider's business dates are in: a fixed offset ("+07:00")
	// or an IANA name.
	Timezone string `json:"timezone"`
	// SettlementDays is N in T+N, in calendar days of Timezone.
	SettlementDays int `json:"settlement_days"`
	// MatchWindow bounds |ledger time - provider time| for tolerance matches.
	MatchWindow Duration `json:"match_window"`
	// AmountToleranceMinor: gross difference still considered equal (0 = exact).
	AmountToleranceMinor int64 `json:"amount_tolerance_minor"`
	// FeeToleranceMinor absorbs per-transaction rounding differences.
	FeeToleranceMinor int64 `json:"fee_tolerance_minor"`
	// BatchToleranceMinor: payout total difference still considered equal.
	BatchToleranceMinor int64 `json:"batch_tolerance_minor"`
	// MaxBatchExclusions bounds how many transactions the subset search may
	// exclude from a payout's candidate window.
	MaxBatchExclusions int `json:"max_batch_exclusions"`
	// Fees per currency.
	Fees map[domain.Currency]domain.FeeRule `json:"fees"`

	loc *time.Location
}

// Duration is a time.Duration that reads and writes as "10m" in JSON.
type Duration time.Duration

// MarshalText renders the duration as "10m0s".
func (d Duration) MarshalText() ([]byte, error) { return []byte(time.Duration(d).String()), nil }

// UnmarshalText parses a Go duration string.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// Location returns the provider's timezone (valid after Config.Validate).
func (p *ProviderRules) Location() *time.Location {
	if p.loc == nil {
		return time.UTC
	}
	return p.loc
}

// FeeFor returns the fee rule for a currency.
func (p *ProviderRules) FeeFor(c domain.Currency) (domain.FeeRule, bool) {
	r, ok := p.Fees[c]
	return r, ok
}

// ParseZone accepts "+07:00"/"-03:30"/"Z" or an IANA name.
func ParseZone(s string) (*time.Location, error) {
	switch {
	case s == "" || s == "Z" || strings.EqualFold(s, "UTC"):
		return time.UTC, nil
	case s[0] == '+' || s[0] == '-':
		t, err := time.Parse("-07:00", s)
		if err != nil {
			return nil, fmt.Errorf("timezone %q: want +HH:MM", s)
		}
		_, off := t.Zone()
		return time.FixedZone("UTC"+s, off), nil
	}
	return time.LoadLocation(s)
}

// Validate checks the config, fills defaults and builds lookup tables.
func (c *Config) Validate() error {
	var errs []error
	c.byName = make(map[string]*ProviderRules, len(c.Providers))
	if c.BatchSearchBudget <= 0 {
		c.BatchSearchBudget = 2_000_000
	}
	for i := range c.Providers {
		p := &c.Providers[i]
		pfx := fmt.Sprintf("providers[%d] %q", i, p.Name)
		if p.Name == "" || strings.ContainsAny(p.Name, " \x00:/") {
			errs = append(errs, fmt.Errorf("%s: name required (no spaces, ':' or '/')", pfx))
		}
		if _, dup := c.byName[p.Name]; dup {
			errs = append(errs, fmt.Errorf("%s: duplicate provider", pfx))
		}
		c.byName[p.Name] = p
		loc, err := ParseZone(p.Timezone)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", pfx, err))
		}
		p.loc = loc
		if p.SettlementDays < 0 || p.SettlementDays > 60 {
			errs = append(errs, fmt.Errorf("%s: settlement_days must be 0..60", pfx))
		}
		if p.MatchWindow <= 0 {
			p.MatchWindow = Duration(10 * time.Minute)
		}
		if p.MaxBatchExclusions <= 0 {
			p.MaxBatchExclusions = 3
		}
		if p.AmountToleranceMinor < 0 || p.FeeToleranceMinor < 0 || p.BatchToleranceMinor < 0 {
			errs = append(errs, fmt.Errorf("%s: tolerances must be >= 0", pfx))
		}
		if len(p.Fees) == 0 {
			errs = append(errs, fmt.Errorf("%s: at least one fee rule required", pfx))
		}
		for cur, r := range p.Fees {
			if !cur.Valid() {
				errs = append(errs, fmt.Errorf("%s: fee currency %q unsupported", pfx, cur))
			}
			if err := r.Validate(); err != nil {
				errs = append(errs, fmt.Errorf("%s fee %s: %w", pfx, cur, err))
			}
		}
	}
	if len(c.Providers) == 0 {
		errs = append(errs, errors.New("no providers configured"))
	}
	return errors.Join(errs...)
}

// Provider looks up a provider's rules.
func (c *Config) Provider(name string) (*ProviderRules, bool) {
	p, ok := c.byName[name]
	return p, ok
}

// Names returns provider names in sorted order.
func (c *Config) Names() []string {
	out := make([]string, 0, len(c.Providers))
	for _, p := range c.Providers {
		out = append(out, p.Name)
	}
	sort.Strings(out)
	return out
}

// Canonical returns the config as stable JSON (providers sorted by name, map
// keys sorted by encoding/json) - the form that is stored and hashed per run.
func (c *Config) Canonical() ([]byte, error) {
	cp := *c
	cp.Providers = append([]ProviderRules(nil), c.Providers...)
	sort.Slice(cp.Providers, func(i, j int) bool { return cp.Providers[i].Name < cp.Providers[j].Name })
	return json.Marshal(cp)
}

// ParseConfig decodes and validates a JSON config.
func ParseConfig(b []byte) (*Config, error) {
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("provider config: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("provider config: %w", err)
	}
	return &c, nil
}
