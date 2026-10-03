// Account-level introspection of the pool's queued state: which txs an account
// is still waiting on, which nonces are reserved by in-flight blocks, what the
// queue costs at most in aggregate, whether the current balance covers that
// budget, and which txs are immediately packable vs deferred (and why).
package pool

import (
	"math/big"
	"sort"
	"strconv"

	"github.com/h3yow/go-txpool-fee-market-go/types"
)

// ItemState classifies one queued transaction against the live ledger state.
type ItemState int

const (
	// ItemPackable: the tx is immediately executable from the current state.
	ItemPackable ItemState = iota
	// ItemNoFunds: the tx alone exceeds the balance remaining at that point.
	ItemNoFunds
	// ItemDeferred: waits behind an earlier unaffordable/blocked tx.
	ItemDeferred
)

// String renders the state as a stable uppercase label.
func (s ItemState) String() string {
	switch s {
	case ItemPackable:
		return "PACKABLE"
	case ItemNoFunds:
		return "NO_FUNDS"
	case ItemDeferred:
		return "DEFERRED"
	default:
		return "UNKNOWN"
	}
}

// QueuedItem is one pending transaction plus the pool's verdict for it.
type QueuedItem struct {
	Tx      *types.Transaction
	State   ItemState
	Reason  string   // human-readable judgement basis; "" when packable
	MaxCost *big.Int // value + GasFeeCap*gas
}

// AccountView is the explainable snapshot of one account's queue.
type AccountView struct {
	// Balance is the live ledger balance.
	Balance *big.Int
	// ReservedNonces are nonces packed into in-flight (unconfirmed) blocks.
	ReservedNonces []uint64
	// QueuedMaxCost is the sum of max costs over pending txs and reservations.
	QueuedMaxCost *big.Int
	// BudgetOK reports whether Balance covers QueuedMaxCost.
	BudgetOK bool
	// Items lists pending txs in nonce order with per-tx packability verdicts
	// evaluated with the exact per-tx cost (value + effectiveTip*gas at the
	// queried base fee) applied against a sequentially spent balance.
	Items []QueuedItem
}

// AccountStatus returns the queued picture of addr evaluated against the live
// ledger state and the given base fee (zero base fee when baseFee == nil).
// Unknown accounts return an empty view with a zero balance.
func (p *Pool) AccountStatus(addr types.Address, baseFee *big.Int) *AccountView {
	if baseFee == nil {
		baseFee = new(big.Int)
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	acc := p.ledger.GetAccount(addr)
	balance := acc.Balance
	if balance == nil {
		balance = new(big.Int)
	}
	v := &AccountView{
		Balance:       new(big.Int).Set(balance),
		QueuedMaxCost: new(big.Int),
	}

	q := p.accounts[addr]
	if q == nil {
		v.BudgetOK = true // empty queue: zero required
		return v
	}

	reserved := make([]uint64, 0, len(q.reserved))
	for n, rt := range q.reserved {
		reserved = append(reserved, n)
		v.QueuedMaxCost.Add(v.QueuedMaxCost, rt.Cost())
	}
	sort.Slice(reserved, func(i, j int) bool { return reserved[i] < reserved[j] })
	v.ReservedNonces = reserved

	nonces := make([]uint64, 0, len(q.txs))
	for n := range q.txs {
		nonces = append(nonces, n)
	}
	sort.Slice(nonces, func(i, j int) bool { return nonces[i] < nonces[j] })
	itemsByNonce := make(map[uint64]QueuedItem, len(q.txs))

	// Packability simulation: execution proceeds in nonce order from the
	// on-chain nonce. In-flight reserved txs are committed first (they sit
	// ahead of pending in a block); each spends value + effectiveTip*gas at
	// the queried base fee. The first tx whose exact cost no longer fits marks
	// the nonce boundary; every pending tx at that nonce or later is not
	// packable (a nonce gap can never be jumped over).
	exactCost := func(tx *types.Transaction) *big.Int {
		tip := tx.EffectiveTip(baseFee)
		return new(big.Int).Add(tx.Value,
			new(big.Int).Mul(tip, new(big.Int).SetUint64(tx.Gas)))
	}
	remaining := new(big.Int).Set(balance)
	blockReason := ""
	visited := make(map[uint64]bool)
	for n := acc.Nonce; blockReason == ""; n++ {
		visited[n] = true
		if rt, isReserved := q.reserved[n]; isReserved {
			need := exactCost(rt)
			if remaining.Cmp(need) < 0 {
				blockReason = "in-flight reserved tx at nonce " + uintStr(n) +
					" already needs exact cost " + need.String() + " > remaining balance " + remaining.String()
				continue
			}
			remaining.Sub(remaining, need)
		} else if tx, isPending := q.txs[n]; isPending {
			maxCost := tx.Cost()
			v.QueuedMaxCost.Add(v.QueuedMaxCost, maxCost)
			item := QueuedItem{Tx: tx, MaxCost: new(big.Int).Set(maxCost)}
			if need := exactCost(tx); remaining.Cmp(need) < 0 {
				item.State = ItemNoFunds
				item.Reason = "remaining balance " + remaining.String() + " < exact cost " + need.String() +
					" (value + effectiveTip*gas at baseFee " + baseFee.String() +
					"); account run stops here for the next block"
				blockReason = item.Reason
			} else {
				item.State = ItemPackable
				remaining.Sub(remaining, need)
			}
			itemsByNonce[n] = item
		} else {
			break // no reservation nor pending tx at n: contiguous run ends
		}
	}
	// Pending txs not reached by the contiguous walk (below on-chain nonce in
	// the confirm race window, or above the blocking point) are deferred.
	for _, pn := range nonces {
		if _, done := itemsByNonce[pn]; done {
			continue
		}
		tx := q.txs[pn]
		if _, counted := visited[pn]; !counted {
			v.QueuedMaxCost.Add(v.QueuedMaxCost, tx.Cost())
		}
		reason := blockReason
		if reason == "" {
			reason = "nonce is not contiguous with the executable run (stale or gapped)"
		}
		itemsByNonce[pn] = QueuedItem{
			Tx: tx, State: ItemDeferred,
			Reason:  "waits behind: " + reason,
			MaxCost: new(big.Int).Set(tx.Cost()),
		}
	}

	v.Items = make([]QueuedItem, 0, len(nonces))
	for _, pn := range nonces {
		v.Items = append(v.Items, itemsByNonce[pn])
	}

	v.BudgetOK = balance.Cmp(v.QueuedMaxCost) >= 0
	return v
}

// uintStr renders a uint64 without pulling in strconv at every call site ... it
// is just strconv.FormatUint, kept tiny for readable verdict messages.
func uintStr(n uint64) string {
	return strconv.FormatUint(n, 10)
}
