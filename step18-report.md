# 第 18 步核验报告：测试机 007 部署健康 + 注册表 vs 测试机 vs 三号工具对照

- 核验时间：2026-11-18 00:46–01:05 CST
- 测试机：狗云美国洛杉矶 VM124655（45.140.88.10），/root/super-agent-007
- 三号：103.148.58.11，容器 pi-agent（只读）
- 方法：全部只读。测试机上跑了 5 个检查脚本（进程/端口/日志/账本 python3 只读/磁盘/逐条命令存在性）；三号上跑了 2 个检查脚本（扩展/CAPABILITIES/技能/CLI `--version` 与 `command -v`/路由代码）；本地读了注册表、watchdog/executor/triage 源码。对照基线：~/workspace/pi-audit/no3-tools.md（2026-11-17 03:20 实测）。
- 红线遵守情况：过夜守护进程未动（未重启、未写 data/）；二号三号只读；未碰任何仓库；未读、未打印任何密钥/token 值。

## 一句话结论

**部署本身是健康的，但 007 现在并不是"有三号那么多工具"的 Agent。** 注册表 73 条里真正能跑的只有 39 条 CLI；测试机上这 39 条只装了 4 条（curl、git、jq、python3）；另外 34 条是非可执行项（文档/SOP/扩展工具位），分诊清单照样列给模型，模型挑中就 resolve_error。三号的工具链是全的（39 个 CLI 名字全部在，版本与基线一致），但 007 要真正拥有它们，生产机必须装全套工具链并修注册表，或者给 007 接一条远程调三号的通道——现在两条路都没有。

## 部署健康（测试机）

| 项 | 结果 | 证据 |
|---|---|---|
| 二进制版本 | ✅ 第 15 步版 | 16,843,552 字节，2026-11-17 23:30:30 +0800；`go version -m`：go1.26.0，module 007-super-agent/cmd/super-agent，modernc.org/sqlite v1.62.0 |
| 注册表 | ✅ 73 条齐全 | 日志 `registry ready: 73 entries`；step1/registry/registry.json 与 data/registry/registry.json md5 一致（6c7851c2…，67,961 字节，与本地一致）；L3 73/73 齐全，无缺无多；L1 12 行；L2 均 ≤25 行（ops.md 正好 25 行，零余量） |
| 配置 | ✅ | /etc/super-agent/.env 存在，600 权限（只核对存在与权限，未读值） |
| 数据目录 | ✅ 完整 | data/{index, ledger.db, memory, prompts, receipts, registry} 齐全；ledger.db 7 张表；memory/l1 只有 2026-11-17.log（26 行，测试数据）；wiki 3 条 concepts |
| 守护进程 | ✅ 在跑 | PID 2476844，2026-11-18 00:25:57 启动；6262 在监听；日志 81 行，无 panic、无 error（唯一含 "fatal" 的行是 executor ready 的提示语 "fatal if key missing"，是文案不是报错） |
| 账本 | ✅ 基本正常 | triage 20 行（全是 11-17 e2e 测试）；expense 14 行，合计 12 毫分，缓存命中行可见（3072 hit tokens）；inbox 13 行全 DONE；queue_watermarks 22 行 |
| 资源 | ✅ 充足 | 磁盘 78G 用 11%（67G 可用）；内存 3.8G，可用 3.2G |
| 小缺口 | ⚠️ 两处 | 部署目录里没有 CHANGELOG.md（仓库里有，同步时没带过来）；测试机无 sqlite3 CLI（只能用 python3 查账本） |

## 发现的问题（按严重度）

### 1. 能跑的工具只有 39/73，测试机上只有 4/39

- 注册表 73 条按 call_type 分：cli 39、sop 13、tool 9、doc 8、service 2、mcp 1、data 1。
- 只有 cli 是可执行形态；其余 34 条是知识/流程/扩展工具位。
- 但 `CompactToolList`（triage.go:179）只过滤 `available:false`，不按 call_type 过滤——72 条可用项全列给模型挑。模型一旦挑中非 cli 项，`ResolveTool` → `Probe` 必然失败 → resolve_error，任务直接失败。
- 三号上这些能力是 Pi 照着 CAPABILITIES 文档亲手做（link-agent.cjs 的 /seo、/ui、/design 是 `runPi` 提示词派发），007 里没有"模型照 SOP 自己做"的执行路径，只有 exec argv 一条路（main.go:150 注释："按总账的 entry_point 原样跑一条命令"；llm.go:389 `argv:= strings.Fields(e.EntryPoint)`）。
- 测试机实测：57 个可检查项里只有 4 个 YES（curl、git、jq、python3），53 个缺；16 个是文档/技能类不可执行项。

