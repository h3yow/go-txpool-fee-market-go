// Package packer drives fee-market based block production on top of the pool
// and ledger, including atomic commit and reorg recovery.
//
// Produce flow (single serialised critical section -> no double packing /
// double spend):
//  1. select fee-ordered pending txs (nonce-contiguous per sender);
//  2. enforce block gas limit while skipping a sender wholesale once its head
//     cannot fit (never splits a sender's run);
//  3. per sender, simulate the CURRENT ledger balance: deduct each tx's
//     value + effectiveTip*gas in nonce order. If the balance was reduced
//     (chain/other entry) so that a tx (and hence the rest of its nonce run)
//     can no longer be paid, only the still-continuously-executable prefix is
//     taken; the suffix stays pending for the next block with ACCOUNT_BUDGET
//     decisions. Other accounts are never blocked;
//  4. Reserve txs in the pool (removed from pending view);
//  5. ledger.Apply atomically deducts balances and advances nonces (the
//     simulation guarantees it cannot fail on the selected set);
//  6. on success Confirm (drop reservations); on the unexpected failure
//     Unreserve (the ledger changed nothing, so the pool view stays
//     consistent).
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
// It returns the committed block and decisions for txs skipped due to the
// block gas limit (they remain pending for the next block).
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
// list. Two independent per-sender stop conditions defer the REMAINDER of that
// sender's nonce run for this block only (to avoid nonce holes), while later
// txs of other senders are still considered:
//
//   - block gas: once the sender's head cannot fit the remaining gas it is
//     deferred with GAS_LIMIT;
//   - account budget: the sender's CURRENT ledger balance, as it stands after
//     the txs already selected earlier in this block, is simulated in nonce
//     order. The first tx whose value + effectiveTip*gas exceeds the simulated
//     balance truncates the run: it and every later tx of this sender are
//     deferred with ACCOUNT_BUDGET. Because only the affordable prefix is
//     selected, the subsequent ledger.Apply cannot fail on a balance shortfall
//     and one broke account never blocks the whole block.
func (pc *Packer) selectForBlock(candidates []*types.Transaction, maxTx int) (selected []*types.Transaction, skipped []*reject.Decision, gasUsed uint64) {
	blockedSenders := make(map[types.Address]bool)
	// Per-sender simulated running balance over the txs selected so far.
	budget := make(map[types.Address]*big.Int)
	// Per-sender deferral reason once blocked, so the message is consistent.
	blockReason := make(map[types.Address]reject.Reason)
	skippedSet := make(map[types.Hash]bool)

	deferTx := func(tx *types.Transaction, reason reject.Reason, format string, args ...any) {
		if skippedSet[tx.Hash()] {
			return
		}
		skippedSet[tx.Hash()] = true
		skipped = append(skipped, reject.New(tx, reason, format, args...))
	}

	for _, tx := range candidates {
		if maxTx > 0 && len(selected) >= maxTx {
			break
		}
		if blockedSenders[tx.From] {
			switch blockReason[tx.From] {
			case reject.ReasonGasLimit:
				deferTx(tx, reject.ReasonGasLimit,
					"sender head did not fit block gas; deferred with nonce chain")
			default:
				deferTx(tx, reject.ReasonAccountBudget,
					"sender balance exhausted mid-run; deferred with nonce chain awaiting a top-up")
			}
			continue
		}
		if gasUsed+tx.Gas > pc.blockGasLmt {
			blockedSenders[tx.From] = true
			blockReason[tx.From] = reject.ReasonGasLimit
			deferTx(tx, reject.ReasonGasLimit,
				"gas %d would exceed block remaining %d (used %d/%d); sender deferred",
				tx.Gas, pc.blockGasLmt-gasUsed, gasUsed, pc.blockGasLmt)
			continue
		}

		// Lazily seed the simulated balance from the CURRENT authoritative
		// ledger state (may have been reduced since admission).
		work, ok := budget[tx.From]
		if !ok {
			acc := pc.ledger.GetAccount(tx.From)
			work = new(big.Int)
			if acc.Balance != nil {
				work.Set(acc.Balance)
			}
			budget[tx.From] = work
		}
		need := new(big.Int).Add(tx.Value,
			new(big.Int).Mul(tx.EffectiveTip(pc.baseFee), new(big.Int).SetUint64(tx.Gas)))
		if work.Cmp(need) < 0 {
			blockedSenders[tx.From] = true
			blockReason[tx.From] = reject.ReasonAccountBudget
			deferTx(tx, reject.ReasonAccountBudget,
				"balance changed: sender %x simulated balance %s < required %s for nonce %d (value %s + effTip %s * gas %d); only the executable prefix is packed, suffix deferred",
				tx.From[:6], work, need, tx.Nonce, tx.Value, tx.EffectiveTip(pc.baseFee), tx.Gas)
			continue
		}
		work.Sub(work, need)
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
