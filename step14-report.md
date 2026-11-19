# 第 14 步报告：修 P0（切流前必须修的 6 条 + 2 个新增跨模块 P0）

- 日期：2026-11-17 晚（北京时间 23:07–23:16）
- 测试机：狗云 VM124655（45.140.88.10），部署目录 /root/super-agent-007
- 本地工作目录：~/workspace/super-agent-007/
- 结论：**8 件全部完成，真实重测全部 PASS。**但实测中发现一个第 14 步新引入的致命 bug（fastSem 泄漏），已修并加回归测试。另有两个诚实说明见末尾“未覆盖 / 注意”。

## 1. 改动清单（文件 + 行数）

| # | 任务 | 文件 | 行数（改后） | 内容 |
|---|---|---|---|---|
| 1 | B-01 网关 60 秒延迟 | internal/gateway/gateway.go | 574 | pick() 三级判定：超 ForcedWake（180s）→ 合并满 60s/180s → 无合并满 3s 防抖即取；config.go 加 Debounce 字段（默认 3s，环境变量 DEBOUNCE_S） |
| 2 | B-02 队列落盘 | internal/ledger/schema.sql（97 行）、internal/ledger/ledger.go（547 行）、internal/gateway/gateway.go | 见上 | ledger.db 新增 inbox_queue 表 + (task_id, inbound_seq) 部分唯一索引（seq>0）+ 状态索引；EnqueueInbox（BEGIN IMMEDIATE，seq>0 去重）、ListInboxByStatus/ListStaleInbox、MarkInboxRunning/Done/Failed（严格按行 id 转移）、RequeueInbox（仅 retry_count==0 时重排 1 次）、RecordStage/LastStage；网关收信先写库再发回执，写库失败回 500 不发回执；启动 ReplayInbox 重放 PENDING/RUNNING，重放跳过剩余防抖；60 秒 sweep 扫荡超期 RUNNING + 补捞内存缺失的 PENDING |
| 3 | B-03 未验证工具探针 | internal/registry/registry.go（361 行）、internal/triage/triage.go（394 行）、cmd/super-agent/main.go（326 行） | 见上 | 保留 Callable() 看 available；新增 NeedsProbe() 判 !verified（内存 verifiedOverride，不回写 registry.json）；Probe()/ProbeEntry()（entry_point 首词：/ 开头 os.Stat，否则 exec.LookPath）；分诊 ResolveTool 选中未验证工具先探针，通过标 verified=true 再执行，失败返回 ErrProbeFailed，main.go 把探针失败记 execution_ledger（command "probe <tool>"） |
| 4 | B-06 告警接恢复 | internal/watchdog/watchdog.go（242 行）、internal/executor/executor.go（492 行）、internal/ledger/ledger.go | 见上 | 水位超 1800s 且 retry_count==0 时自动重排 1 次并记 execution_ledger（tool_id watchdog-requeue）；execution_ledger 加 stage_no / artifact_path 列（Open 时 ALTER TABLE 迁移，查 pragma 后加）；executor Task 加 StageNo/ArtifactPath，ReviewShard 评审通过后 CheckpointStage 落盘，HALT 不记；只做 Stage 边界续跑，不做通用 shell 断点续跑 |
| 5 | B-07 账本备份 | internal/watchdog/backup.go（新增，208 行）、cmd/super-agent/main.go | 见上 | BackupLedger：SQLite VACUUM INTO 到 <dataDir>/backups/ledger-YYYYMMDD.db（独立连接，busy_timeout=5000，同日文件先删后写），保留最近 7 天，结果写入 execution_ledger（tool_id backup-ledger）；BackupLoop：60 秒 tick，北京时间 03:30–03:45 窗口 + backup_last 标记；非影子模式启动 |
| 6 | B-08 双通道 | internal/gateway/gateway.go、internal/executor/executor.go | 见上 | 去掉 MAX_PARALLEL=1，改 fastSem（容量 2）+ heavySem（容量 1），按预估耗时路由；超 30 秒（InlineThreshold）释放 Fast 槽位转 Heavy。Gemini 方案里“ContextBuilder 的 messages 读写加独立 sync.RWMutex”一项经核查不适用：代码里根本没有 ContextBuilder，llm.BuildMessages 是纯函数、每次调用返回全新 []Message，各任务互不共享，不存在要加锁的共享可变状态（grep 验证，无 RWMutex、无共享 messages 切片）；并发安全由各任务自带 messages + ledger 自带互斥锁保证 |
| 7 | B-01×B-02 / B-08×Zone B/C | 并入 1、2、6 | — | 防抖重启去重（重放跳过防抖 + 唯一索引）已落位；B-08×Zone B/C 的 messages 并发锁经核查不适用（见第 6 行：无共享 messages 状态），以检查结论关闭 |
| 8 | 回归 | 全仓库 | — | 见第 3 节 |