### 2. 探针是弱代理，"6 个 ok"里有 2 个跑不起来

- `ProbeEntry`（registry.go:259）：只取 entry_point 首词——含 "/" 就 stat 文件，否则 LookPath。
- `python3 l1_watcher.py` / `python3 refine_daily.py`：只验 python3 在不在，不验脚本在不在。测试机上这两个脚本根本不存在，照样算 ok。所以启动日志 "72 checked, 6 ok, 66 broken, 1 skipped" 里的 6 个 ok，2 个是假阳性。
- 反过来，描述性首词（"SKILL.md:"、"37"、"memory/"、"stdio"、"python"）被当命令查，注定失败——每次启动刷 66 条 broken 日志，全是噪音。
- 另：l1_watcher / refine_daily 在 007 里本就是框架代码重实现（第 6/12 步），这两条是遗留重复项，留着只会误导计数。

### 3. 两个 SEO 脚本的 entry_point 没有路径，执行不住

- cap-seo-content-scorers `content_quality.py`、cap-seo-geo-seo-aeo `audit.py` 只有裸文件名；实物在三号 `/data/workspace/seo-toolkit/claude-seo/scripts/content_quality.py` 和 `/data/workspace/seo-toolkit/geo-seo-aeo-skill/scripts/audit.py`。`BuildArgv` 是原样跑，没有路径解析，模型挑中也跑不起来（探针也会先失败）。
- 同类：cap-fetch-camoufox（`python API（camoufox）`，探针找 "python" 失败——只有 python3）；cap-fetch-scrapling-mcp（`stdio scrapling-mcp server`，首词 stdio）。

### 4. index_metrics 永远是 0 行

- `LedgerFileRecorder` 适配器存在（internal/registry/metrics.go），但没有任何调用方构造它、没有任何地方调 `RecordIndexMetric`（除超限记账 registry.go:341）。后果：13.3 倍省 token 的说法没有账本数字支撑，无法从 ledger 验证。

### 5. L2 ops.md 正好 25 行，零余量

- 第 5 步已知，B-05 P1 项，未修。再加一条工具就超帽。

### 6. 部署目录缺 CHANGELOG.md；测试机缺 sqlite3

- 运维小缺口：查账本只能用 python3（本次就是这么查的）。

### 7. 账本里 599 个非零退出不是新 bug

- execution_ledger 共 610 行，其中 594 行是 session_id='watchdog' 的逐条探针失败行，时间戳全在 2026-11-17 11:31–14:24 UTC——即第 13 步修复之前。第 13 步修复（启动体检只写 1 行汇总）生效后，6 次启动各只写 1 行汇总（15:07–16:25 UTC，tool_id='watchdog-daily-check'，状态 failure 是因为 66 broken > 0，在测试机上是预期的）。代码也对得上：watchdog.go:132 "明细只记日志，不进账本"。
- backup-memory 曾 2 次失败后成功（11-17 15:30:31、15:31:11 失败，15:32:38 成功），是第 15 步 askpass 权限 bug 的现场，已修好。历史数据。

### 8. 9 条 doc-read-only 条目

- verified_basis 含"未独立实测运行时"：toolkit-index、design-write、ui-route、docker-init、logs、start-sh、extensions-overview、memory-layers、playbooks。
- 其中 playbooks：三号上 `/root/.pi/agent/memory/wiki/playbooks` 目录存在但是空的——available:false 实质正确（无内容），Step 1 的"目录不存在"说法不精确。

### 9. /audit 前缀错配：注册表与三号行为不一致

