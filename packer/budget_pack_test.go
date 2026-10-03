package packer

import (
	"math/big"
	"testing"

	"github.com/h3yow/go-txpool-fee-market-go/pool"
	"github.com/h3yow/go-txpool-fee-market-go/reject"
)

// TestProduceBalanceShrunkPacksExecutablePrefix verifies that when an
// account's balance is reduced after admission, packing takes only the still
// executable nonce prefix of that account, keeps the remainder pending with
// INSUFFICIENT_FUNDS decisions, and never blocks other accounts or the block.
func TestProduceBalanceShrunkPacksExecutablePrefix(t *testing.T) {
	pc, p, l := newSystem(t, 0) // baseFee 0: exact cost == value + tip*gas
	// sender1: three txs, exact cost 1100 each (value100 + tip10*gas100).
	l.SetBalance(pad(1), big.NewInt(3300))
	for n := uint64(0); n < 3; n++ {
		mustSubmit(t, p, ptx(1, n, 100, 10, 10, 100))
	}
	// sender2: one tx with a high tip, it must still pack while s1 is cut.
	l.SetBalance(pad(2), big.NewInt(1_000_000))
	mustSubmit(t, p, ptx(2, 0, 0, 100, 100, 100))

	// Externally (chain / another entry point) reduce s1's balance mid-flight:
	// it can now fund nonce 0 (1100) and nonce 1 (2200 cumulative) but not
	// nonce 2 (needs 3300).
	l.SetBalance(pad(1), big.NewInt(2500))

	block, skipped, err := pc.Produce(0)
	if err != nil {
		t.Fatalf("produce must not fail when one sender is short: %v", err)
	}
	if block == nil || len(block.Txs) != 3 {
		t.Fatalf("want 3 txs (s2n0 + s1n0 + s1n1), got block=%v", block)
	}
	var packedS1 []uint64
	for _, tx := range block.Txs {
		if tx.From[0] == 1 {
			packedS1 = append(packedS1, tx.Nonce)
		}
		if tx.From[0] == 2 && tx.Nonce != 0 {
			t.Fatal("sender2 tx missing")
		}
	}
	if len(packedS1) != 2 || packedS1[0] != 0 || packedS1[1] != 1 {
		t.Fatalf("want s1 nonces [0 1] packed, got %v", packedS1)
	}
	if len(skipped) != 1 || skipped[0].Reason != reject.ReasonInsufficientFunds {
		t.Fatalf("want 1 INSUFFICIENT_FUNDS skip for s1n2, got %+v", skipped)
	}
	t.Logf("packed s2n0 + s1n0,s1n1; deferred: %s | %s",
		skipped[0].Reason, skipped[0].Detail)

	// Ledger reflects exactly the prefix: s1 nonce 2, balance 2500-2200=300.
	a1 := l.GetAccount(pad(1))
	if a1.Nonce != 2 || a1.Balance.Cmp(big.NewInt(300)) != 0 {
		t.Fatalf("s1 ledger: want nonce2 bal300, got nonce%d bal%s", a1.Nonce, a1.Balance)
	}
	if a2 := l.GetAccount(pad(2)); a2.Nonce != 1 {
		t.Fatalf("s2 ledger nonce want 1, got %d", a2.Nonce)
	}

	// Pool view matches ledger: only s1n2 remains pending, no nonce gap (it
	// is the exact next nonce), no double deduction possible.
	size, _ := p.Stats()
	if size != 1 {
		t.Fatalf("pool must retain exactly s1n2, size=%d", size)
	}
	view := p.AccountStatus(pad(1), big.NewInt(0))
	if len(view.Items) != 1 || view.Items[0].Tx.Nonce != 2 {
		t.Fatalf("remaining view must be s1n2, got %+v", view.Items)
	}
	if view.Items[0].State == pool.ItemPackable {
		t.Fatalf("s1n2 must not look packable with balance %s", view.Balance)
	}
	t.Logf("pool/ledger aligned: s1 nonce=%d balance=%s, s1n2 %s (%s)",
		a1.Nonce, a1.Balance, view.Items[0].State, view.Items[0].Reason)

	// Repacking without a top-up packs nothing for s1 and does not error.
	b2, sk2, err := pc.Produce(0)
	if err != nil {
		t.Fatalf("second produce: %v", err)
	}
	if b2 != nil {
		t.Fatalf("no packable txs, must not create block, got id %d", b2.ID)
	}
	if len(sk2) != 1 || sk2[0].Reason != reject.ReasonInsufficientFunds {
		t.Fatalf("s1n2 must be reported INSUFFICIENT_FUNDS again, got %+v", sk2)
	}
	if a1 = l.GetAccount(pad(1)); a1.Nonce != 2 || a1.Balance.Cmp(big.NewInt(300)) != 0 {
		t.Fatalf("no-op produce must not touch ledger, got nonce%d bal%s", a1.Nonce, a1.Balance)
	}
	t.Log("second produce with still-short balance: no block, no state change")

	// Top up: the remaining tx packs in the next block, closing the run.
	l.SetBalance(pad(1), big.NewInt(1100))
	b3, sk3, err := pc.Produce(0)
	if err != nil {
		t.Fatalf("produce after top-up: %v", err)
	}
	if b3 == nil || len(b3.Txs) != 1 || b3.Txs[0].Nonce != 2 || len(sk3) != 0 {
		t.Fatalf("want single s1n2 block with no skips, got %v skips=%d", b3, len(sk3))
	}
	if a1 = l.GetAccount(pad(1)); a1.Nonce != 3 || a1.Balance.Cmp(big.NewInt(0)) != 0 {
		t.Fatalf("after final pack want nonce3 bal0, got nonce%d bal%s", a1.Nonce, a1.Balance)
	}
	if size, _ = p.Stats(); size != 0 {
		t.Fatalf("pool must be drained, size=%d", size)
	}
	t.Log("top-up then repack: s1n2 committed, nonce3 bal0, pool drained")
}

