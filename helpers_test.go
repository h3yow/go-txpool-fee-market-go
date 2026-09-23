package txpool

import (
	"errors"
	"testing"
)

func testAddr(n byte) Address {
	var a Address
	a[19] = n
	return a
}

func mkTx(from Address, nonce uint64, gasLimit, value, feeCap, tip uint64) *Transaction {
	return &Transaction{
		From:      from,
		Nonce:     nonce,
		GasLimit:  gasLimit,
		Value:     value,
		GasFeeCap: feeCap,
		GasTipCap: tip,
	}
}

func newTestPool(t *testing.T, cfg Config) (*Pool, *Ledger) {
	t.Helper()
	if cfg.GlobalSlots == 0 {
		cfg = DefaultConfig()
	}
	ledger := NewLedger(100) // 基础费 100
	pool := New(cfg, ledger)
	pool.SetLogger(func(f string, a ...any) { t.Logf("[pool] "+f, a...) })
	return pool, ledger
}

func mustSubmit(t *testing.T, p *Pool, tx *Transaction) {
	t.Helper()
	if err := p.Submit(tx); err != nil {
		t.Fatalf("unexpected reject: tx(from=%x nonce=%d): %v", tx.From[:4], tx.Nonce, err)
	}
}

func assertReject(t *testing.T, err error, target *RejectError) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected rejection %s, got nil", target.Code)
	}
	var re *RejectError
	if !errors.As(err, &re) {
		t.Fatalf("expected *RejectError, got %T: %v", err, err)
	}
	if !errors.Is(err, target) {
		t.Fatalf("expected reject code %s, got %s (%v)", target.Code, re.Code, err)
	}
	t.Logf("correctly classified: code=%s detail=%q", re.Code, re.Reason)
}

func noncesOf(txs []*Transaction) []uint64 {
	out := make([]uint64, len(txs))
	for i, tx := range txs {
		out[i] = tx.Nonce
	}
	return out
}
