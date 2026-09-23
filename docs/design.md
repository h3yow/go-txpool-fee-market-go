# 交易池与费用市场设计文档

本仓库实现了一套交易池与费用市场驱动的区块打包流程，覆盖交易提交、校验、替换、
打包与链回退等入口，保证在并发与链状态变化下**一致且可解释**。

## 1. 费用模型

采用 EIP-1559 风格的两档费用：

- `GasFeeCap`（fee cap，wei/gas）：发送方愿意支付的最高单价；
- `GasTipCap`（tip，wei/gas）：发送方支付给区块生产者的优先费。

给定区块基础费 `baseFee`，交易的**有效 tip** 为：

```
effectiveTip = min(GasTipCap, GasFeeCap - baseFee)   (非负)
```

有效 gas 价格为 `baseFee + effectiveTip`，且不超过 `GasFeeCap`。

**打包排序**：候选交易按 `effectiveTip` 降序排列；同一账户的交易始终保持
nonce 升序（见 §3），因此跨账户合并时，每个账户的交易在排序序列中都是
nonce 连续的。

## 2. 交易提交与校验

`pool.Submit` 对每笔交易做如下检查，拒绝时返回带 `Reason` 的 `*reject.Decision`：

| 原因 | 条件 |
|---|---|
| `MALFORMED` | 交易为 nil、gas 为 0、value/feeCap/tipCap 为 nil 或负数、tipCap > feeCap |
| `NONCE_TOO_LOW` | nonce 低于账本 on-chain nonce，或已被同账户 pending/已提交交易占用 |
| `NONCE_TOO_HIGH` | nonce 超过账户的下一个期望 nonce（存在空洞） |
| `INSUFFICIENT_FUNDS` | 余额 < `value + GasFeeCap * gas`（提交时按最大可能费用检查） |
| `UNDERPRICED` | 替换交易未达费用提升幅度，或池满时出价低于最便宜尾交易 |
| `POOL_FULL` | 全局/每账户容量已满且无法驱逐 |
| `ALREADY_RESERVED` | 该 nonce 已被打包进进行中的区块 |

所有拒绝原因都可通过 `reject.AsDecision(err)` 取出，测试日志中同时打印
**输入（nonce/tip/cap/gas）与判定依据（reason + detail）**，例如：

```
REJECT  nonce=0  tip=5   cap=21  gas=21000 reason=UNDERPRICED | replacement at nonce 0 needs effective tip >= 5 (got 5) and feeCap >= 22 (got 21), bump 10%
```

## 3. 替换规则（same account, same nonce）

同一账户同一 nonce 的替换必须**同时**满足：

- 有效 tip 提升 ≥ `PriceBumpPct`（默认 10%）：
  `newEffectiveTip >= floor(oldEffectiveTip * (100+pct) / 100)`；
- fee cap 同样提升 ≥ 10%：
  `newGasFeeCap >= floor(oldGasFeeCap * (100+pct) / 100)`。

替换是**尺寸中性**的：池内交易数量不变，旧交易被新交易取代。

## 4. 容量管理与驱逐

- 全局容量 `GlobalSlots`（默认 4096）与每账户容量 `AccountSlots`（默认 16）。
- 每账户容量已满时，新交易以 `POOL_FULL` 拒绝。
- 全局容量已满时，按如下规则驱逐：
  1. 候选受害者是每个账户**最高 nonce 的 pending 交易**（即"尾交易"）。
     驱逐尾交易保证账户内剩余交易仍是 nonce 连续的，不会产生空洞；
  2. 受害者按有效 tip 从小到大排序，最低者被选中；
  3. 只有当新交易的有效 tip **严格高于**受害者时才允许驱逐，否则以
     `UNDERPRICED` 拒绝；
  4. 驱逐时会同时移除该受害者及之后的整个 suffix（它们已无法保持连续）。
