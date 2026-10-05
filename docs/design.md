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
| `INSUFFICIENT_FUNDS` | **单笔**口径：余额 < `value + GasFeeCap * gas`（该交易单独就付不起） |
| `ACCOUNT_BUDGET` | **账户整体**口径：该笔交易单独付得起，但把它计入后，账户当前排队队列的最大花费合计超过余额；或打包时余额变小导致该 nonce 起的后缀无法执行而被推迟 |
| `UNDERPRICED` | 替换交易未达费用提升幅度，或池满时出价低于最便宜尾交易 |
| `POOL_FULL` | 全局/每账户容量已满且无法驱逐 |
| `ALREADY_RESERVED` | 该 nonce 已被打包进进行中的区块 |
| `GAS_LIMIT` | 区块 gas 容量放不下该账户的头交易，整条 nonce 链本区块推迟 |

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

## 4. 账户级预算准入（account-level budget）

### 4.1 两种余额口径的区别

同一账户连发多笔 nonce 递增的交易时（批量转账、钱包批量发起），仅做单笔
检查会出现"每笔单独都收得下、合起来根本付不起"的坑：前面收一堆、后面照收，
一到打包就推进不动，还会拖住同一区块里的其他账户。为此准入有两层独立判断：

| 口径 | 计算 | 拒绝原因 | 含义 |
|---|---|---|---|
| 单笔 | `value + GasFeeCap * gas` 与当前余额比较 | `INSUFFICIENT_FUNDS` | 这笔交易**自己**就付不起 |
| 账户整体 | **已交给在途批次（Reserved，已打包未确认）的 live 交易**与账户 pending 队列**全部**交易的最大花费合计，与当前余额比较 | `ACCOUNT_BUDGET` | 这笔本身付得起，但**整个账户（在途占用 + 排队合计）**付不起 |

两层都按**最大可能费用**（`GasFeeCap*gas`）保守计算，先做单笔、再做合计，
因此两类原因互不重叠、一眼可区分。例如余额 1000、5 笔各需最大花费 200 的
交易：前 5 笔合计正好 1000 全部收下；第 6 笔单笔只需 200（< 1000），但合计
变为 1200 > 1000，以 `ACCOUNT_BUDGET` 拒绝，池内仍是前 5 笔。

**为什么在途批次也要计入（本次修复的核心）**：一批交易被 `Reserve`（交给
正在出的块）之后、`ledger.Apply` 真正扣款之前存在一个窗口——账本余额账面
还没被扣。若此时同一账户继续连发，仅按 pending 合计准入会按"余额还够"把
后续交易全收下，可前面那批一旦确认扣款，账户前后合计早已超余额，后面收下
的就打不了包（"收下却打不了包"）。因此账户级预算必须按账户**一起算总账**：

```
账户整体最大预算 = Σ live reserved 最大花费 + Σ pending 最大花费（含本次提交）
准入要求         : 账户整体最大预算 <= 当前账本余额
```

- 只计 **live** 预留：预留 nonce 低于 on-chain nonce 的属于**陈旧预留**
  （账本已经提交并扣过款，只是 `Confirm` 还没跑），必须跳过，否则同一笔钱
  被算两次。确认后（nonce 推进、预留清理）该部分自动不再计入。
- **替换交易**同样过账户整体预算（更贵的替换可能把队列顶过余额上限），
  合计时用新交易花费替换掉旧交易花费；拒绝时旧交易保留、池尺寸不变。
- 被 `ACCOUNT_BUDGET` 拒绝的交易**不进池、不触发驱逐**；预算按账户独立，
  一个账户超额不牵连其他账户；余额补上后同一笔交易（hash 不变）正常入池，
  准入不做任何缓存或熔断。

### 4.2 补足余额后重新入池

准入只看**提交那一刻**的余额（含当刻 live 在途占用），不做任何缓存/熔断。
两种路径都会自然恢复，无需重新提交已在池里的前缀：

- 在途批次确认后，账本真正扣款、nonce 推进、预留清理，账户整体预算自动
  让出这部分占用；
