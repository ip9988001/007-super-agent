# 第 20 步报告：自进化闭环（2026-11-18）

按任务书执行：每天 04:30 自查（精炼 04:00 之后）、每周日 04:30 重建注册表。
只改本地工作区；未碰测试服务器（过夜守护进程还在跑）、未碰二号/三号、
未读/写任何凭证文件；报告与代码里无任何密钥/token/密码原文。
宪法文件、install-toolchain.sh、step1/ 注册表与索引文件均未动
（find 核对：step1/ 与 install-toolchain.sh 今天零改动）。

## 总结果

- 新包 `internal/selfevolve/`：每日自查 + 每周重建 + 影子模式，9 个测试全过。
- 提议表 `selfevolve_proposals` 已进账本 schema（第 7 张表）。
- 注册表加了 entry_point 内存覆盖层（先探针、通过才存），registry.json 文件本身永远不动。
- 沙箱验证脚本 `cmd/selfevolve-verify`：ALL PASS。
- `go vet ./...` 全过；`go test -count=1 -race ./...` 13 个包全绿。

## 改了哪些文件（行数）

| 文件 | 行数 | 改动 |
|---|---|---|
| `internal/selfevolve/selfevolve.go` | 588（新） | 循环、自查、重建、应用分档、标记文件 |
| `internal/selfevolve/review.go` | 164（新） | 提议种类、回复解析、信号文本格式化 |
| `internal/selfevolve/selfevolve_test.go` | 657（新） | 9 个测试 |
| `internal/ledger/ledger.go` | 941（原 547，+394） | 提议表方法、信号聚合、按任务汇总费用 |
| `internal/ledger/schema.sql` | 114（+17） | 新增 `selfevolve_proposals` 表 + 2 个索引 |
| `internal/registry/registry.go` | 503（+约 60） | entry_point 覆盖层、ApplyEntryPointFix、UnmarkVerified、Entry() 返回带覆盖的拷贝、Probe() 探生效入口 |
| `internal/registry/registry_test.go` | +约 70 | 覆盖层测试 + UnmarkVerified 测试 |
| `internal/llm/llm.go` | 513（+约 37） | 冻结自查提示词 + ReviewSelfEvolve 方法 |
| `internal/llm/llm_test.go` | +约 40 | ReviewSelfEvolve 测试 |
| `cmd/super-agent/main.go` | 362（+约 14） | 接线 SelfEvolveLoop + 启动日志 |
| `cmd/selfevolve-verify/main.go` | 214（新） | 沙箱验证脚本 |

## 阈值与分档规则的实现位置

