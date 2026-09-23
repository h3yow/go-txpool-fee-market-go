package txpool

import (
	"sort"
	"sync"
	"time"
)

// Config 是交易池参数。
type Config struct {
	GlobalSlots  int    // 全局容量（按交易笔数计）
	AccountSlots int    // 单账户最多保留笔数
	MaxNonceGap  uint64 // 允许的最大 nonce 前探距离
	PriceBumpPct uint64 // 替换最小费用提升百分比（0 时取默认 10%）
}

func DefaultConfig() Config {
	return Config{
		GlobalSlots:  100,
		AccountSlots: 16,
		MaxNonceGap:  16,
		PriceBumpPct: DefaultPriceBumpPct,
	}
}

// Eviction 记录一次容量驱逐，供调用方观察与日志解释。
type Eviction struct {
	Tx       *Transaction
	Replaced *Transaction
	Reason   string
}

// Pool 是线程安全的交易池。
type Pool struct {
	mu       sync.Mutex
	cfg      Config
	ledger   *Ledger
	accounts map[Address]*accountPool
	byHash   map[[32]byte]Address
	seq      uint64
	size     int

	logf      func(format string, args ...any)
	onEvicted func(ev Eviction)
}

func New(cfg Config, ledger *Ledger) *Pool {
	if cfg.GlobalSlots <= 0 {
		cfg = DefaultConfig()
	}
	if cfg.PriceBumpPct == 0 {
		cfg.PriceBumpPct = DefaultPriceBumpPct
	}
	return &Pool{
		cfg:      cfg,
		ledger:   ledger,
		accounts: make(map[Address]*accountPool),
		byHash:   make(map[[32]byte]Address),
		logf:     func(string, ...any) {},
	}
}

// SetLogger 注入日志函数（判定输入与依据会通过它输出）。
func (p *Pool) SetLogger(f func(format string, args ...any)) {
	if f != nil {
		p.logf = f
	}
}

// SetEvictionHook 注入驱逐回调。
func (p *Pool) SetEvictionHook(f func(Eviction)) { p.onEvicted = f }