- 注册表 trigger_prefix `/audit` 挂在 cap-code-semgrep 上（entry_point semgrep）。
- 三号 link-agent.cjs:318：`/audit` → `bash security-audit-skill/bin/run-audit.sh`（安全审计脚本）。
- 007 收到 /audit 会调 semgrep，三号收到 /audit 会跑安全审计脚本——是两回事。/rea（rea CLI）、/graph（codegraph status）对得上；/seo、/ui、/design 在三号是 runPi 提示词派发，在 007 是跑不起来的注册表条目（见问题 1）。

## 三号实测（2026-11-18 00:47，容器内）

- 扩展：7 个.ts 在，大小与基线一致；停放 30 个。
- CAPABILITIES：67 个 md；design-md 84 品牌（`find design-md -maxdepth 1` 计 85 含父目录）。
- CLI：注册表 39 个 cli 名字背后的二进制/脚本全部在（38 个经 `command -v`，2 个 SEO 脚本经 find 定位），版本与 11-16/11-17 基线一致，无变化：rea 6.8.0、agent-reach 1.6.0、codegraph 1.6.2、rg 13.0.0、semgrep 1.182.1、ffmpeg 5.1.9、node v22.23.4、python 3.11.2、gh 2.103.2、yt-dlp 2026.10.21、jq 1.6、trivy 0.76.1、uv 0.13.5、gitleaks 8.30.1、bandit 1.9.4、pip-audit 2.10.1、ast-grep 0.51.0、lightpanda 1.1.0-nightly、playwright 1.64.1、scrapling 0.5、tvly 0.1.8、java 25.0.5、pandoc 2.17.1.1、yarn 1.22.22、npm 10.9.9、tesseract 5.3.0、magick 6.9.11、mpv 0.35.1。
- impeccable：PATH 调用仍失败（无引擎），绝对路径 4.7.0 可用——与基线一致。
- l1_watcher PID 112、refine_daily PID 113 在跑；gh 已登录 ip9988001；llmtrim 健康（:43117，up 530h53m，v0.13.4）；codegraph status 在 rea 目录可用。
- 技能：/data/skills 40 条目（与基线一致），但 steel-will-* 是 29 个不是基线说的 30 个（基线多算了 1 个）；PI-技能库 35 个 SKILL.md；hyperframes 技能在，含 3 个.mjs（animation-map、contrast-report、package-loader），从未实跑。
- docx/pdf/pptx/xlsx 四个技能：在三号现行路径下仍找不到（与基线一致）——不算注册表漏项，因为三号上也没有。
- 仍缺：soffice、sysctl、docker（与基线一致）；tvly 无 Tavily key；GSC/DataForSEO 凭据无（/data/state 下无相关目录，env 无相关变量）。

## 注册表 73 条 × 测试机 × 三号 对照表