| 规则 | 位置 |
|---|---|
| 同一签名 7 天内 ≥3 次才提议（`ReviewThreshold = 3`） | `internal/selfevolve/selfevolve.go:25`（常量），`:271`（校验处，不足直接丢弃并记日志） |
| 信号窗口 7 天（`ReviewWindowDays = 7`） | `internal/selfevolve/selfevolve.go:27` |
| 自查窗口每天 04:30–04:45、重建周日同一窗口 | `internal/selfevolve/selfevolve.go:101`（`shouldReview`）、`:110`（`shouldRebuild`）、`:137/:146`（循环内检查 + 标记文件 `selfevolve_last` / `selfevolve_rebuild_last`） |
| 信号聚合（5 类计数 + 费用突增） | `internal/ledger/ledger.go:770`（`AggregateSignals`）；签名格式 `kind:tool_id`，见 `ledger.go` 的 `Signature()`；费用突增按天计（`ExpenseSpikeDays`，7 天内突增天数，见下） |
| 同一签名 7 天内去重（只提一次） | `internal/selfevolve/selfevolve.go:275`（`FindProposalBySignature`，`ledger.go:603`） |
| 自动应用档：只有 `registry_entrypoint_fix` 与 `registry_verified` | `internal/selfevolve/review.go:41`（`autoApplicableKind`）；应用入口 `internal/selfevolve/selfevolve.go:328`（`applyProposal`） |
| 宪法/分诊提示词/代码/其他只提议不应用（pending） | 同上分流；宪法档 `KindConstitution`（`review.go:33`），代码里没有任何自动改宪法的路径 |
| entry_point 修正先探针、通过才存内存覆盖层 | `internal/registry/registry.go:285`（`ApplyEntryPointFix`）；覆盖层只活在内存，`Entry()` 返回带覆盖的拷贝（`registry.go` 的 `Entry()`），`Probe()` 探生效入口 |
| 重建只更新 verified 内存覆盖层，不写 registry.json | `internal/selfevolve/selfevolve.go:445`（`RebuildWeekly`）；失败撤标记 `registry.go:275`（`UnmarkVerified`） |
| 每次模型调用写 expense_ledger，task_id=`selfevolve-<日期>` | `internal/llm/llm.go:492`（`ReviewSelfEvolve`，经 `chatWithTask` 记账）；当日汇总行 `internal/selfevolve/selfevolve.go:346`（`writeDailySummary`，model 标 `selfevolve-summary`，见 `:40`） |
| 影子模式只记“本应自查”，不执行不写账本 | `internal/selfevolve/selfevolve.go` 的 `maybeRun` 影子分支 + `logShadowOnce`（同一日期同一事项只记一次）；接线 `cmd/super-agent/main.go:312-316`，启动日志行与任务书要求一字不差：`self-evolve loop started: daily 04:30 Beijing, rebuild Sunday 04:30` |
| 自查提示词冻结在代码里 | `internal/llm/llm.go:476`（`reviewSelfEvolvePrompt`），输入上限 `MaxReviewRunes = 8000`（`llm.go:486`） |

## 测试结果（逐项）

`go test -count=1 -race ./...`：13 个包全绿。`internal/selfevolve` 9 个测试：

| 测试 | 结果 | 覆盖 |
|---|---|---|
| TestReviewThreshold | PASS | 2 次不提议（只记教训、提议表 0 行）、补到 3 次产生 1 条提议并自动应用 |
| TestProposalWriteAndStatusFlow | PASS | 提议入库 3 条（1 applied + 2 pending）；低于阈值的与不在信号里的被丢弃；同一签名 7 天内去重（第二轮 0 新提议）；自动应用只活内存覆盖层、Wiki 教训落盘 |
| TestExpenseTaskIDFormat | PASS | 费用行 2 条（调用行 deepseek-flash 7 毫分 + 汇总行 selfevolve-summary 7 毫分）；二次汇总仍是 7，不 double count |
| TestShadowDoesNotWrite | PASS | 影子模式下账本行数、Wiki、标记文件、快照全部零写入；同一窗口非影子模式正常执行并写标记 |
| TestShouldReviewAndRebuild | PASS | 窗口边界（04:29/04:45 不跑、当天已跑不跑、周一不重建、周日重建） |
| TestParseReviewReply | PASS | 合法/围栏 JSON 解析；非法 JSON 报错；未知种类、缺 tool_id、修路缺新入口的草案逐条丢弃 |
| TestAggregateSignals | PASS | 执行失败 2、探针失败 1、HALT 1、CONCERN 1、分诊错选代理 1、今日费用 300/前 7 日均 100/突增 true |
| TestRebuildWeeklyDiff | PASS | 快照故意写反：新好 1 条、新坏 1 条；registry.json 磁盘内容未变；Wiki 报告与快照更新；探针通过的标内存已验证 |
| TestProposalStatusFlow | PASS | 提议入库→applied→按状态查询；非法状态拒绝；不存在的提议报错；重复 proposal_id 失败 |

另加：`internal/registry` 的 TestEntryPointOverride（坏入口拒绝、好入口存入、磁盘文件未变、不存在/不可用/空入口拒绝）与 TestUnmarkVerified（标记↔撤标记对称），`internal/llm` 的 TestReviewSelfEvolve（费用按 `selfevolve-<日期>` 归属、空信号不调模型），均 PASS。

## 沙箱验证（`go run ./cmd/selfevolve-verify`）：ALL PASS

种子账本：cap-ops-true 执行失败 3 次、探针失败 3 次、监工 HALT 3 次；
cap-ops-git 执行失败 2 次（低于阈值）；费用今天 300 毫分、前 7 天两天 300、五天 50。

