package packer

import (
	"math/big"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/h3yow/go-txpool-fee-market-go/reject"
	"github.com/h3yow/go-txpool-fee-market-go/types"
)

// TestProduceAroundInFlightWindow models the async block-production window the
// bug report is about: a prefix of one account has been reserved and handed to
// a block being produced (ledger not yet charged) while more submissions keep
// arriving. Requirements verified:
//   - the in-flight account is NOT packed across its live reservation (that
//     would nonce-clash with the in-flight block), but other accounts are
//     packed normally, so a block is still produced;
//   - an over-budget burst tx submitted during that window is refused at the
//     pool door (ACCOUNT_BUDGET, distinct from INSUFFICIENT_FUNDS), stays out
//     of the block and does not disturb queued txs;
//   - after the in-flight block settles (Apply + Confirm) and the account is
//     topped up, Produce packs the contiguous suffix in nonce order with no
//     double deduction and no nonce gap.
func TestProduceAroundInFlightWindow(t *testing.T) {
	pc, p, l := newSystem(t, 0) // baseFee 0: max cost == effective cost

	// s1: nonces 0..3 at 200 each (value 0, tip==cap=2, gas 100). s2: one tx
	// at 300 (tip==cap=3).
	l.SetBalance(pad(1), big.NewInt(1000))
	l.SetBalance(pad(2), big.NewInt(1000))
	s1 := make([]*types.Transaction, 0, 4)
	for n := uint64(0); n < 4; n++ {
		tx := ptx(1, n, 0, 2, 2, 100)
		mustSubmit(t, p, tx)
		s1 = append(s1, tx)
	}
	mustSubmit(t, p, ptx(2, 0, 0, 3, 3, 100))
	t.Log("input: s1 nonces 0..3 cost 200 each (balance 1000); s2 nonce0 cost 300")

	// External producer takes the in-flight block: reserve s1 nonces 0,1 only,
	// WITHOUT charging the ledger yet.
	p.Reserve(s1[:2])
	t.Log("s1 nonces 0,1 handed to in-flight block; ledger still balance=1000 nonce=0")

	// Burst during the window: nonce 4 costs 200 standalone, but the account
	// bill is 2*200 (in-flight) + 2*200 (pending 2,3) + 200 = 1000 <= 1000 ->
	// admitted; nonce 5 would make it 1200 -> ACCOUNT_BUDGET refusal.
	if err := p.Submit(ptx(1, 4, 0, 2, 2, 100)); err != nil {
		t.Fatalf("nonce4 within total bill (1000) must be admitted: %v", err)
	}
	t.Log("burst nonce=4 admitted: bill 400 in-flight + 600 queued == 1000")
	err := p.Submit(ptx(1, 5, 0, 2, 2, 100))
	d, ok := reject.AsDecision(err)
	if !ok || d.Reason != reject.ReasonAccountBudget {
		t.Fatalf("nonce5 bill 1200 > 1000 must be ACCOUNT_BUDGET, got %v (%v)", d, err)
	}
	t.Logf("burst nonce=5 refused %s: %s", d.Reason, d.Detail)

	// Meanwhile the pool keeps producing: s1 cannot cross its live reservation,
	// but s2 must still go out in a block of its own (block id 1).
	block, skipped, err := pc.Produce(0)
	if err != nil {
		t.Fatalf("produce with an in-flight s1 must not error: %v", err)
	}
	if block == nil {
		t.Fatal("a block containing s2 must still be produced")
	}
	if len(block.Txs) != 1 || block.Txs[0].From[0] != 2 || block.Txs[0].Nonce != 0 {
		t.Fatalf("block must contain exactly s2n0, got %s", sumIDs(block.Txs))
	}
	if len(skipped) != 0 {
		t.Fatalf("s1 must be skipped silently as an in-flight-head account, not deferred: %s", sumD(skipped))
	}
	t.Log("produced block id 1 while s1 in flight: [s2n0]; s1 suffix blocked by its own in-flight head, not by budget, no other account harmed")

	// The in-flight block settles afterwards as block id 2 (async commit order),
	// then Confirm releases the pool reservations.
	if err := l.Apply(2, big.NewInt(0), s1[:2]); err != nil {
		t.Fatalf("settle in-flight block: %v", err)
	}
	p.Confirm(s1[:2])
	s1acc := l.GetAccount(pad(1))
	if s1acc.Nonce != 2 || s1acc.Balance.Cmp(big.NewInt(600)) != 0 { // 1000-400
		t.Fatalf("after settle s1 want nonce=2 balance=600, got %d/%s", s1acc.Nonce, s1acc.Balance)
	}
	t.Logf("in-flight block settled: s1 nonce=%d balance=%s (charged once)", s1acc.Nonce, s1acc.Balance)

	// Now shrink s1's balance externally so only part of the remaining suffix is
	// executable: balance 600 covers nonces 2,3 (400) but not 4 (needs 600).
	// (It is already exactly 600 after settle; nonce 4 makes the run 600 — so
	// drain a bit more to force the truncation.)
	l.SetBalance(pad(1), big.NewInt(450))
	block2, skipped2, err := pc.Produce(0)
	if err != nil {
		t.Fatalf("produce after mid-flight shrink must not error: %v", err)
	}
	if got := sumIDs(block2.Txs); got != "[s1n2,s1n3]" {
		t.Fatalf("only executable prefix s1n2,s1n3 must pack, got %s", got)
	}
	if len(skipped2) != 1 || skipped2[0].Tx.Nonce != 4 || skipped2[0].Reason != reject.ReasonAccountBudget {
		t.Fatalf("s1n4 deferred ACCOUNT_BUDGET, got %s", sumD(skipped2))
	}
	t.Logf("balance shrunk to 450 after admission: packed %s; s1n4 deferred (%s) stays pending",
		sumIDs(block2.Txs), skipped2[0].Reason)
	if a := l.GetAccount(pad(1)); a.Nonce != 4 || a.Balance.Cmp(big.NewInt(50)) != 0 {
		t.Fatalf("s1 after prefix pack want nonce=4 balance=50, got %d/%s", a.Nonce, a.Balance)
	}
	if size, _ := p.Stats(); size != 1 {
		t.Fatalf("only s1n4 should remain pending, size=%d", size)
	}

	// Top up; the same deferred nonce (still in the pool, never resubmitted)
	// packs in the next block.
	l.AddBalance(pad(1), big.NewInt(150)) // 50+150 = 200 exactly
	block3, skipped3, err := pc.Produce(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped3) != 0 {
		t.Fatalf("after top-up no deferrals, got %s", sumD(skipped3))
	}
	if got := sumIDs(block3.Txs); got != "[s1n4]" {
		t.Fatalf("final block must pack the remaining suffix [s1n4], got %s", got)
	}
	final := l.GetAccount(pad(1))
	if final.Nonce != 5 {
		t.Fatalf("s1 final nonce want 5 (0..4 each exactly once), got %d", final.Nonce)
	}
	if final.Balance.Sign() != 0 { // 450-400 +150 -200 = 0
		t.Fatalf("s1 final balance want 0, got %s", final.Balance)
	}
	if size, _ := p.Stats(); size != 0 {
		t.Fatalf("pool must be drained, got size %d", size)
	}
	t.Logf("top-up +150 then final pack: s1 nonce=%d balance=%s; nonces 0..4 committed exactly once, no gap, no double deduction",
		final.Nonce, final.Balance)
}

