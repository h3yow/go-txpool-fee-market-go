package packer

import (
	"math/big"
	"testing"

	"github.com/h3yow/go-txpool-fee-market-go/ledger"
	"github.com/h3yow/go-txpool-fee-market-go/pool"
	"github.com/h3yow/go-txpool-fee-market-go/reject"
	"github.com/h3yow/go-txpool-fee-market-go/types"
)

func pad(b byte) types.Address { var a types.Address; a[0] = b; return a }

func ptx(from byte, nonce uint64, value, tip, cap int64, gas uint64) *types.Transaction {
	return &types.Transaction{
		From: pad(from), To: pad(99), Nonce: nonce,
		Value: big.NewInt(value), Gas: gas,
		GasTipCap: big.NewInt(tip), GasFeeCap: big.NewInt(cap),
	}
}

func newSystem(t *testing.T, baseFee int64) (*Packer, *pool.Pool, *ledger.Ledger) {
	t.Helper()
	l := ledger.New()
	p := pool.New(pool.DefaultConfig(), l, big.NewInt(baseFee))
	pc := New(p, l, big.NewInt(baseFee))
	return pc, p, l
}

func mustSubmit(t *testing.T, p *pool.Pool, tx *types.Transaction) {
	t.Helper()
	if err := p.Submit(tx); err != nil {
		t.Fatalf("submit s%d n%d: %v", tx.From[0], tx.Nonce, err)
	}
}

