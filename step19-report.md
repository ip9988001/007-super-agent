# 第 19 步报告：007 工具链安装（2026-11-18）

按用户 2026-11-18 01:45 确认的 v2 过滤版清单执行：装 39 个工具 + 9 个助手脚本 + 14 个 skill，
分五批，每批可核验。安装目标机：测试机 VM124655（45.140.88.10，部署目录 /root/super-agent-007）。
来源原则：三号只当清单（装什么、哪个版本），公开工具一律走 GitHub / 官方源、版本钉死跟三号一致；
自家文件一次性打包进私密库当工具种子，以后装机不靠三号。

## 总结果

- 第 1–3 批：39/39 工具已装，版本全部对上三号（lightpanda 除外：滚动 nightly，构建号不同，见下）。
- 第 4 批：工具种子已进私密库 super-agent-007，新 HEAD **7a512ac**；干净克隆验证文件数全对上；
  种子已解开到测试机 /root/super-agent-007/tools/。
- 第 5 批：注册表 73 → **84 条**；探针按 call_type 分流；L2 ops 拆分；go vet + go test -race 全绿；
  测试机影子核验 **79 checked / 79 ok / 0 broken / 5 skipped**；独立端到端 4 个任务，
  无 resolve_error；install-toolchain.sh 已推公开库 007-super-agent，新 HEAD **93cd0cd**。
- 失败：最终状态 0 项失败（过程中 4 项补修，全部修好，见各批）。
- 红线：过夜守护进程（PID 2476844）全程未重启、未动；live ledger.db 未动；
  未碰 xray/nginx、未重启机器；二号三号只读；无密钥/token 值进报告、进库。

## 第 1 批：基础 13 个——全部通过

| 工具 | 版本（实测） | 来源 |
|---|---|---|
| curl | 7.88.1 | apt（Debian 12 自带） |
| git | 2.39.5 | apt |
| jq | 1.6 | apt |
| python3 | 3.11.2 | apt |
| node | v22.23.4 | nodejs.org 官方 tarball |
| npm | 10.9.9 | 随 Node |
| uv | 0.13.5 | astral-sh/uv GitHub release |
| yarn | 1.22.22 | npm |
| pandoc | 2.17.1.1 | apt |
| ffmpeg | 5.1.9-0+deb12u1 | apt |
| rg | 13.0.0 | BurntSushi/ripgrep .deb |
| gh | 2.103.2 | cli/cli .deb |
| java | Temurin 25.0.5+8 | Adoptium API（latest 25 GA） |
| tesseract | 5.3.0 | apt（第 3 批核对） |
| magick | 6.9.11-60 | apt（第 3 批核对） |
| mpv | 0.35.1 | apt（第 3 批核对） |

补修 3 项（sa007-batch1-fix.sh）：uv tarball 解开后是子目录（须 find 找二进制）；
rg.deb asset 名是 `ripgrep_13.0.0_amd64.deb`（无 `-1`）；Debian 12 无 /usr/bin/magick，
已建软链 /usr/local/bin/magick → /usr/bin/convert（与三号一致）。

## 第 2 批：代码审计 9 个——全部通过

| 工具 | 版本（实测） | 来源 |
|---|---|---|
| rea | 6.8.0 | npm rea-agents（源仓库 morluto/rea；REA 6.8.0 要求 Node ≥22.23.4，正是三号用该版本的原因） |
| codegraph | 1.6.2 | npm @colbymchenry/codegraph |
| semgrep | 1.182.1 | uv pip --system |
| ast-grep | 0.51.0 | ast-grep/ast-grep release |
| trivy | 0.76.1 | aquasecurity/trivy release |
| gitleaks | 8.30.1 | gitleaks/gitleaks release |
| bandit | 1.9.4 | uv pip --system |
| pip-audit | 2.10.1 | uv pip --system |
| ghidra | 12.2 | NSA/ghidra 最新稳定版（三号的 ghidra 版本从未记录，只验过 wrapper） |

补修 3 项（sa007-batch2-fix.sh）：Debian 12 的 `uv pip install --system` 须加
`--break-system-packages`；ast-grep asset 名是 `app-x86_64-unknown-linux-gnu.zip`
（不是 `ast-grep_...zip`）；ghidra 解压目录是 `ghidra_12.2_PUBLIC`（无日期后缀，
glob 须用 `ghidra_*_PUBLIC*`）。ghidra 已跑真实 headless 样本（/bin/true）：
exit 0，"Import succeeded"、"Save succeeded"。
**如实记录**：`Decompile` / `Decompile.java` postScript 在 Ghidra 12.2 里不存在
（上游移除/改名），不是安装失败；install-toolchain.sh 的冒烟测试已去掉该参数。