| tool_id | call_type | entry_point | 测试机 VM124655 | 三号 #3（2026-11-18 00:47 实测） |
|---|---|---|---|---|
| cap-fetch-agent-reach | cli | agent-reach | ❌ 缺 | ✅ 二进制在 |
| cap-fetch-bili | cli | bili | ❌ 缺 | ⚠️ CLI 在，最小调用未出数据 |
| cap-fetch-browser-stack | cli | chrome-nosandbox / playwright | ❌ 缺 | ✅ 二进制在 |
| cap-fetch-camoufox | cli | python API（camoufox） | ❌ 缺 | ✅ 二进制在；能力是 Python API，无 CLI 入口 |
| cap-fetch-curl-jq | cli | curl | ✅ YES /usr/bin/curl | ✅ 二进制在 |
| cap-fetch-gh-cli | cli | gh | ❌ 缺 | ✅ 二进制在 |
| cap-fetch-httpx | cli | httpx | ❌ 缺 | ✅ 二进制在 |
| cap-fetch-lightpanda | cli | lightpanda | ❌ 缺 | ✅ 二进制在 |
| cap-fetch-playwright-cli | cli | playwright | ❌ 缺 | ✅ 二进制在 |
| cap-fetch-scrapling-mcp | mcp | stdio scrapling-mcp server | ❌ 缺 | ✅ 二进制在；entry_point 是描述语（stdio…），不是二进制名 |
| cap-fetch-scrapling | cli | scrapling | ❌ 缺 | ✅ 二进制在 |
| cap-fetch-tvly | cli | tvly | ❌ 缺 | ⚠️ 二进制在，无 Tavily key，出不了真数据 |
| cap-fetch-yt-dlp | cli | yt-dlp | ❌ 缺 | ✅ 二进制在 |
| cap-code-codegraph | cli | codegraph | ❌ 缺 | ⚠️ 二进制在；db 只在 rea 目录有，/data/workspace 未初始化 |
| cap-code-ghidra | cli | ghidra headless | ❌ 缺 | ⚠️ wrapper 在，未跑过真实样本 |
| cap-code-git | cli | git | ✅ YES /usr/bin/git | ✅ 二进制在 |
| cap-code-rea | cli | rea | ❌ 缺 | ✅ 二进制在 |
| cap-code-rg | cli | rg | ❌ 缺 | ✅ 二进制在 |
| cap-code-semgrep | cli | semgrep | ❌ 缺 | ✅ 1.182.1（但 /audit 路由实际跑 run-audit.sh，不是 semgrep——见报告） |
| cap-seo-content-scorers | cli | content_quality.py | ❌ 缺 | ✅ 脚本在 seo-toolkit/claude-seo/scripts/content_quality.py（entry_point 无路径） |
| cap-seo-design-md | data | 84 份 DESIGN.md 库 | —（不可执行项） | ✅ 数据在（84 品牌） |
| cap-seo-geo-seo-aeo | cli | audit.py | ❌ 缺 | ✅ 脚本在 seo-toolkit/geo-seo-aeo-skill/scripts/audit.py（entry_point 无路径） |
| cap-seo-llms-txt | sop | llms.txt 模板+规程 | —（不可执行项） | ✅ 模板在 geo-seo-aeo-skill/assets/llms-txt-template.md |
| cap-seo-claude-seo | cli | claude-seo skill 脚本 | —（不可执行项） | ✅ 二进制在 |
| cap-seo-seo-gateway | sop | /seo 路由 | ❌ 缺 | ✅ /seo 路由在 link-agent.cjs:324（runPi 提示词派发） |
| cap-seo-toolkit-index | doc | TOOLKIT-INDEX.md 路由表 | —（不可执行项） | ✅ 在 seo-toolkit/TOOLKIT-INDEX.md |
| cap-design-ui-visual | sop | chrome-nosandbox --screenshot / Playwr | ❌ 缺 | ✅ SOP/技能文档在（CAPABILITIES 或 /data/skills） |
| cap-design-design-md-self | sop | DESIGN.md 自产流程 | —（不可执行项） | ✅ SOP/技能文档在（CAPABILITIES 或 /data/skills） |
| cap-design-design-write | sop | /design --write | ❌ 缺 | ✅ /design 路由在 link-agent.cjs:326（runPi 派发） |
| cap-design-gateway-timeouts | doc | 网关超时矩阵 | —（不可执行项） | ✅ 文档/说明在 |
| cap-design-ui-route | sop | /ui 路由 | ❌ 缺 | ✅ /ui 路由在 link-agent.cjs:325（runPi 派发） |
| cap-design-impeccable-skill | sop | SKILL.md 工作流 | —（不可执行项） | ✅ SOP/技能文档在（CAPABILITIES 或 /data/skills） |
| cap-design-hyperframes | cli | npx hyperframes | ❌ 缺 | ✅ 技能在 /data/skills/hyperframes（含 3 个 .mjs），从未在 #3 上实跑 |
| cap-design-impeccable | cli | impeccable | ❌ 缺 | ⚠️ PATH 调用失败（无引擎），绝对路径 4.7.0 可用 |
| cap-ops-ast-grep | cli | ast-grep | ❌ 缺 | ✅ 二进制在 |
| cap-ops-bandit | cli | bandit | ❌ 缺 | ✅ 二进制在 |
| cap-ops-docker-init | doc | /sbin/docker-init | ❌ 缺 | ❓ 未核实（容器内无 /sbin/docker-init 迹象，基线未提） |
| cap-ops-edge-tts | cli | uvx edge-tts | ❌ 缺 | ✅ 二进制在 |
| cap-ops-ffmpeg | cli | ffmpeg | ❌ 缺 | ✅ 二进制在 |
| cap-ops-gh-token | sop | gh 登录态持久化 | ❌ 缺 | ✅ SOP/技能文档在（CAPABILITIES 或 /data/skills） |
| cap-ops-gitleaks | cli | gitleaks | ❌ 缺 | ✅ 二进制在 |
| cap-ops-jdk25 | cli | java | ❌ 缺 | ✅ 二进制在 |
| cap-ops-jq | cli | jq | ✅ YES /usr/bin/jq | ✅ 二进制在 |
| cap-ops-lean-ctx | cli | lean-ctx | ❌ 缺 | ✅ 二进制在 |
| cap-ops-llmtrim | cli | llmtrim | ❌ 缺 | ✅ 二进制在 |
| cap-ops-logs | doc | /data/logs 位置表 | ❌ 缺 | ❓ 未核实（/data/logs 位置表，本次未查） |
| cap-ops-node | cli | node | ❌ 缺 | ✅ 二进制在 |
| cap-ops-pandoc | cli | pandoc | ❌ 缺 | ✅ 二进制在 |
| cap-ops-pip-audit | cli | pip-audit | ❌ 缺 | ✅ 二进制在 |
| cap-ops-python | cli | python3 | ✅ YES /usr/bin/python3 | ✅ 二进制在 |
| cap-ops-start-sh | doc | start.sh 说明 | —（不可执行项） | ❓ 未核实（容器启动脚本，本次未查） |
| cap-ops-trivy | cli | trivy | ❌ 缺 | ✅ 二进制在 |
| cap-ops-uv | cli | uv | ❌ 缺 | ✅ 二进制在 |
| cap-ops-yarn | cli | yarn | ❌ 缺 | ✅ 二进制在 |
| cap-memory-extensions-overview | doc | 37 扩展总览 | —（不可执行项） | ✅ 文档/说明在 |
| cap-memory-l1-watcher | service | python3 l1_watcher.py | ❌ 缺 | ✅ PID 112 在跑（75808B，与基线一致） |
| cap-memory-memory-layers | doc | memory/ 分层说明 | —（不可执行项） | ✅ 文档/说明在 |
| cap-memory-playbooks | doc | — | —（不可执行项） | ⚠️ wiki/playbooks 目录存在但是空的（无内容） |
| cap-memory-recall-memory | tool | recall_memory | ❌ 缺 | ✅ 扩展已加载（日志命中） |
| cap-memory-refine-daily | service | python3 refine_daily.py | ❌ 缺 | ✅ PID 113 在跑（/data/bin/refine_daily.py） |
| cap-memory-wiki-upsert-page | tool | wiki_upsert_page | ❌ 缺 | ✅ 扩展已加载（日志命中） |
| ext-recall-memory | tool | recall_memory | ❌ 缺 | ✅ 扩展已加载（日志命中） |
| ext-wiki-upsert-page | tool | wiki_upsert_page | ❌ 缺 | ✅ 扩展已加载（日志命中） |
| ext-wiki-lint | tool | wiki_lint | ❌ 缺 | ✅ 扩展已加载（日志命中） |
| ext-self-status | tool | self_status | ❌ 缺 | ✅ 扩展已加载（日志命中） |
| ext-usage-report | tool | usage_report | ❌ 缺 | ✅ 扩展已加载（日志命中） |
| ext-detect-conflicts | tool | detect_conflicts | ❌ 缺 | ✅ 扩展已加载（日志命中） |
| ext-codegraph | tool | codegraph | ❌ 缺 | ✅ 扩展已加载（日志命中） |
| skill-long-term | sop | SKILL.md: steel-will-long-term | —（不可执行项） | ✅ SOP/技能文档在（CAPABILITIES 或 /data/skills） |
| skill-adjudicator | sop | SKILL.md: steel-will-adjudicator | —（不可执行项） | ✅ SOP/技能文档在（CAPABILITIES 或 /data/skills） |
| skill-retrieval | sop | SKILL.md: steel-will-retrieval | —（不可执行项） | ✅ SOP/技能文档在（CAPABILITIES 或 /data/skills） |
| skill-migration | sop | SKILL.md: steel-will-migration | —（不可执行项） | ✅ SOP/技能文档在（CAPABILITIES 或 /data/skills） |
| skill-forge | sop | SKILL.md: steel-will-forge | —（不可执行项） | ✅ SOP/技能文档在（CAPABILITIES 或 /data/skills） |

