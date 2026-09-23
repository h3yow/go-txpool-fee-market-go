package packer

import (
	"math/big"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/h3yow/go-txpool-fee-market-go/reject"
)

// TestConcurrentSubmitAndProduce hammers the system from many goroutines:
// senders submit ordered nonces while producers continuously pack blocks.
// Afterwards the invariants are checked:
//   - every (account,nonce) committed exactly once (no double packing);
//   - account nonce advanced with no hole;
//   - balance deducted equals the exact committed spend (no double spend);
//   - pool and chain views together account for every transaction.
func TestConcurrentSubmitAndProduce(t *testing.T) {
	const senders = 8
	const perSender = 40
	const baseFee = int64(2)
	const gas = uint64(1000)

	pc, p, l := newSystem(t, baseFee)
	startBal := big.NewInt(1_000_000_000)
	for s := 1; s <= senders; s++ {
		l.SetBalance(pad(byte(s)), startBal)
	}

	var submitWG sync.WaitGroup
	for s := 1; s <= senders; s++ {
		s := s
		submitWG.Add(1)
		go func() {
			defer submitWG.Done()
			for n := 0; n < perSender; n++ {
				tip := int64(3 + (n % 5))
				tx := ptx(byte(s), uint64(n), 1, tip, tip+10, gas)
				// Retry capacity/racing rejections: under contention a slot
				// may be momentarily full or a head tx just packed.
				for {
					err := p.Submit(tx)
					if err == nil {
						break
					}
					d, ok := reject.AsDecision(err)
					if !ok {
						t.Errorf("non-decision error: %v", err)
						return
					}
					switch d.Reason {
					case reject.ReasonPoolFull, reject.ReasonUnderpriced,
						reject.ReasonNonceTooHigh, reject.ReasonAlreadyReserved:
						time.Sleep(time.Microsecond)
						continue
					case reject.ReasonNonceTooLow:
						// Raced with an identical commit path? Cannot happen
						// for unique nonces here; treat as fatal.
						t.Errorf("unexpected NONCE_TOO_LOW for s%dn%d: %s", s, n, d.Detail)
						return
					default:
						t.Errorf("unexpected rejection %s for s%dn%d: %s", d.Reason, s, n, d.Detail)
						return
					}
				}
			}
		}()
	}

	// Producer stops once all submitters are done AND one produce finds the
	// pool empty.
	producerDone := make(chan struct{})
	var producedMu sync.Mutex
	var produced [][2]byte // (sender, nonce) commitments

	go func() {
		defer close(producerDone)
		submitsDone := false
		for {
			b, _, err := pc.Produce(64)
			if err != nil {
				t.Errorf("produce: %v", err)
				return
			}
			if b != nil {
				producedMu.Lock()
				for _, tx := range b.Txs {
					produced = append(produced, [2]byte{tx.From[0], byte(tx.Nonce)})
				}
				producedMu.Unlock()
			}
			if !submitsDone {
				select {
				case <-waitGroupChan(&submitWG):
					submitsDone = true
				default:
				}
			}
			if submitsDone && b == nil {
				return
			}
			if b == nil {
				time.Sleep(time.Microsecond)
			}
		}
	}()

	submitWG.Wait()
	<-producerDone

	// --- Invariant checks -------------------------------------------------

	// 1. No (sender,nonce) pair was packed twice.
	count := make(map[[2]byte]int)
	for _, k := range produced {
		count[k]++
	}
	for k, c := range count {
		if c != 1 {
			t.Fatalf("sender%d nonce%d packed %d times", k[0], k[1], c)
		}
	}
	t.Logf("committed txs=%d; every (sender,nonce) unique", len(produced))

	// 2. Every sender advanced to perSender with a contiguous nonce set.
	for s := 1; s <= senders; s++ {
		if nonce := l.GetAccount(pad(byte(s))).Nonce; nonce != perSender {
			t.Fatalf("sender%d final nonce %d != %d (hole or loss)", s, nonce, perSender)
		}
		var got []int
		for k := range count {
			if k[0] == byte(s) {
				got = append(got, int(k[1]))
			}
		}
		sort.Ints(got)
		if len(got) != perSender {
			t.Fatalf("sender%d committed %d txs, want %d", s, len(got), perSender)
		}
		for i, n := range got {
			if n != i {
				t.Fatalf("sender%d nonce set not contiguous at index %d (nonce %d)", s, i, n)
			}
		}
	}

	// 3. Balance exactly equals start minus sum of (value + effFee) per tx.
	spent := make(map[byte]*big.Int)
	for _, k := range produced {
		s, n := k[0], int64(k[1])
		tip := int64(3 + (int(n) % 5))
		tx := ptx(s, uint64(n), 1, tip, tip+10, gas)
		fee := new(big.Int).Mul(tx.EffectiveTip(big.NewInt(baseFee)), big.NewInt(int64(gas)))
		cost := new(big.Int).Add(tx.Value, fee)
		if spent[s] == nil {
			spent[s] = new(big.Int)
		}
		spent[s].Add(spent[s], cost)
	}
	for s := 1; s <= senders; s++ {
		want := new(big.Int).Sub(new(big.Int).Set(startBal), spent[byte(s)])
		if got := l.GetAccount(pad(byte(s))).Balance; got.Cmp(want) != 0 {
			t.Fatalf("sender%d balance %s != expected %s (double deduction?)", s, got, want)
		}
	}
	t.Logf("all %d senders: balances match exact committed spend (no double deduction)", senders)

	// 4. Pool drained.
	if size, _ := p.Stats(); size != 0 {
		t.Fatalf("pool size %d after full commit, want 0", size)
	}
	if pc.Head() == 0 {
		t.Fatal("expected at least one block")
	}
	t.Logf("pool drained; head=%d", pc.Head())
}

