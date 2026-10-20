package domain

import (
	"errors"
	"testing"
)

func TestParseAmount(t *testing.T) {
	cases := []struct {
		name string
		in   string
		f    NumberFormat
		exp  int
		want int64
		err  error
	}{
		{"plain dot", "1500000.00", DotDecimal, 2, 150000000, nil},
		{"grouped dot", "1,500,000.00", DotDecimal, 2, 150000000, nil},
		{"indonesian", "1.500.000,00", CommaDecimal, 2, 150000000, nil},
		{"indonesian ungrouped", "1500000,00", CommaDecimal, 2, 150000000, nil},
		{"no fraction", "150000", DotDecimal, 2, 15000000, nil},
		{"short fraction", "12.5", DotDecimal, 2, 1250, nil},
		{"trailing zeros beyond exponent", "12.500", DotDecimal, 2, 1250, nil},
		{"zero exponent currency", "15000", DotDecimal, 0, 15000, nil},
		{"zero exponent with zero cents", "15000.00", DotDecimal, 0, 15000, nil},
		{"negative sign", "-1.234,56", CommaDecimal, 2, -123456, nil},
		{"accounting parens", "(1,234.56)", DotDecimal, 2, -123456, nil},
		{"plus sign", "+7.00", DotDecimal, 2, 700, nil},
		{"surrounding space", "  42.10 ", DotDecimal, 2, 4210, nil},
		{"small grouped", "1.500", CommaDecimal, 2, 150000, nil}, // fifteen hundred, not 1.5
		{"same text other format", "1.500", DotDecimal, 2, 150, nil},
		{"max", "10000000000000.00", DotDecimal, 2, MaxAmount, nil},

		{"empty", "", DotDecimal, 2, 0, ErrAmountSyntax},
		{"sign only", "-", DotDecimal, 2, 0, ErrAmountSyntax},
		{"precision loss", "12.345", DotDecimal, 2, 0, ErrAmountPrecision},
		{"precision loss jpy", "100.5", DotDecimal, 0, 0, ErrAmountPrecision},
		{"bad grouping", "1.50.000,00", CommaDecimal, 2, 0, ErrAmountSyntax},
		{"leading group too long", "1500.000,00", CommaDecimal, 2, 0, ErrAmountSyntax},
		{"trailing group short", "1,500,00.00", DotDecimal, 2, 0, ErrAmountSyntax},
		{"leading separator", ".500", CommaDecimal, 2, 0, ErrAmountSyntax},
		{"double decimal", "1.000,00,00", CommaDecimal, 2, 0, ErrAmountSyntax},
		{"trailing decimal", "100.", DotDecimal, 2, 0, ErrAmountSyntax},
		{"no integer digits", ".50", DotDecimal, 2, 0, ErrAmountSyntax},
		{"letters", "12a.00", DotDecimal, 2, 0, ErrAmountSyntax},
		{"currency symbol", "Rp1.000,00", CommaDecimal, 2, 0, ErrAmountSyntax},
		{"double sign", "--5", DotDecimal, 2, 0, ErrAmountSyntax},
		{"wrong decimal for format", "1500000,00", DotDecimal, 2, 0, ErrAmountSyntax},
		{"overflow", "10000000000000.01", DotDecimal, 2, 0, ErrAmountRange},
		{"huge", "99999999999999999999999", DotDecimal, 2, 0, ErrAmountRange},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseAmount(tc.in, tc.f, tc.exp)
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("ParseAmount(%q) err = %v, want %v", tc.in, err, tc.err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("ParseAmount(%q) = %d, %v; want %d", tc.in, got, err, tc.want)
			}
		})
	}
}

func TestFormatAmount(t *testing.T) {
	cases := []struct {
		minor int64
		exp   int
		f     NumberFormat
		group bool
		want  string
	}{
		{150000000, 2, CommaDecimal, true, "1.500.000,00"},
		{150000000, 2, DotDecimal, false, "1500000.00"},
		{150000000, 2, DotDecimal, true, "1,500,000.00"},
		{5, 2, DotDecimal, true, "0.05"},
		{0, 2, DotDecimal, true, "0.00"},
		{-123456, 2, CommaDecimal, true, "-1.234,56"},
		{15000, 0, DotDecimal, true, "15,000"},
		{999, 0, DotDecimal, true, "999"},
	}
	for _, tc := range cases {
		if got := FormatAmount(tc.minor, tc.exp, tc.f, tc.group); got != tc.want {
			t.Errorf("FormatAmount(%d) = %q, want %q", tc.minor, got, tc.want)
		}
	}
}

// FuzzParseAmount: the parser must never panic, never exceed MaxAmount, and
// anything it accepts must survive a format/parse round trip unchanged.
func FuzzParseAmount(f *testing.F) {
	for _, s := range []string{"1.500.000,00", "1500000.00", "(1,234.56)", "-0,5", "12.345", "", "1..2", "9999999999999999999"} {
		f.Add(s, true, uint8(2))
	}
	f.Fuzz(func(t *testing.T, s string, comma bool, exp uint8) {
		nf := DotDecimal
		if comma {
			nf = CommaDecimal
		}
		e := int(exp % 4)
		v, err := ParseAmount(s, nf, e)
		if err != nil {
			return
		}
		if v > MaxAmount || v < -MaxAmount {
			t.Fatalf("accepted out-of-range %q -> %d", s, v)
		}
		for _, group := range []bool{false, true} {
			out := FormatAmount(v, e, nf, group)
			back, err := ParseAmount(out, nf, e)
			if err != nil || back != v {
				t.Fatalf("round trip %q -> %d -> %q -> %d (%v)", s, v, out, back, err)
			}
		}
	})
}

func FuzzFormatRoundTrip(f *testing.F) {
	f.Add(int64(150000000), uint8(2), false)
	f.Add(int64(-1), uint8(0), true)
	f.Fuzz(func(t *testing.T, v int64, exp uint8, comma bool) {
		if v > MaxAmount || v < -MaxAmount {
			return
		}
		nf := DotDecimal
		if comma {
			nf = CommaDecimal
		}
		e := int(exp % 4)
		for _, group := range []bool{false, true} {
			s := FormatAmount(v, e, nf, group)
			back, err := ParseAmount(s, nf, e)
			if err != nil || back != v {
				t.Fatalf("%d -> %q -> %d (%v)", v, s, back, err)
			}
		}
	})
}