- 账户充值后（`AddBalance` 等），此前被 `ACCOUNT_BUDGET` 拒绝的同一笔交易
  （hash 相同、字段相同）再次提交会正常入池；已经在池里的前缀无需任何操作，
  下一次打包自然能推进。

### 4.3 账户排队视图 `pool.AccountView(addr)`

提供一个直接回答"这个账户现在排队到底什么情况"的快照入口（池锁内取值、
返回的 `big.Int` 均为拷贝，可在并发提交/打包时安全读取）：

- `OnchainNonce` / `Balance` / `BaseFee`：判定时的权威账户状态与基准费；
- `Pending`：按 nonce 升序的排队交易，每笔带：
  - `Status`：`READY`（连续且余额覆盖，马上能打包）、
    `DEFERRED_BUDGET`（在模拟余额用尽点之后，等充值）、
    `DEFERRED_GAP`（nonce 低于链上 nonce 或落在空洞之后）；
  - `Reason`：状态的人类可读说明；
  - `MaxCost` / `CumulativeMaxCost`：该笔最大花费与到此笔为止的 **pending**
    合计预算（不含在途预留）；
- `ReservedNonces`：已进入在途区块的 live nonce；
- `ReservedMaxBudget`：live 在途预留按最大费用的占用金额（准入保守口径），
  即"账面余额看着没扣、但其实已经说出去的钱"；
- `ReservedEffCost`：同批 live 在途预留按有效费用（`value +
  effectiveTip(baseFee) * gas`）的占用，是逐笔可执行前缀模拟的起点；
- `PendingMaxBudget`：pending 交易最大花费合计；
- `MaxBudget`：**账户整体准入口径** = `ReservedMaxBudget + PendingMaxBudget`；
- `BudgetOK`：`MaxBudget <= Balance`，含在途占用的账户整体是否达标；
- `ReadyCount`：当前马上能打包的 pending 笔数——模拟从
  `余额 - ReservedEffCost` 出发逐笔扣有效费用，余额够且 nonce 连续才记
  READY，因此在途批次未确认时"马上能打包的数量"也会如实变少，不会按没扣过
  的余额虚高。

注意视图里有两组**互相独立**的口径，不要混用：`MaxBudget/BudgetOK` 用最大
费用回答"整个账户（在途占用 + 队列）付不付得起（准入）"；逐笔的
`READY/DEFERRED_BUDGET` 用打包时有效费用 `value + effectiveTip*gas` 顺序模拟
（先扣除在途有效占用），回答"在途批次确认后，此刻还能连续执行到哪一笔
（打包）"。base fee 为 0、`tip == cap` 时两者数值一致。视图结论与真正去
`packer.Produce` 时的取舍一致：在途批次确认前，能报 READY 的只有"扣除它将
扣的钱之后"仍连续付得起的那段。

## 5. 容量管理与驱逐

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

## 6. 打包流程

`packer.Produce` 串行执行以下步骤（`Packer.mu` 临界区）：

1. 从 `pool.Pending(baseFee)` 取候选：每个账户从 on-chain nonce + 已保留数量
   起的连续 nonce 前缀，再按有效 tip 降序合并；
2. 区块 gas 限制（默认 10,000,000）：一旦某账户的头交易无法容纳，**整个账户
   跳过本区块**（`GAS_LIMIT`），但不影响其他账户；
3. **按账户的实时余额模拟（扛余额中途变小）**：链上或其他入口可能在入池之后
   把账户余额改小。选择时对每个账户从账本**当前**余额出发，按候选中的 nonce
   顺序逐笔扣除实际花费 `value + effectiveTip(baseFee) * gas`：
   - 扣得动就纳入本区块，并在该账户的模拟余额上继续扣；
   - 遇到第一笔扣不动的，就在此处**截断**：只取前面仍能连续执行的前缀，该笔
     及其后同账户的全部交易以 `ACCOUNT_BUDGET` 记入 `skipped`、**留在池里等
     下一次**，绝不因为这一个账户付不起就整块区块生不出来，更不会卡住其他
     账户（其他账户照常选择与提交）；
   - 由于选中集合在模拟中必然扣得动，随后的 `ledger.Apply` 不会再因余额不足
     失败；推迟的交易没有被 `Reserve`，仍在 pending 视图中；
