package txpool

import (
	"crypto/sha256"
	"encoding/binary"
	"time"
)

// Address 是账户标识（20 字节，风格同以太坊地址）。
type Address [20]byte

// Transaction 是池内交易，费用模型采用 EIP-1559 风格的
// GasFeeCap（每单位 gas 最高费用）与 GasTipCap（最高优先费/小费）。
type Transaction struct {
	From      Address
	To        *Address
	Nonce     uint64
	Value     uint64
	GasLimit  uint64
	GasFeeCap uint64
	GasTipCap uint64
	Data      []byte

	// 入池后由 Pool 填充的元数据。
	submitTime time.Time
	seq        uint64 // 到达序号，用于确定性平局打破
}

// signingPreimage 返回哈希用的确定性字节编码：
// 固定头部字段 + 可选 To + Data，使相同交易内容得到相同哈希。
func (t *Transaction) signingPreimage() []byte {
	buf := make([]byte, 0, 20+20+8*5+len(t.Data))
	buf = append(buf, t.From[:]...)
	if t.To != nil {
		buf = append(buf, t.To[:]...)
	} else {
		buf = append(buf, make([]byte, 20)...)
	}
	var tmp [8]byte
	binary.BigEndian.PutUint64(tmp[:], t.Nonce)
	buf = append(buf, tmp[:]...)
	binary.BigEndian.PutUint64(tmp[:], t.Value)
	buf = append(buf, tmp[:]...)
	binary.BigEndian.PutUint64(tmp[:], t.GasLimit)
	buf = append(buf, tmp[:]...)
	binary.BigEndian.PutUint64(tmp[:], t.GasFeeCap)
	buf = append(buf, tmp[:]...)
	binary.BigEndian.PutUint64(tmp[:], t.GasTipCap)
	buf = append(buf, tmp[:]...)
	buf = append(buf, t.Data...)
	return buf
}

// Hash 返回交易的确定性标识（sha256 截断前 32 字节）。
func (t *Transaction) Hash() [32]byte {
	return sha256.Sum256(t.signingPreimage())
}

// UpfrontCost 返回入池时的预付成本 Value + GasLimit*GasFeeCap。
// 第二个返回值表示运算是否溢出 uint64；调用方应将溢出视为非法交易。
func (t *Transaction) UpfrontCost() (uint64, bool) {
	gasCost, ok := saturatingMul(t.GasLimit, t.GasFeeCap)
	if !ok {
		return 0, false
	}
	total, ok := saturatingAdd(t.Value, gasCost)
	if !ok {
		return 0, false
	}
	return total, true
}

// effectiveGasPrice 返回区块中实际支付的每 gas 价格：
// baseFee + min(tipCap, feeCap-baseFee)。
func (t *Transaction) effectiveGasPrice(baseFee uint64) (uint64, bool) {
	tip := effectiveTip(t.GasTipCap, t.GasFeeCap, baseFee)
	return saturatingAdd(baseFee, tip)
}

// Block 是打包产出的区块。
type Block struct {
	Number     uint64
	BaseFee    uint64
	ParentHash [32]byte
	Hash       [32]byte
	Txs        []*Transaction
	GasUsed    uint64
	GasLimit   uint64
}

func saturatingMul(a, b uint64) (uint64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	r := a * b
	if r/a != b {
		return 0, false
	}
	return r, true
}

func saturatingAdd(a, b uint64) (uint64, bool) {
	if a > ^uint64(0)-b {
		return 0, false
	}
	return a + b, true
}
