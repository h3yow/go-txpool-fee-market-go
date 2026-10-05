# go-txpool-fee-market-go

A minimal, well-tested transaction pool + fee-market block production
implementation in Go.

## Packages

- `types` — `Transaction`, `Account`, deterministic hash, EIP-1559 fee math.
- `reject` — structured rejection reasons (`MALFORMED`, `NONCE_TOO_LOW`,
  `NONCE_TOO_HIGH`, `UNDERPRICED`, `INSUFFICIENT_FUNDS` (single-tx),
  `ACCOUNT_BUDGET` (whole-account aggregate), `POOL_FULL`,
  `ALREADY_RESERVED`, `GAS_LIMIT`).
- `ledger` — authoritative account state, atomic `Apply`, undo-snapshot
  `Revert`, canonical-state `OverlayAccount`.
- `pool` — fee-prioritised pool: nonce-ordered per-account queues,
  price-bump replacement, deterministic cheapest-tail eviction, **account-level
  aggregate-budget admission that also counts txs already handed to an
  in-flight (packed but unconfirmed) batch**, and `AccountView` queue
  inspection (in-flight occupation broken out).
- `packer` — block production: fee-ordered selection, block gas limit,
  per-sender balance simulation that packs only the still-executable nonce
  prefix when a balance changed mid-flight (suffix deferred, other accounts
  never blocked), reserve/apply/confirm with rollback, a test-only
  Reserve→Apply hook for the concurrent-submit window, and reorg recovery.

## Quick start

```go
l := ledger.New()
l.SetBalance(addr, big.NewInt(1_000_000))

p := pool.New(pool.DefaultConfig(), l, big.NewInt(10))
err := p.Submit(&types.Transaction{...})
if d, ok := reject.AsDecision(err); ok {
    log.Printf("rejected: %s: %s", d.Reason, d.Detail)
}

pc := packer.New(p, l, big.NewInt(10))
// Only each sender's continuously affordable prefix is packed; txs deferred
// because the balance shrank come back as ACCOUNT_BUDGET decisions and remain
// pending for the next Produce (a top-up needs no resubmission).
block, skipped, err := pc.Produce(0)
for _, d := range skipped {
    log.Printf("deferred: %s: %s", d.Reason, d.Detail)
}

// Inspect exactly what one account is queueing right now.
view := p.AccountView(addr)
log.Printf("balance=%s maxBudget=%s budgetOK=%v ready=%d/%d",
    view.Balance, view.MaxBudget, view.BudgetOK, view.ReadyCount, len(view.Pending))
for _, pt := range view.Pending {
    log.Printf("nonce=%d %s: %s", pt.Tx.Nonce, pt.Status, pt.Reason)
}

// reorg
readded, dropped, err := pc.Reorg(targetHead)
```

## Design

See [docs/design.md](docs/design.md) for the fee model, replacement/eviction
rules, reorg handling and the invariants tests verify.

## Development

```bash
go test -race ./...
```