// waitGroupChan returns a channel that closes when wg completes.
func waitGroupChan(wg *sync.WaitGroup) <-chan struct{} {
	ch := make(chan struct{})
	go func() { wg.Wait(); close(ch) }()
	return ch
}

// TestConcurrentReorgSafety runs produce/reorg/repack cycles and asserts the
// pool and ledger converge with no duplicate or lost transactions.
func TestConcurrentReorgSafety(t *testing.T) {
	pc, p, l := newSystem(t, 0)
	l.SetBalance(pad(1), big.NewInt(10_000_000))

	for round := 0; round < 5; round++ {
		for j := 0; j < 3; j++ {
			if err := p.Submit(ptx(1, uint64(round*3+j), 0, 5, 20, 21000)); err != nil {
				t.Fatal(err)
			}
		}
		b, _, err := pc.Produce(0)
		if err != nil || len(b.Txs) != 3 {
			t.Fatalf("round %d: err=%v txs=%d", round, err, len(b.Txs))
		}
		// Reorg one block back: txs return, then immediately repack.
		readded, dropped, err := pc.Reorg(pc.Head() - 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(dropped) != 0 {
			t.Fatalf("round %d drops: %+v", round, dropped)
		}
		if len(readded) != 3 {
			t.Fatalf("round %d readded %d, want 3", round, len(readded))
		}
		if got := l.GetAccount(pad(1)).Nonce; got != uint64(round*3) {
			t.Fatalf("round %d ledger nonce %d want %d", round, got, round*3)
		}
		if size, _ := p.Stats(); size != 3 {
			t.Fatalf("round %d pool size %d want 3", round, size)
		}
		t.Logf("round %d: reorged to head %d; 3 txs returned; nonce=%d pool=3",
			round, pc.Head(), l.GetAccount(pad(1)).Nonce)
		b2, _, err := pc.Produce(0)
		if err != nil || len(b2.Txs) != 3 {
			t.Fatalf("round %d repack: err=%v txs=%d", round, err, len(b2.Txs))
		}
	}
	if got := l.GetAccount(pad(1)).Nonce; got != 15 {
		t.Fatalf("final nonce want 15, got %d", got)
	}
	// Final chain contains 5 blocks x 3 txs; fee per tx = 5*21000 = 105000.
	totalFee := new(big.Int).Mul(big.NewInt(105_000), big.NewInt(15))
	want := new(big.Int).Sub(big.NewInt(10_000_000), totalFee)
	if got := l.GetAccount(pad(1)).Balance; got.Cmp(want) != 0 {
		t.Fatalf("final balance %s want %s", got, want)
	}
	t.Logf("5 produce/reorg/repack cycles converged: nonce=15 balance=%s (no double deduction)", want)
}
