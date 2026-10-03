package pool_test

import (
	"fmt"
	"math/big"

	"github.com/h3yow/go-txpool-fee-market-go/ledger"
	"github.com/h3yow/go-txpool-fee-market-go/pool"
	"github.com/h3yow/go-txpool-fee-market-go/reject"
	"github.com/h3yow/go-txpool-fee-market-go/types"
)

func ExamplePool_AccountStatus() {
	var from types.Address
	from[0] = 3
	l := ledger.New()
	p := pool.New(pool.DefaultConfig(), l, big.NewInt(0))

	// Three txs with max/exact cost 1100 each (value100 + cap10*gas100);
	// balance 2200 covers only two, so nonce 2 is refused at submission with
	// ACCOUNT_BUDGET (distinct from a single-tx INSUFFICIENT_FUNDS).
	l.SetBalance(from, big.NewInt(2200))
	for n := uint64(0); n < 3; n++ {
		tx := &types.Transaction{
			From: from, To: from, Nonce: n,
			Value: big.NewInt(100), Gas: 100,
			GasTipCap: big.NewInt(10), GasFeeCap: big.NewInt(10),
		}
		if err := p.Submit(tx); err != nil {
			d, _ := reject.AsDecision(err)
			fmt.Printf("submit nonce %d -> %s\n", n, d.Reason)
		}
	}

	view := p.AccountStatus(from, big.NewInt(0))
	fmt.Printf("balance=%s queuedMaxCost=%s budgetOK=%v\n",
		view.Balance, view.QueuedMaxCost, view.BudgetOK)
	for _, it := range view.Items {
		fmt.Printf("nonce=%d %s\n", it.Tx.Nonce, it.State)
	}

	// Balance later reduced externally: the status query shows the boundary.
	l.SetBalance(from, big.NewInt(1100))
	view = p.AccountStatus(from, big.NewInt(0))
	for _, it := range view.Items {
		fmt.Printf("after drain nonce=%d %s\n", it.Tx.Nonce, it.State)
	}
	// Output:
	// submit nonce 2 -> ACCOUNT_BUDGET
	// balance=2200 queuedMaxCost=2200 budgetOK=true
	// nonce=0 PACKABLE
	// nonce=1 PACKABLE
	// after drain nonce=0 PACKABLE
	// after drain nonce=1 NO_FUNDS
}
