package domain

import (
	"fmt"
	"testing"
	"time"
)

func BenchmarkParseAmount(b *testing.B) {
	for _, tc := range []struct {
		name string
		in   string
		f    NumberFormat
	}{
		{"comma_grouped", "1.500.000,00", CommaDecimal},
		{"dot_plain", "1500000.00", DotDecimal},
	} {
		b.Run(tc.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, err := ParseAmount(tc.in, tc.f, 2); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkFee(b *testing.B) {
	r := FeeRule{RateBPS: 90, FixedMinor: 20_000}
	var sink int64
	for i := 0; i < b.N; i++ {
		sink += r.Fee(int64(i%5_000_000) * 100)
	}
	_ = sink
}

// BenchmarkFold: a typical payment's events delivered shuffled with a duplicate.
func BenchmarkFold(b *testing.B) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	mk := func(seq int64, s Status, r int64) PaymentEvent {
		return PaymentEvent{EventID: fmt.Sprint("e", seq), Provider: "psp_a", PaymentRef: "R", Status: s, Amount: 100_000, RefundedAmount: r,
			Currency: "IDR", Sequence: seq, OccurredAt: t0.Add(time.Duration(seq) * time.Minute)}
	}
	events := []PaymentEvent{mk(3, StatusPartiallyRefunded, 10_000), mk(1, StatusPending, 0), mk(4, StatusSettled, 0), mk(2, StatusPaid, 0), mk(1, StatusPending, 0)}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if p, _ := Fold(events); p.Status() != StatusPartiallyRefunded {
			b.Fatal(p.Status())
		}
	}
}