- 确定性 tie-break：tip 相同时按发送方地址、nonce 排序，确保结果可复现。

## 5. 打包流程

`packer.Produce` 串行执行以下步骤（`Packer.mu` 临界区）：

1. 从 `pool.Pending(baseFee)` 取候选：每个账户从 on-chain nonce + 已保留数量
   起的连续 nonce 前缀，再按有效 tip 降序合并；
2. 区块 gas 限制（默认 10,000,000）：一旦某账户的头交易无法容纳，**整个账户
   跳过本区块**（`GAS_LIMIT`），但不影响其他账户；
3. `pool.Reserve`：选中交易从 pending 视图移入"已保留"，pending 计数递减，
   防止重复打包；
4. `ledger.Apply` 原子提交：按顺序校验每笔交易的 nonce 与余额，通过则扣除
   `value + effectiveTip * gas`、推进 nonce、记录 undo 快照；
5. 成功：`pool.Confirm` 清理保留；失败：`pool.Unreserve` 把交易退回 pending，
   账本无任何状态变化（§7）。

## 6. 并发与一致性

- 提交、打包、回退分别由 `pool.mu`、`packer.mu`、`ledger.mu` 串行化，
  且打包流程在单一临界区内完成"选择 → 保留 → 提交 → 确认/回滚"，
  不存在重复打包、余额重复扣减或 nonce 空洞的窗口。
- **提交与打包的瞬态一致**：在 `ledger.Apply` 推进了 on-chain nonce 但
  `pool.Confirm` 尚未清理保留交易的窗口内，`nextExpectedLocked` 与 `Pending`
  从 on-chain nonce 起逐个扫描保留位，自动忽略低于 on-chain nonce 的 stale
  reserved，因此提交方不会误判为"nonce 已占用"。
- 测试 `TestConcurrentSubmitAndProduce`（8 发送方 × 40 nonce，并发提交 +
  持续打包）验证了：每笔交易只被打包一次、nonce 集合连续、余额扣除与
  已提交交易的实际花费完全相等、池与账本最终一致。

## 7. 链回退（reorg）处理

`ledger.Apply` 为每个区块记录 `BlockSnapshot`（账户 before-image 与交易列表）。

- `packer.Reorg(targetHead)`：
  1. `ledger.Revert` 逐个回滚区块（ newest-first ），把账户 nonce/余额恢复到
     目标高度的 before-image；
  2. 被回滚区块中的交易以**最老区块优先、区块内原始顺序**重新提交到 pool，
     保证 nonce 0,1,2… 依次入池、不产生空洞；
  3. 重新校验失败的交易（例如余额已不足）以 `*reject.Decision` 形式返回，
     原因可区分（`INSUFFICIENT_FUNDS` 等），不会被静默丢弃。
- 对于分叉后规范链状态与本地快照不同的场景，使用两阶段接口
  `Rollback` → 叠加规范链状态（`ledger.OverlayAccount`）→ `Revalidate`。
- 测试 `TestConcurrentReorgSafety` 验证了 5 轮 produce/reorg/repack 循环后
  nonce 与余额的精确收敛。

## 8. 验证方法

```bash
go build ./...          # 编译
go vet ./...            # 静态检查
go test ./...           # 全部测试
go test -race -count=3 ./...   # 竞态检测（重复 3 次）
```

测试覆盖：

- 提交校验与原因归类（malformed / nonce / balance）；
- 替换费用提升规则（tip 与 fee cap 双 10% 检查）；
- 满载驱逐（最便宜尾交易、严格 outbid、suffix 连续）；
- 每账户容量上限；
- 费用排序与同一账户 nonce 顺序；
- 账本原子提交与失败回滚；
- 多区块回滚与 before-image 恢复；
- 区块 gas 限制与账户链整体延迟；
- 并发提交/打包的重复打包、重复扣减、nonce 空洞不变量；
- 并发 produce/reorg/repack 循环收敛。
