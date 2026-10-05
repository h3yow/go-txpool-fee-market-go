package packer

import (
	"fmt"
	"math/big"
	"testing"

	"github.com/h3yow/go-txpool-fee-market-go/pool"
	"github.com/h3yow/go-txpool-fee-market-go/reject"
	"github.com/h3yow/go-txpool-fee-market-go/types"
)

func sumIDs(txs []*types.Transaction) string {
	s := "["
	for i, tx := range txs {
		if i > 0 {
			s += ","
		}
		s += fmt.Sprintf("s%dn%d", tx.From[0], tx.Nonce)
	}
	return s + "]"
}

func sumD(ds []*reject.Decision) string {
	s := "["
	for i, d := range ds {
		if i > 0 || d.Tx == nil {
			if i > 0 {
				s += ","
			}
			if d.Tx == nil {
				s += d.Reason.String()
				continue
			}
		}
		if d.Tx != nil {
			s += fmt.Sprintf("s%dn%d:%s", d.Tx.From[0], d.Tx.Nonce, d.Reason)
		}
	}
	return s + "]"
}

// TestProduceSurvivesBalanceShrink covers requirement 2: after admission, the
// sender's balance is reduced (chain/other entry) so only a nonce prefix stays
// executable. Produce must pack exactly that prefix, leave the suffix pending
// with ACCOUNT_BUDGET decisions, and never block the block or other accounts.
func TestProduceSurvivesBalanceShrink(t *testing.T) {
	pc, p, l := newSystem(t, 0) // baseFee 0

	// Seed rich balances so admission succeeds; balances are drained later to
	// simulate an external reduction between Submit and Produce.
	l.SetBalance(pad(1), big.NewInt(10_000))
	l.SetBalance(pad(2), big.NewInt(100_000))

	// baseFee 0, value 0, tip==cap => admission max cost and packing effective
	// cost are the same (tip*gas), so the executable prefix is deterministic.
	// sender1: five txs, effective cost 100 each.
	for n := uint64(0); n < 5; n++ {
		mustSubmit(t, p, ptx(1, n, 0, 1, 1, 100))
	}
	// sender2: two txs at a higher tip so they interleave; effective cost 1000.
	mustSubmit(t, p, ptx(2, 0, 0, 10, 10, 100))
	mustSubmit(t, p, ptx(2, 1, 0, 10, 10, 100))
	t.Log("input: s1 nonces 0..4 effCost 100 each; s2 nonces 0..1 effCost 1000 each; balances then drained externally")

	// Admission passed with rich balances; now the money is reduced.
	l.SetBalance(pad(1), big.NewInt(250)) // s1: nonces 0,1 (100 each) fit, 2..4 do not
	l.SetBalance(pad(2), big.NewInt(900)) // s2: nothing fits (1000 > 900)

	block, skipped, err := pc.Produce(0)
	if err != nil {
		t.Fatalf("broke accounts must not fail Produce: %v", err)
	}
	if block == nil {
		t.Fatal("a block with the executable prefix must still be produced, got nil")
	}
	for _, tx := range block.Txs {
		eff := new(big.Int).Add(tx.Value, tx.EffectiveFee(big.NewInt(0)))
		t.Logf("PACKED  s%dn%d effCost=%s", tx.From[0], tx.Nonce, eff)
	}
	if got := sumIDs(block.Txs); got != "[s1n0,s1n1]" {
		t.Fatalf("block must contain s1 nonces 0,1 only, got %s", got)
	}
	for _, d := range skipped {
		t.Logf("DEFER   s%dn%d reason=%s | %s", d.Tx.From[0], d.Tx.Nonce, d.Reason, d.Detail)
	}
	if len(skipped) != 5 { // s1 n2..4 + s2 n0..1
		t.Fatalf("want 5 deferrals (s1 n2..4 and s2 n0..1), got %d: %s", len(skipped), sumD(skipped))
	}
	for _, d := range skipped {
		if d.Reason != reject.ReasonAccountBudget {
			t.Fatalf("want ACCOUNT_BUDGET deferral, got s%dn%d %s", d.Tx.From[0], d.Tx.Nonce, d.Reason)
		}
	}

	// Ledger view: only the prefix committed, no nonce hole, exact spend.
	s1 := l.GetAccount(pad(1))
	s2 := l.GetAccount(pad(2))
	if s1.Nonce != 2 {
		t.Fatalf("s1 nonce must advance to 2, got %d", s1.Nonce)
	}
	if s1.Balance.Cmp(big.NewInt(250-200)) != 0 {
		t.Fatalf("s1 balance want 50 (250-2*100), got %s", s1.Balance)
	}
	if s2.Nonce != 0 || s2.Balance.Cmp(big.NewInt(900)) != 0 {
		t.Fatalf("s2 untouched: nonce 0 bal 900, got %d/%s", s2.Nonce, s2.Balance)
	}
	t.Logf("ledger consistent: s1 nonce=%d balance=%s (100+100 charged once), s2 nonce=%d balance=900",
		s1.Nonce, s1.Balance, s2.Nonce)

	// Pool view: suffix retained, no double packing, no gap (s1 next=2).
	if size, _ := p.Stats(); size != 5 { // s1 n2..4 + s2 n0..1
		t.Fatalf("pool must retain 5 unexecuted txs, got %d", size)
	}
	v := p.AccountView(pad(1))
	t.Logf("s1 view after pack: onchainNonce=%d pending=%d ready=%d budgetOK=%v",
		v.OnchainNonce, len(v.Pending), v.ReadyCount, v.BudgetOK)
	if v.OnchainNonce != 2 || len(v.Pending) != 3 || v.Pending[0].Tx.Nonce != 2 {
		t.Fatalf("s1 pool suffix must be nonces 2,3,4 contiguous, got %s", sumIDsFromView(v.Pending))
	}

	// Top BOTH accounts up; the exact deferred txs now pack in a second block.
	l.AddBalance(pad(1), big.NewInt(1000)) // 50 + 1000 = 1050 >= 3*100=300
	l.AddBalance(pad(2), big.NewInt(2000)) // 900+2000=2900 >= 2*1000=2000
	block2, skipped2, err := pc.Produce(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range skipped2 {
		t.Logf("DEFER2  s%dn%d %s | %s", d.Tx.From[0], d.Tx.Nonce, d.Reason, d.Detail)
	}
	if len(skipped2) != 0 {
		t.Fatalf("after top-up no deferrals expected, got %d: %s", len(skipped2), sumD(skipped2))
	}
	if len(block2.Txs) != 5 {
		t.Fatalf("second block must pack the remaining 5 txs, got %d: %s", len(block2.Txs), sumIDs(block2.Txs))
	}
	if s := l.GetAccount(pad(1)); s.Nonce != 5 {
		t.Fatalf("s1 final nonce want 5, got %d", s.Nonce)
	}
	if s := l.GetAccount(pad(2)); s.Nonce != 2 {
		t.Fatalf("s2 final nonce want 2, got %d", s.Nonce)
	}
	if size, _ := p.Stats(); size != 0 {
		t.Fatalf("pool must be drained, got %d", size)
	}
	t.Logf("after top-up: s1 nonce=%d s2 nonce=%d, all 5 remaining txs packed once, pool drained (no double deduction)",
		l.GetAccount(pad(1)).Nonce, l.GetAccount(pad(2)).Nonce)
}

func sumIDsFromView(pts []*pool.PlannedTx) string {
	s := "["
	for i, pt := range pts {
		if i > 0 {
			s += ","
		}
		s += fmt.Sprintf("n%d", pt.Tx.Nonce)
	}
	return s + "]"
}

// TestProduceDrainedAccountDoesNotBlockOthers checks the degenerate case where
// one account cannot even fund its head tx: the block still commits other
// accounts' work and returns without error.
func TestProduceDrainedAccountDoesNotBlockOthers(t *testing.T) {
	pc, p, l := newSystem(t, 0)
	l.SetBalance(pad(1), big.NewInt(10_000))
	l.SetBalance(pad(2), big.NewInt(10_000))
	mustSubmit(t, p, ptx(1, 0, 100, 1, 2, 100))
	mustSubmit(t, p, ptx(1, 1, 100, 1, 2, 100))
	mustSubmit(t, p, ptx(2, 0, 100, 5, 10, 100))

	l.SetBalance(pad(1), big.NewInt(0))
	l.SetBalance(pad(2), big.NewInt(1000))

	block, skipped, err := pc.Produce(0)
	if err != nil {
		t.Fatalf("drained sender must not error produce: %v", err)
	}
	if block == nil || len(block.Txs) != 1 || block.Txs[0].From[0] != 2 {
		got := "<nil>"
		if block != nil {
			got = sumIDs(block.Txs)
		}
		t.Fatalf("only s2n0 should be packed, got %s", got)
	}
	if len(skipped) != 2 {
		t.Fatalf("s1 n0,n1 deferred, got %d: %s", len(skipped), sumD(skipped))
	}
	for _, d := range skipped {
		t.Logf("deferred s%dn%d: %s | %s", d.Tx.From[0], d.Tx.Nonce, d.Reason, d.Detail)
		if d.Reason != reject.ReasonAccountBudget {
			t.Fatalf("want ACCOUNT_BUDGET, got %s", d.Reason)
		}
	}
	if s := l.GetAccount(pad(1)); s.Nonce != 0 || s.Balance.Sign() != 0 {
		t.Fatalf("s1 must be untouched nonce=0 bal=0, got %d/%s", s.Nonce, s.Balance)
	}
	t.Log("drained s1 deferred wholesale; s2 block committed; no whole-block failure")
}

// TestSubmitWhileBlockInFlightCountsReservation drives the exact reported
// window through Produce itself: after the block's txs are Reserved but before
// the ledger atomically applies them, the same account keeps submitting. The
// in-flight money is un-deducted in the ledger but spoken for, so the whole
// account is judged committed-run + queued-run; the over-committing tx comes
// back as ACCOUNT_BUDGET (distinct from single-tx INSUFFICIENT_FUNDS), never
// enters the pool, and the same tx packs normally after a top-up. No money is
// double-charged and no nonce hole appears.
func TestSubmitWhileBlockInFlightCountsReservation(t *testing.T) {
	pc, p, l := newSystem(t, 0) // baseFee 0, tip==cap=2, gas100 -> cost 200 each
	l.SetBalance(pad(1), big.NewInt(1000))
	mk := func(n uint64) *types.Transaction { return ptx(1, n, 0, 2, 2, 100) }

	// First block: nonces 0,1 (400). While it is in flight the account fires
	// nonces 2..5. With 400 already committed: 2,3,4 fit (400+600=1000) and
	// nonce5 would need 1200, so it is rejected by the account-level gate.
	started := make(chan struct{})
	resume := make(chan struct{})
	var windowErr error
	pc.SetAfterReserveHook(func() {
		close(started)
		<-resume
	})
	defer pc.SetAfterReserveHook(nil)

	for _, n := range []uint64{0, 1} {
		mustSubmit(t, p, mk(n))
	}

	produced := make(chan struct{})
	go func() {
		defer close(produced)
		_, _, windowErr = pc.Produce(0)
	}()
	<-started

	for _, n := range []uint64{2, 3, 4} {
		if err := p.Submit(mk(n)); err != nil {
			t.Errorf("nonce %d must be admitted while the batch is in flight: %v", n, err)
		}
	}
	t.Log("during in-flight block (nonces 0,1 = 400, ledger still shows 1000): nonces 2,3,4 ACCEPT (400+600=1000)")

	over := mk(5)
	err := p.Submit(over)
	d, ok := reject.AsDecision(err)
	if !ok {
		t.Fatalf("nonce5 must be a *Decision while in flight, got %v", err)
	}
	t.Logf("REJECT  nonce=5 standalone 200 < balance 1000 but in-flight 400 + projected 800 = 1200 -> %s | %s",
		d.Reason, d.Detail)
	if d.Reason != reject.ReasonAccountBudget {
		t.Fatalf("want ACCOUNT_BUDGET counting the in-flight batch, got %s", d.Reason)
	}

	// Single-tx shortfall from another account keeps its own reason.
	l.SetBalance(pad(9), big.NewInt(100))
	if d9, _ := reject.AsDecision(p.Submit(ptx(9, 0, 0, 2, 2, 100))); d9 == nil || d9.Reason != reject.ReasonInsufficientFunds {
		t.Fatalf("single-tx shortfall must stay INSUFFICIENT_FUNDS, got %v", d9)
	}

	// The rejected tx never entered and nothing queued was displaced.
	if size, _ := p.Stats(); size != 3 {
		t.Fatalf("only nonces 2,3,4 pending during the window, got %d", size)
	}
	if p.Has(over.Hash()) {
		t.Fatal("rejected nonce5 must not be tracked")
	}

	// Let the in-flight block commit: ledger deducts once, nonce advances,
	// reservations clear.
	close(resume)
	<-produced
	pc.SetAfterReserveHook(nil)
	if windowErr != nil {
		t.Fatalf("in-flight produce: %v", windowErr)
	}
	s1 := l.GetAccount(pad(1))
	if s1.Nonce != 2 || s1.Balance.Cmp(big.NewInt(600)) != 0 {
		t.Fatalf("after block1: want nonce2 bal600 (400 charged once), got nonce%d bal%s", s1.Nonce, s1.Balance)
	}
	t.Logf("block1 committed once: nonce=%d balance=%s; queue is nonces 2,3,4 contiguous", s1.Nonce, s1.Balance)

	// Second block packs the admitted queue against the deducted balance:
	// 600 covers nonces 2,3,4 exactly; no skips, no nonce hole.
	b2, skipped, err := pc.Produce(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 0 || b2 == nil || len(b2.Txs) != 3 {
		t.Fatalf("nonces 2,3,4 should all pack (600 <= 600), got txs=%d skipped=%d: %s",
			lenIf(b2), len(skipped), sumD(skipped))
	}
	s1 = l.GetAccount(pad(1))
	if s1.Nonce != 5 || s1.Balance.Sign() != 0 {
		t.Fatalf("after block2 want nonce5 bal0, got nonce%d bal%s", s1.Nonce, s1.Balance)
	}
	t.Logf("block2 packed %s: nonce=%d balance=%s (no double deduction, no nonce gap)",
		sumIDs(b2.Txs), s1.Nonce, s1.Balance)

	// Top up and resubmit the exact tx rejected during the window: it enters and
	// packs with no resubmission churn for the already-confirmed prefix.
	l.AddBalance(pad(1), big.NewInt(500))
	if err := p.Submit(over); err != nil {
		t.Fatalf("after top-up, previously rejected nonce5 must enter: %v", err)
	}
	b3, skipped3, err := pc.Produce(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped3) != 0 || b3 == nil || len(b3.Txs) != 1 || b3.Txs[0].Nonce != 5 {
		t.Fatalf("nonce5 must pack alone after top-up, got txs=%d skipped=%s", lenIf(b3), sumD(skipped3))
	}
	if got := l.GetAccount(pad(1)); got.Nonce != 6 || got.Balance.Cmp(big.NewInt(300)) != 0 {
		t.Fatalf("final want nonce6 bal300, got nonce%d bal%s", got.Nonce, got.Balance)
	}
	t.Log("top-up +500: the window-rejected nonce5 entered and packed; nonce chain 0..5 complete, pool drained")
	if size, _ := p.Stats(); size != 0 {
		t.Fatalf("pool must be drained, got %d", size)
	}
}

func lenIf(b *Block) int {
	if b == nil {
		return 0
	}
	return len(b.Txs)
}
