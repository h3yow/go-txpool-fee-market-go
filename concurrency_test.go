package txpool

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// TestMultiBlockReorg 两个区块后回退到高度 1：只恢复第二个区块的交易与扣减。
func TestMultiBlockReorg(t *testing.T) {
	pool, ledger := newTestPool(t, DefaultConfig())
	a := testAddr(20)
	ledger.SetBalance(a, 1_000_000)
	pk := NewPacker(pool, ledger)
	pk.SetLogger(func(f string, args ...any) { t.Logf("[pack] "+f, args...) })

	mustSubmit(t, pool, mkTx(a, 0, 5, 0, 200, 10))
	if _, _, err := pk.Build(100_000); err != nil {
		t.Fatal(err)
	}
	balAfter1 := ledger.Balance(a) // 扣 550

	mustSubmit(t, pool, mkTx(a, 1, 5, 0, 200, 10))
	if _, _, err := pk.Build(100_000); err != nil {
		t.Fatal(err)
	}
	if ledger.Nonce(a) != 2 || ledger.HeadNumber() != 2 {
		t.Fatalf("pre-reorg state wrong: nonce=%d head=%d", ledger.Nonce(a), ledger.HeadNumber())
	}

	recovered, err := pk.ReorgTo(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 || recovered[0].Nonce != 1 {
		t.Fatalf("expected only nonce 1 recovered, got %v", noncesOf(recovered))
	}
	if ledger.HeadNumber() != 1 || ledger.Nonce(a) != 1 {
		t.Fatalf("state after partial reorg: head=%d nonce=%d", ledger.HeadNumber(), ledger.Nonce(a))
	}
	if ledger.Balance(a) != balAfter1 {
		t.Fatalf("balance should revert to %d, got %d", balAfter1, ledger.Balance(a))
	}
	if pool.Len() != 1 {
		t.Fatalf("nonce 1 must be back in pool, size=%d", pool.Len())
	}
	t.Logf("DECISION: reorg 2->1 restored nonce=1 balance=%d and returned nonce 1 tx to pool",
		balAfter1)
}

// TestConcurrentSubmitAndPack 大量并发提交 + 并发打包结束后：
// 账本扣减总额必须等于"初始余额-最终余额"，且每个账户 nonce 连续，
// 池中没有任何低于链上 nonce 的交易（无重复打包、无 nonce 空洞）。
func TestConcurrentSubmitAndPack(t *testing.T) {
	cfg := Config{GlobalSlots: 5000, AccountSlots: 5000, MaxNonceGap: 5000, PriceBumpPct: 10}
	pool, ledger := newTestPool(t, cfg)
	pk := NewPacker(pool, ledger)

	const senders = 8
	const perSender = 40
	const startBalance = 1e18
	var submitOK, submitRej int64

	// 预置余额。
	for s := 0; s < senders; s++ {
		ledger.SetBalance(testAddr(byte(s+1)), startBalance)
	}

	// 并发提交：每账户 nonce 0..39，多 goroutine 打乱顺序。
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for s := 0; s < senders; s++ {
				for n := worker; n < perSender; n += 4 {
					addr := testAddr(byte(s + 1))
					tx := mkTx(addr, uint64(n), 2, 0, 200, 10)
					if err := pool.Submit(tx); err != nil {
						atomic.AddInt64(&submitRej, 1)
					} else {
						atomic.AddInt64(&submitOK, 1)
					}
				}
			}
		}(w)
	}

	// 并发持续打包，直到提交者结束且池被排空。
	var packWG sync.WaitGroup
	packWG.Add(1)
	go func() {
		defer packWG.Done()
		for i := 0; i < 200; i++ {
			b, _, err := pk.Build(1_000_000)
			if err != nil {
				t.Errorf("concurrent build failed: %v", err)
				return
			}
			if len(b.Txs) == 0 && atomic.LoadInt64(&submitOK)+atomic.LoadInt64(&submitRej) == senders*perSender {
				return
			}
		}
	}()

	wg.Wait()
	packWG.Wait()
	// 提交者完成后再打一轮，排空剩余 nonce（晚到的前置 nonce 补齐后）。
	for i := 0; i < 50 && pool.Len() > 0; i++ {
		if _, _, err := pk.Build(1_000_000); err != nil {
			t.Fatalf("drain build: %v", err)
		}
	}

	t.Logf("INPUT submit ok=%d rejected=%d final pool size=%d head=#%d",
		submitOK, submitRej, pool.Len(), ledger.HeadNumber())

	// 一致性校验 1：每个已执行 nonce 从 0 连续推进，无空洞。
	for s := 0; s < senders; s++ {
		addr := testAddr(byte(s + 1))
		executed := ledger.Nonce(addr)
		// 账本无法直接枚举历史，逐块校验已在 ledger.applyBlock 中保证；
		// 此处校验余额扣减 = 已执行笔数 * 单笔实际成本 420（gas2*(100+10)）。
		charged := uint64(executed) * 2 * 110
		got := startBalance - ledger.Balance(addr)
		if got != charged {
			t.Fatalf("sender %d: balance deducted %d != executed(%d)*220=%d (double deduction?)",
				s+1, got, executed, charged)
		}
		// 池中不得残留已执行 nonce。
		if ap := pool.accounts[addr]; ap != nil {
			for n := range ap.txs {
				if n < executed {
					t.Fatalf("sender %d: stale nonce %d < stateNonce %d in pool", s+1, n, executed)
				}
			}
		}
	}

	// 一致性校验 2：所有已打包区块中的交易哈希全局唯一（无重复打包）。
	seen := make(map[[32]byte]int)
	ledger.mu.Lock()
	nblocks := uint64(len(ledger.blocks))
	ledger.mu.Unlock()
	for n := uint64(0); n < nblocks; n++ {
		ledger.mu.Lock()
		blk := ledger.blocks[n]
		ledger.mu.Unlock()
		for _, tx := range blk.Txs {
			key := tx.Hash()
			if prev, dup := seen[key]; dup {
				t.Fatalf("tx %x packed in both block #%d and #%d", key[:8], prev, blk.Number)
			}
			seen[key] = int(blk.Number)
		}
	}
	t.Logf("DECISION: %d unique txs across %d blocks, no duplicate pack, no double deduction, no nonce hole",
		len(seen), nblocks)

	// 最终：由于全部 nonce 连续提交且余额充足，所有交易都应被打包（无空洞丢弃）。
	var totalExecuted uint64
	for s := 0; s < senders; s++ {
		totalExecuted += ledger.Nonce(testAddr(byte(s + 1)))
	}
	if totalExecuted != senders*perSender {
		t.Fatalf("expected all %d txs executed, got %d (pool=%d)", senders*perSender, totalExecuted, pool.Len())
	}
	if pool.Len() != 0 {
		t.Fatalf("pool should be drained, size=%d", pool.Len())
	}
}