## 第 3 批：抓取浏览 14 个——全部通过

| 工具 | 版本（实测） | 来源/备注 |
|---|---|---|
| agent-reach | 1.6.0 | GitHub Panniantong/agent-reach，打 v1.6.0 标签装（PyPI 只有 0.1.0） |
| playwright | 1.64.1 | npm + chromium 下载；Chrome for Testing 156.0.8078.4，chromium-1248（与三号一致） |
| lightpanda | 1.1.0-nightly.12226+88fb36db0 | lightpanda-io/browser 滚动 nightly；**构建号与三号的 10033+614c1f56e 不同**，同属 1.1.0-nightly 线（滚动发布无法钉死构建号） |
| scrapling | 0.5 | uv tool，须 `[all]` extra（与三号收据一致） |
| httpx | 0.28.1 | uv tool，须 `[cli]` extra |
| yt-dlp | 2026.10.21 | yt-dlp/yt-dlp release |
| chrome-nosandbox | — | wrapper，指向本机 playwright chromium（Chrome 156.0.8078.4） |
| camoufox | 0.5.9 | uv pip --system；Python API，无 CLI，验证 import |
| edge-tts | 7.3.1 | uv tool |
| hyperframes | 0.9.63 | npm |
| impeccable | 4.7.0 | 技能目录（绝对路径调用已验证 4.7.0；PATH 调用的引擎查找限制与三号相同） |
| tesseract | 5.3.0 | apt |
| magick | 6.9.11-60 | apt |
| mpv | 0.35.1 | apt |

补修 4 项（sa007-batch3-fix.sh，全部修好）：agent-reach 改 git 源；
scrapling 改 `[all]`；httpx 改 `[cli]` 并钉 0.28.1；camoufox 补
`--break-system-packages`。修后核验：agent-reach JSON 报 1.6.0、scrapling 0.5、
httpx CLI 横幅正常、camoufox import ok。

## 第 4 批：技能与知识种子——完成

**一次失败后修好**：首次在三号宿主机上 `tar /data/...` 全部 "Cannot stat"、
打出 45 字节空包——/data 只在 pi-agent 容器内可见，宿主机看不到。
改用 `docker exec pi-agent tar` 流式打出，成功。

种子内容（只读复制，2026-11-18 自三号快照）：

| 目录 | 数量 | 核对 |
|---|---|---|
| skills/（40 个目录，各含 SKILL.md） | 40 | 装 14 个 + 只存档 26 个（24 steel-will + tavily-search + pi-web-restart） |
| capabilities/ | 67 篇 md | 与三号一致 |
| design-md/ | 84 个品牌目录 | 与三号一致 |
| seo-toolkit/ | 491 个文件 | 三号原数 545 含两个 .git 目录（~50 文件，已剔除）与 4 个 __pycache__（被 .gitignore 排除）；另有两处内嵌 .git 仓库（claude-seo、geo-seo-aeo-skill）已拆成普通文件提交，否则干净克隆会丢内容 |
| scripts/ | 9 个 | 8 个 /data/bin 脚本 + run-audit.sh |
| parked-extensions/ | 30 个 .ts | 只存档，不加载 |
| 合计 | **933 个文件** | 另有 tools-seed/README.md、MANIFEST.md |

- 私密库 super-agent-007 新 HEAD **7a512ac**（"feat: add tools seed from #3 snapshot (step 19 batch 4)"），
  干净克隆验证：40/40 SKILL.md、67/84/491/9/30 全对上，seed-tools.sh 可执行。
- 部署脚本 `scripts/seed-tools.sh`：首次部署按子目录拷进 `<deploy>/tools/`，已存在且非空的子目录跳过、不覆盖。
- 种子已解开到测试机 /root/super-agent-007/tools/，计数全对上（40/67/84/491/9/30），
  content_quality.py、audit.py 均在位。

## 第 5 批：注册表修复 + 全量核验——完成

### 注册表改动（registry.json v1.0 → v1.1，73 → 84 条）

- **删 2 条**：cap-memory-l1-watcher、cap-memory-refine-daily（框架内已有实现，遗留项）；
  对应 L3 文件一并删除（否则 Load 报 orphan）。
- **标 available:false 4 条**（v2 不装，L2 行尾标 ❌）：cap-fetch-bili（CLI 存在但最小调用未出数据）、
  cap-fetch-tvly（无 Tavily key）、cap-ops-lean-ctx（已死）、cap-ops-llmtrim（已死）。
- **补路径 3 条**：content_quality.py → `tools/seo-toolkit/claude-seo/scripts/content_quality.py`；
  audit.py → `tools/seo-toolkit/geo-seo-aeo-skill/scripts/audit.py`；
  claude-seo → `tools/seo-toolkit/claude-seo/scripts/claude-seo`（相对部署根目录；
  守护进程须以部署根目录为工作目录启动，systemd WorkingDirectory）。
