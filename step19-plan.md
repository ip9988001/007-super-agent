# 第 19 步计划：三号工具与技能清单 + 007 分批安装方案

> 清单来源：2026-11-18 01:00 三号服务器实测（容器 pi-agent，只读），与第 18 步核验交叉核对。
> 原则（用户 2026-11-18 00:57 定）：三号只当清单（装什么、哪个版本），来源不靠三号。
> 公开工具走 GitHub / 官方源，版本钉死跟三号一致；自家文件（技能、CAPABILITIES、design-md、
> seo-toolkit、助手脚本）一次性打包进私密库当工具种子，以后装机只从私密库拿，三号不在了也能重建。
> 30 个停放扩展只拷文件、不加载；l1_watcher / refine_daily 在 007 里已有框架实现，不用装。

## 一、三号清单（实测数）

### 1. 命令行工具：43 个（注册表 39 + 漏掉的 4 个）

| # | 工具 | 三号版本 | 上游来源（安装时走这里，不走三号） |
|---|---|---|---|
| 1 | curl | 系统自带 | apt |
| 2 | git | 系统自带 | apt |
| 3 | jq | 1.6 | apt |
| 4 | python3 | 3.11.2 | apt |
| 5 | node | v22.23.4 | NodeSource / 官方 |
| 6 | npm | 10.9.9 | 随 node |
| 7 | uv | 0.13.5 | 官方安装脚本 |
| 8 | yarn | 1.22.22 | npm |
| 9 | pandoc | 2.17.1.1 | apt / GitHub release |
| 10 | ffmpeg | 5.1.9 | apt |
| 11 | rg (ripgrep) | 13.0.0 | GitHub release |
| 12 | gh | 2.103.2 | GitHub release / apt |
| 13 | java | 25.0.5 | apt / Temurin |
| 14 | rea | 6.8.0 | GitHub 搜索 rea，按其仓库安装说明 |
| 15 | codegraph | 1.6.2 | GitHub 搜索 codegraph，按其仓库安装说明 |
| 16 | semgrep | 1.182.1 | pip |
| 17 | ast-grep | 0.51.0 | GitHub release |
| 18 | trivy | 0.76.1 | GitHub release / apt |
| 19 | gitleaks | 8.30.1 | GitHub release |
| 20 | bandit | 1.9.4 | pip |
| 21 | pip-audit | 2.10.1 | pip |
| 22 | ghidra | wrapper 可用 | GitHub release（官方） |
| 23 | lean-ctx | 在 | GitHub 搜索 lean-ctx，按仓库安装 |
| 24 | llmtrim | 在（:43117 服务） | GitHub 搜索 llmtrim，按仓库安装 |
| 25 | agent-reach | 1.6.0 | GitHub 搜索 agent-reach，按仓库安装 |
| 26 | bili | 在 | GitHub 搜索 bili，按仓库安装 |
| 27 | playwright | 1.64.1 | npm + 浏览器下载 |
| 28 | lightpanda | 1.1.0-nightly | GitHub release |
| 29 | scrapling | 0.5 | pip |
| 30 | tvly | 0.1.8 | pip（无 Tavily key，三号上同样跑不出数据） |
| 31 | httpx | 在 | pip |
| 32 | yt-dlp | 2026.10.21 | GitHub release / pip |
| 33 | chrome-nosandbox | 在 | Playwright Chromium 无沙箱启动 |
| 34 | camoufox | Python API，无 CLI | pip（camoufox） |
| 35 | edge-tts | 在 | pip / uvx |
| 36 | hyperframes | 技能在，从未实跑 | npx hyperframes |
| 37 | impeccable | 4.7.0（绝对路径可用，PATH 调用失败） | 按其仓库安装，绝对路径 |
| 38 | tesseract | 5.3.0 | apt |
| 39 | magick | 6.9.11 | apt |
| 40 | mpv | 0.35.1 | apt |
| 41 | content_quality.py | 脚本 | seo-toolkit 种子（见第 4 节） |
| 42 | audit.py | 脚本 | seo-toolkit 种子（见第 4 节） |
| 43 | claude-seo | 技能脚本 | seo-toolkit 种子（见第 4 节） |

> 注：tvly 无 key、GSC / DataForSEO 无凭据——三号上同样是坏的，装二进制修不好，等用户给 key。

### 2. 助手脚本：/data/bin 10 个 + 2 个

- cap-drift.sh、cap-scan.sh、chrome-cdp.sh、inventory.sh、inventory2.sh、llmtrim-health.sh、
  link-selftest.cjs、totp.js、refine_daily.py（007 已有框架实现，不装）、chrome-nosandbox
