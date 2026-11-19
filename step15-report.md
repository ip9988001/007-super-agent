# 超级 Agent 007 第 15 步报告：记忆自动备份（2026-11-17）

测试机：狗云 VM124655（45.140.88.10），部署目录 /root/super-agent-007
本地与测试机最终清单 md5 全等：`1e103ce5b19eec18bb084a78a9e7dbad`（126 文件）
Token：已验证可用（login ip9988001，private 库 push 权限）。值只存本地
`~/.config/super-agent-007/github.env`（600）与测试机 `/etc/super-agent/.env`
（600，GITHUB_TOKEN 行 1 条，DEEPSEEK_API_KEY 行 1 条，未动原有行）。

## 1. 改动清单

| 文件 | 行数 | 内容 |
|---|---|---|
| internal/backup/backup.go（新增） | 528 | Runner：开关、remote 双保险校验、白名单复制、git add 显式路径、无变更跳过、pull --rebase --autostash、push 重试 1 次、记账+告警、sanitize |
| internal/backup/backup_test.go（新增） | 452 | 10 个测试：开关关、remote 不匹配、URL 解析、askpass 无 Token、可执行位、sanitize、重试成功、两次失败记账、有上游先 pull、非仓库、真实 git 端到端 |
| internal/config/config.go | 246 | BACKUP_ENABLED / BACKUP_REPO / BACKUP_SERVER_ID / BACKUP_TOKEN_KEY / BACKUP_REPO_DIR（默认关；只记键名） |
| internal/watchdog/watchdog.go | 261 | Watchdog 加 backupHook + SetBackupHook + afterRefine |
| internal/watchdog/refine.go | 163 | RefineLoop 精炼成功后调 afterRefine（失败不触发，钩子失败只记日志） |
| cmd/super-agent/main.go | 348 | 非影子 + 开关开时接线；默认关时打一行 `memory backup disabled (default off)` |
| cmd/backup-verify/main.go | — | 临时验证程序，验证完已删（本地与测试机均无残留） |

接线：RefineLoop → RefineDaily 成功 → 写 refine 标记 → afterRefine →
backup.Runner.Run(ctx, 昨日日期)。备份失败不影响精炼结果与主循环。

## 2. Gemini 4 个避坑点逐条验证

1. **Token 不进 .git/config、不落盘**：只从环境变量键名读；推送用临时
   GIT_ASKPASS 脚本（内容只有 `printf '%s\n' "$SUPER_AGENT_GIT_TOKEN"`，
   不含值，700 权限，用后即删）；双保险：开关显式开 + remote 必须等于
   `ip9988001/super-agent-007`。验证：TestRemoteMismatchRefuses（非指定库
   拒绝，push 零调用）、TestAskpassScriptHasNoToken、TestSanitizeRedactsToken。
2. **白名单 + 防误推**：只复制 `wiki/**/*.md` 与 `l1/<date>.log`；
   `memories/.gitignore` 兜底屏蔽 `*.db*`、`.env`、`*.lock` 等；
   `git add` 只传显式路径；`git diff --cached --quiet` 无变更跳过。
   验证：TestRealGitEndToEnd 远端树只含 4 个期望文件，ledger.db / .env /
   x.lock / notes.txt 均未进；第 2 次运行 NoChange、无新提交。
3. **多机冲突**：按服务器分目录 `memories/<server_id>/`；有上游时推送前
   固定 `git pull --rebase --autostash`。验证：TestPullRunsBeforePushWhenUpstreamExists
   （调用顺序 pull 在 push 前）、无上游时跳过 pull。
4. **旁路容错与记账**：push 最多重试 1 次（间隔 10 秒）；仍失败记 failure
   行 + 告警日志，不阻塞精炼。验证：TestPushRetriesOnceThenSucceeds
   （push 恰好 2 次）、TestPushFailsTwiceRecordsFailure（1 行 failure）；
   真实推送时也实际触发过一次（见第 4 节）。

