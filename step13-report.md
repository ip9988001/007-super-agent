# 超级 Agent 007 第 13 步报告（2026-11-17）

修第一次真实端到端抓出的 4 个缺口 + 监工预审证据包，并重测天气一路。

测试机：狗云 VM124655（45.140.88.10），目录 /root/super-agent-007
守护进程会话：sa-20261117T144003Z-81dfe4，运行 22:40:03–22:42:26 CST（UTC 14:40:03–14:42:26）
代码包 md5：bcd5fc3d08fa1bb49d6d26b5b03a8132（本地与测试机同一包）
本次真实费用：3 次调用，5 毫分（¥0.005）——天气分诊 4 + 删文件分诊 0 + 监工 1

## 0. 编译与测试（测试机实测）
- `go vet ./...`：VET-OK
- `go test -count=1 ./...`：全部 ok（advisor / config / executor / gateway / ledger / llm / memory / registry / triage / watchdog / cmd/super-agent）
- 中途修掉的两处测试问题（均为测试断言过期，非生产代码）：
  1. 三个验证程序的桩仍用旧签名（shadow-verify / refine-verify / triage-verify 的 ModelCallFunc 与 RefineFunc）——已跟新签名。
  2. TestLoad72Entries 仍断言 72 条（第 11 步已加 cap-design-hyperframes，总账 73 条，design 7→8）——改名 TestLoad73Entries；TestFrozenListDeterministic 只数 `- cap-` 行（60 行），漏了 7 条 ext- 与 5 条 skill-——改数 `- ` 行（72 行）。

## 1. 改动清单（5 件）
1. **分诊提示词带工具清单**（internal/triage/triage.go）
   - 新增 `CompactToolList(reg, candidates)`：可用工具每条一行 `tool_id: 一句话`（73 条总账减 1 条 available:false = 72 行），按类别+文件内顺序，确定性输出；one_liner 截断 80 runes。
   - 新增 `BuildTriageFrozen(reg, candidates)`（冻结区：提示词头 + 清单 +“只许从中挑，清单外的一律非法；没有合适工具就回 DIRECT 或 META，禁止编造 tool_id”）与 `BuildTriageTail(text)`（用户区只剩原话）。
   - `ModelCallFunc` 签名改为 `func(taskID, frozen, tail string)`；`BuildTriagePrompt(reg, text)` / `BuildTriagePromptNarrowed(reg, text, candidates)` 相应改签；`llm.TriageCall(taskID, frozen, tail)` 按 zones（frozen system 在前、tail user 在后）发调用，冻结区跨调用稳定可命中缓存。
2. **费用按任务归属**（internal/llm/llm.go）
   - 根因确认：`Chat`/`TriageCall`/`RefineL1` 原先都走 `c.TaskID`（main.go 只设了 `lc.SessionID`，从未设 TaskID），故 expense_ledger 的 task_id 全空。
   - 新增 `chatWithTask(ctx, msgs, taskID)` 真实现；`Chat` 以 `c.TaskID` 委托；新增 `ChatTask(ctx, msgs, taskID)`；`TriageCall(taskID, frozen, tail)` 与 `RefineL1(ctx, taskID, l1Text)` 都把调用方传来的 taskID 记进费用行；`recordExpense(u, taskID)`。
   - 新增 `CostMilliForModel(model, hit, miss, out, peak)`：deepseek-flash/reasoner/chat 按 Flash 价（hit 20 / miss 1000 / out 4000 毫分每百万 token，峰值 ×2；reasoner/chat 是已退役别名，按官方价目页以 Flash 计费）；deepseek-v4-pro 按 147/4402/13207；未知模型默认 Flash 价。`CostMilli` 改为委托它。
   - `internal/watchdog/refine.go` 的 `RefineFunc` 签名改为 `func(ctx, taskID, l1Text)`，RefineDaily 传 `refine-<日期>`。
3. **监工费用进账**（internal/advisor/advisor.go、adapter.go、cmd/super-agent/main.go）
   - `RunAdvisorCheck` 解析 API 响应的 `usage`（prompt_tokens / prompt_cache_hit_tokens / prompt_cache_miss_tokens / completion_tokens）进 `AdvisorDecision.Usage`；未知 verdict 降级 CONCERN（不变）。
   - `Advisor` 增 `Ledger *ledger.Ledger` 字段（nil = 不记账）；`Check` 成功后调 `recordAdvisorExpense`：按 `llm.CostMilliForModel(模型, ...)` 记 expense_ledger（模型名、hit/miss、费用毫分齐全，task_id 与 session_id 取自任务）；记账失败只记日志，不吞 verdict。
   - main.go：`adv.Ledger = led` 接线。
4. **启动体检只写一行汇总**（internal/watchdog/watchdog.go）
   - `CheckRegistry`：坏条目只 `log.Printf` 记日志，账本只写一行汇总 `RecordExecution`（ToolID watchdog-daily-check，Command "watchdog check summary"，有坏条目时 exit 1/status failure，否则 0/success，ErrHead `checked=%d ok=%d broken=%d skipped=%d; broken: <前 5 名或 none>`）。
