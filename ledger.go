package txpool

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sync"
)

// Ledger 是链上账本视图：记录各账户余额、已执行 nonce 与已提交区块。
// 所有公开方法均通过同一把互斥锁串行化，保证打包扣减、余额设置与
// 重组回退并发发生时账本视图始终一致。
type Ledger struct {
	mu       sync.Mutex
	balances map[Address]uint64
	nonces   map[Address]uint64
	baseFee  uint64
	blocks   []*Block
	commits  []*blockCommit // 与 blocks 一一对应，用于回滚
}

func NewLedger(baseFee uint64) *Ledger {
	return &Ledger{
		balances: make(map[Address]uint64),
		nonces:   make(map[Address]uint64),
		baseFee:  baseFee,
	}
}

func (l *Ledger) Balance(addr Address) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.balances[addr]
}

func (l *Ledger) Nonce(addr Address) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.nonces[addr]
}

func (l *Ledger) HeadNumber() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return uint64(len(l.blocks))
}

func (l *Ledger) HeadBaseFee() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.baseFee
}

// HeadHash 返回链头区块哈希；空链返回全零。
func (l *Ledger) HeadHash() [32]byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.blocks) == 0 {
		return [32]byte{}
	}
	return l.blocks[len(l.blocks)-1].Hash
}

// SetBalance 模拟外部链状态变化（如充值）。
func (l *Ledger) SetBalance(addr Address, v uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.balances[addr] = v
}

// ledgerTxCommit 记录单笔交易入账前的快照，供回滚使用。
type ledgerTxCommit struct {
	from        Address
	nonce       uint64 // 该交易使用的 nonce
	charged     uint64 // 实际扣减额 = effectiveGasPrice*GasLimit + Value
	nonceBefore uint64 // 入账前账户 nonce
}

// blockCommit 是一个区块的完整入账记录，可整体撤销。
type blockCommit struct {
	block      *Block
	commits    []ledgerTxCommit
	oldBalance map[Address]uint64 // 仅记录本区块涉及账户的入账前余额
}

