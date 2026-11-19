# 第 21 步报告：多步骤规划器 + LLM 核验 + 执行循环

时间：2026-11-17 晚（UTC+8，写于 19:53 左右）｜状态：**本地实现完成，全部测试通过；未部署到测试服务器**（过夜守护进程 04:40 验证结束前不部署）

设计方案：`step21-plan.md`（85 行，核验器最终定为 deepseek-reasoner，用户 2026-11-18 03:45 定；与规划器 flash 不同模型，防自己给自己判卷；不需新 key，现有 DeepSeek key 已覆盖 flash + reasoner）。

## 1. 改了哪些文件

| 文件 | 行数 | 改动 |
|---|---|---|
| `internal/planner/planner.go` | 876（新） | 规划器包：Plan/Verify/PlanAndVerify/RunPlan/Synthesize，冻结提示词，代码实核注册表，失败阶梯，进度保险，50 步熔断 |
| `internal/planner/planner_test.go` | 543（新） | 14 个测试（见 §3） |
| `cmd/planner-verify/main.go` | 275（新） | 沙箱验证脚本：桩模型跑四项测试集 + 费用实测 |
| `cmd/super-agent/main.go` | 445 | daemon 加 `pl` 字段；`runPlan` + `memorySummary`；PLAN 分支；启动建核验客户端 + 打 `planner ready` 日志 |
| `internal/triage/triage.go` | 400 | 新增 `RoutePlan = "PLAN"`；提示词头四去向 DIRECT\|TOOL\|META\|PLAN；`ParseTriageReply` 接受 PLAN（tool_id 必须为空，未知 tool_id 报错） |
| `internal/triage/triage_test.go` | 388 | 提示词期望改四去向；PLAN 解析用例（合法 + 未知工具非法） |
| `internal/llm/llm.go` | 521 | 新增通用 `CallTask(taskID, frozen, tail)`（frozen→system/A 区，tail→user/C 区，按 taskID 记费用）；`TriageCall` 改为委托它。核验器复用同一客户端构造（`llm.New(cfg.AdvisorModel, apiKey)`），无重复 HTTP 代码，无硬编码 key |
| `internal/ledger/schema.sql` | 133 | 新表 `plan_runs`（plan_id、task_id、task_text、steps、plan_json、verdict、revise_rounds、estimated_cost_milli、created_ts）+ plan_id 唯一索引 + task_id 索引 |
| `internal/ledger/ledger.go` | 1012 | 新增 `PlanRun` 类型、`RecordPlanRun`、`CountPlanRunsByDate`（GLOB `pl-<日期>-[0-9][0-9][0-9]` 精确匹配，重规划 `-r1` 行不抬高编号）、`ListPlanRuns` |
| `internal/executor/executor.go` | 510 | `Result` 加 `StdoutFull`/`StderrFull`（全文，供 artifact 落地）+ `MaxFullOutputBytes = 1MB` 上限 + `capFullOutput`；执行逻辑未动 |

**未动**：宪法文件、`install-toolchain.sh`、`step1/` 注册表与索引文件、`internal/selfevolve/`（只读参考；其文件 mtime 为 19:17，系压缩前第 20 步收尾时的触碰，本步未编辑）、测试服务器、二号/三号服务器、任何 `~/.config/` 下的凭证文件。报告与代码里无任何密钥/token/密码原文。

## 2. 提示词位置（冻结在代码里）

| 提示词 | 文件 + 行号 |
|---|---|
| 规划器 `planPromptHead`（拆步：任务 + 精简工具清单 + 记忆摘要 → 步骤表） | `internal/planner/planner.go:126` |
| 核验器 `verifyPromptHead`（审查员角色，逼它挑刺；逐项回答四个问题；结论三档；不许只说“合理/通过”，找不到问题也要写“已检查合并可能性：步骤X与Y不能合并，因为……”） | `internal/planner/planner.go:142` |
| 综合器 `synthPromptHead`（各步结果综合成最终回复） | `internal/planner/planner.go:157` |

