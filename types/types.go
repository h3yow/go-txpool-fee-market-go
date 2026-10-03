// Package types defines the core transaction and account primitives shared by
// the transaction pool, ledger and packer.
package types

import (
	"crypto/sha256"
	"encoding/binary"
	"math/big"
)

// Address is a 20-byte account identifier.
type Address [20]byte

// Hash is a 32-byte transaction hash.
type Hash [32]byte

// Transaction is an EIP-1559 style transaction.
//
// GasFeeCap is the absolute cap (wei/gas) the sender is willing to pay;
// GasTipCap is the priority fee (tip) offered to the block producer.
type Transaction struct {
	From      Address
	To        Address
	Nonce     uint64
	Value     *big.Int
	Gas       uint64
	GasFeeCap *big.Int
	GasTipCap *big.Int
	Data      []byte
}

// Hash returns a deterministic identifier of the transaction derived from its
// fields via SHA-256. It is stable across copies and sufficient as pool
// identity (the project does not implement signature recovery).
func (t *Transaction) Hash() Hash {
	h := sha256.New()
	h.Write(t.From[:])
	h.Write(t.To[:])
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], t.Nonce)
	h.Write(buf[:])
	if t.Value != nil {
		h.Write(t.Value.Bytes())
	}
	binary.BigEndian.PutUint64(buf[:], t.Gas)
	h.Write(buf[:])
	if t.GasFeeCap != nil {
		h.Write(t.GasFeeCap.Bytes())
	}
	if t.GasTipCap != nil {
		h.Write(t.GasTipCap.Bytes())
	}
	h.Write(t.Data)
	var out Hash
	copy(out[:], h.Sum(nil))
	return out
}

// EffectiveTip returns the effective producer tip under a given base fee:
//
//	tip = min(GasTipCap, GasFeeCap-baseFee), clamped at zero.
func (t *Transaction) EffectiveTip(baseFee *big.Int) *big.Int {
	headroom := new(big.Int).Sub(t.GasFeeCap, baseFee)
	if headroom.Sign() < 0 {
		return new(big.Int)
	}
	if headroom.Cmp(t.GasTipCap) < 0 {
		return headroom
	}
	return new(big.Int).Set(t.GasTipCap)
}

// EffectiveGasPrice returns baseFee + effective tip (never above GasFeeCap).
func (t *Transaction) EffectiveGasPrice(baseFee *big.Int) *big.Int {
	return new(big.Int).Add(baseFee, t.EffectiveTip(baseFee))
}

// Cost returns Value + GasFeeCap*Gas, i.e. the maximum balance that must be
// available for the transaction to be admissible.
func (t *Transaction) Cost() *big.Int {
	fee := new(big.Int).Mul(t.GasFeeCap, new(big.Int).SetUint64(t.Gas))
	return new(big.Int).Add(t.Value, fee)
}

// EffectiveFee returns EffectiveTip(baseFee)*Gas: the fee actually charged at
// commit time for the given base fee (used for packing-time affordability
// simulation; admission keeps using the conservative Cost()).
func (t *Transaction) EffectiveFee(baseFee *big.Int) *big.Int {
	return new(big.Int).Mul(t.EffectiveTip(baseFee), new(big.Int).SetUint64(t.Gas))
}

// Account is a ledger account state.
type Account struct {
	Nonce   uint64
	Balance *big.Int
}
