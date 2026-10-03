// Package pool is a fee-prioritised transaction pool with per-account nonce
// ordering, price-bump replacement and deterministic eviction.
//
// Invariants (pool view == ledger view):
//   - Per account, pending transactions occupy a contiguous nonce run starting
//     at ledgerNonce + numberReserved. Gapped submissions are rejected.
//   - A same-account/same-nonce replacement must raise BOTH the effective tip
//     (at the pool's base fee) and the fee cap by at least PriceBumpPct.
//   - Admission has two funds levels: the tx alone must be affordable
//     (INSUFFICIENT_FUNDS otherwise), AND the account's whole queued run
//     (in-flight reservations + pending txs, with the incoming tx appended)
//     must be payable at maximum cost from the current balance; otherwise it
//     is rejected with ACCOUNT_BUDGET, so queued txs never become collectively
//     unpackable due to admission alone.
//   - When global capacity is full, the globally cheapest per-account tail is
//     evicted, and only if the incoming tx strictly outbids it. Evicting tails
//     (never middles) keeps every account's nonce run contiguous.
package pool

import (
	"math/big"
	"sort"
	"sync"

	"github.com/h3yow/go-txpool-fee-market-go/ledger"
	"github.com/h3yow/go-txpool-fee-market-go/reject"
	"github.com/h3yow/go-txpool-fee-market-go/types"
)

// Config configures the pool.
type Config struct {
	GlobalSlots  uint64 // max pending transactions held globally
	AccountSlots uint64 // max pending transactions per account
	PriceBumpPct uint64 // required tip/feecap bump for replacement, percent
}

// DefaultConfig returns sane defaults: 4096 global slots, 16 per account, 10%.
func DefaultConfig() Config {
	return Config{GlobalSlots: 4096, AccountSlots: 16, PriceBumpPct: 10}
}

var (
	big100 = big.NewInt(100)
)

// Pool stores pending transactions keyed by account+nonce.
type Pool struct {
	cfg     Config
	ledger  *ledger.Ledger
	baseFee *big.Int
	mu      sync.Mutex

	all      map[types.Hash]*types.Transaction
	accounts map[types.Address]*accountQueue
	size     uint64
}

// accountQueue keeps one pending tx per nonce plus packed-but-unconfirmed ones.
type accountQueue struct {
	txs      map[uint64]*types.Transaction // pending (visible to Pending)
	reserved map[uint64]*types.Transaction // packed in an in-flight block
}

// New constructs a Pool backed by the given ledger.
func New(cfg Config, l *ledger.Ledger, baseFee *big.Int) *Pool {
	if baseFee == nil {
		baseFee = new(big.Int)
	}
	return &Pool{
		cfg:      cfg,
		ledger:   l,
		baseFee:  new(big.Int).Set(baseFee),
		all:      make(map[types.Hash]*types.Transaction),
		accounts: make(map[types.Address]*accountQueue),
	}
}

// SetBaseFee updates the base fee used for tip ranking and replacement checks.
func (p *Pool) SetBaseFee(baseFee *big.Int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.baseFee = new(big.Int).Set(baseFee)
}

// nextExpectedLocked returns the next admissible nonce for the account. It
// walks from the on-chain nonce past both in-flight reservations and the
// contiguous pending run. Reservations with nonces below the on-chain nonce
// are stale (the ledger committed them while Confirm had not yet run) and are
// therefore ignored; this keeps the pool view and ledger view consistent in
// that window.
func (p *Pool) nextExpectedLocked(onchainNonce uint64, q *accountQueue) uint64 {
	expected := onchainNonce
	for {
		if _, isReserved := q.reserved[expected]; isReserved {
			expected++
			continue
		}
		if _, ok := q.txs[expected]; ok {
			expected++
			continue
		}
		return expected
	}
}

