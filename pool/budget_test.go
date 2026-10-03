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
