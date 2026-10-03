// Package packer drives fee-market based block production on top of the pool
// and ledger, including atomic commit and reorg recovery.
//
// Produce flow (single serialised critical section -> no double packing /
// double spend):
//  1. select fee-ordered pending txs (nonce-contiguous per sender);
//  2. enforce the block gas limit while skipping a sender wholesale once its
//     head cannot fit (never splits a sender's run); a sender whose live
//     balance was reduced mid-flight contributes only its still-executable
//     nonce prefix — the unaffordable tx and its suffix stay pending, and
//     other senders are unaffected;
//  3. Reserve txs in the pool (removed from pending view);
//  4. ledger.Apply atomically deducts balances and advances nonces;
//  5. on success Confirm (drop reservations); on failure Unreserve (the
//     ledger changed nothing, so the pool view stays consistent).
package packer

import (
	"fmt"
	"math/big"
	"sync"

	"github.com/h3yow/go-txpool-fee-market-go/ledger"
	"github.com/h3yow/go-txpool-fee-market-go/pool"
	"github.com/h3yow/go-txpool-fee-market-go/reject"
	"github.com/h3yow/go-txpool-fee-market-go/types"
)

// DefaultBlockGasLimit is the per-block gas ceiling unless overridden.
const DefaultBlockGasLimit uint64 = 10_000_000

// Block is an assembled block.
type Block struct {
	ID      uint64
	BaseFee *big.Int
	GasUsed uint64
	Txs     []*types.Transaction
}

// Packer coordinates pool selection, ledger commit and reorg recovery.
type Packer struct {
	mu          sync.Mutex
	pool        *pool.Pool
	ledger      *ledger.Ledger
	baseFee     *big.Int
	blockGasLmt uint64
}

// Option customises a Packer.
type Option func(*Packer)

// WithBlockGasLimit overrides the block gas limit.
func WithBlockGasLimit(gas uint64) Option {
	return func(pc *Packer) { pc.blockGasLmt = gas }
}

// New constructs a Packer.
func New(p *pool.Pool, l *ledger.Ledger, baseFee *big.Int, opts ...Option) *Packer {
	if baseFee == nil {
		baseFee = new(big.Int)
	}
	pc := &Packer{pool: p, ledger: l, baseFee: new(big.Int).Set(baseFee), blockGasLmt: DefaultBlockGasLimit}
	for _, o := range opts {
		o(pc)
	}
	return pc
}

// Produce assembles and commits one block. maxTx <= 0 means unbounded count.
// It returns the committed block and decisions for txs left pending: either the
// sender's run did not fit the block gas (GAS_LIMIT) or the live balance no
// longer covers a tx, in which case only the sender's executable nonce prefix
// is packed and the rest is deferred (INSUFFICIENT_FUNDS). A single sender
// running out of funds never blocks the block or other senders.
func (pc *Packer) Produce(maxTx int) (*Block, []*reject.Decision, error) {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	candidates := pc.pool.Pending(pc.baseFee)
	if len(candidates) == 0 {
		return nil, nil, nil // nothing to pack; do not create an empty block
	}

	selected, skipped, gasUsed := pc.selectForBlock(candidates, maxTx)
	if len(selected) == 0 {
		// Every head exceeded the remaining gas (degenerate tiny limit); the
		// txs stay pending with GasLimit decisions and no block is produced.
		return nil, skipped, nil
	}

	pc.pool.Reserve(selected)

	id := pc.ledger.Head() + 1
	if err := pc.ledger.Apply(id, pc.baseFee, selected); err != nil {
		// Commit failed atomically (nothing was deducted): return txs.
		pc.pool.Unreserve(selected)
		return nil, nil, fmt.Errorf("packer: block %d commit failed: %w", id, err)
	}
	pc.pool.Confirm(selected)

	return &Block{ID: id, BaseFee: new(big.Int).Set(pc.baseFee), GasUsed: gasUsed, Txs: selected}, skipped, nil
}

