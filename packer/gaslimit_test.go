package packer

import (
	"math/big"
	"testing"

	"github.com/h3yow/go-txpool-fee-market-go/reject"
)

func TestBlockGasLimitDefersSenderChain(t *testing.T) {
	pc, p, ledger := newSystem(t, 0)
	pc.blockGasLmt = 1500
	ledger.SetBalance(pad(1), big.NewInt(1_000_000))
	ledger.SetBalance(pad(2), big.NewInt(1_000_000))

	// sender1: nonce0 gas1000 (fits), nonce1 gas1000 (would exceed after s2...
	mustSubmit(t, p, ptx(1, 0, 0, 10, 20, 1000))
	mustSubmit(t, p, ptx(1, 1, 0, 10, 20, 1000))
	// sender2 nonce0 gas1000 with higher tip -> packs second (1000+1000=2000
	// would exceed 1500? s1n0=1000 then s2n0=1000 fits exactly 2000? no, limit1500).
	// Order by tip: give s2 tip 100 -> s2 packs before s1n1.
	mustSubmit(t, p, ptx(2, 0, 0, 100, 200, 1000))

	block, skipped, err := pc.Produce(0)
	if err != nil {
		t.Fatal(err)
	}
	// tip order heads: s2n0(100) packs first (gas1000), then s1n0(10) packs
	// (gas total 2000 > 1500) -> sender1 deferred wholesale.
	if len(block.Txs) != 1 || block.Txs[0].From[0] != 2 {
		var ids []byte
		for _, tx := range block.Txs {
			ids = append(ids, tx.From[0])
		}
		t.Fatalf("expected only s2 packed, got %v", ids)
	}
	if len(skipped) != 2 {
		t.Fatalf("expected 2 deferral decisions (s1n0,s1n1), got %d", len(skipped))
	}
	for _, d := range skipped {
		if d.Reason != reject.ReasonGasLimit {
			t.Fatalf("want GAS_LIMIT decision, got %s", d.Reason)
		}
		t.Logf("deferred s%dn%d: %s", d.Tx.From[0], d.Tx.Nonce, d.Detail)
	}
	// s1 chain intact in pool and packs fully once a later block has enough
	// gas (limit is a per-block, chain-tunable parameter).
	pc.blockGasLmt = DefaultBlockGasLimit
	block2, _, err := pc.Produce(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(block2.Txs) != 2 || block2.Txs[0].Nonce != 0 || block2.Txs[1].Nonce != 1 {
		t.Fatalf("expected s1 nonce0,1 next block, got %d txs", len(block2.Txs))
	}
	t.Log("sender1 chain deferred without splitting; packed [0,1] in next block")
}
