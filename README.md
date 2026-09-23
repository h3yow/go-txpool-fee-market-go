# go-txpool-fee-market-go

线程安全的交易池 + EIP-1559 风格费用市场 + 原子区块打包与链重组（reorg）回退的参考实现。
目标：让交易在**提交、替换、打包、链回退**四个入口下得到一致、可解释、不会静默丢失的结论。

无外部依赖，标准库实现（`go 1.25`）。

## 目录结构

| 文件 | 职责 |
| --- | --- |
| `types.go` | 交易、区块类型；哈希（sha256）与溢出安全的成本运算 |
| `errors.go` | 可区分的拒绝原因码 `RejectCode`、哨兵错误、`errors.Is` 归类 |
| `fees.go` | 有效小费计算、10% 替换门槛、确定性全序比较 |
| `account.go` | 单账户按 nonce 索引的交易集合与连续可执行链 |
| `pool.go` | 线程安全交易池：校验、替换、容量驱逐、待打包视图 |
| `ledger.go` | 链上账本：余额/nonce、区块原子入账、按区块整体回滚 |
| `packer.go` | 费用优先堆打包；重组回退并把交易原子送回池中 |

## 费用模型

采用 EIP-1559 双价模型：

- `GasFeeCap`：每单位 gas 愿意支付的**最高**总价（base fee + tip 之和的上限）。
- `GasTipCap`：每单位 gas 愿意支付的**最高小费**。
- 链上有 `baseFee`（本实现为配置常量，`NewLedger(baseFee)`）。

给定区块的 baseFee：

```
effectiveTip   = min(GasTipCap, GasFeeCap-baseFee)   // GasFeeCap <= baseFee 时为 0
effectivePrice = baseFee + effectiveTip
实际扣减       = value + GasLimit * effectivePrice
入池预付预估   = value + GasLimit * GasFeeCap         // 最坏情况，偏保守
```

**为什么入池按 feeCap 预估、打包按实际价格扣减**：余额在入池后、打包前可能变化。
入池用最坏情况预估可保证"只要 baseFee 与余额不恶化，已入池交易一定可执行"；
打包时再以实际价格逐笔扣减，少扣的部分留在账户余额中。
若打包前余额被外部交易路径减少，费用市场会在打包时按可执行链截断（见下），
交易**不会被静默丢弃**，而是留在池中等待。

### 打包排序

所有账户从链上 nonce 起、余额可连续覆盖的交易进入费用优先小顶堆，
排序键 `compareTx` 是一个**确定性全序**：

1. `effectiveTip` 降序（高小费优先）；
2. 到达序号 `seq` 升序（同小费先到先得）；
3. `From` 地址升序；
4. `Nonce` 升序。

弹出堆顶后，只有 gas 仍可容纳时才入选，并把**同一账户的下一笔 nonce** 推入堆。
因此同账户交易在区块内严格 nonce 连续；某笔因 gas 不足被跳过时，
该账户后续 nonce 一并跳过（不允许跨越形成 nonce 空洞），其他账户不受影响。

## 提交校验与拒绝原因

`Pool.Submit` 的失败结果永远是 `*RejectError`，可用 `errors.Is(err, txpool.ErrXxx)` 归类，
错误文本同时携带判定输入（nonce、余额、门槛值等）：

| 原因码 | 触发条件 |
| --- | --- |
| `invalid_format` | `GasLimit==0`、`GasTipCap>GasFeeCap`、预付成本溢出 uint64 |
| `nonce_too_low` | `nonce < 链上已执行 nonce`（重放/过期交易） |
| `nonce_gap` | `nonce > 链上nonce + MaxNonceGap`（跳得太远） |
| `insufficient_balance` | 余额不足以覆盖本交易及此前同账户排队交易的预付成本总和 |
| `replace_underpriced` | 同账户同 nonce 替换但未满足费用提升规则 |
| `already_known` | 字节完全相同的交易已在池中 |
| `pool_full` | 池满且新交易无法驱逐任何更低费交易 |

> nonce 恰好领先链上 nonce 一步（如链上 0、提交 1）是允许的：交易先排队，
> 前序 nonce 落地后自动成为可执行交易，不产生空洞。

## 替换规则（同账户同 nonce）

替换交易必须在**两个维度都**相对旧交易严格提升 `PriceBumpPct`（默认 10%，向上取整）：

```
newFeeCap      >= oldFeeCap      * (100+bump)/100
newEffectiveTip>= oldEffectiveTip* (100+bump)/100
```

两个维度同时约束，防止"抬高 feeCap 却压低有效小费"之类的取巧替换。
注意费用市场的真实语义：**挡在池中的始终是当前持有者的报价**——
后到交易即使绝对价格更高，只要相对当前持有者涨幅不足 10% 也会被拒
（与以太坊 mempool 行为一致）。