// Submit 校验并提交交易入池。任何拒绝都以 *RejectError 返回，
// 调用方可用 errors.Is(err, txpool.ErrXxx) 区分原因。
func (p *Pool) Submit(tx *Transaction) error {
	if tx == nil {
		return newReject(CodeInvalidFormat, ErrInvalidFormat, "nil transaction", nil)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.submitLocked(tx)
}

// submitLocked 执行校验、替换判定与容量驱逐。重组回池也走这条路径，
// 因此回池交易与正常提交接受完全相同的 nonce、余额、格式与替换校验，
// 不会因为是"系统放回"而绕过任何规则。
func (p *Pool) submitLocked(tx *Transaction) error {
	baseFee := p.ledger.HeadBaseFee()
	h := tx.Hash()

	// 1) 格式合法性。
	if detail, ok := validateFormat(tx); !ok {
		p.logf("submit reject from=%x nonce=%d: INVALID_FORMAT (%s)", tx.From[:4], tx.Nonce, detail)
		return newReject(CodeInvalidFormat, ErrInvalidFormat, detail, tx)
	}

	stateNonce := p.ledger.Nonce(tx.From)
	balance := p.ledger.Balance(tx.From)
	ap := p.accounts[tx.From]
	if ap == nil {
		ap = newAccountPool(stateNonce)
	} else {
		ap.stateNonce = stateNonce // 状态可能因区块提交已推进
	}

	// 2) nonce 顺序。
	if tx.Nonce < stateNonce {
		detail := formatDetail("nonce=%d state_nonce=%d", tx.Nonce, stateNonce)
		p.logf("submit reject from=%x nonce=%d: NONCE_TOO_LOW (%s)", tx.From[:4], tx.Nonce, detail)
		return newReject(CodeNonceTooLow, ErrNonceTooLow, detail, tx)
	}
	if tx.Nonce > stateNonce+p.cfg.MaxNonceGap {
		detail := formatDetail("nonce=%d state_nonce=%d max_gap=%d", tx.Nonce, stateNonce, p.cfg.MaxNonceGap)
		p.logf("submit reject from=%x nonce=%d: NONCE_GAP (%s)", tx.From[:4], tx.Nonce, detail)
		return newReject(CodeNonceGap, ErrNonceGap, detail, tx)
	}

	// 3) 余额约束：须覆盖本 nonce 及之前所有在池交易的预付成本
	//    （采用 feeCap 的最坏情况预估，打包时按实际价格扣款）。
	cost, _ := tx.UpfrontCost()
	need := cost
	for _, other := range ap.txs {
		if other.Nonce < tx.Nonce {
			c, ok := other.UpfrontCost()
			if ok {
				if s, ok := saturatingAdd(need, c); ok {
					need = s
				} else {
					need = ^uint64(0)
					break
				}
			}
		}
	}
	if balance < need {
		detail := formatDetail("balance=%d need_upfront=%d (this_tx=%d)", balance, need, cost)
		p.logf("submit reject from=%x nonce=%d: INSUFFICIENT_BALANCE (%s)", tx.From[:4], tx.Nonce, detail)
		return newReject(CodeInsufficientBalance, ErrInsufficientBalance, detail, tx)
	}

	// 4) 同账户同 nonce 替换。
	if old, exists := ap.get(tx.Nonce); exists {
		if old.Hash() == h {
			p.logf("submit reject from=%x nonce=%d: ALREADY_KNOWN", tx.From[:4], tx.Nonce)
			return newReject(CodeAlreadyKnown, ErrAlreadyKnown, "", tx)
		}
		if !meetsReplacement(old.GasFeeCap, old.GasTipCap, tx.GasFeeCap, tx.GasTipCap, baseFee, p.cfg.PriceBumpPct) {
			needFee := bumped(old.GasFeeCap, p.cfg.PriceBumpPct)
			needTip := bumped(effectiveTip(old.GasTipCap, old.GasFeeCap, baseFee), p.cfg.PriceBumpPct)
			detail := formatDetail("old feeCap=%d tip=%d -> need feeCap>=%d effectiveTip>=%d; got feeCap=%d effectiveTip=%d (bump=%d%%)",
				old.GasFeeCap, effectiveTip(old.GasTipCap, old.GasFeeCap, baseFee),
				needFee, needTip, tx.GasFeeCap,
				effectiveTip(tx.GasTipCap, tx.GasFeeCap, baseFee), p.cfg.PriceBumpPct)
			p.logf("submit reject from=%x nonce=%d: REPLACE_UNDERPRICED (%s)", tx.From[:4], tx.Nonce, detail)
			return newReject(CodeReplaceUnderpriced, ErrReplaceUnderpriced, detail, tx)
		}
		p.insertLocked(ap, tx)
		p.byHash[h] = tx.From
		delete(p.byHash, old.Hash())
		p.logf("submit accept from=%x nonce=%d: REPLACED old(feeCap=%d tip=%d) new(feeCap=%d tip=%d)",
			tx.From[:4], tx.Nonce, old.GasFeeCap, old.GasTipCap, tx.GasFeeCap, tx.GasTipCap)
		return nil
	}

	// 5) 容量管理：需要时按确定性规则驱逐低费交易。
	var evictions []*Transaction
	accountFull := ap.len() >= p.cfg.AccountSlots
	globalFull := p.size >= p.cfg.GlobalSlots
	if accountFull || globalFull {
		if accountFull {
			worst := p.worstInAccountLocked(tx.From)
			if worst == nil || tipOf(tx, baseFee) <= tipOf(worst, baseFee) {
				detail := formatDetail("account_slots=%d worst_eff_tip=%d", p.cfg.AccountSlots,
					tipOf(worst, baseFee))
				p.logf("submit reject from=%x nonce=%d: POOL_FULL (account cap; %s)", tx.From[:4], tx.Nonce, detail)
				return newReject(CodePoolFull, ErrPoolFull, detail, tx)
			}
			evictions = append(evictions, worst)
		}
		if globalFull {
			worst := p.worstGlobalLocked(excludeSet(evictions))
			if worst == nil || tipOf(tx, baseFee) <= tipOf(worst, baseFee) {
				detail := formatDetail("global_slots=%d worst_eff_tip=%d incoming_eff_tip=%d",
					p.cfg.GlobalSlots, tipOf(worst, baseFee),
					effectiveTip(tx.GasTipCap, tx.GasFeeCap, baseFee))
				p.logf("submit reject from=%x nonce=%d: POOL_FULL (global cap; %s)", tx.From[:4], tx.Nonce, detail)
				// 回滚已在本轮选择的账户驱逐（尚未真正移除）。
				_ = evictions
				return newReject(CodePoolFull, ErrPoolFull, detail, tx)
			}
			if !containsTx(evictions, worst) {
				evictions = append(evictions, worst)
			}
		}
	}

	for _, vic := range evictions {
		p.evictLocked(vic, tx, capacityReason(vic, tx, baseFee))
	}
	p.insertLocked(ap, tx)
	p.byHash[h] = tx.From
	p.logf("submit accept from=%x nonce=%d feeCap=%d tip=%d evicted=%d",
		tx.From[:4], tx.Nonce, tx.GasFeeCap, tx.GasTipCap, len(evictions))
	return nil
}

// insertLocked 完成入池：分配序号、登记账户与容量。
func (p *Pool) insertLocked(ap *accountPool, tx *Transaction) {
	if _, exists := ap.get(tx.Nonce); !exists {
		p.size++
	}
	tx.seq = p.seq
	p.seq++
	tx.submitTime = time.Now()
	ap.add(tx)
	p.accounts[tx.From] = ap
}

func (p *Pool) evictLocked(vic, incoming *Transaction, reason string) {
	ap := p.accounts[vic.From]
	if ap == nil {
		return
	}
	ap.remove(vic.Nonce)
	p.size--
	delete(p.byHash, vic.Hash())
	if ap.len() == 0 {
		delete(p.accounts, vic.From)
	}
	p.logf("evict tx from=%x nonce=%d eff_tip=%d: %s (incoming from=%x nonce=%d)",
		vic.From[:4], vic.Nonce, tipOf(vic, p.ledger.HeadBaseFee()), reason,
		incoming.From[:4], incoming.Nonce)
	if p.onEvicted != nil {
		p.onEvicted(Eviction{Tx: vic, Replaced: incoming, Reason: reason})
	}
}

// worstGlobalLocked 返回全池费用优先级最低的交易。
func (p *Pool) worstGlobalLocked(exclude map[*Transaction]struct{}) *Transaction {
	var worst *Transaction
	baseFee := p.ledger.HeadBaseFee()
	for _, ap := range p.accounts {
		for _, tx := range ap.txs {
			if exclude != nil {
				if _, skip := exclude[tx]; skip {
					continue
				}
			}
			if worst == nil || compareTx(worst, tx, baseFee) < 0 {
				worst = tx
			}
		}
	}
	return worst
}

func (p *Pool) worstInAccountLocked(addr Address) *Transaction {
	ap := p.accounts[addr]
	if ap == nil {
		return nil
	}
	var worst *Transaction
	baseFee := p.ledger.HeadBaseFee()
	for _, tx := range ap.txs {
		if worst == nil || compareTx(worst, tx, baseFee) < 0 {
			worst = tx
		}
	}
	return worst
}

// Pending 返回当前快照中所有账户的连续可执行交易，按费用优先级全局排序。
func (p *Pool) Pending() []*Transaction {
	p.mu.Lock()
	defer p.mu.Unlock()
	baseFee := p.ledger.HeadBaseFee()
	out := make([]*Transaction, 0, p.size)
	for addr, ap := range p.accounts {
		ap.stateNonce = p.ledger.Nonce(addr)
		bal := p.ledger.Balance(addr)
		out = append(out, ap.executable(bal, baseFee)...)
	}
	sort.Slice(out, func(i, j int) bool { return compareTx(out[i], out[j], baseFee) < 0 })
	return out
}

func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.size
}