// applyBlock 按交易顺序原子入账：
//   - 逐笔校验 nonce 与账户当前 nonce 连续、余额足以支付实际成本；
//   - 任一笔失败则整体不生效，返回错误；
//   - 成功则扣减余额、推进 nonce、累计 gas 并挂接区块。
//
// 调用约定：调用方须持有 Pool 锁（锁顺序固定为 Pool -> Ledger）。
func (l *Ledger) applyBlock(b *Block) (*blockCommit, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	expected := uint64(len(l.blocks)) + 1
	if b.Number != expected {
		return nil, fmt.Errorf("ledger: block number %d, expected %d", b.Number, expected)
	}
	var parent [32]byte
	if len(l.blocks) > 0 {
		parent = l.blocks[len(l.blocks)-1].Hash
	}
	if b.ParentHash != parent {
		return nil, fmt.Errorf("ledger: parent hash mismatch at block %d", b.Number)
	}

	commit := &blockCommit{block: b, oldBalance: make(map[Address]uint64)}
	gasUsed := uint64(0)

	// 先在局部影子状态上执行，全部成功才提交到真实状态，保证原子性。
	shadowBal := make(map[Address]uint64)
	bal := func(addr Address) uint64 {
		if v, ok := shadowBal[addr]; ok {
			return v
		}
		return l.balances[addr]
	}

	for _, tx := range b.Txs {
		if tx.GasLimit == 0 {
			return nil, fmt.Errorf("ledger: tx %x has zero gas limit", shortHash(tx))
		}
		curNonce := l.nonces[tx.From]
		// 影子 nonce：同一区块内该发送者已执行几笔。
		executed := uint64(0)
		for _, c := range commit.commits {
			if c.from == tx.From {
				executed++
			}
		}
		curNonce += executed
		if tx.Nonce != curNonce {
			return nil, fmt.Errorf("ledger: tx %x nonce %d, expected %d (nonce hole or duplicate in block)",
				shortHash(tx), tx.Nonce, curNonce)
		}

		gp, ok := tx.effectiveGasPrice(l.baseFee)
		if !ok {
			return nil, fmt.Errorf("ledger: gas price overflow for tx %x", shortHash(tx))
		}
		gasCost, ok := saturatingMul(gp, tx.GasLimit)
		if !ok {
			return nil, fmt.Errorf("ledger: gas cost overflow for tx %x", shortHash(tx))
		}
		charged, ok := saturatingAdd(tx.Value, gasCost)
		if !ok {
			return nil, fmt.Errorf("ledger: charge overflow for tx %x", shortHash(tx))
		}
		have := bal(tx.From)
		if have < charged {
			return nil, fmt.Errorf("ledger: sender %x balance %d < charged %d for tx %x",
				tx.From[:4], have, charged, shortHash(tx))
		}
		shadowBal[tx.From] = have - charged

		if gas, ok := saturatingAdd(gasUsed, tx.GasLimit); !ok || gas > b.GasLimit {
			return nil, fmt.Errorf("ledger: block gas limit exceeded at tx %x", shortHash(tx))
		} else {
			gasUsed = gas
		}
		commit.commits = append(commit.commits, ledgerTxCommit{
			from:        tx.From,
			nonce:       tx.Nonce,
			charged:     charged,
			nonceBefore: curNonce,
		})
	}

	// 全部通过：落账。
	for addr, v := range shadowBal {
		if _, recorded := commit.oldBalance[addr]; !recorded {
			commit.oldBalance[addr] = l.balances[addr]
		}
		l.balances[addr] = v
	}
	// nonce 推进：每个发送者推进其在本区块执行的笔数。
	for _, c := range commit.commits {
		l.nonces[c.from] = c.nonce + 1
	}
	b.GasUsed = gasUsed
	b.BaseFee = l.baseFee
	b.Hash = hashBlock(b)
	l.blocks = append(l.blocks, b)
	l.commits = append(l.commits, commit)
	return commit, nil
}

// revertCommit 撤销一个区块入账：恢复余额与 nonce，弹出区块。
// 调用约定：调用方须持有 Pool 锁。
func (l *Ledger) revertCommit(c *blockCommit) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.blocks) == 0 || l.blocks[len(l.blocks)-1] != c.block {
		// 仅允许回滚链头区块；调用方自顶向下调用即满足。
		return
	}
	for addr, old := range c.oldBalance {
		l.balances[addr] = old
	}
	// nonce 恢复为各发送者在本区块执行前的值（取该发送者在区块中
	// 第一笔执行交易的 nonceBefore，不能逐笔覆盖，否则会被后续交易覆盖）。
	restored := make(map[Address]bool)
	for _, tc := range c.commits {
		if !restored[tc.from] {
			l.nonces[tc.from] = tc.nonceBefore
			restored[tc.from] = true
		}
	}
	l.blocks = l.blocks[:len(l.blocks)-1]
	l.commits = l.commits[:len(l.commits)-1]
}

// headCommit 返回链头区块的入账记录（调用方须持有 Pool 锁）。
func (l *Ledger) headCommitLocked() *blockCommit {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.commits) == 0 {
		return nil
	}
	return l.commits[len(l.commits)-1]
}

func shortHash(tx *Transaction) []byte {
	h := tx.Hash()
	return h[:6]
}

func hashBlock(b *Block) [32]byte {
	var buf []byte
	var tmp [8]byte
	binary.BigEndian.PutUint64(tmp[:], b.Number)
	buf = append(buf, tmp[:]...)
	buf = append(buf, b.ParentHash[:]...)
	for _, tx := range b.Txs {
		h := tx.Hash()
		buf = append(buf, h[:]...)
	}
	return sha256.Sum256(buf)
}
