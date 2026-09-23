package reject

import (
	"errors"
	"testing"

	"github.com/h3yow/go-txpool-fee-market-go/types"
)

func TestDecisionReasonString(t *testing.T) {
	want := map[Reason]string{
		ReasonMalformed: "MALFORMED", ReasonNonceTooLow: "NONCE_TOO_LOW",
		ReasonNonceTooHigh: "NONCE_TOO_HIGH", ReasonUnderpriced: "UNDERPRICED",
		ReasonInsufficientFunds: "INSUFFICIENT_FUNDS", ReasonPoolFull: "POOL_FULL",
		ReasonAlreadyReserved: "ALREADY_RESERVED", ReasonGasLimit: "GAS_LIMIT",
	}
	for r, s := range want {
		if r.String() != s {
			t.Fatalf("%d: got %s want %s", r, r.String(), s)
		}
	}
	if ReasonUnknown.String() != "UNKNOWN" {
		t.Fatal("unknown reason must render UNKNOWN")
	}
}

func TestAsDecision(t *testing.T) {
	var tx *types.Transaction
	d := New(tx, ReasonPoolFull, "pool full")
	var err error = d
	got, ok := AsDecision(err)
	if !ok || got.Reason != ReasonPoolFull {
		t.Fatalf("AsDecision roundtrip failed: %v", got)
	}
	if _, ok := AsDecision(errors.New("plain")); ok {
		t.Fatal("plain error must not be classified as Decision")
	}
	if d.Error() == "" {
		t.Fatal("Error() must render text")
	}
	t.Logf("decision renders: %s", d.Error())
}