核验器调用顺序：**先做代码实核**（工具逐个对照注册表真实存在且可用状态，`registryCheck`），再调 reasoner，实核表放进 tail 让模型修正；模型说 APPROVE 但实核有问题时，结论强制降级为 REVISE。REVISE 最多 2 轮；2 轮后仍不过，取最好一版执行（`ErrReviseExhausted` 时回落到最后一版合法修订计划），并记账。

## 3. 测试结果

### 3.1 单元测试：`go test -count=1 ./internal/planner/` —— 14/14 PASS

| 测试 | 内容 | 结果 |
|---|---|---|
| TestPlanTwoSteps | 两步任务应拆 2 步 | PASS |
| TestVerifyKeepsFifteenSteps | 十五步任务完整保留 15 步，核验不许砍 | PASS |
| TestVerifyTrimsBloatedPlan | 臃肿计划 12 步（含 3 步废步）→ 核验指出合并 → 9 步，1 轮修改 | PASS |
| TestVerifyRejectsUnknownTool | 步骤用了不存在的工具，模型说 APPROVE 也被实核打回 REVISE 并点名 | PASS |
| TestReviseRoundCap | 修改轮数上限：2 轮后仍 REVISE 即停，取最好一版 | PASS |
| TestVerifyFindingsCoverage | findings 必须覆盖四个问题，缺一个就报错 | PASS |
| TestPlanValidation | 计划校验：未知工具 / 错误 depends_on / 错误编号均拒绝 | PASS |
| TestRunPlanSuccess | 两步执行成功：artifact 落盘、Stage 行、`{{step1}}` 替换进实录命令 | PASS |
| TestProgressInsurance | 连续 3 步零进展即停（`stopped_no_progress`，第 3 步处停，8 行执行记录） | PASS |
| TestRetryThenSkip | 失败重试一次 → 跳过记一笔 → 继续 | PASS |
| TestReplanOnDependencyFailure | 依赖失败 → 重规划一次（plan_runs 原行 + `-r1` 行）→ 继续完成 | PASS |
| TestSubstituteInputs | `{{stepN}}`/`{{stepN.summary}}` 替换单元测试 | PASS |
| TestShadowDoesNotExecute | 影子模式只记 plan_runs，不执行（0 执行行，0 artifact） | PASS |
| TestHaltBlocksPlanStep | 不可逆步骤（rm）执行前过监工：HALT 触发，rm 从未真跑 | PASS |

### 3.2 全量：`go vet ./...` 通过；`go test -count=1 -race ./...` —— 14 个包全部 ok

`cmd/super-agent`、`internal/advisor`、`internal/backup`、`internal/config`、`internal/executor`、`internal/gateway`、`internal/ledger`、`internal/llm`、`internal/memory`、`internal/planner`、`internal/registry`、`internal/selfevolve`、`internal/triage`、`internal/watchdog`。

### 3.3 沙箱验证：`go run ./cmd/planner-verify` —— ALL PASS

| 项 | 结果 |
|---|---|
| ① 两步任务 | PASS（2 步，APPROVE） |
| ② 十五步任务 | PASS（15 步完整保留，APPROVE） |
| ③ 臃肿计划 | PASS（12 步 → 9 步，1 轮修改） |
| ④ 坏工具计划 | PASS（模型 APPROVE 被实核降级为 REVISE，点名坏工具） |
| 费用汇总断言 | PASS（20700 tokens，21 毫分；plan_runs 3 行；registry.json md5 未变） |

### 3.4 主程序冒烟（影子模式，dummy key，未联网）

用 step1 注册表副本在临时目录启动 daemon，日志实测：

```
planner ready: worker=deepseek-flash verifier=deepseek-reasoner shadow=true
```

接线真实生效。冒烟后两个临时目录与临时二进制已删，无残留进程（端口 6269 已空）。