// TestConcurrentInFlightWindowAndSubmit stress-tests the reported window under
// -race with a SINGLE authoritative block committer (two committers racing for
// head+1 is itself invalid): one producer loops Pending -> Reserve ->
// (window open) -> Apply -> Confirm, taking at most 3 s1 txs per round so
// multiple in-flight windows occur; two submitters concurrently burst s1 and
// s2 nonces; a reader continuously asserts the Pending/AccountView contract
// while windows are open (no account offered across its live head). Afterwards
// every s1 nonce must be committed exactly once, nonces contiguous, balances
// equal start minus exact effective spend, and the pool drained.
func TestConcurrentInFlightWindowAndSubmit(t *testing.T) {
	const s1Total = uint64(30)
	const s2Total = uint64(20)
	_, p, l := newSystem(t, 0)
	start := big.NewInt(1_000_000_000)
	l.SetBalance(pad(1), start)
	l.SetBalance(pad(2), start)

	submitsDone := make(chan struct{})
	stopReader := make(chan struct{})
	var submitWG sync.WaitGroup
	submitWG.Add(2)

	// Burst submitter for s1.
	go func() {
		defer submitWG.Done()
		for n := uint64(0); n < s1Total; n++ {
			tx := ptx(1, n, 0, 2, 2, 100) // eff/max cost 200
			for {
				err := p.Submit(tx)
				if err == nil {
					break
				}
				d, ok := reject.AsDecision(err)
				if !ok {
					t.Errorf("s1 n%d non-decision error: %v", n, err)
					return
				}
				// Race window: momentarily gapped, momentarily over the account
				// bill while a prefix is reserved-but-unsettled, or momentarily
				// at the per-account slot cap while the producer drains it.
				if d.Reason == reject.ReasonNonceTooHigh ||
					d.Reason == reject.ReasonAccountBudget ||
					d.Reason == reject.ReasonPoolFull {
					time.Sleep(time.Microsecond)
					continue
				}
				t.Errorf("s1 n%d unexpected %s: %s", n, d.Reason, d.Detail)
				return
			}
		}
	}()

	// Burst submitter for s2 (well funded; packed by the single producer).
	go func() {
		defer submitWG.Done()
		for n := uint64(0); n < s2Total; n++ {
			if err := p.Submit(ptx(2, n, 0, 5, 5, 100)); err != nil {
				t.Errorf("s2 n%d submit: %v", n, err)
				return
			}
			time.Sleep(time.Microsecond)
		}
	}()

	// Reader: while in-flight windows are open, the candidate contract must
	// hold atomically — no account offered across its live head. The check is a
	// single pool-locked snapshot, so it has no time-of-check/time-of-use gap.
	var readerWG sync.WaitGroup
	readerWG.Add(1)
	go func() {
		defer readerWG.Done()
		for {
			select {
			case <-stopReader:
				return
			default:
			}
			if err := p.CheckPendingHeadContract(); err != nil {
				t.Errorf("%v", err)
				return
			}
		}
	}()

	// The single producer/committer. Reserve -> settle with a real delay, so
	// submitters and the reader observe the reserved-but-unsettled window.
	var settledMu sync.Mutex
	var settled []*types.Transaction
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		for {
			cand := p.Pending(big.NewInt(0))
			var batch []*types.Transaction
			s1Taken := 0
			for _, tx := range cand { // Pending keeps per-sender nonce order
				switch tx.From[0] {
				case 1:
					if s1Taken < 3 {
						batch = append(batch, tx)
						s1Taken++
					}
				case 2:
					batch = append(batch, tx)
				}
			}
			if len(batch) == 0 {
				select {
				case <-submitsDone:
					if size, _ := p.Stats(); size == 0 {
						return
					}
				default:
				}
				time.Sleep(time.Microsecond)
				continue
			}
			sort.Slice(batch, func(i, j int) bool {
				if batch[i].From[0] != batch[j].From[0] {
					return batch[i].From[0] < batch[j].From[0]
				}
				return batch[i].Nonce < batch[j].Nonce
			})
			p.Reserve(batch)
			time.Sleep(2 * time.Microsecond) // keep the in-flight window open
			if err := l.Apply(l.Head()+1, big.NewInt(0), batch); err != nil {
				t.Errorf("async apply: %v", err)
				return
			}
			p.Confirm(batch)
			settledMu.Lock()
			settled = append(settled, batch...)
			settledMu.Unlock()
		}
	}()

	submitWG.Wait()
	close(submitsDone)
	<-producerDone
	close(stopReader)
	readerWG.Wait()

	// Invariants.
	if a := l.GetAccount(pad(1)); a.Nonce != s1Total {
		t.Fatalf("s1 final nonce %d want %d", a.Nonce, s1Total)
	}
	settledMu.Lock()
	counts := make(map[[2]byte]int)
	for _, tx := range settled {
		counts[[2]byte{tx.From[0], byte(tx.Nonce)}]++
	}
	settledMu.Unlock()
	for n := uint64(0); n < s1Total; n++ {
		if c := counts[[2]byte{1, byte(n)}]; c != 1 {
			t.Fatalf("s1 nonce %d settled %d times (double pack/loss)", n, c)
		}
	}
	wantS1 := new(big.Int).Sub(start, big.NewInt(int64(s1Total)*200))
	if a := l.GetAccount(pad(1)); a.Balance.Cmp(wantS1) != 0 {
		t.Fatalf("s1 balance %s want %s", a.Balance, wantS1)
	}
	if s2 := l.GetAccount(pad(2)); s2.Nonce != s2Total {
		t.Fatalf("s2 nonce want %d, got %d", s2Total, s2.Nonce)
	}
	wantS2 := new(big.Int).Sub(start, big.NewInt(int64(s2Total)*500))
	if a := l.GetAccount(pad(2)); a.Balance.Cmp(wantS2) != 0 {
		t.Fatalf("s2 balance %s want %s", a.Balance, wantS2)
	}
	if size, _ := p.Stats(); size != 0 {
		t.Fatalf("pool must be drained, got %d", size)
	}
	t.Logf("in-flight window x concurrent submit converged: s1 nonces 0..%d and s2 0..%d each settled once, exact balances, no gap, no double deduction; head=%d",
		s1Total-1, s2Total-1, l.Head())
}
