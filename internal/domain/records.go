package domain

import (
	"strings"
	"time"
)

// LedgerTxn is one internal ledger transaction: what our books say a customer
// paid through a provider. The shape follows a ledgerd-style transfer export
// (id, from/to account, amount in minor units, currency, reference,
// created_at) plus the provider the transfer was posted against.
type LedgerTxn struct {
	ID          string    `json:"id"`
	Provider    string    `json:"provider"`
	Reference   string    `json:"reference"`
	Amount      int64     `json:"amount"`
	Currency    Currency  `json:"currency"`
	CreatedAt   time.Time `json:"created_at"`
	FromAccount string    `json:"from_account_id,omitempty"`
	ToAccount   string    `json:"to_account_id,omitempty"`
}

// LineKind distinguishes an itemised settlement line (one transaction) from an
// aggregate payout (one credit covering many transactions, minus fees).
type LineKind string

// Line kinds.
const (
	LineItem   LineKind = "item"
	LinePayout LineKind = "payout"
)

// SettlementLine is a provider settlement/bank-statement row normalised by an
// adapter. Raw keeps the source row verbatim: when finance asks "what did the
// bank actually say?", the answer is evidence, not our interpretation of it.
type SettlementLine struct {
	ID        string   `json:"id"` // provider-scoped natural key, stable across re-ingestion
	Provider  string   `json:"provider"`
	Kind      LineKind `json:"kind"`
	Reference string   `json:"reference"` // merchant ref (items) or payout ref (payouts)
	Currency  Currency `json:"currency"`
	Gross     int64    `json:"gross"` // == Net when FeeKnown is false
	Fee       int64    `json:"fee"`
	Net       int64    `json:"net"`
	FeeKnown  bool     `json:"fee_known"`
	// TxnAt is the provider's transaction time for items (zero if not reported).
	TxnAt     time.Time `json:"txn_at,omitempty"`
	SettledOn Date      `json:"settled_on"`
	// Payout-only: the provider-clock window the payout covers and its item count
	// (0 = not reported).
	CoversFrom time.Time `json:"covers_from,omitempty"`
	CoversTo   time.Time `json:"covers_to,omitempty"`
	ItemCount  int       `json:"item_count,omitempty"`

	SourceLine int    `json:"source_line,omitempty"`
	Raw        string `json:"raw,omitempty"`
}

// NormalizeRef is the only normalisation applied before exact reference
// matching: trim and upper-case. Anything more aggressive (stripping dashes,
// leading zeros) starts creating false matches between distinct orders; the
// tolerance pass exists for references that are genuinely mangled.
func NormalizeRef(s string) string {
	s = strings.TrimSpace(s)
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'a' && c <= 'z' || c >= 0x80 {
			return strings.ToUpper(s)
		}
	}
	return s
}
