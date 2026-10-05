package pool

import (
	"math/big"
	"testing"

	"github.com/h3yow/go-txpool-fee-market-go/reject"
	"github.com/h3yow/go-txpool-fee-market-go/types"
)

// TestAccountBudgetAdmission covers the account-level aggregate budget gate:
// a run of nonce-increasing txs from one account is admitted only while the
// WHOLE queue's maximum cost fits the balance; the over-budget tx is rejected
// with ACCOUNT_BUDGET (distinct from single-tx INSUFFICIENT_FUNDS), and the
// same tx enters the pool normally once the balance is topped up.
func TestAccountBudgetAdmission(t *testing.T) {
	// baseFee 0: admission cost = value + cap*gas = 0 + 2*100 = 200 per tx.
	p, l := setup(t, DefaultConfig(), 0)
	from := byte(1)
	l.SetBalance(addr(from), big.NewInt(1000))

	// Five txs at 200 max cost each = exactly 1000: all admitted.
	for n := uint64(0); n < 5; n++ {
		tx := newTx(from, n, 0, 1, 2, 100)
		if err := p.Submit(tx); err != nil {
			t.Fatalf("nonce %d should be admitted within budget, got %v", n, err)
		}
		t.Logf("ACCEPT  nonce=%d cost=%d cumulative=%d balance=1000 -> account budget OK",
			n, 200, 200*(n+1))
	}

	// The 6th tx is affordable BY ITSELF (200 < 1000) but the projected queue
	// would need 1200 > 1000: it must be rejected as ACCOUNT_BUDGET, not as
	// single-tx INSUFFICIENT_FUNDS.
	over := newTx(from, 5, 0, 1, 2, 100)
	err := p.Submit(over)
	d, ok := reject.AsDecision(err)
	if !ok {
		t.Fatalf("want *reject.Decision, got %v", err)
	}
	t.Logf("REJECT  nonce=5 cost=200 standalone affordable, projected queue=1200 balance=1000 -> %s | %s",
		d.Reason, d.Detail)
	if d.Reason != reject.ReasonAccountBudget {
		t.Fatalf("want ACCOUNT_BUDGET, got %s (%s)", d.Reason, d.Detail)
	}
	size, _ := p.Stats()
	if size != 5 {
		t.Fatalf("rejected tx must not enter pool; size=%d want 5", size)
	}

	// The single-tx case must keep its own, distinguishable reason.
	poor := byte(2)
	l.SetBalance(addr(poor), big.NewInt(100))
	alone := newTx(poor, 0, 0, 1, 2, 100) // cost 200 > 100 on its own
	err = p.Submit(alone)
	d, _ = reject.AsDecision(err)
	t.Logf("REJECT  nonce=0 standalone cost=200 balance=100 -> %s | %s", d.Reason, d.Detail)
	if d.Reason != reject.ReasonInsufficientFunds {
		t.Fatalf("single-tx shortfall must be INSUFFICIENT_FUNDS, got %s", d.Reason)
	}

	// An unrelated healthy account is not blocked by sender1's over budget.
	other := byte(3)
	l.SetBalance(addr(other), big.NewInt(1000))
	if err := p.Submit(newTx(other, 0, 0, 1, 2, 100)); err != nil {
		t.Fatalf("unrelated account must be admitted: %v", err)
	}
	t.Log("unrelated sender3 still admitted: account budget is per-account")

	// Top up sender1: the exact same tx (same hash) now enters normally.
	l.AddBalance(addr(from), big.NewInt(300))
	if err := p.Submit(over); err != nil {
		t.Fatalf("after top-up to 1300, same tx should enter pool, got %v", err)
	}
	t.Log("top-up +200 headroom (balance 1300 >= projected 1200): same nonce=5 tx now ACCEPT")
	size, _ = p.Stats()
	if size != 7 { // 5 + 1 (sender1) + 1 (sender3)
		t.Fatalf("pool size want 7, got %d", size)
	}
}

