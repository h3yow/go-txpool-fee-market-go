package txpool

import "sort"

// accountPool 维护单个账户按 nonce 索引的交易。
// map 本身无序，任何需要顺序的场景都必须显式按 nonce 排序输出，
// 以此保证同账户交易在打包时 nonce 严格连续、不出现空洞。
type accountPool struct {
	stateNonce uint64
	txs        map[uint64]*Transaction
}

func newAccountPool(stateNonce uint64) *accountPool {
	return &accountPool{stateNonce: stateNonce, txs: make(map[uint64]*Transaction)}
}

func (a *accountPool) len() int { return len(a.txs) }

// add 插入交易；若 nonce 已存在则返回被替换的旧交易（调用方先做费用检查）。
func (a *accountPool) add(tx *Transaction) *Transaction {
	old := a.txs[tx.Nonce]
	a.txs[tx.Nonce] = tx
	return old
}

func (a *accountPool) remove(nonce uint64) *Transaction {
	tx, ok := a.txs[nonce]
	if !ok {
		return nil
	}
	delete(a.txs, nonce)
	return tx
}

func (a *accountPool) get(nonce uint64) (*Transaction, bool) {
	tx, ok := a.txs[nonce]
	return tx, ok
}

// nonces 返回当前持有交易的 nonce，升序排列。
func (a *accountPool) nonces() []uint64 {
	out := make([]uint64, 0, len(a.txs))
	for n := range a.txs {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// executable 返回从 stateNonce 起、nonce 连续且余额依次可覆盖的
// 最长可执行链。余额按实际成本 effectiveGasPrice*GasLimit+Value 逐笔扣减；
// 一旦遇到空洞、不可负担或溢出，后续交易立即截断
// —— nonce 不允许跳跃，因此后续交易此刻都不可执行。
func (a *accountPool) executable(balance, baseFee uint64) []*Transaction {
	out := make([]*Transaction, 0)
	remaining := balance
	for nonce := a.stateNonce; ; nonce++ {
		tx, ok := a.txs[nonce]
		if !ok {
			return out
		}
		gp, ok := tx.effectiveGasPrice(baseFee)
		if !ok {
			return out
		}
		gasCost, ok := saturatingMul(gp, tx.GasLimit)
		if !ok {
			return out
		}
		charged, ok := saturatingAdd(tx.Value, gasCost)
		if !ok || remaining < charged {
			return out
		}
		remaining -= charged
		out = append(out, tx)
	}
}

// drain 移除并返回 nonce >= fromNonce 的全部交易，按 nonce 升序。
// 重组回退时，链上已执行 nonce 减小，需要把这些交易放回池中。
func (a *accountPool) drain(fromNonce uint64) []*Transaction {
	var out []*Transaction
	for _, n := range a.nonces() {
		if n < fromNonce {
			continue
		}
		out = append(out, a.txs[n])
		delete(a.txs, n)
	}
	return out
}