- **/audit 改挂**：cap-code-semgrep 去掉 /audit 前缀；新条 cap-code-run-audit（service，
  `tools/scripts/run-audit.sh`）接 /audit 前缀。
- **camoufox 入口**：`python API（camoufox）` → `python3 -c import camoufox`
  （旧首词 `python` 在 Debian 12 的 PATH 里根本不存在，探针必失败）。
- **版本订正**（第 19 步实测）：ghidra 12.1.4 → 12.2（注册表与 jdk25 条各一处）；
  camoufox 0.5.8 → 0.5.9、Playwright 1.62.0 → 1.64.1。
- **新增 13 条**（L3 token 数为 DeepSeek 官方 tokenizer.json 实测，344–477，均远低于 960 上限）：
  4 CLI（tesseract/magick/mpv/npm）+ 8 脚本（service，tools/scripts/…）+ 1 run-audit（service）。
  新条 status_triad.verified=false（首次调用先走 B-03 探针）。
- **L2/L1**：ops.md 两行加 ❌（仍 25 行）；新建 ops-2.md（15 行，12 条新增 ops 条目）；
  code.md 加 run-audit 行并订正 ghidra 版本（11 行）；fetch.md 两行加 ❌；
  memory.md 删两行（17 行）；L1 计数更新为 13/8/7/8/34/14，总计 84 条。
