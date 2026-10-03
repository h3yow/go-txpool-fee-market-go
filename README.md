# go-txpool-fee-market-go

A minimal, well-tested transaction pool + fee-market block production
implementation in Go.

## Packages

- `types` — `Transaction`, `Account`, deterministic hash, EIP-1559 fee math.
- `reject` — structured rejection reasons (`MALFORMED`, `NONCE_TOO_LOW`,
  `NONCE_TOO_HIGH`, `UNDERPRICED`, `INSUFFICIENT_FUNDS`, `ACCOUNT_BUDGET`,
  `POOL_FULL`, `ALREADY_RESERVED`, `GAS_LIMIT`).
- `ledger` — authoritative account state, atomic `Apply`, undo-snapshot
  `Revert`, canonical-state `OverlayAccount`.
- `pool` — fee-prioritised pool: nonce-ordered per-account queues,
  two-level funds admission (single-tx `INSUFFICIENT_FUNDS` vs whole-queue
  `ACCOUNT_BUDGET`), price-bump replacement, cheapest-tail eviction, and
  `AccountStatus` for an explainable per-account queue view.
- `packer` — block production: fee-ordered selection, block gas limit,
  balance-shrink-safe executable-prefix packing (one sender short of funds
  never blocks the block or other senders), reserve/apply/confirm with
  rollback, reorg recovery.

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
block, skipped, err := pc.Produce(0)

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
