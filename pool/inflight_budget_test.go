package pool

import (
	"fmt"
	"math/big"
	"testing"

	"github.com/h3yow/go-txpool-fee-market-go/reject"
	"github.com/h3yow/go-txpool-fee-market-go/types"
)

// TestAccountBudgetIncludesInFlightReservation is the core regression for the
// reported bug: txs already handed to the in-flight (not yet settled) block
// still encumber the visible ledger balance. A burst of later submissions while
// that window is open must be admitted against
//
//	live-reserved-max-cost + pending-max-cost,
//
// and the tx that tips the account as a whole over its balance is rejected with
// ACCOUNT_BUDGET (distinct from single-tx INSUFFICIENT_FUNDS), never enters the
// pool and never evicts anything. Other accounts are unaffected. After a top-up
// — and once the in-flight block settles — the very same tx enters normally.
func TestAccountBudgetIncludesInFlightReservation(t *testing.T) {
	// baseFee 0, value 0, tip==cap=2, gas 100 => max cost == effective cost = 200.
	p, l := setup(t, DefaultConfig(), 0)
	from := byte(1)
	l.SetBalance(addr(from), big.NewInt(1000))

	// Submit five txs at 200 each: exactly the whole balance.
	first := make([]*types.Transaction, 0, 5)
	for n := uint64(0); n < 5; n++ {
		tx := newTx(from, n, 0, 2, 2, 100)
		if err := p.Submit(tx); err != nil {
			t.Fatalf("submit nonce %d: %v", n, err)
		}
		first = append(first, tx)
	}
	t.Log("input: s1 nonces 0..4, maxCost=200 each, balance=1000 -> exactly funded, all 5 in pool")

	// Simulate the async produce window: Reserve the first three (they leave the
	// pending view), but the ledger has NOT charged them yet (balance stays
	// 1000, nonce stays 0): the block being produced is still in flight.
	p.Reserve(first[:3])
	if size, _ := p.Stats(); size != 2 {
		t.Fatalf("after reserve of 3, pending size want 2, got %d", size)
	}
	t.Log("nonces 0..2 handed to the in-flight block; ledger still shows balance=1000 nonce=0 (not settled)")

	// While the window is open, resubmit a BURST of later nonces. nonce 5 alone
	// costs 200 < 1000, but the account bill is 3*200 (in-flight) + 2*200
	// (pending 3,4) + 200 = 1200 > 1000: it must be refused ACCOUNT_BUDGET.
	burst5 := newTx(from, 5, 0, 2, 2, 100)
	err := p.Submit(burst5)
	d, ok := reject.AsDecision(err)
	if !ok {
		t.Fatalf("want *reject.Decision for over-budget burst tx, got %v", err)
	}
	t.Logf("REJECT  nonce=5 cost=200 standalone affordable; bill = 3 in-flight*200 + 2 pending*200 + 200 = 1200 > balance 1000 -> %s | %s",
		d.Reason, d.Detail)
	if d.Reason != reject.ReasonAccountBudget {
		t.Fatalf("want ACCOUNT_BUDGET (account-wide incl. in-flight), got %s", d.Reason)
	}
	// Refused tx must not enter the pool, must not disturb the 2 pending txs.
	if size, _ := p.Stats(); size != 2 {
		t.Fatalf("refused tx must not enter pool; pending size want 2, got %d", size)
	}
	v := p.AccountView(addr(from))
	if len(v.Pending) != 2 || v.Pending[0].Tx.Nonce != 3 || v.Pending[1].Tx.Nonce != 4 {
		t.Fatalf("existing pending nonces 3,4 must be untouched, got view pending=%d", len(v.Pending))
	}
	if v.MaxBudget.Cmp(big.NewInt(1000)) != 0 || v.ReservedMaxBudget.Cmp(big.NewInt(600)) != 0 {
		t.Fatalf("bill want reserved=600 total=1000, got reserved=%s total=%s",
			v.ReservedMaxBudget, v.MaxBudget)
	}
	// Without the refused tx the bill is exactly 600 reserved + 400 pending =
	// 1000 == balance: the account is still at its limit but not over it.
	if !v.BudgetOK {
		t.Fatalf("without the refused tx the bill 600+400=1000 must be BudgetOK, got %s vs %s",
			v.MaxBudget, v.Balance)
	}
	t.Logf("view mid-flight: liveReserved=%v reservedMax=%s pendingMax=%s maxBudget=%s budgetOK=%v ready=%d",
		v.LiveReservedNonces, v.ReservedMaxBudget, v.PendingMaxBudget, v.MaxBudget, v.BudgetOK, v.ReadyCount)

	// Single-tx shortfall stays a distinguishable reason, even during the window.
	poor := byte(2)
	l.SetBalance(addr(poor), big.NewInt(100))
	alone := newTx(poor, 0, 0, 2, 2, 100) // cost 200 > 100 on its own
	err = p.Submit(alone)
	d, _ = reject.AsDecision(err)
	t.Logf("REJECT  s2 nonce=0 standalone cost=200 balance=100 -> %s | %s", d.Reason, d.Detail)
	if d.Reason != reject.ReasonInsufficientFunds {
		t.Fatalf("single-tx shortfall must stay INSUFFICIENT_FUNDS, got %s", d.Reason)
	}

	// An unrelated well-funded account is admitted despite s1's in-flight bill.
	other := byte(3)
	l.SetBalance(addr(other), big.NewInt(1000))
	if err := p.Submit(newTx(other, 0, 0, 2, 2, 100)); err != nil {
		t.Fatalf("unrelated account must be admitted: %v", err)
	}
	t.Log("unrelated s3 admitted: account budget (incl. in-flight) is per-account")

	// Top s1 up by 300 (balance 1300); the same burst tx now fits the bill.
	l.AddBalance(addr(from), big.NewInt(300))
	if err := p.Submit(burst5); err != nil {
		t.Fatalf("after top-up the same nonce=5 tx should enter, got %v", err)
	}
	t.Log("top-up +300 (balance 1300 >= bill 1200): identical nonce=5 tx now ACCEPT")
	if size, _ := p.Stats(); size != 4 { // s1 pending 3,4,5 + s3 n0
		t.Fatalf("pending size want 4 after burst tx accepted, got %d", size)
	}

	// Settle the in-flight block through the ledger, then Confirm the pool.
	if err := l.Apply(1, big.NewInt(0), first[:3]); err != nil {
		t.Fatalf("settle in-flight block: %v", err)
	}
	p.Confirm(first[:3])
	acc := l.GetAccount(addr(from))
	if acc.Nonce != 3 || acc.Balance.Cmp(big.NewInt(700)) != 0 { // 1300 - 3*200
		t.Fatalf("after settle s1 want nonce=3 balance=700, got nonce=%d balance=%s", acc.Nonce, acc.Balance)
	}
	v = p.AccountView(addr(from))
	t.Logf("after settle: onchainNonce=%d balance=%s liveReserved=%v pendingNonces=%v reservedMax=%s pendingMax=%s budgetOK=%v ready=%d",
		v.OnchainNonce, v.Balance, v.LiveReservedNonces,
		[]uint64{v.Pending[0].Tx.Nonce, v.Pending[1].Tx.Nonce, v.Pending[2].Tx.Nonce},
		v.ReservedMaxBudget, v.PendingMaxBudget, v.BudgetOK, v.ReadyCount)
	if len(v.LiveReservedNonces) != 0 || v.ReservedMaxBudget.Sign() != 0 {
		t.Fatalf("settled reservations must no longer encumber the bill, got live=%v reservedMax=%s",
			v.LiveReservedNonces, v.ReservedMaxBudget)
	}
	// Remaining pending 3,4,5 at 200 each = 600 <= balance 700: whole bill OK
	// and all three ready for the next block.
	if v.PendingMaxBudget.Cmp(big.NewInt(600)) != 0 || v.MaxBudget.Cmp(big.NewInt(600)) != 0 {
		t.Fatalf("post-settle pending bill want 600, got pending=%s total=%s",
			v.PendingMaxBudget, v.MaxBudget)
	}
	if !v.BudgetOK || v.ReadyCount != 3 {
		t.Fatalf("post-settle: budgetOK=true ready=3, got budgetOK=%v ready=%d", v.BudgetOK, v.ReadyCount)
	}
}