4. `pool.Reserve`：选中交易从 pending 视图移入"已保留"，pending 计数递减，
   防止重复打包；
5. `ledger.Apply` 原子提交：按顺序校验每笔交易的 nonce 与余额，通过则扣除
   `value + effectiveTip * gas`、推进 nonce、记录 undo 快照；
6. 成功：`pool.Confirm` 清理保留；极端情况下提交仍失败则 `pool.Unreserve`
   把交易退回 pending，账本无任何状态变化（§7、§8）。

**打包后池视图与账本严格对齐**：只有被实际执行的前缀离开 pending 并推进
on-chain nonce，后缀以链上 nonce 为新起点保持连续，因此既不会重复扣钱，也
不会留下 nonce 空档。`Produce` 的返回值把取舍讲清楚：`Block.Txs` 是实际入块
的前缀，`skipped` 中每个 `*reject.Decision` 标明是 `GAS_LIMIT` 还是
`ACCOUNT_BUDGET` 及对应 nonce、模拟余额与所需金额；账户充值后再次 `Produce`
即可把留下的后缀继续打包（无需重新提交）。

## 7. 并发与一致性

- 提交、打包、回退分别由 `pool.mu`、`packer.mu`、`ledger.mu` 串行化，
  且打包流程在单一临界区内完成"选择 → 保留 → 提交 → 确认/回滚"，
  不存在重复打包、余额重复扣减或 nonce 空洞的窗口。
- **提交与打包的瞬态一致**：在 `ledger.Apply` 推进了 on-chain nonce 但
  `pool.Confirm` 尚未清理保留交易的窗口内，`nextExpectedLocked` 与 `Pending`
  从 on-chain nonce 起逐个扫描保留位，自动忽略低于 on-chain nonce 的 stale
  reserved，因此提交方不会误判为"nonce 已占用"。
- **一边出块一边并发提交**：`Reserve` 之后、`Apply` 之前的窗口里，账本余额
  尚未扣除本块将扣的钱。准入与 `AccountView` 在这个窗口仍按
  "live 在途占用 + 排队合计 ≤ 账面余额"把关：付不起整体账的那笔以
  `ACCOUNT_BUDGET` 当场拒绝（不进池、不顶掉已有交易），而不是先收下等本块
  扣款后再打不了包。由于 `Reserve` 持有的池锁会在锁释放后才允许提交、
  `Apply` 又对选中集合做原子校验，同一笔钱不会被扣两次；本块确认后 nonce
  推进、池里剩下的后缀与新 on-chain nonce 严格连续，无空档。测试通过
  `Packer.SetAfterReserveHook`（仅供测试，在 Reserve 与 Apply 之间注入动作）
  确定性地复现该窗口，并叠加并发压测。
- 测试 `TestConcurrentSubmitAndProduce`（8 发送方 × 40 nonce，并发提交 +
  持续打包）验证了：每笔交易只被打包一次、nonce 集合连续、余额扣除与
  已提交交易的实际花费完全相等、池与账本最终一致。
- 测试 `TestConcurrentInFlightBudgetAdmission` 先在确定性窗口内断言窗口提交
  被 `ACCOUNT_BUDGET` 拒且不牵连其他账户，再以 8 发送方 × 40 nonce、每块
  16 笔、余额分批充值的方式并发提交/出块，断言无重复扣款、无 nonce 空档、
  最终余额精确为 0 且 `-race` 干净。

## 8. 链回退（reorg）处理

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

## 9. 验证方法

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
- 并发 produce/reorg/repack 循环收敛；
- **账户级整体预算准入**（`pool/budget_test.go`）：同一账户多笔连发时第 N 笔
  单笔付得起但合计超额被 `ACCOUNT_BUDGET` 拒绝（与单笔 `INSUFFICIENT_FUNDS`
  明确区分）、不影响其他账户、充值后同一笔交易正常入池；以及更贵替换把队列
  顶过预算时被拒且保持尺寸中性；
