package ingest

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/dwijamahardika/recond/internal/domain"
)

const benchRows = 10_000

func BenchmarkPSPARead(b *testing.B) {
	var buf bytes.Buffer
	w, _ := NewPSPAWriter(&buf, "psp_a")
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < benchRows; i++ {
		_, _ = w.Write(PSPARow{SettlementID: fmt.Sprintf("STL-%08d", i), MerchantRef: fmt.Sprintf("ORD-%07d", i), TxnTime: t0.Add(time.Duration(i) * time.Second),
			SettlementDate: domain.CivilDate(2026, 10, 3), Currency: "IDR", Gross: int64(10_000+i) * 100, Fee: int64(150 + i%7)})
	}
	_ = w.Flush()
	src := buf.Bytes()
	b.SetBytes(int64(len(src)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n := 0
		if err := (PSPA{}).Read(bytes.NewReader(src), Options{Provider: "psp_a"}, func(domain.SettlementLine, string) error { n++; return nil }, func(Reject) { b.Fatal("reject") }); err != nil || n != benchRows {
			b.Fatal(err, n)
		}
	}
	b.ReportMetric(float64(benchRows*b.N)/b.Elapsed().Seconds(), "rows/s")
}

func BenchmarkBankBRead(b *testing.B) {
	var buf bytes.Buffer
	loc := time.FixedZone("UTC+7", 7*3600)
	w, _ := NewBankBWriter(&buf, "bank_b", loc)
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, loc)
	for i := 0; i < benchRows; i++ {
		_, _ = w.Write(BankBRow{ValueDate: domain.CivilDate(2026, 10, 2), TxnTime: t0.Add(time.Duration(i) * time.Second), RefNo: fmt.Sprintf("ORD-%07d", i),
			Description: "QR CREDIT", Credit: int64(10_000+i) * 100, Charges: 20_000, ChargesKnown: true, Currency: "IDR"})
	}
	_ = w.Flush()
	src := buf.Bytes()
	b.SetBytes(int64(len(src)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n := 0
		if err := (BankB{}).Read(bytes.NewReader(src), Options{Provider: "bank_b", Location: loc}, func(domain.SettlementLine, string) error { n++; return nil }, func(Reject) { b.Fatal("reject") }); err != nil || n != benchRows {
			b.Fatal(err, n)
		}
	}
	b.ReportMetric(float64(benchRows*b.N)/b.Elapsed().Seconds(), "rows/s")
}

func BenchmarkDecodeEvent(b *testing.B) {
	body := []byte(`{"event_id":"evt_00000000000000a1","payment_ref":"ORD-20261001-0000001","status":"paid","amount":15000000,"currency":"IDR","sequence":2,"occurred_at":"2026-10-01T03:15:22.123Z"}`)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := DecodeEvent(body, "psp_a"); err != nil {
			b.Fatal(err)
		}
	}
}
