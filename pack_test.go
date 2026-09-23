package txpool

import "testing"

// TestPackAndCharge 打包时余额按 baseFee+tip 实际扣减，且打包后
// 池内移除、nonce 推进，池视图与账本一致。
func TestPackAndCharge(t *testing.T) {
	pool, ledger := newTestPool(t, DefaultConfig())
	a := testAddr(7)
	ledger.SetBalance(a, 10_000)

	// gas=5 value=100 feeCap=200 tip=10 baseFee=100
	// 实际 gas 价格 = 100 + min(10, 100) = 110；扣减 = 100 + 5*110 = 650。
	mustSubmit(t, pool, mkTx(a, 0, 5, 100, 200, 10))
	mustSubmit(t, pool, mkTx(a, 1, 5, 100, 200, 10)) // 第二笔，打包后总扣减 1300

	pk := NewPacker(pool, ledger)
	pk.SetLogger(func(f string, args ...any) { t.Logf("[pack] "+f, args...) })
	b, skips, err := pk.Build(100_000)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(b.Txs) != 2 {
		t.Fatalf("expected 2 packed, got %d skips=%d", len(b.Txs), len(skips))
	}
	if got := ledger.Balance(a); got != 10_000-1300 {
		t.Fatalf("balance after pack = %d, want %d", got, 10_000-1300)
	}
	if got := ledger.Nonce(a); got != 2 {
		t.Fatalf("nonce after pack = %d, want 2", got)
	}
	if pool.Len() != 0 {
		t.Fatalf("packed txs must leave pool, size=%d", pool.Len())
	}
	t.Logf("DECISION: charged 1300 (2x 650 actual cost), nonce advanced to 2, pool drained")

	// 旧 nonce 重放应被拒绝。
	err = pool.Submit(mkTx(a, 0, 1, 0, 200, 1))
	assertReject(t, err, ErrNonceTooLow)
}

// TestPackInsufficientAtExecution 入池用 feeCap 预估通过，但余额不足以
// 覆盖实际成本时，该交易及其后续同账户交易不得入选（防止 nonce 空洞）。
func TestPackInsufficientAtExecution(t *testing.T) {
	pool, ledger := newTestPool(t, DefaultConfig())
	a := testAddr(8)
	// 入池时按 feeCap 预估：两笔 10*200=2000，合计 4000，余额充足允许入池。
	ledger.SetBalance(a, 4000)
	mustSubmit(t, pool, mkTx(a, 0, 10, 0, 200, 10)) // 实际 1100
	mustSubmit(t, pool, mkTx(a, 1, 10, 0, 200, 10)) // 实际 1100
	// 外部状态变化：区块打包前该账户转出资金（如其他交易路径），
	// 余额只剩 1500：够 nonce 0 的实际成本 1100，不够两笔 2200。
	ledger.SetBalance(a, 1500)
	t.Logf("INPUT balance reduced externally 4000->1500 before packing; actual cost 1100/tx")
	b, _, err := NewPacker(pool, ledger).Build(100_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Txs) != 1 || b.Txs[0].Nonce != 0 {
		t.Fatalf("only nonce 0 can execute after balance change, got %v", noncesOf(b.Txs))
	}
	// 第二笔仍在池中排队，nonce 不空洞。
	if pool.Len() != 1 {
		t.Fatalf("nonce 1 must remain queued, pool size=%d", pool.Len())
	}
	t.Log("DECISION: unaffordable nonce 1 skipped, kept queued; no nonce hole in block")
}