// TestAccountViewCountsInFlightEncumbrance checks the inspection entry point
// directly: with a live in-flight prefix, BUDGET and READY must reflect the
// reserved money; and the "ready now" count must match what the next block can
// actually take after settle — never over-count.
func TestAccountViewCountsInFlightEncumbrance(t *testing.T) {
	// baseFee 0: max cost cap*gas=200; effective (packing) cost tip*gas=100.
	p, l := setup(t, DefaultConfig(), 0)
	from := byte(1)
	l.SetBalance(addr(from), big.NewInt(1000))
	for n := uint64(0); n < 5; n++ {
		if err := p.Submit(newTx(from, n, 0, 1, 2, 100)); err != nil {
			t.Fatal(err)
		}
	}
	t.Log("input: s1 nonces 0..4 maxCost=200 effCost=100, balance=1000")

	// Reserve nonces 0,1 (handed to in-flight block); balance still uncharged.
	inflight := []*types.Transaction{newTx(from, 0, 0, 1, 2, 100), newTx(from, 1, 0, 1, 2, 100)}
	p.Reserve(inflight)

	v := p.AccountView(addr(from))
	t.Logf("mid-flight view: onchainNonce=%d balance=%s liveReserved=%v reservedMax=%s pendingMax=%s maxBudget=%s budgetOK=%v ready=%d",
		v.OnchainNonce, v.Balance, v.LiveReservedNonces, v.ReservedMaxBudget,
		v.PendingMaxBudget, v.MaxBudget, v.BudgetOK, v.ReadyCount)
	if v.ReservedMaxBudget.Cmp(big.NewInt(400)) != 0 {
		t.Fatalf("2 live reservations max cost want 400, got %s", v.ReservedMaxBudget)
	}
	if v.PendingMaxBudget.Cmp(big.NewInt(600)) != 0 || v.MaxBudget.Cmp(big.NewInt(1000)) != 0 {
		t.Fatalf("bills want pending=600 total=1000, got pending=%s total=%s",
			v.PendingMaxBudget, v.MaxBudget)
	}
	if !v.BudgetOK {
		t.Fatal("1000 total bill <= 1000 balance must be BudgetOK")
	}
	// Effective simulation: balance 1000 must first cover the 2 in-flight txs at
	// 100 effective each (800 left), then pending 2,3 (100 each, 600 left);
	// pending 4 would leave 500 -> actually all three fit! Pick a smaller
	// balance to show truncation: drain balance to 450 externally.
	l.SetBalance(addr(from), big.NewInt(450))
	v = p.AccountView(addr(from))
	t.Logf("drained mid-flight: balance=%s reservedEff=200 free=250 -> pending 2,3 ready(100 each), 4 deferred; budgetOK=%v ready=%d",
		v.Balance, v.BudgetOK, v.ReadyCount)
	if v.BudgetOK {
		t.Fatalf("total max bill 1000 > balance 450 must be BudgetOK=false")
	}
	if v.ReadyCount != 2 {
		t.Fatalf("after 200 in-flight effective encumbrance, only nonces 2,3 fit 250 free; ready=%d want 2", v.ReadyCount)
	}
	for i, pt := range v.Pending {
		want := TxPlanReady
		if pt.Tx.Nonce == 4 {
			want = TxPlanDeferredBudget
		}
		t.Logf("  pending nonce=%d cumMax(incl reserved)=%s status=%s | %s",
			pt.Tx.Nonce, pt.CumulativeMaxCost, pt.Status, pt.Reason)
		if pt.Status != want {
			t.Fatalf("nonce %d want %s got %s", pt.Tx.Nonce, want, pt.Status)
		}
		if i == 0 && pt.CumulativeMaxCost.Cmp(big.NewInt(600)) != 0 { // 400 reserved + 200
			t.Fatalf("cumulative max at nonce2 must include 400 reserved, got %s", pt.CumulativeMaxCost)
		}
	}
}