// TestReplacementAccountBudget ensures a fee-bumped replacement that passes the
// price rule can still be refused when it would push the account's whole queue
// over budget, and that it stays size-neutral (old tx retained on refusal).
func TestReplacementAccountBudget(t *testing.T) {
	p, l := setup(t, DefaultConfig(), 0)
	from := byte(1)
	l.SetBalance(addr(from), big.NewInt(1000))

	// nonces 0..4, max cost 200 each -> queue budget exactly 1000.
	for n := uint64(0); n < 5; n++ {
		if err := p.Submit(newTx(from, n, 0, 1, 2, 100)); err != nil {
			t.Fatal(err)
		}
	}
	// Replace nonce4: tip 1->2, cap 2->3 clears the 10% bump
	// (floor(1*1.1)=1, floor(2*1.1)=2), but its max cost rises 200->300 and
	// the projected queue becomes 1100 > 1000.
	bumped := newTx(from, 4, 0, 2, 3, 100)
	err := p.Submit(bumped)
	d, _ := reject.AsDecision(err)
	t.Logf("REJECT  replacement nonce=4 cost 200->300 projected queue=1100 balance=1000 -> %s | %s",
		d.Reason, d.Detail)
	if d == nil || d.Reason != reject.ReasonAccountBudget {
		t.Fatalf("want ACCOUNT_BUDGET on over-budget replacement, got %v", err)
	}
	size, _ := p.Stats()
	if size != 5 {
		t.Fatalf("refused replacement must keep size 5, got %d", size)
	}

	// After a top-up the same bumped replacement is accepted (size still 5).
	l.AddBalance(addr(from), big.NewInt(200))
	if err := p.Submit(bumped); err != nil {
		t.Fatalf("bumped replacement after top-up: %v", err)
	}
	t.Log("top-up to 1200 >= projected 1100: bumped replacement ACCEPT, size stays 5")
	if size, _ = p.Stats(); size != 5 {
		t.Fatalf("replacement size-neutral, want 5 got %d", size)
	}
}

// TestAccountViewReportsQueue inspects the queue-inspection entry point:
// pending txs, aggregate maximum budget, budget fit, and per-tx packability
// with reasons — including a balance reduction that only leaves a prefix ready.
func TestAccountViewReportsQueue(t *testing.T) {
	// baseFee 0: max cost cap*gas = 200; effective (packing) cost tip*gas = 100.
	p, l := setup(t, DefaultConfig(), 0)
	from := byte(1)
	l.SetBalance(addr(from), big.NewInt(1000))
	for n := uint64(0); n < 5; n++ {
		if err := p.Submit(newTx(from, n, 0, 1, 2, 100)); err != nil {
			t.Fatal(err)
		}
	}

	v := p.AccountView(addr(from))
	t.Logf("view: onchainNonce=%d balance=%s pending=%d maxBudget=%s budgetOK=%v ready=%d",
		v.OnchainNonce, v.Balance, len(v.Pending), v.MaxBudget, v.BudgetOK, v.ReadyCount)
	if v.MaxBudget.Cmp(big.NewInt(1000)) != 0 || !v.BudgetOK || v.ReadyCount != 5 {
		t.Fatalf("fully funded queue: maxBudget=1000 budgetOK=true ready=5, got %s/%v/%d",
			v.MaxBudget, v.BudgetOK, v.ReadyCount)
	}
	for _, pt := range v.Pending {
		t.Logf("  nonce=%d maxCost=%s cumMax=%s status=%s | %s",
			pt.Tx.Nonce, pt.MaxCost, pt.CumulativeMaxCost, pt.Status, pt.Reason)
		if pt.Status != TxPlanReady {
			t.Fatalf("nonce %d should be READY, got %s", pt.Tx.Nonce, pt.Status)
		}
	}

	// Balance reduced externally to 250: admission-budget basis now fails, and
	// the effective-fee simulation packs nonces 0,1 (100 each) and defers 2..4.
	l.SetBalance(addr(from), big.NewInt(250))
	v = p.AccountView(addr(from))
	t.Logf("after drain: balance=%s maxBudget=%s budgetOK=%v ready=%d",
		v.Balance, v.MaxBudget, v.BudgetOK, v.ReadyCount)
	if v.BudgetOK {
		t.Fatal("queue maxBudget 1000 > balance 250: BudgetOK must be false")
	}
	if v.ReadyCount != 2 {
		t.Fatalf("only nonce 0,1 effective-affordable (100 each), ready=%d want 2", v.ReadyCount)
	}
	for i, pt := range v.Pending {
		t.Logf("  nonce=%d status=%s | %s", pt.Tx.Nonce, pt.Status, pt.Reason)
		want := TxPlanReady
		if i >= 2 {
			want = TxPlanDeferredBudget
		}
		if pt.Status != want {
			t.Fatalf("nonce %d want %s, got %s", pt.Tx.Nonce, want, pt.Status)
		}
	}

	// In-flight reservations are surfaced separately.
	p.Reserve([]*types.Transaction{v.Pending[0].Tx})
	v = p.AccountView(addr(from))
	if len(v.ReservedNonces) != 1 || v.ReservedNonces[0] != 0 {
		t.Fatalf("reserved nonces want [0], got %v", v.ReservedNonces)
	}
	t.Logf("after reserve(nonce0): reserved=%v pending=%d ready=%d",
		v.ReservedNonces, len(v.Pending), v.ReadyCount)

	// Unknown account: empty but well-formed view.
	empty := p.AccountView(addr(42))
	if len(empty.Pending) != 0 || !empty.BudgetOK || empty.ReadyCount != 0 {
		t.Fatalf("unknown account view should be empty/OK, got %+v", empty)
	}
}