func TestProduceFeeOrderAndCommit(t *testing.T) {
	pc, p, l := newSystem(t, 10)
	l.SetBalance(pad(1), big.NewInt(1_000_000))
	l.SetBalance(pad(2), big.NewInt(1_000_000))

	mustSubmit(t, p, ptx(2, 0, 0, 5, 20, 21000)) // eff tip 5
	mustSubmit(t, p, ptx(1, 0, 0, 9, 20, 21000)) // eff tip 9 -> first
	mustSubmit(t, p, ptx(1, 1, 0, 1, 20, 21000)) // must follow s1n0

	block, skipped, err := pc.Produce(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 0 {
		t.Fatalf("unexpected skips: %v", skipped)
	}
	if len(block.Txs) != 3 {
		t.Fatalf("want 3 packed, got %d", len(block.Txs))
	}
	order := []struct {
		f byte
		n uint64
	}{{1, 0}, {2, 0}, {1, 1}}
	for i, w := range order {
		if block.Txs[i].From[0] != w.f || block.Txs[i].Nonce != w.n {
			t.Fatalf("pack position %d want s%dn%d, got s%dn%d",
				i, w.f, w.n, block.Txs[i].From[0], block.Txs[i].Nonce)
		}
	}
	t.Log("pack order s1n0(9) -> s2n0(5) -> s1n1(1): fee priority + per-sender nonce chain")

	// Ledger reflects the commit: nonces advanced, fees deducted.
	// eff tip: baseFee10 -> min(tip,cap-10): s1n0 min(9,10)=9; s2n0 5; s1n1 1.
	// balances: s1 1e6 - (9+1)*21000 = 790000 ; s2 1e6 - 5*21000 = 895000
	if got := l.GetAccount(pad(1)); got.Nonce != 2 || got.Balance.Cmp(big.NewInt(790_000)) != 0 {
		t.Fatalf("sender1 ledger wrong: nonce=%d bal=%s", got.Nonce, got.Balance)
	}
	if got := l.GetAccount(pad(2)); got.Nonce != 1 || got.Balance.Cmp(big.NewInt(895_000)) != 0 {
		t.Fatalf("sender2 ledger wrong: nonce=%d bal=%s", got.Nonce, got.Balance)
	}
	t.Logf("ledger committed: s1 nonce2 bal790000, s2 nonce1 bal895000")

	// Pool is drained and no stale reservations remain.
	size, _ := p.Stats()
	if size != 0 {
		t.Fatalf("pool should be empty, size=%d", size)
	}
	if p.Has(block.Txs[0].Hash()) {
		t.Fatal("packed tx must not remain Has() true")
	}

	// Second block immediately after: empty pending -> nil block, no block id bump.
	b2, _, err := pc.Produce(0)
	if err != nil {
		t.Fatal(err)
	}
	if b2 != nil {
		t.Fatalf("empty pool must not produce a block, got id %d", b2.ID)
	}
	if pc.Head() != 1 {
		t.Fatalf("head must remain 1, got %d", pc.Head())
	}
	t.Log("empty produce commits nothing; head stays 1")
}

func TestReorgReaddsTransactions(t *testing.T) {
	pc, p, l := newSystem(t, 0)
	l.SetBalance(pad(1), big.NewInt(1_000_000))
	mustSubmit(t, p, ptx(1, 0, 1000, 2, 10, 21000))
	mustSubmit(t, p, ptx(1, 1, 1000, 2, 10, 21000))

	b1, _, err := pc.Produce(0)
	if err != nil || len(b1.Txs) != 2 {
		t.Fatalf("block1: err=%v txs=%d", err, len(b1.Txs))
	}
	t.Logf("block %d packed %d txs; s1 nonce now 2, balance %s",
		b1.ID, len(b1.Txs), l.GetAccount(pad(1)).Balance)

	// Reorg to head 0: state restored, txs flow back into the pool.
	readded, dropped, err := pc.Reorg(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 0 {
		t.Fatalf("unexpected drops on reorg: %+v", dropped)
	}
	if len(readded) != 2 {
		t.Fatalf("want 2 readded, got %d", len(readded))
	}
	a := l.GetAccount(pad(1))
	if a.Nonce != 0 || a.Balance.Cmp(big.NewInt(1_000_000)) != 0 {
		t.Fatalf("state not restored: nonce=%d bal=%s", a.Nonce, a.Balance)
	}
	size, _ := p.Stats()
	if size != 2 {
		t.Fatalf("pool should hold 2 again, size=%d", size)
	}
	t.Logf("reorg complete: ledger nonce=%d balance=%s restored; pool size=%d",
		a.Nonce, a.Balance, size)

	// Repacking yields the same two txs exactly once (no double pack / spend).
	b2, _, err := pc.Produce(0)
	if err != nil || len(b2.Txs) != 2 {
		t.Fatalf("repack: err=%v txs=%d", err, len(b2.Txs))
	}
	a = l.GetAccount(pad(1))
	if a.Nonce != 2 {
		t.Fatalf("after repack nonce want 2, got %d", a.Nonce)
	}
	// cost per tx = 1000 value + 2*21000 = 43000; twice => 914000.
	if a.Balance.Cmp(big.NewInt(914_000)) != 0 {
		t.Fatalf("after repack balance want 914000, got %s", a.Balance)
	}
	t.Logf("repack identical txs once: nonce=%d balance=%s (no double deduction)", a.Nonce, a.Balance)
}

func TestReorgDropsInvalidWithReason(t *testing.T) {
	pc, p, l := newSystem(t, 0)
	l.SetBalance(pad(1), big.NewInt(1_000_000))
	mustSubmit(t, p, ptx(1, 0, 1000, 2, 10, 21000))
	b1, _, err := pc.Produce(0)
	if err != nil {
		t.Fatal(err)
	}

	// Fork onto a canonical chain on which the sender was drained by another
	// tx. Phase 1: roll back our block (restores local snapshot)...
	reverted, err := pc.Rollback(0)
	if err != nil || len(reverted) != 1 {
		t.Fatalf("rollback: err=%v txs=%d", err, len(reverted))
	}
	// ...phase 2: overlay the canonical side's account state (nonce advanced
	// elsewhere / balance drained)...
	l.OverlayAccount(pad(1), 0, big.NewInt(0))
	// ...phase 3: revalidate the reverted tx. It must NOT be silently kept.
	readded, dropped := pc.Revalidate(reverted)
	if len(readded) != 0 || len(dropped) != 1 {
		t.Fatalf("want 0 readded/1 dropped, got %d/%d (%+v)", len(readded), len(dropped), dropped)
	}
	if dropped[0].Reason != reject.ReasonInsufficientFunds {
		t.Fatalf("want INSUFFICIENT_FUNDS, got %s: %s", dropped[0].Reason, dropped[0].Detail)
	}
	t.Logf("reorg revalidation dropped s1n0: %s | %s", dropped[0].Reason, dropped[0].Detail)
	if p.Has(b1.Txs[0].Hash()) {
		t.Fatal("dropped tx must not be in pool")
	}
	size, _ := p.Stats()
	if size != 0 {
		t.Fatalf("pool size want 0, got %d", size)
	}
}