- **在途批次窗口的整体预算**（`pool/budget_test.go` 的
  `TestAccountBudgetIncludesInFlightReservations`、
  `TestAccountViewCountsInFlightOccupation`）：同一账户的前几笔被 `Reserve`
  进在途批次、账本尚未扣款时连发后续，超出"在途占用 + 排队合计"的那笔以
  `ACCOUNT_BUDGET` 拒绝（区别于单笔 `INSUFFICIENT_FUNDS`）、不进池不顶掉
  已有交易；批次确认后仍超额时继续被拒，充值后同一笔正常入池；视图的
  `ReservedMaxBudget/ReservedEffCost/PendingMaxBudget/MaxBudget/BudgetOK/
  ReadyCount` 与逐笔状态在有 live 预留、预留期间余额变小、确认后三种情形下
  与真实打包取舍一致；
- **出块窗口提交**（`packer/budget_test.go` 的
  `TestSubmitWhileBlockInFlightCountsReservation`）：用
  `SetAfterReserveHook` 确定性卡在 Reserve 与 Apply 之间提交：窗口内超额笔
  以账户级原因被拒、其他账户不受牵连；放行后本块只扣一次、无 nonce 空档，
  第二块打排队后缀，充值后窗口被拒的那笔入池并继续打包；
- **并发出块/提交预算收敛**（`packer/concurrent_test.go` 的
  `TestConcurrentInFlightBudgetAdmission`）：确定性窗口断言 + 8 发送方 × 40
  nonce、每块 16 笔、余额分批充值的并发压测，断言每笔只打一次、无重复扣款、
  无 nonce 空档、最终精确收敛且 `-race` 干净；
- **余额中途变小只打可执行前缀**（`packer/budget_test.go`）：入池后把两个
  账户余额改小，`Produce` 只打包仍连续付得起的前缀，后缀以 `ACCOUNT_BUDGET`
  留在池中，其他账户/整块区块不被卡死；断言账本只扣前缀、nonce 无空档、池
  与账本视图一致、无重复扣减；充值后再次 `Produce` 后缀全部一次性入块；
  另覆盖头交易都付不起的极端账户不阻断出块；
- **账户排队视图**：`pool.AccountView` 的
  `ReservedMaxBudget/ReservedEffCost/PendingMaxBudget/MaxBudget/BudgetOK`、
  逐笔 `READY / DEFERRED_BUDGET / DEFERRED_GAP` 及原因、在途保留 nonce 与
  充值前后的变化。

### 9.1 本地手工验证步骤

不依赖测试框架也能快速自查（`baseFee=0, tip=cap=2, gas=100` 时每笔最大花费 =
有效花费 = 200）：

1. 账户 A 余额 1000，提交 nonce 0,1 后用 packer 的 `Produce`（或直接
   `pool.Reserve`）把它们交给在途批次，此时 `ledger.GetAccount(A).Balance`
   仍显示 1000；
2. 在批次确认前继续提交 nonce 2,3,4（在途 400 + 排队 600 = 1000，全部收下），
   再提交 nonce 5：单笔 200 付得起，但 400 + 800 = 1200 > 1000，必须返回
   `ACCOUNT_BUDGET`，且 `Pool.Stats` 与窗口前一致、其他账户提交不受影响；
3. 调 `pool.AccountView(A)` 应看到 `ReservedMaxBudget=400`、
   `PendingMaxBudget=600`、`MaxBudget=1000`、`BudgetOK=true`、`ReadyCount=3`；
4. 批次确认（`ledger.Apply` + `pool.Confirm`）后余额变 600、nonce 变 2；
   `AddBalance` 补足后重提同一笔 nonce 5 正常入池，再次 `Produce` 后缀
   nonce 2..5 连续入块，余额精确、无重复扣款。