5. **监工预审证据包**（internal/advisor/adapter.go、internal/executor/executor.go）
   - `Advisor` 接口 `CollectEvidenceBundle(task, trigger)`；executor 的 `Check` 把 trigger 透传；cmd/super-agent 的 shadowAdvisor 与 cmd/executor-verify 的桩同步改签。
   - adapter：`trigger == TriggerPreAction` 且 `task.Command` 非空时，TaskPrompt 追加拟执行命令 + 预审备注（“动作尚未执行：只核对拟执行命令与目标是否与任务原话一致，不验证完成度；不要以‘无法证实已完成’为由告警”）；`IrreversibleKind == KindDeleteFile` 时从命令里取目标路径（跳过命令名与 `-` 选项）放进 `EvidenceRequest.ArtifactPaths`，让监工看到目标存在与否。delivery/shard 不变。
   - 新增 `deleteTarget(command)` 辅助。

## 2. 重测结果（原文，测试机 ledger.db）
守护进程启动（非影子，74 条含临时测试条目）：
- `registry ready: 74 entries`；`llm ready: model=deepseek-flash`；`advisor ready: model=deepseek-reasoner`；`gateway ready: listening 127.0.0.1:6262 (POST /wake), session=sa-20261117T144003Z-81dfe4`

天气一路（POST 14:40:09Z，等 75 秒后查）：
```
TRIAGE: [('e2e13-weather', 'DIRECT', 1, '')]
EXPENSE: [(7, 'e2e13-weather', 'sa-20261117T144003Z-81dfe4', 'deepseek-flash', 0, 3309, 56, 4)]
```
- 日志：`task=e2e13-weather route=DIRECT tier=1 (no tool to run)`
- 结论：PASS——模型不再编造 tool_id "weather"，走 DIRECT；费用行 task_id = e2e13-weather（旧缺口已修）；费用核对：3309 miss + 56 out = 4 毫分。

删临时文件一路（POST 14:41:24Z，等 75 秒后查；临时注册表条目 e2e13-delete-tmp-file，memory 类）：
```
TRIAGE: [('e2e13-delete', 'TOOL', 1, 'e2e13-delete-tmp-file')]
EXECUTION: [(597, 'e2e13-delete', 'e2e13-delete-tmp-file', 'rm /tmp/advisor-e2e13-test.txt', 0, 'success', 'NOTE', '拟执行命令 rm /tmp/advisor-e2e13-test.txt 与任务原话的目标路径完全一致，单文件删除无通配风险；属预审阶段，仅核对一致性，命令与目标匹配无误。')]
EXPENSE: [(8, 'e2e13-delete', 'sa-20261117T144003Z-81dfe4', 'deepseek-flash', 3072, 250, 40, 0), (9, 'e2e13-delete', 'sa-20261117T144003Z-81dfe4', 'deepseek-reasoner', 128, 198, 130, 1)]
```
- `ls /tmp/advisor-e2e13-test.txt`：`No such file or directory`，文件已删；日志：`task=e2e13-delete tool=e2e13-delete-tmp-file exit=0 status=success`
- 结论：PASS——监工 verdict = NOTE（上次是 CONCERN），备注明确写了“属预审阶段，仅核对一致性”，不再以“无法证实已完成”告警；reasoner 费用行已进账（hit 128 / miss 198 / out 130，1 毫分）；删文件分诊命中缓存 3072 token，费用 0 毫分（四舍五入）。

启动体检汇总行（execution_ledger，仅 1 行）：
```
WATCHDOG-SUMMARY: [('watchdog-daily-check', 'watchdog-daily-check', 'failure', 'checked=73 ok=7 broken=66 skipped=1; broken: cap-fetch-agent-reach, cap-fetch-bili, cap-fetch-browser-stack, cap-fetch-camoufox, cap-fetch-gh-cli 等 66 条')]
```
- 日志：`watchdog check: 73 checked, 7 ok, 66 broken, 1 skipped`（73 = 74 条减 1 条 available:false 的跳过；66 条坏明细只在日志里）
- 结论：PASS——一次启动只写 1 行汇总，不再写 66 行 failure。

## 3. 收尾（PASS）
- 守护进程 SIGTERM 停：`ps` 无 super-agent 残留，6262 已释放。
- 注册表恢复 73 条（total=73），L2 无 e2e13 字样，L3 临时文件已删，data/ 已从备份恢复。

## 4. 测试过程说明（非代码问题，两次记录）
- 第一轮重测（22:38）两次 POST 后 20/30 秒查库全空：原因是网关 `pick()` 只取等待 ≥60 秒合并窗口的信，等的时间不够，不是代码没跑。改等 75 秒后重测通过。
- 第一轮部署时 `go vet` 抓出 3 个验证程序桩签名未更新（见 §0），已修。
- 备注：精炼的真实 DeepSeek 调用（RefineL1 新签名）只做了单元/桩验证，未在本轮跑真实精炼；RefineLoop 04:00 窗口仍只单元测试过。