// TestAccountBudgetIncludesInFlightReservations reproduces the reported bug:
// txs already handed to the block being produced (Reserved) have NOT yet been
// deducted from the ledger balance in that window, but that money is spoken
// for. Subsequent submissions must be judged against the WHOLE account —
// in-flight reservations plus the pending queue — and the tx that over-commits
// the account must come back as ACCOUNT_BUDGET, distinct from a single-tx
// INSUFFICIENT_FUNDS, must not enter the pool or evict anything, and the same
// tx enters normally after the batch confirms and the balance is topped up.
func TestAccountBudgetIncludesInFlightReservations(t *testing.T) {
	// baseFee 0, tip == cap == 2, gas 100: max cost == effective cost == 200.
	p, l := setup(t, DefaultConfig(), 0)
	from := byte(1)
	l.SetBalance(addr(from), big.NewInt(1000))

	mk := func(n uint64) *types.Transaction { return newTx(from, n, 0, 2, 2, 100) }

	// Front of the run: nonces 0,1 are handed to the in-flight batch.
	for _, n := range []uint64{0, 1} {
		if err := p.Submit(mk(n)); err != nil {
			t.Fatalf("seed nonce %d: %v", n, err)
		}
	}
	p.Reserve([]*types.Transaction{mk(0), mk(1)})
	if size, _ := p.Stats(); size != 0 {
		t.Fatalf("reserved txs leave the pending view, size=%d want 0", size)
	}
	t.Log("in-flight batch holds nonces 0,1 (max cost 400); ledger balance still shows 1000 (not deducted yet)")

	// While the batch is unconfirmed, the account keeps submitting.
	for _, n := range []uint64{2, 3, 4} {
		err := p.Submit(mk(n))
		if err != nil {
			t.Fatalf("nonce %d must be admitted: reservations 400 + pending through %d fit 1000, got %v", n, n, err)
		}
	}
	t.Log("nonces 2,3,4 ACCEPT: in-flight 400 + queued 600 = 1000 <= balance 1000")

	// The 6th tx (nonce 5) is affordable ALONE (200 < 1000), yet the whole
	// account would need 400 (in-flight) + 800 (queued) = 1200 > 1000.
	over := mk(5)
	err := p.Submit(over)
	d, ok := reject.AsDecision(err)
	if !ok {
		t.Fatalf("want *reject.Decision for nonce5, got %v", err)
	}
	t.Logf("REJECT  nonce=5 cost=200 standalone affordable; in-flight 400 + projected queued 800 = 1200 > balance 1000 -> %s | %s",
		d.Reason, d.Detail)
	if d.Reason != reject.ReasonAccountBudget {
		t.Fatalf("want ACCOUNT_BUDGET counting the in-flight batch, got %s", d.Reason)
	}

	// It never entered the pool and nothing queued was evicted for it.
	if size, _ := p.Stats(); size != 3 {
		t.Fatalf("rejected tx must not enter pool; pending size=%d want 3 (nonces 2,3,4)", size)
	}
	if p.Has(over.Hash()) {
		t.Fatal("ACCOUNT_BUDGET tx must not be tracked by the pool")
	}
	for _, n := range []uint64{2, 3, 4} {
		if !p.Has(mk(n).Hash()) {
			t.Fatalf("existing queued nonce %d must not be evicted by a rejected tx", n)
		}
	}

	// Single-tx shortfall keeps its own reason even while reservations exist.
	poor := byte(2)
	l.SetBalance(addr(poor), big.NewInt(100))
	alone := newTx(poor, 0, 0, 2, 2, 100) // cost 200 > 100 on its own
	err = p.Submit(alone)
	d, _ = reject.AsDecision(err)
	t.Logf("REJECT  sender2 nonce=0 standalone cost=200 balance=100 -> %s | %s", d.Reason, d.Detail)
	if d.Reason != reject.ReasonInsufficientFunds {
		t.Fatalf("single-tx shortfall must stay INSUFFICIENT_FUNDS, got %s", d.Reason)
	}

	// An unrelated healthy account is admitted normally.
	other := byte(3)
	l.SetBalance(addr(other), big.NewInt(1000))
	if err := p.Submit(newTx(other, 0, 0, 2, 2, 100)); err != nil {
		t.Fatalf("unrelated account must be admitted: %v", err)
	}
	t.Log("unrelated sender3 admitted: the budget gate is per-account")

	// The queue view must report money occupied by the in-flight batch too.
	v := p.AccountView(addr(from))
	t.Logf("view while in-flight: reserved=%v reservedMax=%s reservedEff=%s pendingMax=%s maxBudget=%s budgetOK=%v ready=%d",
		v.ReservedNonces, v.ReservedMaxBudget, v.ReservedEffCost, v.PendingMaxBudget, v.MaxBudget, v.BudgetOK, v.ReadyCount)
	if len(v.ReservedNonces) != 2 || v.ReservedNonces[0] != 0 || v.ReservedNonces[1] != 1 {
		t.Fatalf("reserved nonces want [0 1], got %v", v.ReservedNonces)
	}
	if v.ReservedMaxBudget.Cmp(big.NewInt(400)) != 0 || v.ReservedEffCost.Cmp(big.NewInt(400)) != 0 {
		t.Fatalf("in-flight occupation want 400/400, got %s/%s", v.ReservedMaxBudget, v.ReservedEffCost)
	}
	if v.PendingMaxBudget.Cmp(big.NewInt(600)) != 0 || v.MaxBudget.Cmp(big.NewInt(1000)) != 0 {
		t.Fatalf("budgets want pending 600 / whole-run 1000, got %s / %s", v.PendingMaxBudget, v.MaxBudget)
	}
	if !v.BudgetOK || v.ReadyCount != 3 {
		t.Fatalf("whole run 1000 <= 1000 with 3 queued ready, got budgetOK=%v ready=%d", v.BudgetOK, v.ReadyCount)
	}

	// The in-flight batch finalises: ledger deducts its 400 (balance 600),
	// nonce advances to 2, reservations are confirmed.
	if err := l.Apply(1, big.NewInt(0), []*types.Transaction{mk(0), mk(1)}); err != nil {
		t.Fatalf("apply in-flight batch: %v", err)
	}
	p.Confirm([]*types.Transaction{mk(0), mk(1)})
	if got := l.GetAccount(addr(from)); got.Nonce != 2 || got.Balance.Cmp(big.NewInt(600)) != 0 {
		t.Fatalf("after confirmation want nonce2 bal600, got %d/%s", got.Nonce, got.Balance)
	}

	// Still over budget right after confirmation (queued 600 + 200 > 600).
	if err := p.Submit(over); err == nil {
		t.Fatal("nonce5 must still be ACCOUNT_BUDGET right after the batch deducted")
	} else {
		d, _ := reject.AsDecision(err)
		t.Logf("REJECT  post-confirm nonce=5: queued 600 + 200 > balance 600 -> %s", d.Reason)
	}

	// Top up: the exact same tx now enters normally and packs after the queue.
	l.AddBalance(addr(from), big.NewInt(600))
	if err := p.Submit(over); err != nil {
		t.Fatalf("after top-up to 1200, same nonce5 tx must enter, got %v", err)
	}
	t.Log("top-up +600 (balance 1200): same nonce=5 tx now ACCEPT")
	if size, _ := p.Stats(); size != 5 { // sender1 nonces 2..5 + sender3 nonce0
		t.Fatalf("global pending size want 5, got %d", size)
	}
	if vv := p.AccountView(addr(from)); len(vv.Pending) != 4 {
		t.Fatalf("sender1 queue want 4 txs (nonces 2..5), got %d", len(vv.Pending))
	}
}