- 安全审计：/data/workspace/security-audit-skill/bin/run-audit.sh（/audit 路由的真实落点）
- 路由器 link-agent.cjs 本身不搬（007 用自己的分诊 + 注册表代替）；只搬它引用的脚本与文档。

### 3. 技能：40 个目录（/data/skills 实测）

- **steel-will 29 个**：adjudicator、autonomic-nervous、backwrite、bootstrap、candidate-screener、
  clone-imprint、experience-injector、failure-learner、forge、gateway、governance、
  hallucination-eliminator、heartbeat、l1-logger、long-term、memory-skeleton、migration、ontology、
  reflection、retrieval、scheduler、scheduler-v2、security、semantic-retrieval、sensory、subconscious、
  swarm、transcendence、vector-radar
- **其他 11 个**：agent-reach、browser-tools、edge-tts、github-get、hyperframes、impeccable、lightpanda、
  pi-web-restart、reverse-engineer-anything、scrapling、tavily-search
- 注册表只挂 5 个可调技能（long-term、adjudicator、retrieval、migration、forge）；
  其余 24 个 steel-will 技能文件全拷过来当参考资料，不挂注册表（14 个是基础代码、10 个是空壳/已废，
  全挂上去模型会挑到死工具——Gemini 第 6 轮已纠过）。
- 注：全盘搜到 335 个 SKILL.md 是多棵镜像树重复计数（/data/skills、/data/pi-home、
  /data/pi-iron-will、/root/.pi/agent 四处镜像同一批技能），正经技能集就是上面这 40 个目录。

### 4. 知识库与自家文件（进私密库当工具种子）

- CAPABILITIES：/data/workspace/CAPABILITIES，67 个 md
- design-md：/data/workspace/design-md-library/design-md，84 个品牌，167 个文件
- seo-toolkit：/data/workspace/seo-toolkit，545 个文件（含 content_quality.py、audit.py、llms-txt 模板）
- 技能文件：上面 40 个目录全文
- 助手脚本：上面 10 个 + run-audit.sh
- 停放扩展 30 个：文件拷进种子备查，不加载

### 5. 扩展与服务

- 扩展：7 个加载中 + 30 个停放（/root/.pi/agent/extensions、extensions-parked）
- 服务：Xvfb（:99 在跑，截图用）；l1_watcher / refine_daily 不装（007 框架内已有）

## 二、分批安装（逐步跟上，每批做完汇报一次）

| 批 | 内容 | 数量 | 验收 |
|---|---|---|---|
| 第 1 批 | 基础工具链：curl、git、jq、python3（已有 4 个）+ node、npm、uv、yarn、pandoc、ffmpeg、rg、gh、java | 13（新装 9） | 逐个 --version 对上三号版本 |
| 第 2 批 | 代码/审计：rea、codegraph、semgrep、ast-grep、trivy、gitleaks、bandit、pip-audit、ghidra、lean-ctx、llmtrim | 11 | 逐个 --version 对上；llmtrim 服务能起 |
| 第 3 批 | 抓取/浏览器：agent-reach、bili、playwright（+浏览器）、lightpanda、scrapling、tvly、httpx、yt-dlp、chrome-nosandbox、camoufox、edge-tts、hyperframes、impeccable、tesseract、magick、mpv | 16 | 逐个能跑；playwright 能开浏览器；camoufox 能 import |
| 第 4 批 | 技能与知识种子：40 个技能目录 + CAPABILITIES 67 + design-md 84 品牌 + seo-toolkit 545 文件 + 助手脚本 + 停放扩展文件，打包进私密库 memories/no2-seed 旁边的 tools-seed/，部署脚本解开 | 种子 1 份 | 私密库 HEAD 核对；干净克隆能解出全部文件，数量对上 |
| 第 5 批 | 注册表修复 + 全量核验：entry_point 补全路径、/audit 改挂 run-audit.sh、探针按 call_type 分流、补 4 个 CLI + 8 个脚本条目、删 l1_watcher/refine_daily 遗留项、L2 ops.md 拆分；重跑第 18 步核验 + 一个真实任务端到端 | — | 测试机探针 43/43 通过；真实任务不再 resolve_error |

## 三、安装目标与红线

- 装到测试机 VM124655（007 当前部署机），装法写成可重复的 install-toolchain.sh，以后生产机换了能一键重装。
- 全程不碰过夜守护进程（04:40 还要验它）；二号三号只读；不读、不打印任何密钥/token 值。
- 每批做完汇报一次（装了几个、版本号、失败几个），不攒到最后。

## 四、待用户确认

- 清单数对不对（43 工具、40 技能、7+30 扩展、67 CAPABILITIES、84 品牌、545 seo-toolkit 文件）；
- 分批顺序行不行；
- 确认后按第 1 批开始动手。