## 容量与驱逐

`Config.GlobalSlots`（全局笔数上限）与 `Config.AccountSlots`（单账户笔数上限）
任一触顶时，新交易必须**严格更优**（更高有效小费）才能入池：

1. 单账户触顶：新交易有效小费必须 > 该账户现有最低有效小费，否则 `pool_full`；
2. 全局触顶：新交易有效小费必须 > 全池最低有效小费，否则 `pool_full`；
3. 同小费一律不驱逐（严格大于），配合全序比较使驱逐结果完全确定；
4. 被驱逐交易通过 `Pool.SetEvictionHook` 回调暴露，日志打印 victim 与 incoming 的费用依据。

## 并发一致性与链重组

**锁顺序**：全局固定为 `Pool.mu -> Ledger.mu`（不允许反向获取）。
`Packer.Build` 在持有 `Pool` 锁的整个区间内完成选交易 → 账本原子入账 → 池状态推进，
因此并发提交/打包下：

- 同一交易不可能进入两个区块（打包成功才从池移除）；
- 余额不会重复扣减（账本入账带逐笔 nonce 与余额校验，失败整体回滚）；
- 不会出现 nonce 空洞（账本要求块内 nonce 与账户当前 nonce 严格连续）。

`Ledger.applyBlock` 先在影子状态上执行全部交易，任一笔失败则整块不落账。

**重组 `Packer.ReorgTo(targetNumber)`** 同样在 `Pool` 锁内原子完成：

1. 从链头逐块 `revertCommit`：恢复每笔余额扣减，nonce 恢复为该账户在区块执行前的值；
2. 所有区块回退完成后，把池中各账户 `stateNonce` 同步到回退后的账本视图；
3. 回退区块中的原始交易走与正常提交**完全相同**的校验路径重新入池
   （格式/nonce/余额/替换规则一致；池满时按驱逐规则处理，失败显式返回而非丢失）；
4. 回退交易若在链外已被一笔合规的更高费交易替换，则池中保留替换者，无重复。

回退期间池视图与账本视图对外不暴露中间状态。

## 验证方法

```bash
go test ./... -race -v          # 全量用例 + 竞态检测，日志打印输入与判定依据
go vet ./...                    # 静态检查
gofmt -l .                      # 格式检查（无输出即通过）
```

测试覆盖（每个用例的日志均打印输入字段与判定依据）：

| 用例 | 覆盖点 |
| --- | --- |
| `TestSubmitValidation` | 五类拒绝原因的正确归类与可区分性 |
| `TestSubmitAcceptsQueuedNonce` | nonce 排队、补齐后连续打包 |
| `TestReplacementBump` | 10% 双维度替换门槛 |
| `TestEvictionOnGlobalFull` | 满载确定性驱逐最低费交易 + 低费被拒 |
| `TestEvictionTieBreak` | 同费不驱逐的确定性 |
| `TestFeeOrdering` | 有效小费决定跨账户打包顺序 |
| `TestPackAndCharge` | 实际价格扣减、nonce 推进、旧 nonce 重放拒绝 |
| `TestPackInsufficientAtExecution` | 外部余额变化后的可执行链截断，不产生空洞 |
| `TestGasLimitTruncation` | gas 不足跳过并记录 `SkipReason`，其他账户继续 |
| `TestReorgRewindsTxsAndBalance` | 回退后交易回池、余额/nonce 恢复、重打无重复扣减 |
| `TestReorgWithReplacement` | 回退交易已被高费替换的场景 |
| `TestMultiBlockReorg` | 跨多个区块的部分回退 |
| `TestConcurrentSubmitAndPack` | 8 账户 × 40 笔并发提交 + 并发打包：无重复打包、无重复扣减、无 nonce 空洞 |
| `TestConcurrentReplacements` | 同 nonce 并发替换：唯一胜者且确定为最高合规报价 |

## 最小使用示例

```go
ledger := txpool.NewLedger(100)                       // baseFee=100
ledger.SetBalance(alice, 1_000_000)
pool := txpool.New(txpool.DefaultConfig(), ledger)
packer := txpool.NewPacker(pool, ledger)

err := pool.Submit(&txpool.Transaction{
    From: alice, Nonce: 0, GasLimit: 21000,
    GasFeeCap: 200, GasTipCap: 10,
})
var rej *txpool.RejectError
if errors.As(err, &rej) {
    switch {
    case errors.Is(err, txpool.ErrReplaceUnderpriced): // ...
    case errors.Is(err, txpool.ErrInsufficientBalance): // ...
    }
}

block, skips, err := packer.Build(30_000_000)          // 打包并原子提交
recovered, err := packer.ReorgTo(block.Number - 1)    // 重组回退
```
