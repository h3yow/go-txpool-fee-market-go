package pool

import (
	"fmt"
	"math/big"
	"testing"

	"github.com/h3yow/go-txpool-fee-market-go/ledger"
	"github.com/h3yow/go-txpool-fee-market-go/reject"
	"github.com/h3yow/go-txpool-fee-market-go/types"
)

func addr(b byte) types.Address {
	var a types.Address
	a[0] = b
	return a
}

func newTx(from byte, nonce uint64, value, tip, cap int64, gas uint64) *types.Transaction {
	return &types.Transaction{
		From:      addr(from),
		To:        addr(99),
		Nonce:     nonce,
		Value:     big.NewInt(value),
		Gas:       gas,
		GasTipCap: big.NewInt(tip),
		GasFeeCap: big.NewInt(cap),
	}
}

func setup(t *testing.T, cfg Config, baseFee int64) (*Pool, *ledger.Ledger) {
	t.Helper()
	l := ledger.New()
	p := New(cfg, l, big.NewInt(baseFee))
	return p, l
}

// expectReason submits and asserts the decision reason; it logs the input and
// the pool's judgement basis (required by the test logging spec).
func expectReason(t *testing.T, p *Pool, tx *types.Transaction, want reject.Reason) {
	t.Helper()
	err := p.Submit(tx)
	if want == reject.ReasonUnknown {
		if err != nil {
			t.Fatalf("input nonce=%d tip=%s cap=%s gas=%d: expected accept, got %v",
				tx.Nonce, tx.GasTipCap, tx.GasFeeCap, tx.Gas, err)
		}
		t.Logf("ACCEPT  nonce=%-2d tip=%-3s cap=%-3s gas=%-5d reason=OK",
			tx.Nonce, tx.GasTipCap, tx.GasFeeCap, tx.Gas)
		return
	}
	if err == nil {
		t.Fatalf("input nonce=%d tip=%s: expected %s, got accept", tx.Nonce, tx.GasTipCap, want)
	}
	d, ok := reject.AsDecision(err)
	if !ok {
		t.Fatalf("expected *reject.Decision, got %T: %v", err, err)
	}
	t.Logf("REJECT  nonce=%-2d tip=%-3s cap=%-3s gas=%-5d reason=%s | %s",
		tx.Nonce, tx.GasTipCap, tx.GasFeeCap, tx.Gas, d.Reason, d.Detail)
	if d.Reason != want {
		t.Fatalf("want reason %s, got %s (%s)", want, d.Reason, d.Detail)
	}
}

func TestSubmitValidation(t *testing.T) {
	p, l := setup(t, DefaultConfig(), 10)
	l.SetBalance(addr(1), big.NewInt(1_000_000))
	from := byte(1)

	// Format legality.
	bad := newTx(from, 0, 0, 5, 3, 100) // tip > feecap
	t.Run("malformed_tip_gt_cap", func(t *testing.T) { expectReason(t, p, bad, reject.ReasonMalformed) })
	zeroGas := newTx(from, 0, 0, 1, 20, 0)
	t.Run("malformed_zero_gas", func(t *testing.T) { expectReason(t, p, zeroGas, reject.ReasonMalformed) })

	// Happy path.
	t.Run("accept_nonce0", func(t *testing.T) {
		expectReason(t, p, newTx(from, 0, 100, 5, 20, 21000), reject.ReasonUnknown)
	})
	// Nonce ordering.
	t.Run("nonce_too_high_gap", func(t *testing.T) {
		expectReason(t, p, newTx(from, 2, 100, 5, 20, 21000), reject.ReasonNonceTooHigh)
	})
	t.Run("accept_nonce1", func(t *testing.T) {
		expectReason(t, p, newTx(from, 1, 100, 5, 20, 21000), reject.ReasonUnknown)
	})
	t.Run("nonce_too_low_dup", func(t *testing.T) {
		expectReason(t, p, newTx(from, 0, 100, 9, 20, 21000), reject.ReasonUnderpriced /*replacement path check below*/)
	})

	// Balance constraint: cost = value + cap*gas = 500_000 + 20*21000 = 920_000
	poor := byte(2)
	l.SetBalance(addr(poor), big.NewInt(1000))
	t.Run("insufficient_funds", func(t *testing.T) {
		expectReason(t, p, newTx(poor, 0, 500_000, 5, 20, 21000), reject.ReasonInsufficientFunds)
	})
}

func TestReplacementBump(t *testing.T) {
	p, l := setup(t, DefaultConfig(), 10)
	l.SetBalance(addr(1), big.NewInt(10_000_000))
	from := byte(1)

	// baseFee 10: tip of (tip=5,cap=20) => min(5, 20-10=10) = 5.
	base := newTx(from, 0, 0, 5, 20, 21000)
	if err := p.Submit(base); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Logf("seed nonce=0 tip=5 cap=20 -> effective tip %d", base.EffectiveTip(big.NewInt(10)))

	// 10% bump rule: min effective tip = floor(5*110/100)=5 (5 >=5 passes),
	// min cap = floor(20*110/100)=22.
	weakCap := newTx(from, 0, 0, 5, 21, 21000)
	t.Run("cap_bump_insufficient", func(t *testing.T) { expectReason(t, p, weakCap, reject.ReasonUnderpriced) })

	// tip=6 -> eff 6 >= 5, cap=22 >= 22 -> accepted.
	ok := newTx(from, 0, 0, 6, 22, 21000)
	t.Run("bump_accepted", func(t *testing.T) { expectReason(t, p, ok, reject.ReasonUnknown) })

	size, _ := p.Stats()
	if size != 1 {
		t.Fatalf("replacement must keep size 1, got %d", size)
	}
	t.Logf("after replacement pool size=%d (must remain 1)", size)

	// identical resubmission idempotent.
	if err := p.Submit(ok); err != nil {
		t.Fatalf("identical resubmission should be a no-op, got %v", err)
	}
	t.Logf("identical resubmission: no-op OK")
}

func TestLedgerNonceAdvancesRejectLow(t *testing.T) {
	p, l := setup(t, DefaultConfig(), 0)
	l.SetBalance(addr(1), big.NewInt(10_000_000))
	from := byte(1)
	if err := p.Submit(newTx(from, 0, 0, 1, 5, 21000)); err != nil {
		t.Fatal(err)
	}
	// Simulate the account nonce advancing on chain (e.g. externally).
	l.SetNonce(addr(1), 1)
	err := p.Submit(newTx(from, 0, 0, 1, 5, 21000))
	d, _ := reject.AsDecision(err)
	t.Logf("after chain nonce advanced to 1, resubmit nonce=0 -> %s: %s", d.Reason, d.Detail)
	if d.Reason != reject.ReasonNonceTooLow {
		t.Fatalf("want NONCE_TOO_LOW, got %s", d.Reason)
	}
}

func ExamplePool_fees() {
	l := ledger.New()
	l.SetBalance(addr(7), big.NewInt(1_000_000))
	p := New(DefaultConfig(), l, big.NewInt(10))
	_ = p.Submit(newTx(7, 0, 0, 5, 20, 21000))
	tx := newTx(7, 0, 0, 8, 25, 21000)
	err := p.Submit(tx)
	fmt.Println(err)
	// Output:
	// <nil>
}