// onBlockCommitted 在区块提交成功后推进池状态：
// 移除已打包交易并按账本 nonce 清理过期项，保证池视图与账本一致。
// 调用约定：由持有 Pool 锁的 Packer 调用。
func (p *Pool) onBlockCommitted(b *Block) {
	for _, tx := range b.Txs {
		ap := p.accounts[tx.From]
		if ap == nil {
			continue
		}
		if cur, ok := ap.get(tx.Nonce); ok && cur.Hash() == tx.Hash() {
			ap.remove(tx.Nonce)
			p.size--
			delete(p.byHash, tx.Hash())
		}
	}
	// 以账本为准校正各账户 stateNonce，并清理所有低于链上 nonce 的残留。
	for addr, ap := range p.accounts {
		st := p.ledger.Nonce(addr)
		ap.stateNonce = st
		for n, tx := range ap.txs {
			if n < st {
				ap.remove(n)
				p.size--
				delete(p.byHash, tx.Hash())
			}
		}
		if ap.len() == 0 {
			delete(p.accounts, addr)
		}
	}
	p.logf("pool advanced after block #%d: size=%d", b.Number, p.size)
}

// prepareReorgLocked 在账本回退完成后、回池之前同步池视图：
// 以账本 nonce 为准更新各账户链上位置（不删除任何交易，
// 回退的 nonce 对应的交易将由随后的 reorgReaddLocked 放回）。
func (p *Pool) prepareReorgLocked() {
	for addr, ap := range p.accounts {
		ap.stateNonce = p.ledger.Nonce(addr)
		_ = addr
	}
	p.logf("pool prepared for reorg: head_number=%d size=%d", p.ledger.HeadNumber(), p.size)
}

