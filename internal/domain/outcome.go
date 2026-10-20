package domain

// Class is the verdict a reconciliation run reaches for a group of records.
type Class string

// Outcome classes.
const (
	ClassMatched             Class = "matched"
	ClassAmountMismatch      Class = "amount_mismatch"
	ClassFeeMismatch         Class = "fee_mismatch"
	ClassLateSettlement      Class = "late_settlement"
	ClassDuplicateSettlement Class = "duplicate_settlement"
	ClassMissingInSettlement Class = "missing_in_settlement"
	ClassMissingInLedger     Class = "missing_in_ledger"
	// ClassAwaitingSettlement is informational, not a discrepancy: the ledger
	// transaction is unmatched but its T+N deadline has not passed yet.
	ClassAwaitingSettlement Class = "awaiting_settlement"
)

// Classes lists every class in report order.
var Classes = []Class{
	ClassMatched, ClassAmountMismatch, ClassFeeMismatch, ClassLateSettlement,
	ClassDuplicateSettlement, ClassMissingInSettlement, ClassMissingInLedger,
	ClassAwaitingSettlement,
}

// IsDiscrepancy reports whether the class goes to the exception queue.
func (c Class) IsDiscrepancy() bool {
	return c != ClassMatched && c != ClassAwaitingSettlement && c != ""
}

// Valid reports whether c is a known class.
func (c Class) Valid() bool {
	for _, k := range Classes {
		if k == c {
			return true
		}
	}
	return false
}

// Rule records how a group was formed, so a reviewer can judge how much to
// trust a match (a reference match is stronger evidence than amount+time).
type Rule string

// Match rules, strongest evidence first.
const (
	RuleReference     Rule = "reference"      // 1:1 on normalised reference
	RuleRefunded      Rule = "refunded"       // fully refunded before settlement: nothing due
	RuleTolerance     Rule = "tolerance"      // 1:1 on amount within tolerance + time window
	RuleBatch         Rule = "batch"          // N:1 payout
	RuleBatchResidual Rule = "batch_residual" // payout money no ledger transaction explains
	RuleUnmatched     Rule = "unmatched"
)
