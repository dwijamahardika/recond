package ingest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/dwijamahardika/recond/internal/domain"
)

// DecodeEvent parses one webhook body (the same JSON object the HTTP endpoint
// receives and the event files contain). Unknown fields are ignored -
// providers add fields over time - but every field recond relies on is
// validated.
func DecodeEvent(b []byte, provider string) (domain.PaymentEvent, error) {
	var e domain.PaymentEvent
	if err := json.Unmarshal(b, &e); err != nil {
		return e, fmt.Errorf("%w: %w", domain.ErrInvalidEvent, err)
	}
	if provider != "" {
		if e.Provider != "" && e.Provider != provider {
			return e, fmt.Errorf("%w: body provider %q does not match %q", domain.ErrInvalidEvent, e.Provider, provider)
		}
		e.Provider = provider
	}
	e.Currency = domain.Currency(strings.ToUpper(string(e.Currency)))
	e.Status = domain.Status(strings.ToLower(string(e.Status)))
	e.OccurredAt = e.OccurredAt.UTC()
	return e, e.Validate()
}

// EventHash fingerprints an event's content for conflict detection.
func EventHash(e domain.PaymentEvent) string {
	return RowHash(e.EventID, e.Provider, e.PaymentRef, string(e.Status), strconv.FormatInt(e.Amount, 10),
		strconv.FormatInt(e.RefundedAmount, 10), string(e.Currency), strconv.FormatInt(e.Sequence, 10),
		strconv.FormatInt(e.OccurredAt.UnixNano(), 10))
}

// ReadEvents reads a JSON Lines file of webhook events, one per line, in
// delivery order.
func ReadEvents(r io.Reader, opt Options, emit Emit[domain.PaymentEvent], reject func(Reject)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for line := 1; sc.Scan(); line++ {
		b := sc.Bytes()
		if len(strings.TrimSpace(string(b))) == 0 {
			continue
		}
		e, err := DecodeEvent(b, opt.Provider)
		if err != nil {
			reject(Reject{Line: line, Reason: err.Error(), Raw: truncate(string(b))})
			continue
		}
		if err := emit(e, EventHash(e)); err != nil {
			return err
		}
	}
	return sc.Err()
}

// EventWriter writes JSON Lines event files.
type EventWriter struct{ w *bufio.Writer }

// NewEventWriter returns a buffered JSON Lines writer.
func NewEventWriter(w io.Writer) *EventWriter { return &EventWriter{w: bufio.NewWriter(w)} }

func (ew *EventWriter) Write(e domain.PaymentEvent) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := ew.w.Write(b); err != nil {
		return err
	}
	return ew.w.WriteByte('\n')
}

// Flush flushes buffered events.
func (ew *EventWriter) Flush() error { return ew.w.Flush() }
