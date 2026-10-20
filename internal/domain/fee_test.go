package domain

import (
	"testing"
	"time"
)

func TestFeeRule(t *testing.T) {
	cases := []struct {
		name  string
		rule  FeeRule
		gross int64
		want  int64
	}{
		{"zero gross", FeeRule{RateBPS: 150}, 0, 0},
		{"exact", FeeRule{RateBPS: 150}, 1_000_000, 15_000},
		{"half up rounds .5 up", FeeRule{RateBPS: 150}, 100, 2},                          // 1.5 -> 2
		{"half up keeps .49", FeeRule{RateBPS: 149}, 100, 1},                             // 1.49 -> 1
		{"half even .5 to even", FeeRule{RateBPS: 250, Rounding: RoundHalfEven}, 100, 2}, // 2.5 -> 2
		{"half even .5 odd", FeeRule{RateBPS: 150, Rounding: RoundHalfEven}, 100, 2},     // 1.5 -> 2
		{"down", FeeRule{RateBPS: 199, Rounding: RoundDown}, 100, 1},
		{"up", FeeRule{RateBPS: 101, Rounding: RoundUp}, 100, 2},
		{"fixed component", FeeRule{RateBPS: 90, FixedMinor: 20_000}, 1_000_000, 29_000},
		{"capped at gross", FeeRule{RateBPS: 0, FixedMinor: 50_000}, 10_000, 10_000},
		{"no overflow at max", FeeRule{RateBPS: 10_000}, MaxAmount, MaxAmount},
		{"invalid rate clamped, no panic", FeeRule{RateBPS: 1 << 40}, MaxAmount, MaxAmount},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rule.Fee(tc.gross); got != tc.want {
				t.Fatalf("Fee(%d) = %d, want %d", tc.gross, got, tc.want)
			}
		})
	}
}

func TestFeeRuleValidate(t *testing.T) {
	if err := (FeeRule{RateBPS: 150, Rounding: RoundHalfUp}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []FeeRule{{RateBPS: -1}, {RateBPS: 10_001}, {FixedMinor: -5}, {Rounding: "bankers"}} {
		if bad.Validate() == nil {
			t.Errorf("expected %+v to be invalid", bad)
		}
	}
}

func TestDate(t *testing.T) {
	wib := time.FixedZone("WIB", 7*3600)
	// 18:30 UTC on the 12th is 01:30 on the 13th in Jakarta.
	ts := time.Date(2026, 10, 12, 18, 30, 0, 0, time.UTC)
	if got := DateOf(ts, time.UTC).String(); got != "2026-10-12" {
		t.Fatalf("utc date %s", got)
	}
	if got := DateOf(ts, wib).String(); got != "2026-10-13" {
		t.Fatalf("wib date %s", got)
	}
	d, err := ParseDate("02/01/2006", "28/02/2026")
	if err != nil || d.AddDays(1).String() != "2026-03-01" {
		t.Fatalf("parse/add: %v %v", d, err)
	}
	if !d.StartIn(wib).Equal(time.Date(2026, 2, 28, 0, 0, 0, 0, wib)) {
		t.Fatal("StartIn")
	}
	var back Date
	if err := back.UnmarshalText([]byte(d.String())); err != nil || back != d {
		t.Fatal("text round trip")
	}
}

func TestNormalizeRef(t *testing.T) {
	for in, want := range map[string]string{
		"ORD-1":        "ORD-1",
		"  ord-1  ":    "ORD-1",
		"Ord-20261012": "ORD-20261012",
		"":             "",
	} {
		if got := NormalizeRef(in); got != want {
			t.Errorf("NormalizeRef(%q) = %q, want %q", in, got, want)
		}
	}
}