// reorgReadd 将重组中回退的交易重新放回池中（持锁）。
// 返回成功入池数量与未能回池的错误（通常仅在容量不足时出现）。
func (p *Pool) reorgReadd(txs []*Transaction) (readded int, errs []error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reorgReaddLocked(txs)
}

// reorgReaddLocked 的锁内版本，供同样持有 Pool 锁的 Packer 原子调用。
func (p *Pool) reorgReaddLocked(txs []*Transaction) (readded int, errs []error) {
	for _, tx := range txs {
		if err := p.submitLocked(tx); err != nil {
			errs = append(errs, err)
			continue
		}
		readded++
	}
	p.logf("reorg readd: %d back in pool, %d rejected, size=%d", readded, len(errs), p.size)
	return readded, errs
}

func validateFormat(tx *Transaction) (string, bool) {
	if tx.GasLimit == 0 {
		return "gasLimit must be > 0", false
	}
	if tx.GasTipCap > tx.GasFeeCap {
		return formatDetail("gasTipCap=%d greater than gasFeeCap=%d", tx.GasTipCap, tx.GasFeeCap), false
	}
	if _, ok := tx.UpfrontCost(); !ok {
		return "upfront cost overflows uint64", false
	}
	return "", true
}

func tipOf(tx *Transaction, baseFee uint64) uint64 {
	if tx == nil {
		return 0
	}
	return effectiveTip(tx.GasTipCap, tx.GasFeeCap, baseFee)
}

func excludeSet(txs []*Transaction) map[*Transaction]struct{} {
	if len(txs) == 0 {
		return nil
	}
	m := make(map[*Transaction]struct{}, len(txs))
	for _, t := range txs {
		m[t] = struct{}{}
	}
	return m
}

func containsTx(txs []*Transaction, target *Transaction) bool {
	for _, t := range txs {
		if t == target {
			return true
		}
	}
	return false
}

func capacityReason(vic, incoming *Transaction, baseFee uint64) string {
	return formatDetail("capacity full; evict lower fee: victim eff_tip=%d <= incoming eff_tip=%d",
		tipOf(vic, baseFee), tipOf(incoming, baseFee))
}