## 三号上有、注册表里没有的（漏掉的能力）

| 类别 | 漏掉的 | 备注 |
|---|---|---|
| CLI（4 个） | tesseract、magick/convert、mpv、npm | 均在三号 PATH 上，版本已核（5.3.0 / 6.9.11 / 0.35.1 / 10.9.9） |
| /data/bin 助手脚本（8 个） | cap-drift.sh、cap-scan.sh、chrome-cdp.sh、inventory.sh、inventory2.sh、llmtrim-health.sh、totp.js、link-selftest.cjs | 注册表只收了 refine_daily.py 和 chrome-nosandbox |
| 安全审计 | security-audit-skill/bin/run-audit.sh | /audit 路由的真实落点（见问题 9） |
| 技能（7 个 + 24 个） | browser-tools、github-get、lightpanda、pi-web-restart、reverse-engineer-anything、scrapling、tavily-search；24 个 steel-will 技能（29 个里只有 5 个进了注册表） | 5 个进注册表的是 long-term、adjudicator、retrieval、migration、forge |
| 停放的扩展（30 个） | extensions-parked/ 下 30 个 .ts | 未加载，按设计不算"有"，但用户问"三号有多少工具"时要说明这 30 个是 dormant 的 |
| 服务 | Xvfb（:99 在跑） | 不是 CLI，注册表无 service 类目给它 |
| 不算漏的 | docx/pdf/pptx/xlsx 技能 | 三号上也没有 |