// TestProduceOtherSendersUnaffectedByZeroBalance is a stricter variant: a
// sender admitted with a queue but later fully funded by nothing (balance 0)
// contributes zero txs while well-funded senders pack normally.
func TestProduceOtherSendersUnaffectedByZeroBalance(t *testing.T) {
	pc, p, l := newSystem(t, 0)
	l.SetBalance(pad(1), big.NewInt(10_000))
	mustSubmit(t, p, ptx(1, 0, 100, 10, 10, 100))
	mustSubmit(t, p, ptx(1, 1, 100, 10, 10, 100))
	l.SetBalance(pad(2), big.NewInt(10_000))
	mustSubmit(t, p, ptx(2, 0, 0, 5, 5, 100))

	// Drain s1 completely before packing.
	l.SetBalance(pad(1), big.NewInt(0))
	block, skipped, err := pc.Produce(0)
	if err != nil || block == nil {
		t.Fatalf("block must still be produced: block=%v err=%v", block, err)
	}
	if len(block.Txs) != 1 || block.Txs[0].From[0] != 2 {
		t.Fatalf("only s2 must pack, got %d txs", len(block.Txs))
	}
	if len(skipped) != 2 {
		t.Fatalf("both s1 txs reported as deferred, got %d", len(skipped))
	}
	for _, d := range skipped {
		if d.Reason != reject.ReasonInsufficientFunds {
			t.Fatalf("want INSUFFICIENT_FUNDS, got %s", d.Reason)
		}
		t.Logf("deferred s%dn%d: %s", d.Tx.From[0], d.Tx.Nonce, d.Detail)
	}
	if size, _ := p.Stats(); size != 2 {
		t.Fatalf("both s1 txs stay pending, size=%d", size)
	}
}