// Submit validates and inserts a transaction. Rejections return a
// *reject.Decision with a distinguishable reason.
func (p *Pool) Submit(tx *types.Transaction) error {
	if err := validateFormat(tx); err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// The ledger nonce is authoritative: anything below it is stale even if
	// the pool still holds an identical copy.
	acc := p.ledger.GetAccount(tx.From)
	if tx.Nonce < acc.Nonce {
		return reject.New(tx, reject.ReasonNonceTooLow,
			"nonce %d below on-chain nonce %d", tx.Nonce, acc.Nonce)
	}

	h := tx.Hash()
	q := p.accounts[tx.From]
	if q != nil {
		if old, ok := q.txs[tx.Nonce]; ok {
			if old.Hash() == h {
				return nil // identical resubmission: idempotent no-op
			}
			return p.replaceLocked(tx, old)
		}
		if _, reserved := q.reserved[tx.Nonce]; reserved {
			return reject.New(tx, reject.ReasonAlreadyReserved,
				"nonce %d already packed into an in-flight block", tx.Nonce)
		}
	}

	if q != nil {
		expected := p.nextExpectedLocked(acc.Nonce, q)
		if tx.Nonce < expected {
			return reject.New(tx, reject.ReasonNonceTooLow,
				"nonce %d already occupied (expected %d)", tx.Nonce, expected)
		}
		if tx.Nonce > expected {
			return reject.New(tx, reject.ReasonNonceTooHigh,
				"nonce %d leaves a gap: expected %d (on-chain %d, %d reserved)",
				tx.Nonce, expected, acc.Nonce, len(q.reserved))
		}
	} else if tx.Nonce != acc.Nonce {
		// No local queue: anything above the on-chain nonce is a gap.
		return reject.New(tx, reject.ReasonNonceTooHigh,
			"nonce %d does not match on-chain nonce %d", tx.Nonce, acc.Nonce)
	}

	balance := acc.Balance
	if balance == nil {
		balance = new(big.Int)
	}
	// Two-level funds gate. First the single tx taken alone ...
	if cost := tx.Cost(); balance.Cmp(cost) < 0 {
		return reject.New(tx, reject.ReasonInsufficientFunds,
			"balance %s < value+maxFee cost %s (nonce %d)", balance, cost, tx.Nonce)
	}
	// ... then the account's whole queued run (in-flight reservations plus
	// pending txs) with this tx appended. Every admitted tx extends the
	// contiguous nonce run, and eviction only ever trims OTHER accounts, so
	// this picture cannot change before insertLocked below.
	if q != nil {
		if total := p.budgetAfterAdmitLocked(q, tx); balance.Cmp(total) < 0 {
			return reject.New(tx, reject.ReasonAccountBudget,
				"account %x cannot fund its whole queue: balance %s < aggregated max cost %s "+
					"(incoming nonce %d cost %s on top of %d queued txs); top up the balance and resubmit",
				tx.From[:6], balance, total, tx.Nonce, tx.Cost(), len(q.reserved)+len(q.txs))
		}
	}

	return p.insertLocked(tx)
}

// budgetAfterAdmitLocked returns the aggregate maximum cost the account would
// be committed to if tx were admitted at its nonce: sum of value+GasFeeCap*gas
// over every in-flight reservation and every pending tx, with a same-nonce
// pending tx superseded by the incoming replacement. Caller holds mu.
func (p *Pool) budgetAfterAdmitLocked(q *accountQueue, tx *types.Transaction) *big.Int {
	total := new(big.Int)
	for _, r := range q.reserved {
		total.Add(total, r.Cost())
	}
	for n, t := range q.txs {
		if n == tx.Nonce {
			continue // replacement: old tx is swapped for the incoming one
		}
		total.Add(total, t.Cost())
	}
	total.Add(total, tx.Cost())
	return total
}

// validateFormat checks field-level legality.
func validateFormat(tx *types.Transaction) error {
	if tx == nil {
		return reject.New(nil, reject.ReasonMalformed, "nil transaction")
	}
	if tx.Gas == 0 {
		return reject.New(tx, reject.ReasonMalformed, "gas limit is zero")
	}
	if tx.Value == nil || tx.GasFeeCap == nil || tx.GasTipCap == nil {
		return reject.New(tx, reject.ReasonMalformed, "value/feeCap/tipCap must not be nil")
	}
	if tx.Value.Sign() < 0 || tx.GasFeeCap.Sign() < 0 || tx.GasTipCap.Sign() < 0 {
		return reject.New(tx, reject.ReasonMalformed, "value/fees must not be negative")
	}
	if tx.GasTipCap.Cmp(tx.GasFeeCap) > 0 {
		return reject.New(tx, reject.ReasonMalformed,
			"tipCap %s exceeds feeCap %s", tx.GasTipCap, tx.GasFeeCap)
	}
	return nil
}