补充修补（实测中发现，见第 4 节）：
- internal/gateway/gateway.go processLane：Fast 完成路径补 `<-g.fastSem` 释放槽位（此前漏放，致命）。
- internal/gateway/gateway.go putBack：丢弃旧条时把其 inbox 行标 FAILED（此前旧条行已被 take() 标 RUNNING，丢弃后会幽灵 RUNNING 30 分钟）。
- internal/gateway/gateway_test.go（635 行）：新增 TestSequentialThree（3 条顺序任务回归测试）；TestInboxDedup / TestInboxReplay / TestSweepStale 改为轮询等 DONE 行再断言（此前 handler 返回后立刻查 DONE 行有竞态，在测试机上曾误报失败一次）。

## 2. 本地验证

- `go vet ./...`：PASS
- `go test -count=1 -race ./...`：全部 11 个包 PASS（含 gateway 11 个测试、ledger 6、registry 10、triage 10、executor 10、watchdog 12）
- 回归测试有效性验证：临时注释掉 fastSem 释放，TestSequentialThree 失败，原文 `timeout: only [seq1 seq2] completed, fast lane likely leaked`，与测试机实测症状一致；恢复后通过。

## 3. 测试机真实重测结果（原文）

守护进程 3 次启动（session sa-20261117T151406Z-c5b7b6 / sa-20261117T151543Z-5e013a），真实 DeepSeek 调用，e2e14* 费用合计 1 毫分（¥0.001）。

### Phase A：重启重放（B-02 旧行）
```
replay inbox rows: [('e2e14-latency', 'DONE', 0), ('e2e14-weather', 'DONE', 0), ('e2e14-probe-ok', 'DONE', 1), ('e2e14-probe-fail', 'DONE', 0), ('e2e14-ping', 'DONE', 0)]
replay triage: [('e2e14-latency', 'DIRECT', 1, ''), ('e2e14-weather', 'DIRECT', 1, ''), ('e2e14-probe-ok', 'TOOL', 0, 'cap-e2e14-probe-ok'), ('e2e14-probe-fail', 'TOOL', 0, 'cap-e2e14-probe-fail'), ('e2e14-ping', 'DIRECT', 1, '')]
replay execution: [('e2e14-probe-ok', 'cap-e2e14-probe-ok', 'echo e2e14-probe-ok', 0, 'success', 'e2e14-probe-ok'), ('e2e14-probe-fail', 'cap-e2e14-probe-fail', 'probe cap-e2e14-probe-fail', 1, 'failure', '')]
```
RUNNING 旧行重排 1 次（retry_count=1）后处理，PENDING 旧行直接重放，全部只处理一次。