## 对切流的直接含义

**测试机现在只能当"会分诊、会记账、会精炼"的空架子跑。** 任何真实抓取/审计/SEO/设计任务，在测试机上都会 resolve_error 或执行失败——不是模型问题，是工具根本没装。切流前生产机必须二选一：

- **A. 生产机 = 三号同款环境**：把三号容器里的工具链（39 个 CLI + seo-toolkit 脚本 + CAPABILITIES 文档 + 技能）完整搬到生产机（或直接让 007 跑在三号容器里/旁边），并修注册表（entry_point 补全路径、34 条非 cli 项标清不可执行或给执行路径）。这是"新二号"本来要的样子。
- **B. 生产机保持精简，007 远程调三号**：给 007 补一条远程执行通道（SSH 到三号容器里跑命令），工具继续长在三号身上，007 变成三号的前台。现在没有这条通道，要新做一步。

无论哪条路，先修的三件事（按优先级）：

1. entry_point 补路径/去描述性写法：content_quality.py、audit.py、camoufox、scrapling-mcp；并核对每一项的"探针通过 = 真能跑"。
2. CompactToolList 只列可执行项，或给 SOP/doc/tool 类加执行路径（否则模型挑中即失败）；/audit 前缀改挂 run-audit.sh（或另立条目）。
3. 探针按 call_type 分流：cli 查命令、service 查脚本文件、doc/sop/tool 不探针只记知识；l1_watcher/refine_daily 两条遗留项可删（功能已在框架内）。

另：部署机补 sqlite3（或文档写明用 python3 查账本）、部署同步带上 CHANGELOG.md、L2 ops.md 拆分（B-05，25 行已满）。

## 数字总汇

- 注册表 73 条：cli 39（测试机有 4、三号有 39）、sop 13、tool 9、doc 8、service 2、mcp 1、data 1。
- 测试机探针：72 查、6 ok（含 2 假阳性）、66 broken、1 跳过（playbooks，available:false）。
- 三号 CLI：注册表 39 个名字全在 + 额外 4 个（tesseract、magick、mpv、npm）= 43 个；另有 8 个 /data/bin 助手脚本、7+24 个技能、30 个停放扩展不在注册表。
- 账本：triage 20 行、expense 14 行（合计 12 毫分）、inbox 13 行全 DONE、index_metrics 0 行、execution_ledger 610 行（594 行是修复前的旧逐条探针失败行，修复后每启动只 1 行汇总）。
- 部署：二进制 go1.26.0、16,843,552 字节、2026-11-17 23:30；守护进程 PID 2476844、00:25:57 启动、6262 监听、日志 81 行无报错；磁盘 67G 可用、内存 3.2G 可用。