// bumpFloor returns ceil(old * (100+pct) / 100) - 1 ... actually the minimum
// acceptable new bid: floor(old*(100+pct)/100); new must be >= it.
func bumpFloor(old *big.Int, pct uint64) *big.Int {
	r := new(big.Int).Mul(old, new(big.Int).SetUint64(100+pct))
	return r.Div(r, big100)
}

// replaceLocked enforces the fee-bump replacement rule.
func (p *Pool) replaceLocked(tx, old *types.Transaction) error {
	minTip := bumpFloor(old.EffectiveTip(p.baseFee), p.cfg.PriceBumpPct)
	minCap := bumpFloor(old.GasFeeCap, p.cfg.PriceBumpPct)
	newTip := tx.EffectiveTip(p.baseFee)
	if newTip.Cmp(minTip) < 0 || tx.GasFeeCap.Cmp(minCap) < 0 {
		return reject.New(tx, reject.ReasonUnderpriced,
			"replacement at nonce %d needs effective tip >= %s (got %s) and feeCap >= %s (got %s), bump %d%%",
			tx.Nonce, minTip, newTip, minCap, tx.GasFeeCap, p.cfg.PriceBumpPct)
	}
	// Re-check funds for the replacement (it may cost more): first the
	// replacement alone, then the account's whole queued budget with the old
	// same-nonce tx superseded by the replacement.
	acc := p.ledger.GetAccount(tx.From)
	balance := acc.Balance
	if balance == nil {
		balance = new(big.Int)
	}
	if cost := tx.Cost(); balance.Cmp(cost) < 0 {
		return reject.New(tx, reject.ReasonInsufficientFunds,
			"replacement balance %s < cost %s (nonce %d)", balance, cost, tx.Nonce)
	}
	// Size-neutral swap.
	q := p.accounts[tx.From]
	if total := p.budgetAfterAdmitLocked(q, tx); balance.Cmp(total) < 0 {
		return reject.New(tx, reject.ReasonAccountBudget,
			"replacement would make account %x unaffordable as a queue: balance %s < aggregated max cost %s (nonce %d)",
			tx.From[:6], balance, total, tx.Nonce)
	}
	q.txs[tx.Nonce] = tx
	delete(p.all, old.Hash())
	p.all[tx.Hash()] = tx
	return nil
}

// insertLocked adds the tx, enforcing the per-account cap and global capacity
// via deterministic cheapest-tail eviction. Caller holds mu.
func (p *Pool) insertLocked(tx *types.Transaction) error {
	q := p.accounts[tx.From]
	if q == nil {
		q = &accountQueue{txs: map[uint64]*types.Transaction{}, reserved: map[uint64]*types.Transaction{}}
		p.accounts[tx.From] = q
	}
	if uint64(len(q.txs)) >= p.cfg.AccountSlots {
		return reject.New(tx, reject.ReasonPoolFull,
			"account %x already holds %d pending txs (per-account cap %d)",
			tx.From[:6], len(q.txs), p.cfg.AccountSlots)
	}

	incomingTip := tx.EffectiveTip(p.baseFee)
	for p.size >= p.cfg.GlobalSlots {
		victim := p.cheapestTailLocked(tx.From, tx.Nonce)
		if victim == nil {
			return reject.New(tx, reject.ReasonUnderpriced,
				"pool full (%d slots) and no evictable tail cheaper than incoming tip %s",
				p.cfg.GlobalSlots, incomingTip)
		}
		vtip := victim.EffectiveTip(p.baseFee)
		if vtip.Cmp(incomingTip) >= 0 {
			return reject.New(tx, reject.ReasonUnderpriced,
				"pool full and cheapest tail tip %s >= incoming tip %s; cannot outbid",
				vtip, incomingTip)
		}
		p.evictTailLocked(victim)
	}

	q.txs[tx.Nonce] = tx
	p.all[tx.Hash()] = tx
	p.size++
	// Reap queues emptied by eviction only AFTER the new tx is installed,
	// otherwise the incoming sender's freshly-created empty queue would be
	// garbage-collected before its first tx lands.
	p.gcLocked()
	return nil
}