### Phase B：B-01 / 天气回归 / B-03
```
B-01 POST dt=0.01s resp={'received': '2026-11-17T15:14:11Z', 'task_id': 'e2e14b-latency', 'ts': '2026-11-17T15:14:11Z'}
B-01 triage rows=[('DIRECT', 1, '')] observed_after=4.01s (limit 10s)
weather triage rows=[('DIRECT', 1, '')] observed_after=4.01s
weather expense rows=[('e2e14b-weather', 'deepseek-flash', 0)]
probe-ok execution rows=[('cap-e2e14-probe-ok', 0, 'success', 'e2e14-probe-ok')] observed_after=3.51s
probe-fail probe rows=[('cap-e2e14-probe-fail', 'probe cap-e2e14-probe-fail', 1, 'failure', 'triage: 工具预检探针失败: "cap-e2e14-probe-fail": registry probe: 命令 "no-such-cmd-e2e14-xyz" 不在 PATH: exec: "no-such-cmd-e2e14-xyz": executable file not found in $PATH')] observed_after=3.51s
```
- B-01：单封从 POST 到 triage 行出现 **4.01 秒**（<10 秒，含 3 秒防抖 + 约 1 秒模型调用）。
- 天气：DIRECT，不编 tool_id；费用行带 task_id（0 毫分，缓存命中）。
- B-03：探针通过（echo）执行成功 exit=0，stdout `e2e14-probe-ok`；探针失败记账，command `probe cap-e2e14-probe-fail`，status failure。

### Phase C：B-08 双通道
```
long triage rows=[('TOOL', 0, 'cap-e2e14-long', '2026-11-17T15:14:29Z')] observed_after=3.51s (handler started)
short triage rows=[('DIRECT', 1, '', '2026-11-17T15:14:39Z')] observed_after=4.51s (must be < long completion)
long execution rows=[('cap-e2e14-long', -1, 'failure', '2026-11-17T15:14:59Z')] observed_after=20.04s
B-08 VERDICT: short triage at POST+13.0s, long execution at POST+29.6s -> SHORT NOT BLOCKED
```
长活 sleep 60 在 30 秒被 InlineThreshold 杀掉（exit=-1）；短任务在长活运行中完成，不被堵住。`go test -race ./...` 测试机全部通过（见第 2 节，测试机同步跑过）。

### Phase D：B-06 水位重排
```
inserted fake stale RUNNING row e2e14b-stale seq=99 updated_ts=2026-11-17T12:14:59Z
stale row after sweep: [('DONE', 1)] (waited 8.0s)
stale triage: [('e2e14b-stale', 'DIRECT')]
stale requeue ledger rows: [('watchdog-requeue', 'requeue stale inbox', 'success')]
```
超期 RUNNING 行 8 秒内被扫荡重排 1 次并处理，记账完整。

### Phase E：B-07 备份
```
ok  	007-super-agent/internal/watchdog	0.141s
no backups dir yet (expected: window is 03:30 Beijing)
1
backup copy tables: [('execution_ledger',), ('expense_ledger',), ('inbox_queue',), ('index_metrics',), ('queue_watermarks',), ('sqlite_sequence',), ('triage_metrics',)]
backup copy inbox rows: (12,)
```
- watchdog TestBackupLedger 在测试机上 PASS（0.141s）。
- 守护进程日志有 `backup loop started: daily 03:30 Beijing`（1 条）。
- 对真实 ledger.db 的文件拷贝做 VACUUM INTO，备份库 7 张表齐全、可打开，inbox 12 行。
- backups 目录尚未生成（符合预期：窗口在 03:30，现在 23:15，未到时间）。

### Phase F：B-02 kill 重启重放
```
POST dt=0.006s resp={'received': '2026-11-17T15:15:41Z', 'task_id': 'e2e14b-kill', 'ts': '2026-11-17T15:15:41Z'}; killing pid 2465718 NOW
SIGKILL sent at POST+0.007s (debounce is 3s, handler cannot have started)
triage rows for e2e14b-kill right after kill: [(0,)]
inbox row for e2e14b-kill: [('PENDING', 0)]
daemon dead after SIGKILL
port 6262 free
== restart daemon (replay) ==
2026/11/17 23:15:43 gateway replay: 1 inbox rows requeued
kill-test inbox row after restart: [('DONE', 0)] (waited 0.0s)
kill-test triage count (must be exactly 1): [(1,)]
kill-test triage row: [('DIRECT', 1)]
```
POST 后 0.007 秒 SIGKILL，重启后任务处理且只处理一次（triage 恰 1 行）。