## 4. 实测费用（桩模拟，非真实 API 用量）

桩模型假设用量（每次调用记一条 expense_ledger 行）：规划 flash：prompt 1500（1200 命中 + 300 未命中）+ completion 400 = **2 毫分**；核验 reasoner（按 Flash 价计，见诚实缺口）：prompt 2500（2000 命中 + 500 未命中）+ completion 500 = **3 毫分**。

- 四项测试集总计：规划 3 次 + 核验 5 次 = **20700 tokens，21 毫分 = ¥0.021**。
- **一次完整规划+核验（1 次规划 + 1 次核验）：4900 tokens，5 毫分 = ¥0.005**，低于方案预估的约 ¥0.015——但注意：这是桩的**假设用量**，真实提示词含 79 条可用工具的 CompactToolList + 记忆摘要 + 计划 JSON，token 数只会更大；真实费用等上线后从 expense_ledger 读，以实测为准。

## 5. 关键行为（已实现）

- 分诊：PLAN 去向只在任务需要多步时走；单步路径（DIRECT/TOOL/META）行为不变。
- 执行循环：按 approved plan 顺序跑，复用现有 executor；每步结果只把摘要（≤500 字）传给下一步，全文存 `dataDir/artifacts/<taskID>/step-N.txt` + `reply.md`；账本每步记 stage_no；不可逆步骤执行前照常过监工（HALT 最多 2 轮，HALT-final 立停）。
- 失败阶梯：重试一次 → 跳过记一笔 → 回规划器重新规划（只许一次，仅当后面步骤依赖这步、或是最后一步）→ 停下汇报。进度保险优先于重规划：连续 3 步零进展即停；另设 50 步熔断只防死循环。
- 账本：规划与核验调用各记一行费用，task_id 沿用任务号；plan_runs 记计划与结论。
- 影子模式：只出规划+核验结论（记一行 shadow 回执），不执行、不综合。
- 方案里写的“72 条”工具清单已过期：代码用的是**实时 CompactToolList**（84 条中 79 条可用），不是写死的 72 条。

## 6. 诚实缺口（没做完或做不到的）

1. **真实模型端到端未跑**：本步全程桩模型 + 本地 stub，未调真实 DeepSeek API。真实规划/核验质量（尤其核验器是否真会挑刺、是否误砍 15 步计划）与真实费用均未知，等部署后实测。
2. **核验器费用按 Flash 价计**：`CostMilliForModel` 目前所有模型都按 Flash 价算；reasoner 真实价未知，费用账本里的核验行可能低估。
3. **计划执行是同步的**：`RunPlan` 阻塞当前任务通道；长计划执行期间同通道的新任务要等。异步计划执行器是后续工作。
4. **跨崩溃续跑未实现**：stage 行已写（数据在），但网关不会按 plan_id 续跑崩溃中断的计划；崩溃后需人工重派。
5. **`{{stepN.summary}}` 可能被 ValidateArgs 拒**：摘要含 shell 元字符时替换失败（fail-closed，有意为之；遇上时走失败阶梯）。
6. **第 9 步遗留未修**：runTool 不传任务参数（`/audit` 端到端 exit 2 的根因），本步未动；计划步骤经同一 runTool 路径，需要结构化参数的工具仍受此限。
7. **其余前期缺口未动**：index_metrics 仍 0 行；L2 拆分后合计 1291 token 超旧 850 上限（Gate 6 待修订）；8 个助手脚本仍是三号硬编码路径；run-audit.sh 仍无本地规则快照；deps.sh/systemd 单元仍不在仓库（切流前置）。
8. **daemon 的 `runPlan` 本身无单元测试**：只在冒烟里验证了接线日志；真实 PLAN 任务走网关的端到端待部署后验证。
9. **A/B 测试与学习回路不在本步**：按计划是后续步骤。
10. **未部署**：测试服务器、04:40 过夜验证均未碰；本步所有验证都在本地工作区完成。