// cheapestTailLocked finds the per-account highest-nonce pending tx with the
// smallest effective tip. Tails are the only evictable txs because removing a
// middle nonce would split the account's contiguous run.
//
// Tails of the incoming sender that sit BELOW the incoming nonce are not
// candidates: removing one would gap the incoming tx itself.
func (p *Pool) cheapestTailLocked(incomingFrom types.Address, incomingNonce uint64) *types.Transaction {
	var best *types.Transaction
	for addr, q := range p.accounts {
		if len(q.txs) == 0 {
			continue
		}
		var maxNonce uint64
		first := true
		for n := range q.txs {
			if first || n > maxNonce {
				maxNonce, first = n, false
			}
		}
		cand := q.txs[maxNonce]
		if addr == incomingFrom && maxNonce < incomingNonce {
			continue
		}
		if best == nil || tipsBefore(cand, best, p.baseFee) {
			best = cand
		}
	}
	return best
}

// tipsBefore reports whether a ranks before b as a cheaper eviction target:
// lower effective tip; ties broken by sender address then nonce, for stable,
// explainable behaviour.
func tipsBefore(a, b *types.Transaction, baseFee *big.Int) bool {
	ta := a.EffectiveTip(baseFee)
	tb := b.EffectiveTip(baseFee)
	if c := ta.Cmp(tb); c != 0 {
		return c < 0
	}
	if c := addrCmp(a.From, b.From); c != 0 {
		return c < 0
	}
	return a.Nonce < b.Nonce
}

func addrCmp(a, b types.Address) int {
	for i := 0; i < len(a); i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// evictTailLocked removes the highest-nonce pending tx of the victim's
// account (the victim itself). If higher nonces existed they could never be
// contiguous after this point, so the whole suffix is dropped.
func (p *Pool) evictTailLocked(victim *types.Transaction) {
	q := p.accounts[victim.From]
	maxNonce := victim.Nonce
	for n := range q.txs {
		if n > maxNonce {
			maxNonce = n
		}
	}
	for n := maxNonce; ; n-- {
		t, ok := q.txs[n]
		if !ok {
			break
		}
		delete(q.txs, n)
		delete(p.all, t.Hash())
		p.size--
		if n == victim.Nonce {
			break
		}
	}
}

// Pending returns transactions ready to pack. Per account only the nonce
// prefix contiguous from the on-chain nonce (past reservations) is offered.
// Across accounts the heads are merged by effective tip descending, so each
// account's transactions always appear in nonce order.
func (p *Pool) Pending(baseFee *big.Int) []*types.Transaction {
	if baseFee == nil {
		baseFee = new(big.Int)
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	type head struct {
		tx        *types.Transaction
		nextN     uint64
		q         *accountQueue
		exhausted bool
	}
	var heads []*head
	for addr, q := range p.accounts {
		if len(q.txs) == 0 {
			continue
		}
		acc := p.ledger.GetAccount(addr)
		n := acc.Nonce
		for {
			if _, isReserved := q.reserved[n]; isReserved {
				n++
				continue
			}
			break
		}
		tx, ok := q.txs[n]
		if !ok {
			continue // pending run starts after a gap; nothing packable
		}
		heads = append(heads, &head{tx: tx, nextN: n + 1, q: q})
	}

	out := make([]*types.Transaction, 0, p.size)
	for {
		// Among non-exhausted heads pick the highest-tip one; ties: lower
		// address then lower nonce (deterministic).
		best := -1
		for i, h := range heads {
			if h.exhausted {
				continue
			}
			if best == -1 || headsPickBefore(h.tx, heads[best].tx, baseFee) {
				best = i
			}
		}
		if best == -1 {
			break
		}
		h := heads[best]
		out = append(out, h.tx)
		if next, ok := h.q.txs[h.nextN]; ok {
			h.tx = next
			h.nextN++
		} else {
			h.exhausted = true
		}
	}
	return out
}

// headsPickBefore: a picks before b when its effective tip is higher;
// ties by smaller sender address then smaller nonce (deterministic).
func headsPickBefore(a, b *types.Transaction, baseFee *big.Int) bool {
	if c := a.EffectiveTip(baseFee).Cmp(b.EffectiveTip(baseFee)); c != 0 {
		return c > 0
	}
	if c := addrCmp(a.From, b.From); c != 0 {
		return c < 0
	}
	return a.Nonce < b.Nonce
}

// Reserve marks the transactions as packed into an in-flight block. They leave
// the pending view (and the pending capacity counter) but stay tracked so
// Confirm/Unreserve can act on them by identity.
func (p *Pool) Reserve(txs []*types.Transaction) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, tx := range txs {
		q := p.accounts[tx.From]
		if q == nil {
			continue
		}
		old, ok := q.txs[tx.Nonce]
		if !ok {
			continue
		}
		delete(q.txs, tx.Nonce)
		q.reserved[tx.Nonce] = old
		p.size--
	}
}

// Confirm drops reserved transactions after their block commits. Matching is
// by sender+nonce (the authoritative position), which also clears the pool's
// hash index using the actually stored object.
func (p *Pool) Confirm(txs []*types.Transaction) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, tx := range txs {
		q := p.accounts[tx.From]
		if q == nil {
			continue
		}
		if rt, ok := q.reserved[tx.Nonce]; ok {
			delete(q.reserved, tx.Nonce)
			delete(p.all, rt.Hash())
		}
	}
	p.gcLocked()
}