- **L3**：新增 13 份（644–953 字符，均 ≤1600，SOP 输入/步骤/验收齐全）；
  修订 3 份（两 SEO 脚本的 /data/workspace 路径改为部署相对路径；semgrep 的 SOP 去掉 /audit 前缀）；
  8 个脚本的 L3 如实写明三号专属限制（硬编码 /data/* 路径，007 上部分节为空；link-selftest 在 007 上不可用；
  run-audit.sh 本地规则快照缺失时回落 registry 在线规则）。

### 探针改动（internal/registry/registry.go，watchdog 委托同一实现）

按 call_type 分流：cli 查命令/路径（"python3 -c …" 跑一次只读 import 检查，10 秒超时）；
service 只查脚本文件在位；doc/sop/tool/mcp/data 不探针（只记知识，不可执行）；
未知 call_type 按 cli 处理。L2() 支持多份（主文件在先、续文件按名排序拼接，每份单独校验 ≤25 行）。

### 测试

- 本地 go vet 全过；go test -count=1 -race 12 个包全绿。
- 更新的测试：TestLoad84Entries（84 条；分类 13/8/7/8/34/14）、TestL2SplitOps（ops 拼后 40 行）、
  TestProbeEntryByCallType（知识条目不探针、service 只查文件、python3 -c 真实 import 检查）、
  TestL2OneCategory（改测 seo 单份）、TestTier0PrefixHit（/audit → cap-code-run-audit）、
  冻结清单 72 → 79 行（84 条 − 5 条 available:false）、TestHandleWritesL1（/rea 替 /audit，
  因 run-audit 在测试工作目录下探针不到相对路径——生产环境以部署根目录启动，不受影响）。
- 一次偶发：TestMergeAndSerial 在 -race 下失败一次（receipt 文件数时序问题），
  重跑 1 次与 3 次均通过；与本次改动无关（未碰 gateway 代码）。

### 部署与核验（测试机）

- 新注册表 + 索引已同步到 /root/super-agent-007/data/（只换文件；live ledger.db 未动，
  610 行执行记录完好）；新二进制已部署（旧版留 super-agent.step15.bak）；
  过夜守护进程 PID 2476844 全程存活，未重启。
- 影子核验（临时数据目录 + 端口 6264，cwd=部署根目录）：`registry ready: 84 entries`，
  **`watchdog check: 79 checked, 79 ok, 0 broken, 5 skipped`**。
  - 说明：79 = 84 − 5 不可用；其中可执行条目 48 个（39 CLI + 9 service）全部探针通过，
    31 个知识条目不探针 + L3 检查通过。任务书写的 "43/43" 是 cli 类型条目数
    （43 = 39 可用 + 4 不可用跳过）；实际探针通过的可执行条目是 48/48。
- L2 拆分后 token 实测（官方 tokenizer）：ops 拼后 40 行共 1291 token（ops.md 766 + ops-2.md 525）；
  单份仍 ≤25 行、ops.md 766 未超旧 850 上限，但两份合计超过旧 850——34 条 ops 工具的诚实成本。
  L1 231 token（原 214，因计数行变长）；L3 84 份，最大 700、最小 261，与第 11 步一致。

### 独立端到端（临时数据目录 + 端口 6263，非影子，真实密钥）

| 任务 | 路由 | 工具 | 结果 |
|---|---|---|---|
| /rea --version | TOOL / tier 0 | cap-code-rea | exit 0，success |
| 用 jq 把一段 JSON 里的 name 字段取出来 | DIRECT / tier 1 | —（模型决定直接答，未调工具） | 费用 4 毫分；是模型的合法 DIRECT 决定，不是 resolve_error |
| /audit 审一下 tools/scripts/cap-drift.sh | TOOL / tier 0 | cap-code-run-audit | exit 2，failure——run-audit.sh 未收到目标参数（runTool 目前按 entry_point 原样跑，不传任务参数；第 9 步已知缺口） |
| 看一下本机 Python 是什么版本 | TOOL / tier 1 | cap-ops-python | exit 0，success；费用 1 毫分 |

**无 resolve_error**。Tier-1 工具执行链路（79 条冻结清单 → 模型选工具 → 探针 → 真执行）
在新注册表上已验证。两轮守护进程均已 SIGTERM，端口 6263 已释放。

### install-toolchain.sh 与公开库

- `~/workspace/super-agent-007/install-toolchain.sh`（244 行）：三批已验证命令的合订版
  （已并入全部补修：rg 正确 asset 名、uv 找子目录二进制、ast-grep 正确 asset 名、
  ghidra glob `ghidra_*_PUBLIC*`、uv pip `--break-system-packages`、
  agent-reach 走 git 标签、scrapling[all]、httpx[cli]、hyperframes 钉 0.9.63、
  ghidra 冒烟测试去掉不存在的 Decompile 参数）；末尾有完整核验节（逐个 --version + ghidra headless）。
  只含公开源步骤；bash -n 通过；全 diff 扫过，无密钥、无 token、无私密路径、无记忆内容。
  impeccable 一节为可选：仅当 DEPLOY_DIR 下已有种子文件时建软链，否则跳过。
- 公开库 007-super-agent 新 HEAD **93cd0cd**（32 个文件：框架改动 31 个 + install-toolchain.sh；
  本地工作区与公开库除本次改动外零漂移）。API 核对：172 个跟踪文件，
  install-toolchain.sh 在位，无 .env/.db/data//wiki//memories/ 路径。**库仍为私密，未翻公开**
  （翻公开由 04:40 过夜验证任务按用户授权执行）。

## 遗留与缺口（诚实记录）

1. 8 个助手脚本是三号专属（硬编码 /data/* 路径）：探针只验文件在位，L3 已写明哪些节在 007 上为空；
   要全功能，须按部署目录改路径（不在本批范围）。
2. run-audit.sh 在 007 上缺本地规则快照（/data/workspace/semgrep-rules），默认回落 registry 在线规则
   （需联网）；OUT_DIR 默认 /data/audit-out，建议显式设置。
3. runTool 目前按 entry_point 原样跑、不传任务参数（第 9 步已知缺口）：/audit 任务因此 exit 2；
   模型拼参数的第二步接线仍待后续步骤。
4. lightpanda 是滚动 nightly，构建号无法钉死（与三号的构建号不同，同属 1.1.0-nightly 线）。
5. L2 ops 拼后 40 行实测 1291 token（ops.md 766 + ops-2.md 525），超过旧 850 单份上限——
   拆分后每份仍 ≤25 行，总量是 34 条工具的诚实成本。
6. index_metrics 仍是 0 行（LedgerFileRecorder 无调用方，第 18 步已指出）；13.3× 的账本支撑仍未补。
7. 04:40 过夜验证（03:30 备份 + 04:00 精炼真实窗口）结果未到；到时须先核验再汇报。
8. GitHub classic PAT 2027-01-09 到期，须提前轮换（无提醒机制）。

## 文件清单

- 本地工作区：`~/workspace/super-agent-007/step19-report.md`（本报告）、
  `install-toolchain.sh`、`step19-plan.md`、`step19-plan-v2.md`、
  `sa007-batch1.sh`、`sa007-batch1-fix.sh`、`sa007-batch2.sh`、`sa007-batch2-fix.sh`、
  `sa007-batch3.sh`、`sa007-batch3-fix.sh`；注册表 `step1/registry/registry.json`（v1.1，84 条）；
  索引 `step1/index/`（L1 + L2×7 + L3×84）。
- 私密库 super-agent-007：HEAD **7a512ac**（tools-seed/ 933 个文件 + scripts/seed-tools.sh）。
- 公开库 007-super-agent：HEAD **93cd0cd**（仍私密）。
- 测试机：/root/super-agent-007/tools/（种子已解开）、/root/super-agent-007/super-agent（新二进制，
  旧版在 super-agent.step15.bak）、/root/super-agent-007/data/registry/registry.json（84 条）。