// TestConcurrentReplacements 同账户同 nonce 并发替换后，池中每 nonce 恰好一笔。
func TestConcurrentReplacements(t *testing.T) {
	pool, ledger := newTestPool(t, Config{GlobalSlots: 100, AccountSlots: 100, MaxNonceGap: 100, PriceBumpPct: 10})
	a := testAddr(30)
	ledger.SetBalance(a, 1e18)

	var wg sync.WaitGroup
	var ok, rej int64
	// 报价采用 2 的幂次几何间距：每个更高报价对任意更早报价都翻倍，
	// 必然满足 10% 提升门槛，因此任意到达顺序下绝对最高报价都应获胜。
	bid := func(i int) (uint64, uint64) { return 200 * (uint64(1) << i), 10 * (uint64(1) << i) }
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			fee, tip := bid(i)
			tx := mkTx(a, 0, 10, 0, fee, tip)
			if err := pool.Submit(tx); err != nil {
				atomic.AddInt64(&rej, 1)
			} else {
				atomic.AddInt64(&ok, 1)
			}
		}(i)
	}
	wg.Wait()
	if pool.Len() != 1 {
		t.Fatalf("exactly one tx must remain for nonce 0, size=%d", pool.Len())
	}
	wantFee, wantTip := bid(19)
	t.Logf("INPUT 20 concurrent geometric-spaced bids -> ok=%d rejected=%d, final nonce0 feeCap=%d tip=%d",
		ok, rej, pool.accounts[a].txs[0].GasFeeCap, pool.accounts[a].txs[0].GasTipCap)
	got := pool.accounts[a].txs[0]
	if got.GasFeeCap != wantFee || got.GasTipCap != wantTip {
		t.Fatalf("highest bid must win under geometric spacing, got feeCap=%d tip=%d, want %d/%d",
			got.GasFeeCap, got.GasTipCap, wantFee, wantTip)
	}
	t.Log("DECISION: concurrent replacements serialized; highest bid deterministically wins")
}

var _ = fmt.Sprintf