// TestReorgRewindsTxsAndBalance 重组回退后，交易回池、余额与 nonce 恢复，
// 重新打包得到同样的扣减结果（无重复扣减）。
func TestReorgRewindsTxsAndBalance(t *testing.T) {
	pool, ledger := newTestPool(t, DefaultConfig())
	a := testAddr(9)
	ledger.SetBalance(a, 10_000)
	mustSubmit(t, pool, mkTx(a, 0, 5, 100, 200, 10))
	mustSubmit(t, pool, mkTx(a, 1, 5, 100, 200, 10))

	pk := NewPacker(pool, ledger)
	pk.SetLogger(func(f string, args ...any) { t.Logf("[pack] "+f, args...) })
	b1, _, err := pk.Build(100_000)
	if err != nil {
		t.Fatal(err)
	}
	if ledger.HeadNumber() != 1 || ledger.Balance(a) != 8700 || ledger.Nonce(a) != 2 {
		t.Fatalf("unexpected post-block state head=%d bal=%d nonce=%d",
			ledger.HeadNumber(), ledger.Balance(a), ledger.Nonce(a))
	}

	// 回退到创世高度（0）。
	recovered, err := pk.ReorgTo(0)
	if err != nil {
		t.Fatalf("reorg: %v", err)
	}
	if len(recovered) != 2 {
		t.Fatalf("expected 2 recovered txs, got %d", len(recovered))
	}
	if ledger.HeadNumber() != 0 {
		t.Fatalf("head after reorg = %d, want 0", ledger.HeadNumber())
	}
	if ledger.Balance(a) != 10_000 {
		t.Fatalf("balance not restored: %d", ledger.Balance(a))
	}
	if ledger.Nonce(a) != 0 {
		t.Fatalf("nonce not restored: %d", ledger.Nonce(a))
	}
	if pool.Len() != 2 {
		t.Fatalf("recovered txs must be back in pool, size=%d", pool.Len())
	}
	t.Log("DECISION: reorg restored balance 10000 and nonce 0; 2 txs back in pool")

	// 重新打包：必须仍然成功，且扣减额与第一次相同（没有重复扣减）。
	b2, _, err := pk.Build(100_000)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if len(b2.Txs) != 2 || ledger.Balance(a) != 8700 {
		t.Fatalf("rebuild inconsistent: txs=%d balance=%d", len(b2.Txs), ledger.Balance(a))
	}
	if b2.Number != 1 || b2.ParentHash != b1.ParentHash {
		t.Fatalf("rebuilt block must restart at number 1 from same parent")
	}
	t.Log("DECISION: rebuilt block #1 charges exactly once again; no double deduction")
}

// TestReorgWithReplacement 回退的交易若已被更高费交易替换，则不重新入池，
// 链上 nonce 回退后替换交易仍可正常执行。
func TestReorgWithReplacement(t *testing.T) {
	pool, ledger := newTestPool(t, DefaultConfig())
	a := testAddr(10)
	ledger.SetBalance(a, 100_000_000)
	mustSubmit(t, pool, mkTx(a, 0, 10, 0, 200, 10))
	pk := NewPacker(pool, ledger)
	if _, _, err := pk.Build(100_000); err != nil {
		t.Fatal(err)
	}
	// 链上 nonce=1 后无法直接替换 nonce 0，先回退：回池 old nonce0。
	recovered, err := pk.ReorgTo(0)
	if err != nil || len(recovered) != 1 {
		t.Fatalf("reorg recovered=%d err=%v", len(recovered), err)
	}
	// 用合规的更高费交易替换它。
	mustSubmit(t, pool, mkTx(a, 0, 10, 0, 300, 50))
	if pool.Len() != 1 {
		t.Fatalf("replacement should keep size 1, got %d", pool.Len())
	}
	b, _, err := pk.Build(100_000)
	if err != nil || len(b.Txs) != 1 || b.Txs[0].GasTipCap != 50 {
		t.Fatalf("expected replacement tx packed, txs=%d err=%v", len(b.Txs), err)
	}
	t.Log("DECISION: after reorg the bumped replacement replaces recovered tx and packs")
}

// TestGasLimitTruncation gas 不足时跳过当前账户链头并继续为其他账户打包。
func TestGasLimitTruncation(t *testing.T) {
	pool, ledger := newTestPool(t, DefaultConfig())
	big, small := testAddr(11), testAddr(12)
	ledger.SetBalance(big, 1e18)
	ledger.SetBalance(small, 1e18)
	mustSubmit(t, pool, mkTx(big, 0, 100, 0, 200, 90)) // 高小费但占满 gas
	mustSubmit(t, pool, mkTx(small, 0, 10, 0, 200, 5))
	pk := NewPacker(pool, ledger)
	pk.SetLogger(func(f string, a ...any) { t.Logf("[pack] "+f, a...) })
	b, skips, err := pk.Build(50)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Txs) != 1 || b.Txs[0].From != small {
		t.Fatalf("small tx should fit after big tx skipped, got %v", noncesOf(b.Txs))
	}
	if len(skips) != 1 {
		t.Fatalf("expected 1 skip reason, got %d", len(skips))
	}
	t.Logf("DECISION: big high-tip tx skipped (%s); smaller sender packed", skips[0].Reason)
}
