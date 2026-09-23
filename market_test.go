package txpool

import "testing"

// TestReplacementBump 同账户同 nonce 替换必须满足 10% 费用提升。
func TestReplacementBump(t *testing.T) {
	pool, ledger := newTestPool(t, DefaultConfig())
	a := testAddr(1)
	ledger.SetBalance(a, 100_000_000)

	old := mkTx(a, 0, 10, 0, 200, 10) // feeCap=200, tip=10
	mustSubmit(t, pool, old)

	// baseFee=100：旧有效小费 = min(10, 200-100)=10。
	// 提升 10% 需要 feeCap>=220 且新有效小费>=bumped(10)=11。
	t.Run("feeCap bumped but tip not -> underpriced", func(t *testing.T) {
		tx := mkTx(a, 0, 10, 0, 220, 10)
		t.Logf("INPUT old feeCap=200 tip=10 -> new feeCap=220 tip=10, baseFee=100, bumpPct=10")
		err := pool.Submit(tx)
		assertReject(t, err, ErrReplaceUnderpriced)
	})
	t.Run("both dimensions bumped -> replaced", func(t *testing.T) {
		tx := mkTx(a, 0, 10, 0, 220, 11)
		t.Logf("INPUT new feeCap=220 tip=11 (need feeCap>=220 effTip>=11)")
		mustSubmit(t, pool, tx)
		got, ok := pool.accounts[a].get(0)
		if !ok || got.GasTipCap != 11 {
			t.Fatalf("expected replacement to win, got ok=%v tip=%d", ok, got.GasTipCap)
		}
		if pool.Len() != 1 {
			t.Fatalf("replacement must not change pool size, got %d", pool.Len())
		}
		t.Log("DECISION: replacement accepted, pool size unchanged")
	})
}

// TestEvictionOnGlobalFull 容量满载时按有效小费最低 + 到达序确定性驱逐。
func TestEvictionOnGlobalFull(t *testing.T) {
	cfg := Config{GlobalSlots: 3, AccountSlots: 10, MaxNonceGap: 100, PriceBumpPct: 10}
	pool, ledger := newTestPool(t, cfg)

	// 三个不同账户，各一笔，小费 5 / 3 / 8，全部占满容量。
	senders := []Address{testAddr(1), testAddr(2), testAddr(3)}
	tips := []uint64{5, 3, 8}
	for i, s := range senders {
		ledger.SetBalance(s, 100_000_000)
		mustSubmit(t, pool, mkTx(s, 0, 10, 0, 200, tips[i]))
	}
	if pool.Len() != 3 {
		t.Fatalf("expected full pool size 3, got %d", pool.Len())
	}

	var evicted *Transaction
	pool.SetEvictionHook(func(ev Eviction) {
		t.Logf("EVICT HOOK: victim from=%x nonce=%d effTip=%d reason=%q",
			ev.Tx.From[:4], ev.Tx.Nonce, tipOf(ev.Tx, 100), ev.Reason)
		evicted = ev.Tx
	})

	// 新账户小费 4：低于最低在池小费 3？不，4 > 3，应驱逐 tip=3 的账户 2。
	newSender := testAddr(4)
	ledger.SetBalance(newSender, 100_000_000)
	incoming := mkTx(newSender, 0, 10, 0, 200, 4)
	t.Logf("INPUT incoming effTip=4; pool tips=%v; expecting tip=3 evicted", tips)
	mustSubmit(t, pool, incoming)

	if evicted == nil || evicted.From != senders[1] {
		t.Fatalf("expected sender[1] (tip 3) evicted, got %v", evicted)
	}
	if _, exists := pool.accounts[senders[1]]; exists {
		t.Fatal("evicted sender's account must be removed from pool")
	}
	if pool.Len() != 3 {
		t.Fatalf("pool size must remain at cap, got %d", pool.Len())
	}
	t.Log("DECISION: lowest-fee tx deterministically evicted, capacity stays bounded")

	// 再来一笔小费 1：打不过当前最低（tip=4），应拒绝且无驱逐。
	low := testAddr(5)
	ledger.SetBalance(low, 100_000_000)
	evicted = nil
	err := pool.Submit(mkTx(low, 0, 10, 0, 200, 1))
	assertReject(t, err, ErrPoolFull)
	if evicted != nil {
		t.Fatal("no eviction must happen when incoming fee cannot beat worst tx")
	}
}

// TestEvictionTieBreak 相同小费时先到者不被驱逐（到达序平局打破，确定可预测）。
func TestEvictionTieBreak(t *testing.T) {
	cfg := Config{GlobalSlots: 2, AccountSlots: 10, MaxNonceGap: 100, PriceBumpPct: 10}
	pool, ledger := newTestPool(t, cfg)
	a1, a2, a3 := testAddr(1), testAddr(2), testAddr(3)
	ledger.SetBalance(a1, 1e18)
	ledger.SetBalance(a2, 1e18)
	ledger.SetBalance(a3, 1e18)
	mustSubmit(t, pool, mkTx(a1, 0, 10, 0, 200, 5))
	mustSubmit(t, pool, mkTx(a2, 0, 10, 0, 200, 5))
	// 等小费的后来者：严格更优才驱逐，同费一律拒绝，规则确定可预测。
	err := pool.Submit(mkTx(a3, 0, 10, 0, 200, 5))
	assertReject(t, err, ErrPoolFull)
	t.Log("DECISION: equal-tip incoming cannot evict (strictly greater fee required)")
}

// TestFeeOrdering 不同账户按有效小费决定打包先后。
func TestFeeOrdering(t *testing.T) {
	pool, ledger := newTestPool(t, DefaultConfig())
	lo, hi := testAddr(1), testAddr(2)
	ledger.SetBalance(lo, 1e18)
	ledger.SetBalance(hi, 1e18)
	mustSubmit(t, pool, mkTx(lo, 0, 10, 0, 200, 2))  // effTip=2
	mustSubmit(t, pool, mkTx(hi, 0, 10, 0, 300, 90)) // effTip=min(90,200)=90

	pk := NewPacker(pool, ledger)
	pk.SetLogger(func(f string, a ...any) { t.Logf("[pack] "+f, a...) })
	b, _, err := pk.Build(100_000)
	if err != nil {
		t.Fatal(err)
	}
	if b.Txs[0].From != hi || b.Txs[1].From != lo {
		t.Fatalf("expected high-tip first, got %x,%x", b.Txs[0].From[:4], b.Txs[1].From[:4])
	}
	t.Log("DECISION: block orders by effective tip desc (90 before 2)")
}
