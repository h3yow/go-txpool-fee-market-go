package txpool

import (
	"container/heap"
	"fmt"
)

// SkipReason 记录某笔交易在打包阶段被跳过的原因。
type SkipReason struct {
	Tx     *Transaction
	Reason string
}

// Packer 负责费用市场驱动的区块打包与链重组处理。
// 锁顺序固定为 Pool.mu -> Ledger.mu，打包（含账本扣减）与
// 回退都在 Pool 锁内原子完成，因此并发下不会出现：
//   - 同一交易被两个区块重复打包；
//   - 余额被重复扣减；
//   - nonce 空洞（账本逐笔校验连续 nonce）。
type Packer struct {
	pool   *Pool
	ledger *Ledger
	logf   func(format string, args ...any)
}

func NewPacker(p *Pool, l *Ledger) *Packer {
	return &Packer{pool: p, ledger: l, logf: func(string, ...any) {}}
}

// SetLogger 注入日志函数。
func (pk *Packer) SetLogger(f func(format string, args ...any)) {
	if f != nil {
		pk.logf = f
	}
}

// cand 是费用优先堆中的候选：某账户当前可执行链的链头交易。
type cand struct {
	tx  *Transaction
	acc Address
	idx int // 该账户可执行链下标
}

type candHeap struct {
	items   []cand
	baseFee uint64
}

func (h candHeap) Len() int { return len(h.items) }
func (h candHeap) Less(i, j int) bool {
	// compareTx <0 表示 i 更优先，Less 返回 true。
	return compareTx(h.items[i].tx, h.items[j].tx, h.baseFee) < 0
}
func (h candHeap) Swap(i, j int) { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *candHeap) Push(x any)   { h.items = append(h.items, x.(cand)) }
func (h *candHeap) Pop() any {
	n := len(h.items)
	x := h.items[n-1]
	h.items = h.items[:n-1]
	return x
}

// Build 按费用优先、同账户 nonce 连续的规则打包一个区块并原子提交：
//  1. 取每账户从链上 nonce 起、余额可覆盖的连续可执行链；
//  2. 各链链头进入费用优先堆，按有效小费全局竞价；
//  3. 弹出费用最高者，gas 可容纳则入选，并把该账户下一笔链上交易推入堆；
//     不可容纳则跳过该账户（nonce 不连续的后续交易不能越过它入选）；
//  4. 账本原子入账成功后才推进池状态；任一步失败整体不生效。
func (pk *Packer) Build(gasLimit uint64) (*Block, []SkipReason, error) {
	pk.pool.mu.Lock()
	defer pk.pool.mu.Unlock()

	baseFee := pk.ledger.HeadBaseFee()

	type chain struct{ txs []*Transaction }
	chains := make(map[Address]*chain)
	h := &candHeap{baseFee: baseFee}
	for addr, ap := range pk.pool.accounts {
		ap.stateNonce = pk.ledger.Nonce(addr)
		bal := pk.ledger.Balance(addr)
		txs := ap.executable(bal, baseFee)
		if len(txs) == 0 {
			continue
		}
		chains[addr] = &chain{txs: txs}
		heap.Push(h, cand{tx: txs[0], acc: addr, idx: 0})
	}

	var selected []*Transaction
	var skips []SkipReason
	gasUsed := uint64(0)
	skipped := make(map[Address]bool)

	for h.Len() > 0 {
		c := heap.Pop(h).(cand)
		if skipped[c.acc] {
			continue
		}
		next, ok := saturatingAdd(gasUsed, c.tx.GasLimit)
		if !ok || next > gasLimit {
			skipped[c.acc] = true
			reason := fmt.Sprintf("block gas remaining %d < tx gasLimit %d; later nonces of this sender skipped to preserve nonce order",
				gasLimit-gasUsed, c.tx.GasLimit)
			skips = append(skips, SkipReason{Tx: c.tx, Reason: reason})
			pk.logf("pack skip from=%x nonce=%d: %s", c.tx.From[:4], c.tx.Nonce, reason)
			continue
		}
		selected = append(selected, c.tx)
		gasUsed = next
		pk.logf("pack select from=%x nonce=%d eff_tip=%d gas=%d cumulative_gas=%d",
			c.tx.From[:4], c.tx.Nonce, tipOf(c.tx, baseFee), c.tx.GasLimit, gasUsed)

		ch := chains[c.acc]
		if c.idx+1 < len(ch.txs) {
			heap.Push(h, cand{tx: ch.txs[c.idx+1], acc: c.acc, idx: c.idx + 1})
		}
	}

	number := pk.ledger.HeadNumber() + 1
	parent := pk.ledger.HeadHash()
	b := &Block{
		Number:     number,
		ParentHash: parent,
		Txs:        selected,
		GasLimit:   gasLimit,
	}

	// 原子入账：失败则池与账本均不改变，可安全重试。
	if _, err := pk.ledger.applyBlock(b); err != nil {
		pk.logf("pack block #%d apply FAILED (pool unchanged): %v", number, err)
		return nil, nil, fmt.Errorf("packer: apply block: %w", err)
	}
	pk.pool.onBlockCommitted(b)
	pk.logf("pack block #%d committed: txs=%d gas_used=%d hash=%x", number, len(selected), gasUsed, b.Hash[:8])
	return b, skips, nil
}

// ReorgTo 将链头回退到 number 高度（number 必须 < 当前链头），
// 回退区块中的交易在同一把 Pool 锁内重回交易池，账本余额与
// nonce 同步恢复，因此池视图与账本视图不会出现中间不一致状态。
func (pk *Packer) ReorgTo(number uint64) ([]*Transaction, error) {
	pk.pool.mu.Lock()
	defer pk.pool.mu.Unlock()

	head := pk.ledger.HeadNumber()
	if number > head {
		return nil, fmt.Errorf("packer: cannot reorg forward: target %d > head %d", number, head)
	}

	var recovered []*Transaction
	for pk.ledger.HeadNumber() > number {
		c := pk.ledger.headCommitLocked()
		if c == nil {
			break
		}
		// 按区块内顺序倒序收集，使回池顺序的 nonce 单调（纯语义，不影响正确性）。
		for i := len(c.commits) - 1; i >= 0; i-- {
			tc := c.commits[i]
			if tx, ok := pk.findTxInCommit(c, tc.from, tc.nonce); ok {
				recovered = append(recovered, tx)
			}
		}
		pk.ledger.revertCommit(c)
		pk.logf("reorg reverted block #%d, txs=%d", c.block.Number, len(c.commits))
	}

	// 所有区块回退完成后再校正账户链上 nonce，然后统一回池，
	// 保证回池校验看到的是最终账本视图（nonce 已完全恢复）。
	pk.pool.prepareReorgLocked()

	readded, errs := pk.pool.reorgReaddLocked(recovered)
	pk.logf("reorg to %d complete: recovered=%d readded=%d rejected=%d",
		number, len(recovered), readded, len(errs))
	if len(errs) > 0 {
		// 回池失败（如池满）不影响链回退本身的正确性，交由上层处理。
		return recovered, fmt.Errorf("packer: %d tx(s) could not re-enter pool after reorg (first: %w)",
			len(errs), errs[0])
	}
	return recovered, nil
}

// findTxInCommit 从区块提交记录中定位原始交易对象。
func (pk *Packer) findTxInCommit(c *blockCommit, from Address, nonce uint64) (*Transaction, bool) {
	for _, tx := range c.block.Txs {
		if tx.From == from && tx.Nonce == nonce {
			return tx, true
		}
	}
	return nil, false
}