// TestPendingSkipsAccountWithInFlightHead ensures the packer candidate list
// never jumps over a live reservation at the on-chain nonce: that account is
// absent from Pending while other accounts still appear. Once the block settles
// (Apply + Confirm) the suffix is offered again. The Apply/Confirm split window
// (ledger charged but Confirm not yet run) must also read consistently.
func TestPendingSkipsAccountWithInFlightHead(t *testing.T) {
	p, l := setup(t, DefaultConfig(), 0)
	l.SetBalance(addr(1), big.NewInt(10_000))
	l.SetBalance(addr(2), big.NewInt(10_000))

	s1 := make([]*types.Transaction, 0, 4)
	for n := uint64(0); n < 4; n++ {
		tx := newTx(1, n, 0, 5, 10, 100)
		if err := p.Submit(tx); err != nil {
			t.Fatal(err)
		}
		s1 = append(s1, tx)
	}
	s2n0 := newTx(2, 0, 0, 1, 10, 100)
	if err := p.Submit(s2n0); err != nil {
		t.Fatal(err)
	}

	// Hand s1 nonces 0,1 to the in-flight block (ledger uncharged).
	p.Reserve(s1[:2])
	cand := p.Pending(big.NewInt(0))
	t.Logf("candidates while s1 head nonce 0 is in-flight: %v", idsOf(cand))
	for _, tx := range cand {
		if tx.From[0] == 1 {
			t.Fatalf("s1 must be skipped while its head nonce is reserved in-flight, found nonce %d", tx.Nonce)
		}
	}
	if len(cand) != 1 || cand[0].From[0] != 2 {
		t.Fatalf("only s2n0 should be candidate, got %v", idsOf(cand))
	}

	// New burst submissions against s1 still work (nonce ordering unaffected):
	// nonce 4 is contiguous with pending 2,3.
	if err := p.Submit(newTx(1, 4, 0, 5, 10, 100)); err != nil {
		t.Fatalf("submit s1 nonce4 during in-flight window: %v", err)
	}
	t.Log("s1 nonce=4 admitted during the in-flight window (expected nonce after reserved 0,1 + pending 2,3)")

	// Settle: ledger charges 0,1; BEFORE Confirm runs, the pool still holds stale
	// reservations below the new on-chain nonce — Pending must read through that.
	if err := l.Apply(1, big.NewInt(0), s1[:2]); err != nil {
		t.Fatal(err)
	}
	cand = p.Pending(big.NewInt(0))
	if len(cand) != 4 {
		t.Fatalf("in the Apply->Confirm window s1 suffix 2,3,4 + s2 must show (stale reservations ignored), got %d: %v",
			len(cand), idsOf(cand))
	}
	t.Logf("Apply done, Confirm pending: stale reservations ignored; candidates=%v", idsOf(cand))
	p.Confirm(s1[:2])
	cand = p.Pending(big.NewInt(0))
	if len(cand) != 4 {
		t.Fatalf("after confirm want 4 candidates (s1 2,3,4 + s2), got %d: %v", len(cand), idsOf(cand))
	}
	if cand[0].From[0] != 1 || cand[0].Nonce != 2 {
		t.Fatalf("s1 suffix must restart at nonce 2, got %v", idsOf(cand))
	}
	t.Logf("after confirm: candidates=%v — contiguous suffix, no nonce gap", idsOf(cand))
}

func idsOf(txs []*types.Transaction) []string {
	out := make([]string, 0, len(txs))
	for _, tx := range txs {
		out = append(out, fmt.Sprintf("s%dn%d", tx.From[0], tx.Nonce))
	}
	return out
}
