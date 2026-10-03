package pool

import (
	"math/big"
	"testing"

	"github.com/h3yow/go-txpool-fee-market-go/reject"
	"github.com/h3yow/go-txpool-fee-market-go/types"
)

// TestAccountBudgetAdmission covers the account-level budget gate: each tx
// alone may be affordable while the account's queued run as a whole is not.
func TestAccountBudgetAdmission(t *testing.T) {
	p, l := setup(t, DefaultConfig(), 0)
	from := byte(5)
	// Each tx: value 100, cap 10, gas 100 -> max cost = 100 + 10*100 = 1100.
	mk := func(n uint64) *types.Transaction { return newTx(from, n, 100, 1, 10, 100) }

	// Balance 2500: tx0 (1100) and tx1 (2200 cumulative) fit, tx2 would push
	// the queue to 3300 > 2500 and must be rejected with ACCOUNT_BUDGET while
	// each individual tx only costs 1100 (so it is NOT a single-tx shortfall).
	l.SetBalance(addr(from), big.NewInt(2500))

	expectReason(t, p, mk(0), reject.ReasonUnknown)
	expectReason(t, p, mk(1), reject.ReasonUnknown)
	view := p.AccountStatus(addr(from), big.NewInt(0))
	t.Logf("after n0,n1: balance=%s queuedMaxCost=%s budgetOK=%v packable=%d",
		view.Balance, view.QueuedMaxCost, view.BudgetOK, countState(view, ItemPackable))

	err := p.Submit(mk(2))
	d, ok := reject.AsDecision(err)
	if !ok {
		t.Fatalf("n2: expected *Decision, got %v", err)
	}
	if d.Reason != reject.ReasonAccountBudget {
		t.Fatalf("n2: want ACCOUNT_BUDGET, got %s: %s", d.Reason, d.Detail)
	}
	t.Logf("REJECT  nonce=2 reason=%s | %s", d.Reason, d.Detail)

	size, _ := p.Stats()
	if size != 2 {
		t.Fatalf("rejected tx must not enter pool, size=%d want 2", size)
	}

	// A single-tx shortfall must still be classified INSUFFICIENT_FUNDS, not
	// ACCOUNT_BUDGET (the two reasons must stay distinguishable).
	poor := byte(6)
	l.SetBalance(addr(poor), big.NewInt(100))
	t.Logf("poor account balance=100, single tx maxCost=1100 -> single-tx gate fires first")
	expectReason(t, p, newTx(poor, 0, 100, 1, 10, 100), reject.ReasonInsufficientFunds)

	// Top up and resubmit: identical tx now passes the account budget gate.
	l.SetBalance(addr(from), big.NewInt(10_000))
	expectReason(t, p, mk(2), reject.ReasonUnknown)
	view = p.AccountStatus(addr(from), big.NewInt(0))
	if !view.BudgetOK {
		t.Fatalf("after top-up queuedMaxCost=%s balance=%s must be affordable",
			view.QueuedMaxCost, view.Balance)
	}
	if got := countState(view, ItemPackable); got != 3 {
		t.Fatalf("all 3 queued txs packable after top-up, got %d", got)
	}
	t.Logf("top-up accepted nonce=2: balance=%s queuedMaxCost=%s packable=3",
		view.Balance, view.QueuedMaxCost)
}

