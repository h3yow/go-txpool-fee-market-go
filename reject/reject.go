// Package reject defines structured, distinguishable rejection reasons used at
// every transaction entry point (submit/replace/pack/reorg).
package reject

import (
	"errors"
	"fmt"

	"github.com/h3yow/go-txpool-fee-market-go/types"
)

// Reason is a machine-readable rejection category.
type Reason int

const (
	ReasonUnknown Reason = iota
	// ReasonMalformed: missing/invalid fields (nil amounts, tip>feecap, gas=0).
	ReasonMalformed
	// ReasonNonceTooLow: nonce below the account's on-chain nonce (or a
	// confirmed/reserved tx already occupies it).
	ReasonNonceTooLow
	// ReasonNonceTooHigh: nonce leaves a gap vs the account's expected nonce.
	ReasonNonceTooHigh
	// ReasonUnderpriced: replacement without sufficient price bump, or cannot
	// beat the cheapest pending transaction when the pool is full.
	ReasonUnderpriced
	// ReasonInsufficientFunds: balance below value + gasFeeCap*gas.
	ReasonInsufficientFunds
	// ReasonPoolFull: capacity exhausted and no evictable cheaper tx exists.
	ReasonPoolFull
	// ReasonAlreadyReserved: tx content is already in-flight in a block.
	ReasonAlreadyReserved
	// ReasonGasLimit: requested gas exceeds the block gas limit.
	ReasonGasLimit
)

// String maps a reason to a short stable label.
func (r Reason) String() string {
	switch r {
	case ReasonMalformed:
		return "MALFORMED"
	case ReasonNonceTooLow:
		return "NONCE_TOO_LOW"
	case ReasonNonceTooHigh:
		return "NONCE_TOO_HIGH"
	case ReasonUnderpriced:
		return "UNDERPRICED"
	case ReasonInsufficientFunds:
		return "INSUFFICIENT_FUNDS"
	case ReasonPoolFull:
		return "POOL_FULL"
	case ReasonAlreadyReserved:
		return "ALREADY_RESERVED"
	case ReasonGasLimit:
		return "GAS_LIMIT"
	default:
		return "UNKNOWN"
	}
}

// Decision is the explainable outcome of validating a transaction at an entry
// point. It implements error so rejection flows share one return type.
type Decision struct {
	Tx     *types.Transaction
	Reason Reason
	Detail string
}

func (d *Decision) Error() string {
	id := shortHash(d.Tx)
	if d.Tx != nil {
		return fmt.Sprintf("tx %s rejected [%s]: %s", id, d.Reason, d.Detail)
	}
	return fmt.Sprintf("rejected [%s]: %s", d.Reason, d.Detail)
}

// New builds a Decision error.
func New(tx *types.Transaction, reason Reason, format string, args ...any) *Decision {
	return &Decision{
		Tx:     tx,
		Reason: reason,
		Detail: fmt.Sprintf(format, args...),
	}
}

// AsDecision extracts a *Decision from err if possible.
func AsDecision(err error) (*Decision, bool) {
	var d *Decision
	if errors.As(err, &d) {
		return d, true
	}
	return nil, false
}

func shortHash(tx *types.Transaction) string {
	if tx == nil {
		return "<nil>"
	}
	h := tx.Hash()
	return fmt.Sprintf("0x%x…%x", h[:4], h[28:])
}
