// Package ledger is the minimal authoritative account-state book keeping layer
// used by the pool (admission checks) and the packer (commit/revert).
//
// Apply() is atomic: a block whose transactions cannot all be executed in
// order (nonce mismatch or balance shortfall) is rejected wholesale and leaves
// no partial state change, which prevents balance double-spend and nonce gaps.
// Revert() restores before-images recorded by Apply(), enabling reorgs.
package ledger

import (
	"errors"
	"fmt"
	"math/big"
	"sync"

	"github.com/h3yow/go-txpool-fee-market-go/types"
)

// Sentinel errors returned by Apply so callers can classify commit failures.
var (
	ErrNonceMismatch      = errors.New("ledger: nonce mismatch during apply")
	ErrInsufficientFunds  = errors.New("ledger: insufficient funds during apply")
	ErrNonContiguousBlock = errors.New("ledger: non-contiguous block id")
)

// Ledger holds account state and an undo log enabling chain reorgs.
type Ledger struct {
	mu       sync.RWMutex
	accounts map[types.Address]*types.Account
	blocks   []*BlockSnapshot
}

// BlockSnapshot captures the state change caused by one packed block so it can
// be rolled back on a reorg.
type BlockSnapshot struct {
	BlockID uint64
	// Txs are the committed transactions, returned on revert for pool readd.
	Txs     []*types.Transaction
	Changes []AccountChange
}

// AccountChange is the before-image of one account touched by a block.
type AccountChange struct {
	Addr     types.Address
	Before   types.Account
	TxHashes []types.Hash
}

// New creates an empty Ledger.
func New() *Ledger {
	return &Ledger{accounts: make(map[types.Address]*types.Account)}
}

func copyAccount(a types.Account) types.Account {
	c := a
	if a.Balance != nil {
		c.Balance = new(big.Int).Set(a.Balance)
	}
	return c
}

// SetBalance seeds an account (tests / genesis setup).
func (l *Ledger) SetBalance(addr types.Address, balance *big.Int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.accounts[addr]
	if a == nil {
		a = &types.Account{Nonce: 0}
		l.accounts[addr] = a
	}
	a.Balance = new(big.Int).Set(balance)
}

// SetNonce seeds the account nonce (tests / chain sync).
func (l *Ledger) SetNonce(addr types.Address, nonce uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.accounts[addr]
	if a == nil {
		a = &types.Account{Balance: new(big.Int)}
		l.accounts[addr] = a
	}
	a.Nonce = nonce
}

// AddBalance credits an account.
func (l *Ledger) AddBalance(addr types.Address, amount *big.Int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.accounts[addr]
	if a == nil {
		a = &types.Account{Balance: new(big.Int)}
		l.accounts[addr] = a
	}
	if a.Balance == nil {
		a.Balance = new(big.Int)
	}
	a.Balance.Add(a.Balance, amount)
}

// GetAccount returns a copy of the current account state (zero-value account
// with nonce 0 / balance 0 if unknown).
func (l *Ledger) GetAccount(addr types.Address) types.Account {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if a := l.accounts[addr]; a != nil {
		return copyAccount(*a)
	}
	return types.Account{Balance: new(big.Int)}
}

// CanAfford reports whether the account currently has exactly the expected
// nonce and a balance covering cost.
func (l *Ledger) CanAfford(addr types.Address, nonce uint64, cost *big.Int) (bool, types.Account) {
	acc := l.GetAccount(addr)
	if acc.Balance == nil {
		acc.Balance = new(big.Int)
	}
	ok := acc.Nonce == nonce && acc.Balance.Cmp(cost) >= 0
	return ok, acc
}