// selectForBlock walks the fee-ordered, per-sender-nonce-ordered candidate
// list. Two conditions can stop a sender for the remainder of THIS block, and
// neither of them fails the block or blocks other senders:
//
//   - the sender's head tx cannot fit in the remaining block gas
//     (GAS_LIMIT);
//   - the live balance no longer covers the tx's exact cost (value +
//     effectiveTip*gas): only the executable nonce PREFIX of that sender is
//     packed; the unaffordable head and every tx queued behind it stay pending
//     for the next block (INSUFFICIENT_FUNDS).
//
// Per sender the live balance is spent sequentially as ledger.Apply would, so
// the selected prefix is guaranteed applicable; nonce runs are never split.
func (pc *Packer) selectForBlock(candidates []*types.Transaction, maxTx int) (selected []*types.Transaction, skipped []*reject.Decision, gasUsed uint64) {
	gasBlocked := make(map[types.Address]bool)
	fundsBlocked := make(map[types.Address]bool)
	remaining := make(map[types.Address]*big.Int)
	skippedSet := make(map[types.Hash]bool)
	addSkip := func(tx *types.Transaction, reason reject.Reason, format string, args ...any) {
		if !skippedSet[tx.Hash()] {
			skipped = append(skipped, reject.New(tx, reason, format, args...))
			skippedSet[tx.Hash()] = true
		}
	}
	for _, tx := range candidates {
		if maxTx > 0 && len(selected) >= maxTx {
			break
		}
		if gasBlocked[tx.From] {
			addSkip(tx, reject.ReasonGasLimit,
				"sender head did not fit block gas; deferred with nonce chain")
			continue
		}
		if fundsBlocked[tx.From] {
			addSkip(tx, reject.ReasonInsufficientFunds,
				"deferred with nonce chain behind an earlier tx whose cost the reduced balance cannot cover")
			continue
		}
		if gasUsed+tx.Gas > pc.blockGasLmt {
			gasBlocked[tx.From] = true
			addSkip(tx, reject.ReasonGasLimit,
				"gas %d would exceed block remaining %d (used %d/%d); sender deferred",
				tx.Gas, pc.blockGasLmt-gasUsed, gasUsed, pc.blockGasLmt)
			continue
			// Balance gate against the live (possibly externally reduced)
			// balance, spent in nonce order exactly like ledger.Apply.
		}
		bal, ok := remaining[tx.From]
		if !ok {
			bal = new(big.Int).Set(pc.ledger.GetAccount(tx.From).Balance)
			if bal == nil {
				bal = new(big.Int)
			}
			remaining[tx.From] = bal
		}
		tip := tx.EffectiveTip(pc.baseFee)
		exact := new(big.Int).Add(tx.Value,
			new(big.Int).Mul(tip, new(big.Int).SetUint64(tx.Gas)))
		if bal.Cmp(exact) < 0 {
			fundsBlocked[tx.From] = true
			addSkip(tx, reject.ReasonInsufficientFunds,
				"live balance %s < exact cost %s (nonce %d, value+effectiveTip*gas); "+
					"only the executable prefix is packed, remainder deferred to next block",
				bal, exact, tx.Nonce)
			continue
		}
		bal.Sub(bal, exact)
		selected = append(selected, tx)
		gasUsed += tx.Gas
	}
	return selected, skipped, gasUsed
}

// Reorg rewinds the chain to targetHead. Ledger balances/nonces are restored
// from before-images; transactions of reverted blocks are revalidated and
// readded to the pool (oldest block first, original intra-block order) so
// nonce chains are rebuilt without gaps.
//
// For a fork whose canonical side carries different state, use Rollback +
// state overlay + Revalidate instead.
func (pc *Packer) Reorg(targetHead uint64) (readded []*types.Transaction, dropped []*reject.Decision, err error) {
	reverted, rerr := pc.Rollback(targetHead)
	if rerr != nil {
		return nil, nil, rerr
	}
	if len(reverted) == 0 {
		return nil, nil, nil
	}
	readded, dropped = pc.Revalidate(reverted)
	return readded, dropped, nil
}

// Rollback rewinds the ledger to targetHead and returns the detached
// transactions in oldest-first order WITHOUT readmitting them. Between
// Rollback and Revalidate the caller may overlay canonical-chain state.
func (pc *Packer) Rollback(targetHead uint64) ([]*types.Transaction, error) {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	snapshots, err := pc.ledger.Revert(targetHead)
	if err != nil {
		return nil, err
	}
	// Release any stale reservations referencing detached txs.
	var all []*types.Transaction
	for i := len(snapshots) - 1; i >= 0; i-- {
		all = append(all, snapshots[i].Txs...)
		pc.pool.Unreserve(snapshots[i].Txs)
	}
	return all, nil
}

// Revalidate re-admits transactions (e.g. after Rollback + state overlay),
// classifying each rejection with a distinguishable reason.
func (pc *Packer) Revalidate(txs []*types.Transaction) (readded []*types.Transaction, dropped []*reject.Decision) {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	decisions := pc.pool.Readd(txs)
	for i, d := range decisions {
		if d == nil {
			readded = append(readded, txs[i])
		} else {
			dropped = append(dropped, d)
		}
	}
	return readded, dropped
}

// Head returns the current committed block height.
func (pc *Packer) Head() uint64 {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	return pc.ledger.Head()
}