// TestAccountViewCountsInFlightOccupation checks that the queue-inspection
// entry point keeps the same account-level basis as admission and packing when
// part of the run is already in an in-flight batch: both the aggregate budget
// answer and the "how many are packable right now" answer must subtract the
// money committed to the unconfirmed batch.
func TestAccountViewCountsInFlightOccupation(t *testing.T) {
	// baseFee 0, tip == cap == 2, gas 100: 200 max cost == 200 effective cost.
	p, l := setup(t, DefaultConfig(), 0)
	from := byte(1)
	l.SetBalance(addr(from), big.NewInt(700))
	mk := func(n uint64) *types.Transaction { return newTx(from, n, 0, 2, 2, 100) }

	for _, n := range []uint64{0, 1} {
		if err := p.Submit(mk(n)); err != nil {
			t.Fatal(err)
		}
	}
	p.Reserve([]*types.Transaction{mk(0), mk(1)}) // in-flight batch occupies 400
	if err := p.Submit(mk(2)); err != nil {       // 400 + 200 = 600 <= 700
		t.Fatalf("nonce2 must be admitted with the in-flight batch counted: %v", err)
	}
	// n3 would need 400 + 400 = 800 > 700: rejected, so it never enters.
	d, _ := reject.AsDecision(p.Submit(mk(3)))
	if d == nil || d.Reason != reject.ReasonAccountBudget {
		t.Fatalf("nonce3 want ACCOUNT_BUDGET, got %v", d)
	}
	t.Log("in-flight 400 + queued 400 = 800 > 700: nonce3 rejected ACCOUNT_BUDGET at admission")

	v := p.AccountView(addr(from))
	t.Logf("view: balance=%s reservedNonces=%v reservedMax=%s pendingMax=%s maxBudget=%s budgetOK=%v ready=%d",
		v.Balance, v.ReservedNonces, v.ReservedMaxBudget, v.PendingMaxBudget, v.MaxBudget, v.BudgetOK, v.ReadyCount)
	if v.ReservedMaxBudget.Cmp(big.NewInt(400)) != 0 || v.PendingMaxBudget.Cmp(big.NewInt(200)) != 0 {
		t.Fatalf("want reserved 400 / pending 200, got %s / %s", v.ReservedMaxBudget, v.PendingMaxBudget)
	}
	if v.MaxBudget.Cmp(big.NewInt(600)) != 0 || !v.BudgetOK || v.ReadyCount != 1 {
		t.Fatalf("whole run 600 <= 700, one queued tx packable after reserving 400: maxBudget=%s budgetOK=%v ready=%d",
			v.MaxBudget, v.BudgetOK, v.ReadyCount)
	}

	// Balance shrinks externally while the batch is in flight. The ledger still
	// shows the reduced number and the 400 remains un-deducted, so both views
	// must tighten: the queue as a whole no longer fits and no pending tx is
	// immediately packable (700-200 drain leaves 500; 500-400 in-flight = 100
	// < 200 for nonce2).
	l.SetBalance(addr(from), big.NewInt(500))
	v = p.AccountView(addr(from))
	t.Logf("after drain to 500: reservedMax=%s maxBudget=%s budgetOK=%v ready=%d; nonce2=%s | %s",
		v.ReservedMaxBudget, v.MaxBudget, v.BudgetOK, v.ReadyCount, v.Pending[0].Status, v.Pending[0].Reason)
	if v.BudgetOK {
		t.Fatalf("whole run 600 > balance 500: BudgetOK must be false")
	}
	if v.ReadyCount != 0 || v.Pending[0].Status != TxPlanDeferredBudget {
		t.Fatalf("nonce2 must be DEFERRED_BUDGET (500-400 in-flight=100 < 200), ready=%d status=%s",
			v.ReadyCount, v.Pending[0].Status)
	}

	// Once the batch confirms, nonce advances and the stale reservations no
	// longer count against the budget (the ledger actually holds the deduction).
	if err := l.Apply(1, big.NewInt(0), []*types.Transaction{mk(0), mk(1)}); err != nil {
		t.Fatal(err)
	}
	p.Confirm([]*types.Transaction{mk(0), mk(1)})
	v = p.AccountView(addr(from))
	if v.OnchainNonce != 2 || len(v.ReservedNonces) != 0 || v.ReservedMaxBudget.Sign() != 0 {
		t.Fatalf("after confirm: nonce2/no live reservations/0 occupied, got nonce=%d reserved=%v occupied=%s",
			v.OnchainNonce, v.ReservedNonces, v.ReservedMaxBudget)
	}
	t.Logf("after confirm: onchainNonce=%d reserved=%v occupied=0; queued nonce2 judged against the real deducted balance",
		v.OnchainNonce, v.ReservedNonces)
}