// Apply commits a packed block atomically. Every transaction must execute in
// order: per sender, nonces must be strictly consecutive starting from the
// on-chain nonce, and the balance must cover value + effective tip*gas at
// commit time. blockID must extend the current head by exactly one.
func (l *Ledger) Apply(blockID uint64, baseFee *big.Int, txs []*types.Transaction) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	head := uint64(len(l.blocks))
	if blockID != head+1 {
		return fmt.Errorf("%w: got %d, expected %d", ErrNonContiguousBlock, blockID, head+1)
	}

	// Dry-run on local copies: build before-images and resulting balances.
	// working copies keyed by address, initialised lazily from state.
	type work struct {
		acc     types.Account
		changed bool
	}
	working := make(map[types.Address]*work)
	get := func(addr types.Address) *work {
		w, ok := working[addr]
		if !ok {
			if a := l.accounts[addr]; a != nil {
				w = &work{acc: copyAccount(*a)}
			} else {
				w = &work{acc: types.Account{Balance: new(big.Int)}}
			}
			working[addr] = w
		}
		return w
	}

	changes := make([]AccountChange, 0)
	beforeByAddr := make(map[types.Address]int)

	for i, tx := range txs {
		w := get(tx.From)
		if w.acc.Nonce != tx.Nonce {
			return fmt.Errorf("%w: tx[%d] from %x nonce %d != account nonce %d",
				ErrNonceMismatch, i, tx.From[:6], tx.Nonce, w.acc.Nonce)
		}
		tip := tx.EffectiveTip(baseFee)
		fee := new(big.Int).Mul(tip, new(big.Int).SetUint64(tx.Gas))
		need := new(big.Int).Add(tx.Value, fee)
		if w.acc.Balance.Cmp(need) < 0 {
			return fmt.Errorf("%w: tx[%d] from %x nonce %d balance %s < cost %s",
				ErrInsufficientFunds, i, tx.From[:6], tx.Nonce, w.acc.Balance, need)
		}
		if _, seen := beforeByAddr[tx.From]; !seen {
			beforeByAddr[tx.From] = len(changes)
			changes = append(changes, AccountChange{Addr: tx.From, Before: copyAccount(w.acc)})
		}
		w.acc.Balance.Sub(w.acc.Balance, need)
		w.acc.Nonce++
		w.changed = true
		ci := beforeByAddr[tx.From]
		changes[ci].TxHashes = append(changes[ci].TxHashes, tx.Hash())
	}

	// Commit phase: no more failures possible.
	for addr, w := range working {
		if w.changed {
			l.accounts[addr] = &types.Account{Nonce: w.acc.Nonce, Balance: new(big.Int).Set(w.acc.Balance)}
		}
	}
	l.blocks = append(l.blocks, &BlockSnapshot{
		BlockID: blockID,
		Txs:     txs,
		Changes: changes,
	})
	return nil
}

// Head returns the current block height recorded in the ledger.
func (l *Ledger) Head() uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return uint64(len(l.blocks))
}

// OverlayAccount replays an authoritative account state from the canonical
// side of a reorg after Revert has restored the local pre-fork snapshot.
func (l *Ledger) OverlayAccount(addr types.Address, nonce uint64, balance *big.Int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.accounts[addr]
	if a == nil {
		a = &types.Account{}
		l.accounts[addr] = a
	}
	a.Nonce = nonce
	a.Balance = new(big.Int).Set(balance)
}

// Revert rewinds the ledger to targetHead (must be below the current head),
// restoring before-images of every block above it newest-first. The detached
// snapshots are returned so the caller can re-admit their transactions.
func (l *Ledger) Revert(targetHead uint64) ([]*BlockSnapshot, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	head := uint64(len(l.blocks))
	if targetHead > head {
		return nil, fmt.Errorf("ledger: revert target %d above head %d", targetHead, head)
	}
	if targetHead == head {
		return nil, nil
	}

	detached := make([]*BlockSnapshot, 0, head-targetHead)
	for uint64(len(l.blocks)) > targetHead {
		snap := l.blocks[len(l.blocks)-1]
		l.blocks = l.blocks[:len(l.blocks)-1]
		// Restore before-images. Newest block first; a sender touched by
		// several reverted blocks is restored repeatedly until the oldest
		// before-image wins, which is exactly the target-head state.
		for _, ch := range snap.Changes {
			before := copyAccount(ch.Before)
			a := l.accounts[ch.Addr]
			if a == nil {
				a = &types.Account{}
				l.accounts[ch.Addr] = a
			}
			a.Nonce = before.Nonce
			if before.Balance != nil {
				a.Balance = new(big.Int).Set(before.Balance)
			} else {
				a.Balance = new(big.Int)
			}
		}
		detached = append(detached, snap)
	}
	return detached, nil
}
