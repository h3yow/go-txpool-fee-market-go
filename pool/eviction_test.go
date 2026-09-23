package pool

import (
	"math/big"
	"testing"

	"github.com/h3yow/go-txpool-fee-market-go/reject"
)

func TestEvictionOnFull(t *testing.T) {
	// Tiny pool: 4 global slots, 4 per account, baseFee 0 => tip=min(tip,cap).
	p, l := setup(t, Config{GlobalSlots: 4, AccountSlots: 4, PriceBumpPct: 10}, 0)
	for i := byte(1); i <= 4; i++ {
		l.SetBalance(addr(i), big.NewInt(1_000_000_000))
	}

	// Fill: tips 10, 20, 30, 40 from distinct senders.
	tips := []int64{10, 20, 30, 40}
	for i, tip := range tips {
		tx := newTx(byte(i+1), 0, 0, tip, tip+100, 1000)
		if err := p.Submit(tx); err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
	}
	t.Logf("pool full with tips %v; cheapest tail = sender1 tip 10", tips)

	// Incoming tip 5 cannot outbid cheapest tail (10) -> UNDERPRICED.
	weak := newTx(5, 0, 0, 5, 105, 1000)
	l.SetBalance(addr(5), big.NewInt(1_000_000_000))
	expectReason(t, p, weak, reject.ReasonUnderpriced)

	// Incoming tip 11 strictly beats tip 10 -> evicts sender1's tail.
	strong := newTx(5, 0, 0, 11, 111, 1000)
	expectReason(t, p, strong, reject.ReasonUnknown)

	size, accs := p.Stats()
	t.Logf("after eviction size=%d accounts=%d", size, accs)
	if size != 4 {
		t.Fatalf("size should stay at cap 4, got %d", size)
	}
	if p.Has(weak.Hash()) {
		t.Fatal("weak tx must not be in pool")
	}
	evicted := newTx(1, 0, 0, 10, 110, 1000)
	if p.Has(evicted.Hash()) {
		t.Fatal("sender1 tip-10 tx should have been evicted")
	}
	t.Log("judgement basis: evict globally cheapest per-account tail; require strict tip outbid")
}

func TestEvictionSuffixKeepsContiguous(t *testing.T) {
	// 3 slots global, single account with 3 txs; a higher tip from a second
	// sender must evict the tail (nonce2), and the suffix logic must not leave
	// a dangling higher nonce (none here), but reject gapped new inserts later.
	p, l := setup(t, Config{GlobalSlots: 3, AccountSlots: 3, PriceBumpPct: 10}, 0)
	l.SetBalance(addr(1), big.NewInt(1_000_000_000))
	l.SetBalance(addr(2), big.NewInt(1_000_000_000))

	for n := uint64(0); n < 3; n++ {
		if err := p.Submit(newTx(1, n, 0, int64(10+n), 100, 1000)); err != nil {
			t.Fatal(err)
		}
	}
	// sender1 tails: nonce2 tip12 is the only (and cheapest) tail. A new
	// sender2 tx with tip 50 evicts it -> sender1 keeps nonce0,nonce1.
	if err := p.Submit(newTx(2, 0, 0, 50, 200, 1000)); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	q := p.accounts[addr(1)]
	_, has0 := q.txs[0]
	_, has1 := q.txs[1]
	_, has2 := q.txs[2]
	p.mu.Unlock()
	if !has0 || !has1 || has2 {
		t.Fatalf("sender1 should retain nonce 0,1 only; got 0=%v 1=%v 2=%v", has0, has1, has2)
	}
	t.Log("evicted tail nonce=2; sender1 retains contiguous prefix [0,1]")

	// Re-adding nonce2 must NOT cause eviction of sender1's own prefix tail
	// (nonce1) - that would gap the incoming tx. tip 30 cannot outbid
	// sender2's tail (50) either, so it is rejected UNDERPRICED.
	if err := p.Submit(newTx(1, 2, 0, 30, 100, 1000)); err == nil {
		t.Fatal("expected nonce2 tip30 to be rejected without creating a gap")
	} else {
		d, _ := reject.AsDecision(err)
		t.Logf("nonce2 tip30 correctly rejected: %s: %s", d.Reason, d.Detail)
	}

	// tip 60 outbids sender2's tail (50): sender2 evicted, sender1 becomes
	// contiguous [0,1,2] again.
	if err := p.Submit(newTx(1, 2, 0, 60, 200, 1000)); err != nil {
		t.Fatalf("nonce2 tip60 readd failed: %v", err)
	}
	p.mu.Lock()
	q0 := p.accounts[addr(1)]
	_, n0 := q0.txs[0]
	_, n1 := q0.txs[1]
	_, n2 := q0.txs[2]
	_, s2 := p.accounts[addr(2)]
	p.mu.Unlock()
	if !n0 || !n1 || !n2 {
		t.Fatal("sender1 should now hold nonces 0,1,2")
	}
	if s2 {
		t.Fatal("sender2 should have been evicted")
	}
	t.Log("nonce2 re-admitted at tip60; sender1 run [0,1,2] contiguous; sender2 evicted")
}

func TestPerAccountCap(t *testing.T) {
	p, l := setup(t, Config{GlobalSlots: 100, AccountSlots: 2, PriceBumpPct: 10}, 0)
	l.SetBalance(addr(1), big.NewInt(1_000_000_000))
	_ = p.Submit(newTx(1, 0, 0, 1, 10, 100))
	_ = p.Submit(newTx(1, 1, 0, 1, 10, 100))
	err := p.Submit(newTx(1, 2, 0, 1, 10, 100))
	d, _ := reject.AsDecision(err)
	t.Logf("3rd tx from same sender -> %s: %s", d.Reason, d.Detail)
	if d.Reason != reject.ReasonPoolFull {
		t.Fatalf("want POOL_FULL (account cap), got %s", d.Reason)
	}
}

func TestPendingOrdering(t *testing.T) {
	p, l := setup(t, Config{GlobalSlots: 100, AccountSlots: 10, PriceBumpPct: 10}, 5)
	l.SetBalance(addr(1), big.NewInt(1_000_000_000))
	l.SetBalance(addr(2), big.NewInt(1_000_000_000))

	// sender1: nonce0 tip 100, nonce1 tip 1 (must still follow nonce0).
	_ = p.Submit(newTx(1, 0, 0, 100, 200, 100))
	_ = p.Submit(newTx(1, 1, 0, 1, 200, 100))
	// sender2: nonce0 tip 50.
	_ = p.Submit(newTx(2, 0, 0, 50, 200, 100))

	got := p.Pending(big.NewInt(5))
	t.Logf("pending order: [s1n0 tip100, s2n0 tip50, s1n1 tip1] expected")
	if len(got) != 3 {
		t.Fatalf("want 3 pending, got %d", len(got))
	}
	want := []struct {
		from byte
		n    uint64
	}{{1, 0}, {2, 0}, {1, 1}}
	for i, w := range want {
		if got[i].From[0] != w.from || got[i].Nonce != w.n {
			t.Fatalf("position %d want sender%d nonce%d, got sender%d nonce%d",
				i, w.from, w.n, got[i].From[0], got[i].Nonce)
		}
	}
	t.Log("per-sender nonce order preserved even though s1n1 is the cheapest tx")
}