## 3. 本地与测试机测试结果（原文）

- 本地 `go vet ./...`：干净；`go test -count=1 -race ./...`：全部 ok
  （backup 21.9s，含两次 10 秒重试等待）。
- 测试机 `go vet ./...`：VET-OK；`go test -count=1 -race ./...`：全部 ok
  （backup 22.1s）；守护进程短暂启动：`llm ready`、`advisor ready`、
  `refine loop started`、`memory backup disabled (default off)`、
  `backup loop started`、`gateway ready`，停后无残留进程、6262 已释放。

## 4. 真实 GitHub 推送验证（原文）

- Token 部署：`/etc/super-agent/.env` 由 173 字节变为 227 字节，权限 600，
  GITHUB_TOKEN 行 1 条，DEEPSEEK_API_KEY 行 1 条。
- 克隆：`git clone https://ip9988001@github.com/ip9988001/super-agent-007.git`
  成功，HEAD `addaaba feat: minimal runnable skeleton v0.1.0`；
  建测试分支 `test/step15-backup-verify`（main 不动）。
- 推送（备份逻辑实跑，假数据 `/tmp/sa007-step15-fakedata`，server-id
  `vm124655-test`，不含测试机真实记忆）：
  `result: {Skipped:false NoChange:false Committed:true Files:2 Branch:test/step15-backup-verify} err: <nil>`，
  新提交 `571d093 backup memory vm124655-test 2026-11-17`；
  账本行：`('backup-memory-2026-11-17', 'backup-memory', 'success', 'committed files=2 branch=test/step15-backup-verify')`。
- API 核对（测试分支，均为 200）：
  `memories/vm124655-test/wiki/concepts/backup-marker.md`（79 字节）、
  `memories/vm124655-test/l1/2026-11-17.log`（7 字节）、
  `memories/.gitignore`（103 字节）。
- 清理：API 删测试分支返回 204，复查 404；main SHA 删除前后一致
  `addaaba233b4483a1e9b0c9d9795f0f06c62d5c4`，仓库干净。
- 失败路径实测：第一次推送因 askpass 脚本权限问题失败，重试 1 次后仍失败，
  记 failure 行 + 告警日志（见第 5 节 bug 1），精炼与主程序未受影响。

## 5. 验证中抓到的 2 个真 bug（已修，均有测试钉死）

1. **Runner 自建的临时 askpass 脚本不可执行**：`writeAskpass` 用
   `os.Chmod(0o600)`，git exec 时报 `Permission denied`，导致第一次真实
   推送失败（重试后仍失败，记 failure 行）。修为 `0o700`；测试
   TestAskpassScriptHasNoToken 增加可执行位断言。
2. **remote URL 含 userinfo 时解析失败**：克隆用的
   `https://ip9988001@github.com/...` 形，旧解析只认 `https://github.com/` 前缀，
   会把合法私密库误判为非 GitHub 地址而拒绝推送。`parseGitHubRepo` 改为
   先剥 scheme 再剥 userinfo；新增 3 组测试用例
   （`https://<user>@github.com/...`、gitlab 拒绝）。

## 6. 未覆盖 / 待办

- 真实 04:00 精炼触发仍未跑过（与第 14 步同款缺口）：RefineLoop 钩子只在
  单元/假 git 层面与本次手动 Run 验证过；等一次真实 04:00 或接受此缺口。
- Token 90 天有效期，台账到期日 2027-01-09：到期后备份自动失效（只记
  failure 行），须提前轮换；当前代码不做到期提醒。
- 本地临时远端全链路已跑（TestRealGitEndToEnd：提交→无变更跳过→新增再提交，
  远端提交数 1→1→2）；真实 GitHub 推送已跑（第 4 节），不再是“待 token”。
- 临时文件清理：测试机 /tmp 只留 `sa007-step15-daemon.log` 与
  `sa007-step15-manifest-final.txt` 作证据；二号 /tmp 无残留密码/token 文件。