// Unreserve returns in-flight transactions to the pending view after a failed
// produce call (ledger rejected the block); no state changed underneath.
func (p *Pool) Unreserve(txs []*types.Transaction) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, tx := range txs {
		q := p.accounts[tx.From]
		if q == nil {
			continue
		}
		if rt, ok := q.reserved[tx.Nonce]; ok {
			delete(q.reserved, tx.Nonce)
			if _, exists := q.txs[tx.Nonce]; !exists {
				q.txs[tx.Nonce] = rt
				p.size++
			}
		}
	}
	p.gcLocked()
}

// Readd re-admits transactions from a reverted block, revalidating each one
// against the current ledger state and pool capacity. It returns a decision
// per rejected transaction; accepted ones carry nil.
func (p *Pool) Readd(txs []*types.Transaction) []*reject.Decision {
	// First release any stale reservations that happened to match.
	p.Unreserve(txs)

	results := make([]*reject.Decision, len(txs))
	for i, tx := range txs {
		if err := p.Submit(tx); err != nil {
			d, _ := reject.AsDecision(err)
			results[i] = d
		}
	}
	return results
}

// gcLocked drops empty queues (caller holds mu).
func (p *Pool) gcLocked() {
	for addr, q := range p.accounts {
		if len(q.txs) == 0 && len(q.reserved) == 0 {
			delete(p.accounts, addr)
		}
	}
}

// Stats returns pending size and the number of tracked accounts.
func (p *Pool) Stats() (size, accounts uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.size, uint64(len(p.accounts))
}

// Has reports whether a tx hash is tracked (pending or reserved).
func (p *Pool) Has(h types.Hash) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.all[h]
	if ok {
		return true
	}
	for _, q := range p.accounts {
		for _, tx := range q.reserved {
			if tx.Hash() == h {
				return true
			}
		}
	}
	return false
}

// PendingTipFloor returns the lowest effective tip among all pending txs.
func (p *Pool) PendingTipFloor(baseFee *big.Int) *big.Int {
	p.mu.Lock()
	defer p.mu.Unlock()
	floor := (*big.Int)(nil)
	for _, q := range p.accounts {
		// gather sorted nonces for deterministic scan
		nonces := make([]uint64, 0, len(q.txs))
		for n := range q.txs {
			nonces = append(nonces, n)
		}
		sort.Slice(nonces, func(i, j int) bool { return nonces[i] < nonces[j] })
		for _, n := range nonces {
			t := q.txs[n].EffectiveTip(baseFee)
			if floor == nil || t.Cmp(floor) < 0 {
				floor = new(big.Int).Set(t)
			}
		}
	}
	if floor == nil {
		return new(big.Int)
	}
	return floor
}
