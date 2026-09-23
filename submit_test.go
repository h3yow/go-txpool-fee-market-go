package txpool

import (
	"errors"
	"testing"
)

// TestSubmitValidation 覆盖格式、nonce 顺序与余额三类拒绝原因的可区分性。
func TestSubmitValidation(t *testing.T) {
	pool, ledger := newTestPool(t, DefaultConfig())
	a := testAddr(1)
	ledger.SetBalance(a, 1000)

	tests := []struct {
		name   string
		tx     *Transaction
		target *RejectError
	}{
		{
			name:   "zero gas limit",
			tx:     mkTx(a, 0, 0, 0, 200, 10),
			target: ErrInvalidFormat,
		},
		{
			name:   "tip greater than fee cap",
			tx:     mkTx(a, 0, 21000, 0, 200, 201),
			target: ErrInvalidFormat,
		},
		{
			name:   "nonce too low after one tx executed",
			tx:     mkTx(a, 0, 21000, 0, 200, 10), // 提交在 nonce 推进后
			target: ErrNonceTooLow,
		},
		{
			name:   "nonce gap too far",
			tx:     mkTx(a, 100, 21000, 0, 200, 10),
			target: ErrNonceGap,
		},
		{
			name:   "insufficient balance for upfront cost",
			tx:     mkTx(a, 1, 21000, 900, 200, 10), // 21000*200 远超余额
			target: ErrInsufficientBalance,
		},
	}

	// 先打包一笔使链上 nonce 推进到 1，为 nonce-too-low 用例做准备。
	mustSubmit(t, pool, mkTx(a, 0, 5, 0, 200, 10))
	pk := NewPacker(pool, ledger)
	pk.SetLogger(func(f string, a ...any) { t.Logf("[pack] "+f, a...) })
	if _, _, err := pk.Build(100_000); err != nil {
		t.Fatalf("setup block failed: %v", err)
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Logf("INPUT from=%x nonce=%d gas=%d value=%d feeCap=%d tip=%d balance=%d stateNonce=%d",
				tc.tx.From[:4], tc.tx.Nonce, tc.tx.GasLimit, tc.tx.Value,
				tc.tx.GasFeeCap, tc.tx.GasTipCap, ledger.Balance(a), ledger.Nonce(a))
			err := pool.Submit(tc.tx)
			assertReject(t, err, tc.target)
		})
	}
}

// TestSubmitAcceptsQueuedNonce 验证 nonce+1 的排队交易可先入池，
// 待前序交易执行后自动成为可执行交易（不留空洞）。
func TestSubmitAcceptsQueuedNonce(t *testing.T) {
	pool, ledger := newTestPool(t, DefaultConfig())
	a := testAddr(2)
	ledger.SetBalance(a, 10_000_000)

	mustSubmit(t, pool, mkTx(a, 1, 5, 0, 200, 10))
	if got := len(pool.Pending()); got != 0 {
		t.Fatalf("nonce 1 with stateNonce 0 must not be pending yet, got %d", got)
	}
	t.Log("DECISION: nonce=1 queued (gap of 1), not executable until nonce=0 lands")

	mustSubmit(t, pool, mkTx(a, 0, 5, 0, 200, 10))
	pending := pool.Pending()
	if len(pending) != 2 {
		t.Fatalf("after filling nonce 0, expected 2 contiguous pending txs, got %d", len(pending))
	}
	t.Logf("DECISION: %d txs executable in pending snapshot (global fee order)", len(pending))

	// 同账户 nonce 连续性与顺序必须在打包结果中体现。
	pk := NewPacker(pool, ledger)
	pk.SetLogger(func(f string, a2 ...any) { t.Logf("[pack] "+f, a2...) })
	b, _, err := pk.Build(100_000)
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}
	if len(b.Txs) != 2 || b.Txs[0].Nonce != 0 || b.Txs[1].Nonce != 1 {
		t.Fatalf("block must pack sender txs in nonce order 0,1; got %v", noncesOf(b.Txs))
	}
	t.Log("DECISION: block packs same-sender txs in nonce order 0,1")
}

// TestAlreadyKnown 完全相同的交易重复提交得到可区分原因。
func TestAlreadyKnown(t *testing.T) {
	pool, ledger := newTestPool(t, DefaultConfig())
	a := testAddr(3)
	ledger.SetBalance(a, 10_000_000)
	tx := mkTx(a, 0, 5, 0, 200, 10)
	mustSubmit(t, pool, tx)
	err := pool.Submit(mkTx(a, 0, 5, 0, 200, 10))
	assertReject(t, err, ErrAlreadyKnown)
	if !errors.Is(err, ErrAlreadyKnown) {
		t.Fatal("errors.Is classification failed")
	}
}
