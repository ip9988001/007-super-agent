# 第 19 步计划 v2：到底装哪些工具、装哪些 skill（已过滤）

> 用户 2026-11-18 01:05 纠正：v1 清单把之前说过不要装的东西又列进去了。
> 本版按审计实测证据逐项过筛：只装真能干活的；死的、没验证的、不属于 007 的，全部单列不装。
> 证据来源：~/workspace/pi-audit/verification-20261117.md（已验证项 4-1/4-2/4-5）、
> no3-hands.md §4–§5、no3-tools.md、super-agent-design.md、第 18 步核验报告。

## 一、装的工具：39 个

> 43 个实测在三号上的命令行 − 4 个不装的 = 39 个。

| 批 | 工具 | 三号版本 | 备注 |
|---|---|---|---|
| 基础 13 | curl、git、jq、python3、node、npm、uv、yarn、pandoc、ffmpeg、rg、gh、java | 见 v1 表 | curl/git/jq/python3 已在测试机上 |
| 代码审计 9 | rea 6.8.0、codegraph 1.6.2、semgrep 1.182.1、ast-grep 0.51.0、trivy 0.76.1、gitleaks 8.30.1、bandit 1.9.4、pip-audit 2.10.1、ghidra | 见 v1 表 | ghidra 只验过 wrapper，未跑过真实样本，装完要实测；codegraph 的库只在 rea 目录有 |
| 抓取浏览 14 | agent-reach 1.6.0、playwright 1.64.1、lightpanda 1.1.0-nightly、scrapling 0.5、httpx、yt-dlp 2026.10.21、chrome-nosandbox、camoufox（Python API）、edge-tts、hyperframes、impeccable 4.7.0、tesseract 5.3.0、magick 6.9.11、mpv 0.35.1 | 见 v1 表 | impeccable 只绝对路径可用；hyperframes 三号上从未实跑，装完要实测 |
| SEO 脚本 3 | content_quality.py、audit.py、claude-seo | 脚本 | 在 seo-toolkit 种子里，随第 4 批来 |

## 二、不装的工具：4 个（逐条有证据）

| 工具 | 为什么不装 | 证据 |
|---|---|---|
| lean-ctx | 已死 | 累计只省 1.2K token，最后使用 2026-10-26，22 天零使用；能力表写的 60% 只有一个单样本、无累计口径支撑（审计已验证 4-2、4-3） |
| llmtrim | 已死 | 实测只省 5.43%（88 次），最后压缩 2026-11-01，此后 16 天零新增；007 的省 token 靠三级索引（实测 13.3 倍），不需要它（审计已验证 4-1） |
| tvly | 没 key，装了也跑不出数据 | 三号上同样无 Tavily key，出不了真数据；等用户给 key 再装 |
| bili | 没验证过它真能干活 | CLI 在，但最小调用未出数据；先不装，验证过能出数据再补 |

## 三、装的 skill：14 个

- **注册表 5 个**（已在 007 注册表里，补 SKILL.md 文件到机器上）：long-term、adjudicator、retrieval、migration、forge
- **工具型 9 个**（每个背后都有一个上面要装的真工具）：agent-reach、browser-tools、edge-tts、github-get、hyperframes、impeccable、lightpanda、reverse-engineer-anything、scrapling

## 四、不装的 skill

| skill | 为什么不装 | 证据 |
|---|---|---|
| 24 个 steel-will skill（除上面 5 个外） | 不是真技能，是说明书 | ① 三号上 7 个记忆子系统目录全是空目录（swarm_matrix、sensory_cortex、reflections、autonomic_nervous、forge、subconscious、transcendence，2026-10-25 建后 0 文件）；② 网关日志 grep `steel-will` = 0 行，从未被调用过；③ 其中一部分功能 007 框架里已经有了（L1 记录、记忆分层、调度、网关、心跳、召回），再装一遍是重复；④ 4 个治理 skill 只有文档，扩展和策略文件没接线。SKILL.md 文件只存档进私密库当资料，不注册、不计数 |
| tavily-search | 对应 tvly，没 key | 同上，等 key |
| pi-web-restart | 三号专属运维 | 重启的是 Pi 的 web 服务，007 没有这个服务 |
| 30 个停放扩展 | 人为精简掉的 | 2026-10-26 人工精简，只留 7 个在用；文件可存档备查，不加载 |

## 五、装的助手脚本：9 个

cap-drift.sh、cap-scan.sh、chrome-cdp.sh、inventory.sh、inventory2.sh、link-selftest.cjs、totp.js、
chrome-nosandbox、run-audit.sh（/audit 的真实落点）。
不装：llmtrim-health.sh（死工具的脚本）、refine_daily.py（007 框架里已有）。

## 六、装的知识文件（进私密库当工具种子，不靠三号）

CAPABILITIES 67 篇、design-md 84 个品牌（167 个文件）、seo-toolkit 545 个文件、
上面 14 个 skill 的 SKILL.md、9 个助手脚本、30 个停放扩展文件（只存档）。
种子与记忆种子并列：memories/no2-seed/ 旁边放 tools-seed/。

## 七、分批（不变，每批做完汇报一次）

1. 第 1 批：基础 13 个（新装 9 个）
2. 第 2 批：代码审计 9 个
3. 第 3 批：抓取浏览 14 个
4. 第 4 批：14 个 skill + 知识种子进私密库
5. 第 5 批：注册表修复（路径、/audit、探针分流、补漏）+ 全量核验 + 真实任务端到端

## 八、总数对账

- 装：39 工具 + 9 脚本 + 14 skill = 62 项
- 不装：4 工具（2 死 + 2 暂缓）+ 26 skill（24 steel-will + tavily-search + pi-web-restart）+ 30 停放扩展（存档不加载）+ 2 脚本（1 死 + 1 已有）