### Phase G：停干净 + 注册表恢复
```
no super-agent process left
port 6262 free
no leftover sleep processes
2026/11/17 23:15:48 shutting down...
2026/11/17 23:15:48 stopped
registry entries: 76 -> 73
L2 memory.md lines: 22 -> 19
temp L3 files removed
entries=73 L1=12 L2=16 L3=1181
callable cap-memory-playbooks: false
inbox_queue exists: (1,)
execution_ledger columns: ['id', 'ts', 'session_id', 'task_id', 'tool_id', 'command', 'exit_code', 'stdout_tail', 'err_head', 'duration_ms', 'status', 'advisor_verdict', 'advisor_retry_count', 'advisor_note', 'stage_no', 'artifact_path']
inbox status counts: [('DONE', 13)]
inbox dedup index: [('idx_inbox_dedup',)]
e2e14* expense rows (must all have task_id): [('e2e14-latency', 'deepseek-flash', 0), ('e2e14-weather', 'deepseek-flash', 1), ('e2e14-ping', 'deepseek-flash', 0), ('e2e14b-latency', 'deepseek-flash', 0), ('e2e14b-weather', 'deepseek-flash', 0), ('e2e14b-short', 'deepseek-flash', 0), ('e2e14b-stale', 'deepseek-flash', 0), ('e2e14b-kill', 'deepseek-flash', 0)]
total cost e2e14b (milli CNY): (1,)
triage rows e2e14*: (13,)
```
- 守护进程 SIGTERM 后干净停止，无残留进程、无端口占用、无残留 sleep 进程。
- 注册表恢复 73 条（registry-verify 验证通过）。
- inbox 13 行全 DONE，无卡住的行；费用行 6 条空 task_id 经核实全部是第 13 步首次 e2e（22:26–22:31 CST）的旧行，第 14 步全部 8 条费用行都带 task_id。

## 4. 实测中发现的致命 bug（已修）

**fastSem 泄漏（第 14 步新引入，B-08 双通道代码）**：
- 现象：守护进程启动后前两条任务正常，从第三条起所有任务再也不处理，无任何日志，循环空转。inbox 行：第三条 RUNNING、其余 PENDING；goroutine dump 显示 worker 循环活着在 select，但没有任何 handler/processLane 协程。
- 根因：`processLane` 的 Fast 完成路径（`<-done` 分支）漏写 `<-g.fastSem` 释放槽位，只有迁移（timer）分支释放。两条任务各永久占一个槽位（容量 2），第三条起 `tryDispatch` 永远走 `putBack`，循环里反复取回又放回。
- 证据：SIGQUIT goroutine dump（日志 /tmp/sa007-step14-daemon.log，测试机）+ 本地回归测试反向验证（注释掉释放后 `timeout: only [seq1 seq2] completed, fast lane likely leaked`）。
- 修复：Fast 完成路径补释放；putBack 丢弃分支把旧条 inbox 行标 FAILED（防幽灵 RUNNING）；新增 TestSequentialThree 回归测试。
- 教训：单元测试此前全是单任务网关，漏泄漏不可见；双通道这类并发记账必须有“多条顺序任务”的回归测试。

## 5. 未覆盖 / 注意

1. **真实 03:30 备份窗口未跑过**：BackupLoop 已接线、单元测试在测试机上通过、真实库文件拷贝 VACUUM INTO 可用，但每日 03:30 的真实触发要等到凌晨，未在本次验证。切流前建议等一次真实 03:30 或接受此缺口。
2. **真实 04:00 精炼窗口未跑过**：第 12 步遗留，仍未验证（与第 14 步无关）。
3. **真实 DeepSeek RefineL1 未跑过**：第 12/13 步遗留，仍未验证。
4. 部署脚本（阶段 2b/2c）保存在工作目录：`sa007-step14-stage2b.sh`、`sa007-step14-stage2c.sh`；测试机 /tmp 下留有 3 份守护进程日志（daemon.log、daemon2.log、daemon3.log）作证据，未删。
5. 密码文件：每次用后即删（trap），#2 /tmp 无残留密码文件；测试机 /tmp 已清理，只留日志。
6. 记忆自动备份不在本步做，留给第 15 步。