```
review 2026-11-18: signals=12 lessons=2 proposals=4 applied=1 pending=3 rejected=0 cost=7 毫分
proposal se-2026-11-18-001 | exec_fail:cap-ops-true | applied | 证据 3 次
proposal se-2026-11-18-002 | probe_fail:cap-ops-true | pending | 证据 3 次
proposal se-2026-11-18-003 | advisor_halt:cap-ops-true | pending | 证据 3 次
proposal se-2026-11-18-004 | expense_spike | pending | 证据 3 次
expense task=selfevolve-2026-11-18 model=deepseek-flash prompt=1000 hit=800 miss=200 completion=300 cost=7 毫分
expense task=selfevolve-2026-11-18 model=selfevolve-summary prompt=1000 hit=800 miss=200 completion=300 cost=7 毫分
rebuild 2026-11-18: checked=2 ok=2 broken=0 newly_broken=0 newly_ok=0 discovered=6
selfevolve-verify: ALL PASS
```

**实测费用（沙箱跑一次自查）：1300 token（prompt 1000 + completion 300），7 毫分 = ¥0.007。**
注意：这是桩模型模拟的一次 Flash 调用量（800 命中 + 200 未命中 + 300 输出，
按 llm 包的官方价算出 7 毫分），不是真实 API 用量；真实用量等 04:30 在线上
跑起来后从 expense_ledger 读。重建的 `discovered=6` 是候选清单在本机
（开发机）上命中的命令（go、make 等），不是测试机上的数。

## 诚实缺口（没做完或做不到的，如实写）

1. **真实 04:30 窗口还没跑过**：自查与重建只在测试与沙箱里跑过（桩模型），
   真实模型调用、真实费用、真实 04:30 定时都还没在线上验证——须等明天
   04:30 之后看账本（过夜守护进程是第 15 步的旧二进制，未含第 20 步；
   部署要等 04:40 过夜验证结束之后，见任务书硬约束）。
2. **汇总行 double count 的口径**：当日汇总费用行（model=`selfevolve-summary`）
   的 token 与费用与调用行重复；`SummarizeExpenseByTask` 已排除该标记，
   但 `watchdog.DailySavings` 是按全表 SUM 的，会把汇总行也算进去——
   自进化每天的费用本身只有几毫分，误差极小，但口径上确实存在，
   未改 DailySavings（不在本步范围）。
3. **分诊错选是代理信号，不是实测**：账本里没有“分诊错了”的直接记录，
   用“Tier-1 挑了工具、同一任务执行随后失败”代理；工具本身有 bug 也会
   被算成错选，模型点评时须结合错误样本判断。
4. **纯 resolve_error 账本里没有**：模型选了不存在/不可用的工具时，
   main.go 只记日志、不写 execution_ledger；自查能看到的只是探针失败行
   （command 以 `probe ` 开头）。要全量须另改 main.go 的 resolve 记账，
   不在本步范围。
5. **日期口径是北京，账本 ts 是 UTC**：聚合用 `date(ts,'+8 hours')`
   转北京日期；与 L1 文件的北京日期一致，但与 `DailySavings` 的 UTC
   口径不同，两处的日汇总在 00:00–08:00（北京时间）窗口内会对不上。
6. **“新发现的本机工具”是候选清单，不是全盘扫描**：只查 16 个常见命令
   （`discoveryCandidates`），全盘 PATH 扫描噪音大、也慢，没做。
7. **重建快照是本地文件**：`data/selfevolve/rebuild_last.json` 只活在
   本机数据目录，不进记忆备份；换机器部署后第一次重建是基线、差异为空。
8. **提议去重按签名，不按种类**：同一签名 7 天内只产生一条提议；
   若一条提议 pending 时同一问题又冒出另一种修法，须等 7 天或等人先处理。
9. **entry_point 修正重启即失效**：覆盖层只活在内存，永久修正仍须由人
   写进 registry.json；自查的 Wiki 教训里有修正记录，供人照抄入库。
10. **A/B 测试未做**：按任务书留给下一步。
