package domain

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 12, 3, 0, 0, 0, time.UTC)

func ev(id string, seq int64, s Status, refunded int64) PaymentEvent {
	return PaymentEvent{
		EventID: id, Provider: "psp_a", PaymentRef: "ORD-1", Status: s, Amount: 10_000,
		RefundedAmount: refunded, Currency: "IDR", Sequence: seq,
		OccurredAt: t0.Add(time.Duration(seq) * time.Minute),
	}
}

func TestStateMachineTransitions(t *testing.T) {
	cases := []struct {
		name     string
		events   []PaymentEvent // in delivery order
		want     Status
		rejected int
	}{
		{"happy path", []PaymentEvent{ev("1", 1, StatusPending, 0), ev("2", 2, StatusPaid, 0), ev("3", 3, StatusSettled, 0)}, StatusSettled, 0},
		{"late pending cannot overwrite paid", []PaymentEvent{ev("2", 2, StatusPaid, 0), ev("1", 1, StatusPending, 0)}, StatusPaid, 0},
		{"settled before paid arrives", []PaymentEvent{ev("3", 3, StatusSettled, 0), ev("1", 1, StatusPending, 0), ev("2", 2, StatusPaid, 0)}, StatusSettled, 0},
		{"duplicates are idempotent", []PaymentEvent{ev("2", 2, StatusPaid, 0), ev("2", 2, StatusPaid, 0), ev("2", 2, StatusPaid, 0)}, StatusPaid, 0},
		{"expiry", []PaymentEvent{ev("1", 1, StatusPending, 0), ev("2", 2, StatusExpired, 0)}, StatusExpired, 0},
		{"expiry after payment is an anomaly", []PaymentEvent{ev("2", 2, StatusPaid, 0), ev("3", 3, StatusExpired, 0)}, StatusPaid, 1},
		{"paid after expiry is an anomaly", []PaymentEvent{ev("2", 2, StatusExpired, 0), ev("3", 3, StatusPaid, 0)}, StatusExpired, 1},
		{"provider regression in sequence order", []PaymentEvent{ev("2", 2, StatusPaid, 0), ev("3", 3, StatusPending, 0)}, StatusPaid, 1},
		{"partial refund", []PaymentEvent{ev("2", 2, StatusPaid, 0), ev("3", 3, StatusPartiallyRefunded, 2_500)}, StatusPartiallyRefunded, 0},
		{"cumulative refunds out of order", []PaymentEvent{ev("4", 4, StatusPartiallyRefunded, 6_000), ev("2", 2, StatusPaid, 0), ev("3", 3, StatusPartiallyRefunded, 2_500)}, StatusPartiallyRefunded, 0},
		{"refund completes", []PaymentEvent{ev("2", 2, StatusPaid, 0), ev("3", 3, StatusPartiallyRefunded, 2_500), ev("4", 4, StatusRefunded, 0)}, StatusRefunded, 0},
		{"partial refund then settled stays partially_refunded", []PaymentEvent{ev("2", 2, StatusPaid, 0), ev("3", 3, StatusPartiallyRefunded, 2_500), ev("5", 5, StatusSettled, 0)}, StatusPartiallyRefunded, 0},
		{"refund amount regression rejected", []PaymentEvent{ev("2", 2, StatusPaid, 0), ev("3", 3, StatusPartiallyRefunded, 5_000), ev("4", 4, StatusPartiallyRefunded, 1_000)}, StatusPartiallyRefunded, 1},
		{"refund exceeding amount rejected", []PaymentEvent{ev("2", 2, StatusPaid, 0), ev("3", 3, StatusPartiallyRefunded, 20_000)}, StatusPaid, 1},
		{"refund implies paid", []PaymentEvent{ev("3", 3, StatusPartiallyRefunded, 1_000)}, StatusPartiallyRefunded, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := applyIncrementally(tc.events)
			if p.Status() != tc.want || p.Rejected != tc.rejected {
				t.Fatalf("status=%s rejected=%d, want %s/%d (%+v)", p.Status(), p.Rejected, tc.want, tc.rejected, p)
			}
			folded, rej := Fold(tc.events)
			if !reflect.DeepEqual(folded, p) || len(rej) != tc.rejected {
				t.Fatalf("Fold disagrees with incremental:\nfold=%+v\nincr=%+v", folded, p)
			}
		})
	}
}

func TestStateMachineNoSequenceUsesTime(t *testing.T) {
	// A provider that sends no sequence numbers: event time decides.
	a := PaymentEvent{EventID: "b", Provider: "bank_b", PaymentRef: "R", Status: StatusPaid, Amount: 1, Currency: "IDR", OccurredAt: t0.Add(time.Second)}
	b := PaymentEvent{EventID: "a", Provider: "bank_b", PaymentRef: "R", Status: StatusPending, Amount: 1, Currency: "IDR", OccurredAt: t0}
	if p := applyIncrementally([]PaymentEvent{a, b}); p.Status() != StatusPaid || !p.PaidAt.Equal(a.OccurredAt) {
		t.Fatalf("got %+v", p)
	}
}

// applyIncrementally mimics the store: drop duplicate ids, take the O(1) path
// when possible, replay the full history otherwise.
func applyIncrementally(events []PaymentEvent) Payment {
	var p Payment
	hist := make([]PaymentEvent, 0, len(events))
	seen := map[string]bool{}
	for _, e := range events {
		if seen[e.EventID] {
			continue
		}
		seen[e.EventID] = true
		hist = append(hist, e)
		if _, _, ok := p.Observe(e); !ok {
			p, _ = Fold(hist)
		}
	}
	return p
}

// TestStateMachineOrderIndependence is the property behind out-of-order
// webhook safety: for random histories (including anomalous ones), every
// permutation with random duplication yields exactly the state of Fold.
func TestStateMachineOrderIndependence(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	statuses := []Status{StatusPending, StatusPaid, StatusSettled, StatusExpired, StatusPartiallyRefunded, StatusRefunded}
	for iter := 0; iter < 3000; iter++ {
		n := 1 + rng.Intn(6)
		events := make([]PaymentEvent, n)
		useSeq := rng.Intn(2) == 0
		for i := range events {
			s := statuses[rng.Intn(len(statuses))]
			e := PaymentEvent{
				EventID: fmt.Sprintf("e%d", i), Provider: "p", PaymentRef: "r", Status: s, Amount: 10_000, Currency: "IDR",
				OccurredAt: t0.Add(time.Duration(rng.Intn(5)) * time.Second), // collisions on purpose
			}
			if useSeq {
				e.Sequence = int64(rng.Intn(4))
			}
			if s == StatusPartiallyRefunded {
				e.RefundedAmount = int64(1 + rng.Intn(12_000))
			}
			events[i] = e
		}
		want, _ := Fold(events)
		for perm := 0; perm < 8; perm++ {
			delivery := append([]PaymentEvent(nil), events...)
			for _, e := range events {
				if rng.Intn(3) == 0 {
					delivery = append(delivery, e)
				}
			}
			rng.Shuffle(len(delivery), func(i, j int) { delivery[i], delivery[j] = delivery[j], delivery[i] })
			if got := applyIncrementally(delivery); !reflect.DeepEqual(got, want) {
				t.Fatalf("order dependence:\nevents=%+v\ndelivery=%+v\ngot=%+v\nwant=%+v", events, delivery, got, want)
			}
		}
	}
}

func TestEventValidate(t *testing.T) {
	good := ev("1", 1, StatusPaid, 0)
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := good
	bad.Status = "captured"
	bad.Amount = -1
	if err := bad.Validate(); err == nil {
		t.Fatal("expected invalid")
	}
}
