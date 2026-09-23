package ledger

import (
	"errors"
	"math/big"
	"testing"

	"github.com/h3yow/go-txpool-fee-market-go/types"
)

func la(b byte) types.Address { var a types.Address; a[0] = b; return a }

func ltx(from byte, nonce uint64, value, tip, cap int64, gas uint64) *types.Transaction {
	return &types.Transaction{
		From: la(from), To: la(99), Nonce: nonce,
		Value: big.NewInt(value), Gas: gas,
		GasTipCap: big.NewInt(tip), GasFeeCap: big.NewInt(cap),
	}
}

func TestApplyAndRevert(t *testing.T) {
	l := New()
	// baseFee 10, tx: value 100, tip cap 5, fee cap 20, gas 1000.
	// effective tip = min(5, 20-10) = 5; fee = 5000; total = 5100.
	l.SetBalance(la(1), big.NewInt(100_000))
	l.SetBalance(la(2), big.NewInt(100_000))

	txs := []*types.Transaction{ltx(1, 0, 100, 5, 20, 1000), ltx(2, 0, 0, 5, 20, 1000)}
	if err := l.Apply(1, big.NewInt(10), txs); err != nil {
		t.Fatalf("block1 apply: %v", err)
	}
	a1 := l.GetAccount(la(1))
	t.Logf("after block1: sender1 nonce=%d balance=%s (want nonce1 bal94900)", a1.Nonce, a1.Balance)
	if a1.Nonce != 1 || a1.Balance.Cmp(big.NewInt(94_900)) != 0 {
		t.Fatalf("sender1 wrong state: %+v", a1)
	}
	if l.Head() != 1 {
		t.Fatalf("head want 1, got %d", l.Head())
	}

	// Non-contiguous block id must be rejected.
	err := l.Apply(3, big.NewInt(10), nil)
	if !errors.Is(err, ErrNonContiguousBlock) {
		t.Fatalf("want ErrNonContiguousBlock, got %v", err)
	}
	t.Logf("block id 3 rejected: %v", err)

	// Block2: sender1 second tx.
	if err := l.Apply(2, big.NewInt(10), []*types.Transaction{ltx(1, 1, 100, 5, 20, 1000)}); err != nil {
		t.Fatalf("block2 apply: %v", err)
	}
	if got := l.GetAccount(la(1)).Nonce; got != 2 {
		t.Fatalf("nonce want 2, got %d", got)
	}

	// Revert to head 1: both blocks2 removed and sender1 nonce/balance restored
	// to the after-block1 image (nonce1, balance 94900).
	snaps, err := l.Revert(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 {
		t.Fatalf("want 1 detached snapshot, got %d", len(snaps))
	}
	a1 = l.GetAccount(la(1))
	t.Logf("after revert to head1: sender1 nonce=%d balance=%s (want 1 / 94900)", a1.Nonce, a1.Balance)
	if a1.Nonce != 1 || a1.Balance.Cmp(big.NewInt(94_900)) != 0 {
		t.Fatalf("sender1 not restored: %+v", a1)
	}
	a2 := l.GetAccount(la(2))
	if a2.Nonce != 1 || a2.Balance.Cmp(big.NewInt(95_000)) != 0 {
		t.Fatalf("sender2 should be untouched by revert, got %+v", a2)
	}
}

func TestApplyAtomicOnFailure(t *testing.T) {
	l := New()
	l.SetBalance(la(1), big.NewInt(100_000))
	// tx0 is fine; tx1 (same sender) asks value 100_000 with fee on top ->
	// insufficient. Block must fail with NO partial deduction.
	block := []*types.Transaction{
		ltx(1, 0, 0, 5, 20, 1000),
		ltx(1, 1, 100_000, 5, 20, 1000),
	}
	err := l.Apply(1, big.NewInt(10), block)
	if !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("want ErrInsufficientFunds, got %v", err)
	}
	t.Logf("atomic rejection: %v", err)
	a := l.GetAccount(la(1))
	if a.Nonce != 0 || a.Balance.Cmp(big.NewInt(100_000)) != 0 {
		t.Fatalf("state changed despite failed apply: %+v", a)
	}
	if l.Head() != 0 {
		t.Fatalf("no block may be recorded, head=%d", l.Head())
	}
	t.Log("no balance deducted and no nonce advanced after failed apply")
}

func TestApplyNonceMismatch(t *testing.T) {
	l := New()
	l.SetBalance(la(1), big.NewInt(1_000_000))
	err := l.Apply(1, big.NewInt(0), []*types.Transaction{ltx(1, 5, 0, 1, 2, 100)})
	if !errors.Is(err, ErrNonceMismatch) {
		t.Fatalf("want ErrNonceMismatch, got %v", err)
	}
	t.Logf("nonce gap rejected: %v", err)
}

func TestRevertMultipleBlocksRestoresOldestImage(t *testing.T) {
	l := New()
	l.SetBalance(la(1), big.NewInt(1_000_000))
	tx0 := ltx(1, 0, 1000, 1, 5, 100) // fee 100 (baseFee0), total 1100
	tx1 := ltx(1, 1, 1000, 1, 5, 100) // total 1100
	if err := l.Apply(1, big.NewInt(0), []*types.Transaction{tx0}); err != nil {
		t.Fatal(err)
	}
	if err := l.Apply(2, big.NewInt(0), []*types.Transaction{tx1}); err != nil {
		t.Fatal(err)
	}
	b := l.GetAccount(la(1)).Balance
	if b.Cmp(big.NewInt(997_800)) != 0 {
		t.Fatalf("balance after 2 blocks want 997800, got %s", b)
	}
	snaps, err := l.Revert(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 2 {
		t.Fatalf("want 2 snapshots, got %d", len(snaps))
	}
	a := l.GetAccount(la(1))
	if a.Nonce != 0 || a.Balance.Cmp(big.NewInt(1_000_000)) != 0 {
		t.Fatalf("full revert failed: nonce=%d balance=%s", a.Nonce, a.Balance)
	}
	t.Logf("reverted 2 blocks; restored nonce=%d balance=%s", a.Nonce, a.Balance)
}
