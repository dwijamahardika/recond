package domain

// Book is an in-memory payment state store with the same semantics as the
// persistent one: duplicate event ids are dropped, an event newer than
// everything seen for its payment is applied in O(1), and an older one
// triggers a replay of that payment's history.
type Book struct {
	state map[PaymentKey]*Payment
	hist  map[PaymentKey][]PaymentEvent
	seen  map[string]struct{}
}

// ApplyResult describes what happened to one delivered event.
type ApplyResult struct {
	Duplicate  bool    // event id already seen: no effect
	OutOfOrder bool    // older than the newest seen: history replayed
	Verdict    Verdict // applied or rejected by the transition rules
	Reason     string
	Before     Status
	After      Status
}

// NewBook returns an empty book.
func NewBook() *Book {
	return &Book{state: map[PaymentKey]*Payment{}, hist: map[PaymentKey][]PaymentEvent{}, seen: map[string]struct{}{}}
}

// EventKey scopes event ids per provider.
func EventKey(provider, eventID string) string { return provider + "\x00" + eventID }

// Apply delivers one event, in arrival order.
func (b *Book) Apply(e PaymentEvent) ApplyResult {
	ek := EventKey(e.Provider, e.EventID)
	if _, dup := b.seen[ek]; dup {
		return ApplyResult{Duplicate: true}
	}
	b.seen[ek] = struct{}{}
	k := PaymentKey{Provider: e.Provider, Ref: e.PaymentRef}
	p := b.state[k]
	if p == nil {
		p = &Payment{}
		b.state[k] = p
	}
	before := p.Status()
	b.hist[k] = append(b.hist[k], e)
	res := ApplyResult{Before: before}
	if v, reason, ok := p.Observe(e); ok {
		res.Verdict, res.Reason = v, reason
	} else {
		var rej []Rejection
		*p, rej = Fold(b.hist[k])
		res.OutOfOrder, res.Verdict = true, Applied
		for _, r := range rej {
			if r.EventID == e.EventID {
				res.Verdict, res.Reason = Rejected, r.Reason
			}
		}
	}
	res.After = p.Status()
	return res
}

// Payments returns a snapshot of every payment's state.
func (b *Book) Payments() map[PaymentKey]Payment {
	out := make(map[PaymentKey]Payment, len(b.state))
	for k, p := range b.state {
		out[k] = *p
	}
	return out
}

// History returns the distinct events seen for a payment, in arrival order.
func (b *Book) History(k PaymentKey) []PaymentEvent { return b.hist[k] }