// TestAccountStatusView verifies the introspection snapshot distinguishes
// immediately packable, no-funds and deferred items once the live balance
// shrinks under the queued run.
func TestAccountStatusView(t *testing.T) {
	p, l := setup(t, DefaultConfig(), 0)
	from := byte(7)
	// tip == cap == 10 -> exact cost at baseFee 0 equals max cost = 1100,
	// which keeps the admission budget and the packability simulation aligned.
	mk := func(n uint64) *types.Transaction { return newTx(from, n, 100, 10, 10, 100) }
	l.SetBalance(addr(from), big.NewInt(3300))
	for n := uint64(0); n < 3; n++ {
		if err := p.Submit(mk(n)); err != nil {
			t.Fatalf("submit n%d: %v", n, err)
		}
	}

	// Healthy view.
	view := p.AccountStatus(addr(from), big.NewInt(0))
	if view.BudgetOK != true || countState(view, ItemPackable) != 3 {
		t.Fatalf("healthy view: budgetOK=%v packable=%d items=%d",
			view.BudgetOK, countState(view, ItemPackable), len(view.Items))
	}
	t.Logf("healthy: balance=%s queuedMaxCost=%s budgetOK=%v",
		view.Balance, view.QueuedMaxCost, view.BudgetOK)

	// Balance reduced externally so only the first two txs remain executable.
	l.SetBalance(addr(from), big.NewInt(2300))
	view = p.AccountStatus(addr(from), big.NewInt(0))
	if view.BudgetOK {
		t.Fatalf("queuedMaxCost=%s > balance=%s must show BudgetOK=false",
			view.QueuedMaxCost, view.Balance)
	}
	wantStates := []ItemState{ItemPackable, ItemPackable, ItemNoFunds}
	for i, it := range view.Items {
		t.Logf("item nonce=%d state=%s maxCost=%s | %s", it.Tx.Nonce, it.State, it.MaxCost, it.Reason)
		if it.State != wantStates[i] {
			t.Fatalf("item %d: want %s got %s", i, wantStates[i], it.State)
		}
	}

	// Drain further: the first tx alone no longer fits.
	l.SetBalance(addr(from), big.NewInt(500))
	view = p.AccountStatus(addr(from), big.NewInt(0))
	wantStates = []ItemState{ItemNoFunds, ItemDeferred, ItemDeferred}
	for i, it := range view.Items {
		t.Logf("item nonce=%d state=%s | %s", it.Tx.Nonce, it.State, it.Reason)
		if it.State != wantStates[i] {
			t.Fatalf("drained item %d: want %s got %s", i, wantStates[i], it.State)
		}
	}

	// Unknown account: empty, affordable view.
	empty := p.AccountStatus(addr(42), big.NewInt(0))
	if !empty.BudgetOK || len(empty.Items) != 0 {
		t.Fatalf("unknown account view: budgetOK=%v items=%d", empty.BudgetOK, len(empty.Items))
	}
}

// TestBudgetAdmissionCountsReserved ensures in-flight reserved txs count
// against the account budget for new submissions behind them.
func TestBudgetAdmissionCountsReserved(t *testing.T) {
	p, l := setup(t, DefaultConfig(), 0)
	from := byte(8)
	l.SetBalance(addr(from), big.NewInt(2200))
	if err := p.Submit(newTx(from, 0, 100, 1, 10, 100)); err != nil {
		t.Fatal(err)
	}
	p.Reserve([]*types.Transaction{newTx(from, 0, 100, 1, 10, 100)})
	view := p.AccountStatus(addr(from), big.NewInt(0))
	if len(view.ReservedNonces) != 1 || view.ReservedNonces[0] != 0 {
		t.Fatalf("want reserved nonce [0], got %v", view.ReservedNonces)
	}
	// n1 costs 1100; 1100 reserved + 1100 incoming = 2200 fits exactly.
	if err := p.Submit(newTx(from, 1, 100, 1, 10, 100)); err != nil {
		t.Fatalf("n1 at exact budget limit should fit: %v", err)
	}
	// n2 would make it 3300 > 2200.
	err := p.Submit(newTx(from, 2, 100, 1, 10, 100))
	d, _ := reject.AsDecision(err)
	if d == nil || d.Reason != reject.ReasonAccountBudget {
		t.Fatalf("n2 behind reservation: want ACCOUNT_BUDGET, got %v", d)
	}
	t.Logf("reservation counted: %s | %s", d.Reason, d.Detail)
}

func countState(v *AccountView, s ItemState) int {
	n := 0
	for _, it := range v.Items {
		if it.State == s {
			n++
		}
	}
	return n
}
