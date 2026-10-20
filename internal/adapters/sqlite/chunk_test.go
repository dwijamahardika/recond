package sqlite

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/dwijamahardika/recond/internal/config"
	"github.com/dwijamahardika/recond/internal/domain"
	"github.com/dwijamahardika/recond/internal/gen"
	"github.com/dwijamahardika/recond/internal/ingest"
)

type memFile struct{ bytes.Buffer }

func (*memFile) Close() error { return nil }

// A webhook backfill split across many chunks, and across two files, must
// leave every payment exactly where a fold of its full history puts it - and
// where the generator meant it to be.
func TestEventChunksMatchFold(t *testing.T) {
	// 97: point queries per payment; 4999: chunks large enough to preload the
	// payments table once and then rely on the cross-chunk cache.
	for _, size := range []int{97, 4999} {
		t.Run(fmt.Sprint("chunk=", size), func(t *testing.T) { testChunks(t, size) })
	}
}

func testChunks(t *testing.T, size int) {
	old := eventChunk
	eventChunk = size // odd sizes: payments straddle chunk boundaries
	t.Cleanup(func() { eventChunk = old })

	rules, err := config.LoadRules("")
	if err != nil {
		t.Fatal(err)
	}
	ds, err := gen.Generate(gen.Config{Seed: 4, Payments: 6000, Rules: rules, DupEventRate: 0.3})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]*memFile{}
	if _, err := ds.Render(func(name string) (io.WriteCloser, error) {
		f := &memFile{}
		files[name] = f
		return f, nil
	}); err != nil {
		t.Fatal(err)
	}
	var events []domain.PaymentEvent
	if err := ingest.ReadEvents(bytes.NewReader(files[gen.EventsFile].Bytes()), ingest.Options{}, func(e domain.PaymentEvent, _ string) error {
		events = append(events, e)
		return nil
	}, func(r ingest.Reject) { t.Fatalf("reject %+v", r) }); err != nil {
		t.Fatal(err)
	}

	// Expected out-of-order deliveries, computed directly from the stream:
	// a new event older than the newest already seen for its payment.
	wantOOO := 0
	newest := map[domain.PaymentKey]domain.Version{}
	seen := map[string]bool{}
	for _, e := range events {
		if seen[e.EventID] {
			continue
		}
		seen[e.EventID] = true
		k := domain.PaymentKey{Provider: e.Provider, Ref: e.PaymentRef}
		v := e.Version()
		if n, ok := newest[k]; ok && v.Less(n) {
			wantOOO++
		} else {
			newest[k] = v
		}
	}

	s, _ := open(t)
	ctx := context.Background()
	gotOOO := 0
	half := len(events) / 2
	for i, part := range [][]domain.PaymentEvent{events[:half], events[half:]} {
		st, err := s.Ingest(ctx, FileMeta{Kind: "webhooks", Source: "webhooks", Name: "w", SHA256: string(rune('a' + i))}, func(in *Ingester) error {
			for _, e := range part {
				if err := in.Event(e, ingest.EventHash(e)); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if st.OutOfOrder == 0 || st.Duplicates == 0 {
			t.Fatalf("file %d: expected out-of-order and duplicate deliveries, got %+v", i, st)
		}
		gotOOO += st.OutOfOrder
	}
	if gotOOO != wantOOO {
		t.Fatalf("out-of-order count %d depends on chunking; want %d", gotOOO, wantOOO)
	}
	cur, err := s.LoadInputs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	folded, err := s.LoadInputsAt(ctx, cur.MaxFileID, cur.MaxEventSeq)
	if err != nil {
		t.Fatal(err)
	}
	if len(cur.Payments) != len(ds.Truth.FinalStatus) {
		t.Fatalf("%d payments stored, %d generated", len(cur.Payments), len(ds.Truth.FinalStatus))
	}
	for k, want := range ds.Truth.FinalStatus {
		p := cur.Payments[k]
		if p.Status() != want {
			t.Fatalf("%v: projection says %s, truth %s", k, p.Status(), want)
		}
		if p != folded.Payments[k] {
			t.Fatalf("%v: projection %+v != fold %+v", k, p, folded.Payments[k])
		}
	}
}
