# 007 注册表 v1.7 中文备份

> 全中文备份，与英文注册表 `step1/registry/registry-v1.7.json` 对照。平时 007 只读英文注册表；本文件供人查阅、审计、对照时用，不进模型提示词。

> 生成时间：2026-11-18；来源：v1.6 注册表 84 条逐条核查 + 测试机 `tools/skills/` 40 个 SKILL.md 实查。

## 一、9 个大类

### fetch 取数（13 条，可调 10）

从外部世界拿内容的工具。只要是 007 需要获取本机上没有的信息，就从这个大类里挑：抓一个网页、调一个 API、读一个 GitHub 仓库、搜一个平台、渲染一个需要执行 JavaScript 的页面。包含三种拿法：真浏览器（chrome-nosandbox、camoufox、lightpanda、playwright，用于需要执行 JS 的页面和被机器人验证挡住的站点）、底层 HTTP（curl + jq，用于快速、可脚本化的抓取和 JSON 解析）、平台入口（GitHub、YouTube、B站、抖音等平台的只读入口，用于结构化地读平台内容）。任务是“去网上拿点东西”时，先到这里找。不包含本地文件读取（那是代码和运维的事）、代码搜索（code）、记忆召回（memory）。

### code 代码（8 条，可调 7）

跟源代码打交道的工具。任务涉及读代码、找函数、理解一个程序是怎么工作的、反编译一个二进制文件、检查代码有没有安全问题时，从这个大类里挑。包含代码搜索（ripgrep、codegraph，用于在仓库里找代码）、逆向分析（Ghidra、REA，用于分析二进制和编译后的程序）、代码审计（Semgrep，用于按规则扫描源码找漏洞和坏模式）。任务是“理解这段代码”或“在代码库里找这个东西”时，先到这里找。不包含从网上拉代码（那是 fetch 的平台入口）、写新代码（写代码是主循环用文件工具直接做，不走注册表）。

### seo 搜索优化（7 条，可调 3）

分析和优化网站在搜索引擎与 AI 回答里的表现（SEO / GEO / AEO）。任务是检查一个网站的搜索表现、做 SEO 体检、给内容打分、生成帮助 AI 理解站点的摘要文件（llms.txt）时，从这个大类里挑。包含内容打分（可读性、关键词覆盖等纯文本工具）、站点体检（技术 SEO 检查、结构化数据）、claude-seo 技能包（一整套 SEO agent 与脚本）。任务是“让这个站排得更好”或“检查这个站的 SEO”时，先到这里找。不包含通用网页抓取（fetch）、UI 设计（design）。

### design 设计（11 条，可调 2）

生成、检查、改进视觉设计与用户界面。任务涉及做一个界面、按标准检查设计、肉眼验证一个页面、合成视频时，从这个大类里挑。包含 UI 生成（界面布局与设计语言）、视觉检查（截图比对、用 impeccable 引擎按设计规则体检）、视频合成。任务是“把这个做得好看”或“检查这个设计对不对”时，先到这里找。不包含 SEO（seo）、抓页面内容（fetch）。还有一条铁律：本项目的 DESIGN.md（从代码反推的规范）与参照库的 84 份 DESIGN.md（别人站点的分析）同名但绝不能混用，混用会污染下一次体检。

### security 安全（5 条，可调 5）

扫描代码与系统安全漏洞的工具。任务是找安全问题——硬编码密钥、注入漏洞、不安全的函数调用、错误配置时，从这个大类里挑。包含源码静态安全扫描（Bandit 专查 Python，Semgrep 规则覆盖多语言）及相关检查。任务是“这段代码安全吗”或“找漏洞”时，先到这里找。扫描器只报候选、不下结论，是否真有漏洞由 agent 判断。不包含通用代码搜索（code 的 search 子类）、系统管理（ops）。

### runtime 运行时（7 条，可调 7）

语言运行时与数据处理工具，是其它工具依赖的引擎。包含解释器与处理器：Python、Node.js、jq（JSON 处理器）、Java（JDK）、npm。这些本身不是任务——它们是跑脚本、处理数据的引擎。主循环需要跑一段 Python、解析 JSON、执行 JavaScript 时，运行时从这里来。任务需要用到某种语言时，先到这里确认运行时在不在。不包含脚本本身（脚本在 ops）、基于这些运行时搭建的应用。

### media 媒体（6 条，可调 6）

处理文档与媒体文件的工具——转换、变换、提取文字、生成语音。任务涉及非纯文本文件时，从这个大类里挑：转码视频（ffmpeg）、处理图片（ImageMagick）、从图片提取文字（Tesseract OCR）、转换文档格式（Pandoc）、播放音视频（mpv）、文字转语音（edge-tts）。任务是“转换这个文件”、“读出这张图里的字”、“把这段文字变成语音”时，先到这里找。不包含从网上拿媒体（fetch）、写代码（code）。

### ops 运维（16 条，可调 7）

操作系统与基础设施的工具——容器、服务、脚本、部署。任务是管服务器、跑容器、执行维护脚本、管理凭证、编排部署时，从这个大类里挑。包含容器管理（Docker）、GitHub 身份验证、运维脚本库（漂移检测、能力扫描、Chrome DevTools 协议等）。任务是“跑这个维护任务”或“管这台服务器”时，先到这里找。注意：部分脚本仍硬编码三号路径，在 007 上相关章节为空，读输出前先看各条目的备注。不包含安全扫描（security）、语言运行时（runtime）、媒体处理（media）。

### memory 记忆（14 条，可调 0）

记忆系统——007 如何存储、召回、精炼知识。这是一个系统类，不是可调工具类：14 条里 0 个能被模型直接调。主循环每轮自动调 Recall（取相关记忆），记忆系统在后台处理 wiki 页面、L1 日志、L3 精炼知识。这里的条目是记忆分层（L1/L2/L3）、wiki 结构（entities、concepts、sources、synthesis）、召回机制、5 个核心 skill（长期库、裁决、检索、迁移、锻造）的文档与内部工具。任务需要历史上下文或过去的经验时，主循环的 Recall 会处理，模型不需要挑记忆工具。这个大类存在是为了归档与审计，不是为了工具选择。

## 二、17 个子类

### code / audit 代码审计（2 条）

按规则扫描源码找问题。Semgrep 是静态规则扫描（漏洞模式、坏实践）；run-audit.sh 是完整审计入口（Cloudflare 安全审计 skill × Semgrep 双层）。与 security 大类的区别：这里是通用代码审计规则，security 是专查安全漏洞。

### code / reverse 逆向分析（2 条）

分析二进制与编译后的程序。Ghidra 是本地逆向平台（反编译、看汇编），REA 是逆向总控（175 个命令，覆盖二进制、固件、游戏文件）。任务是“这个程序是怎么实现的”且没有源码时用。Ghidra 跑 headless、不开 GUI；REA 靠环境变量注册 provider，目前 ghidra 与 jadx 已就绪。

### code / search 代码搜索（4 条）

在代码库里找东西。ripgrep 是主力（按内容搜，感知 .gitignore），codegraph 是代码图谱（把仓库索引成符号关系，按定义和引用查）。找函数、找调用、找某个字符串时用。注意：codegraph 在 007 上暂无现成索引，用前须先建索引。

### design / ui-gen UI 生成（4 条）

生成界面与设计语言。按品牌与目标生成 UI 布局、设计规范、DESIGN.md。任务是“做一个界面”时用。/design --write 会直接改目标源码，备份全靠 prompt 一句话，默认只读。截图验证的规程也登记在这里，打分检查见 visual-check。

### design / video-compose 视频合成（2 条）

视频合成。把素材合成为视频。hyperframes 是 HTML 即源码的视频合成框架：组合、动画、标题卡、字幕、配音、音频反应视觉与转场全用带 data-* 时序属性的 HTML + GSAP 时间线写，npx hyperframes 做 lint/inspect/preview/render。2026-11-19 已在测试机实测：CLI 0.9.63 init + render 出真 MP4（10 秒，29 KB），不再是待核实；另有 hyperframes-skill（核心 skill 集 6 个）工作流条目。

### design / visual-check 视觉检查（5 条）

肉眼验证设计。用浏览器截图、比对、按规则打分。任务是“这个设计对不对、好不好看”时用。impeccable 检测器是打分主力（离线、确定性），但它的 skill 工作流 24 个命令里只有 detect 真实现了；网关超时矩阵是机制说明文档，不是检查器。2026-11-19 新增 Hallmark（50 套主题，sloplint 真浏览器 8 宽度检查）与 taste-skill（design-taste-frontend，Figma/x.ai 水准美术指导 + 浏览器检查 + 完工门），两者都已实测跑通。

### fetch / browser 浏览器（6 条）

真浏览器通道。页面需要执行 JavaScript 才能出内容、或被 Cloudflare Turnstile 等机器人验证挡住时用。chrome-nosandbox 是通用无头 Chrome，camoufox 是反检测浏览器（能过 Turnstile），lightpanda 是轻量渲染，playwright 是自动化框架。只在 HTTP 抓不到内容时才上浏览器，浏览器比 curl 慢、贵。

### fetch / http 网络请求（4 条）

底层 HTTP 取数与解析。curl 抓网页、调 API，jq 解析 JSON。速度最快、成本最低，是默认首选。页面是静态 HTML 或 API 直接给 JSON 时用这个，不用上浏览器。GitHub CLI（gh）与 Python 库 httpx 也归在这里；Tavily CLI 已登记但无 key、不可用。

### fetch / platform 平台入口（3 条）

平台只读入口。GitHub、YouTube、B站、抖音等平台的结构化读取：读仓库、读视频信息、读榜单。平台有现成 CLI 或 API 时用这个，比裸抓网页更稳、字段更全。目标不是已知平台时，改用 http 或 browser 子类。

### media / doc-media 文档媒体（6 条）

文档与媒体处理。ffmpeg（音视频转码/抽帧/抽音）、ImageMagick（图片处理）、Tesseract（图片 OCR）、Pandoc（文档格式转换）、mpv（播放与探查）、edge-tts（文字转语音）。任务涉及非纯文本文件时用。流水线顺序：先抽取、再识读、再转换、最后发声。

### memory / knowledge 知识（5 条）

知识。wiki、playbooks 等知识条目。稳定知识的存放处：wiki 分 entities、concepts、sources、synthesis 四类，扩展总览记录 37 个 TypeScript 扩展，裁决 skill 处理内容冲突。注意：playbooks 目录在盘上不存在，是纸面约定。

### memory / memory-system 记忆系统（9 条）

记忆系统。recall 召回、L1/L3 精炼、5 个核心 skill 等记忆机制。主循环内置调用，不供模型挑选。recall 是每轮自动调的统一召回入口，wiki_upsert_page 是写稳定知识的入口，self-status、usage-report、detect-conflicts 报告系统自身状态。

### ops / ops-mgmt 运维管理（16 条）

运维管理。Docker 容器管理、GitHub 身份验证、运维脚本库（漂移检测、能力扫描、Chrome DevTools 协议等）。管服务器、跑维护任务时用。注意：原 ops-mgmt（8）与运维管理（8）是两组不同工具，已合并为一个子类；部分脚本硬编码三号路径，007 上相关章节为空；lean-ctx 与 llmtrim 已死、只存档；link-selftest 是三号专属。

### runtime / lang 语言运行时（7 条）

语言运行时。Python、Node.js、jq、JDK、npm、yarn、uv。跑脚本与处理数据的引擎。主循环需要执行某种语言时从这里取运行时。注意：yarn 已装但本项目不用，一律 npm；JDK 存在是因为 Ghidra 需要它。

### security / scan 安全扫描（5 条）

源码安全扫描。Bandit 专查 Python（eval、硬编码密钥等），gitleaks 扫泄露密钥，pip-audit 审 Python 依赖 CVE，trivy 扫容器与配置，ast-grep 按 AST 结构做搜索与改写。任务是“这段代码有没有安全漏洞”时用。扫描器只报候选，结论由 agent 下。

### seo / content 内容（3 条）

内容打分器。纯文本工具，吃 txt/md/html，输出可读性、关键词覆盖、结构评分。任务是“这篇内容写得好不好”时用。84 份 DESIGN.md 库是设计参照素材，TOOLKIT-INDEX.md 是 SEO 工具链路由表（有几处已过期）。

### seo / seo-tools SEO 工具（4 条）

SEO 体检与技能包。GEO/SEO/AEO 体检器（抓一页出 JSON 体检报告）、claude-seo 技能包（26 个 SEO skill + 19 个 agent + 60 个脚本）。任务是“这个站 SEO 怎么样”时用。注意：claude-seo 在 007 上缺 .venv 与数据目录，llms.txt 只有模板、没有生成器。

## 三、84 条工具定义

每条格式：名字：定语。定义。描述。长度 70-200 字。

### fetch / platform 平台入口

- agent-reach：多平台内容只读入口。一个 CLI 覆盖 GitHub、YouTube、V2EX、网页、B站、小红书、推特、Reddit、雪球、BOSS 等频道，输出全为 JSON。适合结构化拉取平台内容；任意网站改用 curl 或浏览器。（英文 one_liner 见注册表；调用类型 cli；可用；入口 `agent-reach`）

- bili：B站命令行客户端 0.6.2，只读热门、排行、搜索、UP 主、收藏、历史、动态，还能把视频音频抽成 ASR 可用的 WAV。点赞投币发动态须扫码登录。最小调用未出数据，未验证。[待核实]（英文 one_liner 见注册表；调用类型 cli；不可用；入口 `bili`）

- yt-dlp：视频元数据、字幕、下载利器，版本 2026.10.21。YouTube 主流程可用，但缺 JS runtime，部分格式与站点受限。（英文 one_liner 见注册表；调用类型 cli；可用；入口 `yt-dlp`）


### fetch / browser 浏览器

- browser-stack：两条真浏览器通道。chrome-nosandbox（PATH 上，--headless=new --dump-dom 最省事）与 Playwright 三内核（chromium/firefox/webkit，可交互）。curl 拿不到内容时用。（英文 one_liner 见注册表；调用类型 cli；可用；入口 `chrome-nosandbox`）

- camoufox：反检测浏览器 Camoufox 0.5.9 + 指纹生成器 fpgen，可绕过 Cloudflare Turnstile 等机器人验证。camoufox 这个 CLI 只管下载与切换浏览器，真正抓取要在 Python 里用 Camoufox(...)。（英文 one_liner 见注册表；调用类型 cli；可用；入口 `camoufox`）

- lightpanda：无渲染 JS 执行引擎（Zig 写的浏览器内核），能跑 JS 但不画界面，直接把页面吐成 markdown、html 或 semantic_tree。JS 渲染页、又不想上全浏览器时用。（英文 one_liner 见注册表；调用类型 cli；可用；入口 `lightpanda`）

- playwright-cli：Playwright 1.64.1 命令行。装内核、截图、存 PDF、起 MCP、看 trace 全通；headed 类（open/codegen）补装 xvfb+xauth 后也通，前提加 xvfb-run -a。（英文 one_liner 见注册表；调用类型 cli；可用；入口 `playwright`）

- scrapling-mcp：scrapling 的 MCP 门。stdio server，把同一套 scrapling 引擎包成 MCP 工具给 agent 用；与 CLI 是同一个包的两个入口。只在调用方讲 MCP 时用。（英文 one_liner 见注册表；调用类型 mcp；可用；入口 `stdio scrapling-mcp server`）

- scrapling：抓取框架 0.5。CLI + MCP 双通道，stealthy_fetch 带 TLS 指纹伪装，可绕过 Cloudflare Turnstile 等验证；反爬、stealth 渲染、会话保持都在这。curl 失败时用。（英文 one_liner 见注册表；调用类型 cli；可用；入口 `scrapling`）


### fetch / http 网络请求

- curl-jq：最底层 HTTP 取数。curl 7.88.1 可用，jq 1.6 已装，可直接管道解析 JSON。抓 HTML 要带全套浏览器请求头，只带 UA 常被回 40 字节空壳或验证页。（英文 one_liner 见注册表；调用类型 cli；可用；入口 `curl`）

- gh-cli：GitHub 官方 CLI 2.103.2。旧描述称未登录、无 GH_TOKEN、API 不可用，但备注称 2026-11-17 复核已登录，以复核为准。两者矛盾，待核实。[待核实]（英文 one_liner 见注册表；调用类型 cli；可用；入口 `gh`）

- httpx：Python HTTP 库 0.28.1，非独立命令，须在 Python 里调用。必须绕开 env 代理（trust_env=False），否则报 Invalid port。Shell 抓取用 curl。（英文 one_liner 见注册表；调用类型 cli；可用；入口 `httpx`）

- tvly：Tavily 官方命令行。search、extract、crawl、map、research 一把梭，--json 适合给 agent 用。无 Tavily key，三号上同样不可用。[待核实]（英文 one_liner 见注册表；调用类型 cli；不可用；入口 `tvly`）


### code / search 代码搜索

- codegraph：代码图谱 CLI 1.6.2（node:sqlite 内建后端）。把仓库索引成符号 + 调用图后按需查；007 上暂无现成索引，用前先对目标仓库建索引。（英文 one_liner 见注册表；调用类型 cli；可用；入口 `codegraph`）

- git：git 2.39.5，环境里唯一的 VCS。审计时主要用来查改了什么、谁改的、哪次提交引入的。查历史用它，搜代码用 rg；它是审计改动的起点。（英文 one_liner 见注册表；调用类型 cli；可用；入口 `git`）

- rg：ripgrep 13.0.0，代码搜索主力。.gitignore 感知 + 多线程 + UTF-8，比 grep 快一个量级。找字符串、函数、模式时用。（英文 one_liner 见注册表；调用类型 cli；可用；入口 `rg`）

- ext-codegraph：查询 pi 内置的代码图谱数据库，支持符号搜索、依赖分析、调用链追踪。与 cap-code-codegraph 同一真身（二进制），Go 版二合一，薄封装即工具。（英文 one_liner 见注册表；调用类型 tool；可用；入口 `codegraph`）


### code / reverse 逆向分析

- ghidra：本地逆向平台 12.2（PUBLIC），装在 /data/tools/ghidra。headless 分析器可跑（JDK 25），不用开 GUI。反编译二进制时用。（英文 one_liner 见注册表；调用类型 cli；可用；入口 `ghidra headless`）

- rea：逆向与二进制分析总控 CLI 6.8.0（175 命令 / 229 MCP 工具）。provider 靠环境变量注册，ghidra + jadx 已就绪，ida、hopper、kuna 缺。逆向任务的入口。（英文 one_liner 见注册表；调用类型 cli；可用；入口 `rea`）


### code / audit 代码审计

- semgrep：本地静态规则扫描 1.182.1（pip 装）。007 上无本地规则快照，默认走在线规则（需联网），离线扫要先下规则；规则库离线可跑，不用登录。（英文 one_liner 见注册表；调用类型 cli；可用；入口 `semgrep`）

- run-audit：安全审计入口（run-audit.sh）。Cloudflare security-audit-skill × Semgrep 双层审计：本地初筛出候选，再由 agent 对照规程研判。完整审计时用。（英文 one_liner 见注册表；调用类型 service；可用；入口 `tools/scripts/run-audit.sh`）


### seo / content 内容

- content-scorers：claude-seo 里三个纯文本内容工具（吃 txt/md/html，支持 stdin 与 --json）。content_quality.py 出 0-100 质量分，content_humanize.py 去 AI 腔，content_verify.py 查主张与引用缺口。（英文 one_liner 见注册表；调用类型 cli；可用；入口 `python3 tools/seo-toolkit/claude-seo/scripts/content_quality.py`）

- design-md：84 个真实站点设计语言的 DESIGN.md（每份 4-44KB，VoltAgent/awesome-design-md，MIT）。/design 路由按品牌取 tokens + 铁律，或按它改造目标项目。参照素材，非本项目规范。（英文 one_liner 见注册表；调用类型 data；可用；入口 `84 份 DESIGN.md 库`）

- toolkit-index：/data/workspace/seo-toolkit/TOOLKIT-INDEX.md（85 行），整条 seo 工具链的唯一路由表；/seo 路由每次都把它的前 140 行塞进 prompt。它是事实来源，但有几处已过期。[待核实]（英文 one_liner 见注册表；调用类型 doc；可用；入口 `TOOLKIT-INDEX.md 路由表`）


### seo / seo-tools SEO 工具

- geo-seo-aeo：纯标准库的确定性 SEO/GEO/AEO 体检器。抓一页输出 JSON 事实（布尔 + 计数，不含总分），零依赖、秒级、可复现。单页事实体检时用。（英文 one_liner 见注册表；调用类型 cli；可用；入口 `python3 tools/seo-toolkit/geo-seo-aeo-skill/scripts/audit.py`）

- llms-txt：给 LLM 读的站点摘要文件（<domain>/llms.txt）。容器里没有生成器脚本，只有模板 + 检测 + 规程；成品靠 /seo --write 的 prompt 让模型现写，或手写。（英文 one_liner 见注册表；调用类型 sop；可用；入口 `llms.txt 模板+规程`）

- claude-seo：26 个 SEO skill + 19 个 agent + 60 个 py 脚本的技能库（v2.5.0）。脚本在 tools/seo-toolkit/claude-seo/，走系统 python3；007 上无 .venv 与数据目录，setup/doctor 步会报缺。[待核实]（英文 one_liner 见注册表；调用类型 cli；可用；入口 `tools/seo-toolkit/claude-seo/scripts/claude-seo`）

- seo-gateway：/seo 路由。本地 audit.py 出确定性 JSON，拼进 prompt 后由模型出修复包；quick 180s、deep 420s，--write 才碰源码。完整 SEO 整改时用。（英文 one_liner 见注册表；调用类型 sop；可用；入口 `/seo 路由`）


### design / ui-gen UI 生成

- ui-visual：设计体检之外想用眼睛验时的两条通道。chrome-nosandbox --screenshot（一次成型 0.8s）和 Playwright（多视口、全页、回归比对）。肉眼验证时用。（英文 one_liner 见注册表；调用类型 sop；可用；入口 `chrome-nosandbox --screenshot / Playwright`）

- design-md-self：两套东西都叫 DESIGN.md。一个是给本项目立法（impeccable document 从代码反推，tokens 是规范），一个是抄别人的法（84 份真实站点分析，只是参照素材）。混用会直接污染下一次体检。（英文 one_liner 见注册表；调用类型 sop；可用；入口 `DESIGN.md 自产流程`）

- design-write：/design <品牌> <目标路径> --write。把 design-md 库的风格喂给模型，让模型直接改目标源码并把 DESIGN.md 复制进项目根；备份完全靠 prompt 一句话，网关零兜底。写盘分支风险最高。（英文 one_liner 见注册表；调用类型 sop；可用；入口 `/design --write`）

- ui-route：/ui [--deep] [--write] <URL|路径> [附加说明]。detect 确定性打分 → 规则聚合 → 模型出修复包，三段串行，纯只读（除非显式 --write）。（英文 one_liner 见注册表；调用类型 sop；可用；入口 `/ui 路由`）


### design / visual-check 视觉检查

- gateway-timeouts：网关超时矩阵（文档）。03 批报的 /stats 20s 丢工具版本行今天没能复现（实测 1.63s 全通），但机制确认成立：bash 复合命令吃满 20s 预算，超时即 SIGKILL、输出只剩已打印的行。（英文 one_liner 见注册表；调用类型 doc；可用；入口 `网关超时矩阵`）

- impeccable-skill：SKILL.md（119 行）+ reference/ 41 篇，一套给模型看的设计总监工作流提示词。24 个命令只有名字和规程，CLI 里除 detect 外一个都没实现。（英文 one_liner 见注册表；调用类型 sop；可用；入口 `SKILL.md 工作流`）

- impeccable：本地设计反模式检测器（引擎 0.1.16 / skill 4.7.0，二进制 21MB，整技能 23MB）。零 API、零联网、离线确定性；文件 0.06s、URL 2-29s。/ui 路由的打分完全靠它。（英文 one_liner 见注册表；调用类型 cli；可用；入口 `impeccable`）

- hallmark-skill：Hallmark v2 设计 skill（SKILL.md 154 行，50 套主题）。做看起来像设计过的网页，不像 AI 生成的；redesign 改版、audit 只出报告；sloplint 在真浏览器 8 个宽度检查。2026-11-19 实测：静态 2 FAIL/1 REFLEX/1 WARN，--render 5 张截图 3 FAIL/1 REFLEX/3 WARN。浏览器检查要 puppeteer-core + CHROME_PATH（chrome-with-nosandbox）。（英文 one_liner 见注册表；调用类型 sop；可用；入口 `SKILL.md: hallmark`）

- taste-skill：taste-skill design-taste-frontend v2.5.1（SKILL.md 381 行）。给 coding agent 的美术指导，做 Figma/x.ai 水准公司官网；一屏一块、左右对齐、真实照片；浏览器检查 + 完工门。2026-11-19 实测：consistency.mjs 真浏览器跑通，完整 PASS/WARN/FAIL 表。浏览器检查要 CHROME_PATH（chrome-with-nosandbox）。（英文 one_liner 见注册表；调用类型 sop；可用；入口 `SKILL.md: design-taste-frontend`）


### design / video-compose 视频合成

- hyperframes：HTML 即源码的视频合成框架。组合、动画、标题卡、字幕、配音、音频反应视觉与转场全用带 data-* 时序属性的 HTML + GSAP 时间线写，npx hyperframes 做 lint/inspect/preview/render。2026-11-19 已在测试机实测：CLI 0.9.63 init + render 出真 MP4（10 秒，29 KB）。（英文 one_liner 见注册表；调用类型 cli；可用；入口 `npx hyperframes`）

- hyperframes-skill：HyperFrames 核心 skill 集（6 个 skill：hyperframes、hyperframes-cli、hyperframes-registry、media-use、reverse-any-video、style-sampling）。HTML 写视频，渲染成确定性 MP4；主入口 /hyperframes。2026-11-19 实测：init + render 出真 MP4（10 秒，29 KB）。本地渲染免费，不用 API key。（英文 one_liner 见注册表；调用类型 sop；可用；入口 `SKILL.md: hyperframes`）


### security / scan 安全扫描

- ast-grep：按 AST 结构做搜索与改写（sg），比正则稳、比 semgrep 轻，天然支持 --update-all 批量重写。结构化代码搜索与重构时用。（英文 one_liner 见注册表；调用类型 cli；可用；入口 `ast-grep`）

- bandit：Python 源码安全 linter（AST 级）1.9.4，专抓 eval、subprocess(shell=True)、硬编码密码这类模式。扫 Python 安全问题时用。（英文 one_liner 见注册表；调用类型 cli；可用；入口 `bandit`）

- gitleaks：密钥与凭据硬编码扫描器（正则 + 熵）8.30.1，单二进制，离线即可跑。发布或分享代码前查泄露密钥时用；单二进制、离线即可跑。（英文 one_liner 见注册表；调用类型 cli；可用；入口 `gitleaks`）

- pip-audit：按 PyPI 漏洞库（OSV/PyPI advisory）审 Python 依赖的 CVE，官方 PyPA 出品 2.10.1。审当前解释器环境时用。（英文 one_liner 见注册表；调用类型 cli；可用；入口 `pip-audit`）

- trivy：依赖漏洞 + 配置错误扫描器（CVE 库 + misconfig policy），单二进制。扫容器镜像、文件系统与配置时用；CVE 库加 misconfig 策略双覆盖。（英文 one_liner 见注册表；调用类型 cli；可用；入口 `trivy`）


### runtime / lang 语言运行时

- jdk25：/data/tools/jdk25 = Temurin JDK 25.0.5（bin 下 30 个工具），是 Ghidra 12.2 的必需运行时；PATH 上的 java 就是它，007 上只有这一套。（英文 one_liner 见注册表；调用类型 cli；可用；入口 `java`）

- jq：命令行 JSON 切片的默认工具（容器内已装，1.6），不用再拿 python -m json.tool 顶。Shell 管道里处理 JSON 时用。（英文 one_liner 见注册表；调用类型 cli；可用；入口 `jq`）

- node：Node.js v22.23.4，网关、37 个 pi 扩展、全部前端工具链的唯一运行时。bot-gateway 与 link-agent 都跑在它上面。（英文 one_liner 见注册表；调用类型 cli；可用；入口 `node`）

- python：系统 Python 3.11.2（/usr/bin/python3），脚本层与 pip 层工具的底座，没有 pyenv/conda。常驻脚本都用它跑。（英文 one_liner 见注册表；调用类型 cli；可用；入口 `python3`）

- uv：uv 0.13.5，装 Python 命令行工具的唯一入口（uv tool install），工具落 /data 持久卷；是装 Python 命令行工具的唯一入口。（英文 one_liner 见注册表；调用类型 cli；可用；入口 `uv`）

- yarn：yarn 1.22.22（classic 1.x，不是 berry 2+），已装好，和 npm 10.9.9 共用同一个 node v22.23.4；本机与本项目不用它，一律 npm。仅备查。（英文 one_liner 见注册表；调用类型 cli；可用；入口 `yarn`）

- npm：npm 10.9.9（随 Node 22.23.4 官方包），Node 的包管理器；全局工具（rea、codegraph、playwright）都经它装。（英文 one_liner 见注册表；调用类型 cli；可用；入口 `npm`）


### media / doc-media 文档媒体

- edge-tts：edge-tts 7.3.1，借微软 Edge 在线 TTS 把文字转 mp3（322 音色，zh-CN 8 个）；edge-playback 的 mpv 依赖已补齐，但本容器无音频设备，真播出来是无声。生成语音用。（英文 one_liner 见注册表；调用类型 cli；可用；入口 `uvx edge-tts`）

- ffmpeg：音视频转码、抽帧、抽音的标配（Debian 12 包，已装，5.1.9）。媒体转换与抽取时用；转码、抽帧、抽音都归它，是音视频处理的标配。（英文 one_liner 见注册表；调用类型 cli；可用；入口 `ffmpeg`）

- pandoc：文档格式互转（md ↔ html/docx/latex）2.17.1.1，排版无关的纯结构转换器。换文档格式时用；md、html、docx、latex 之间纯结构转换。（英文 one_liner 见注册表；调用类型 cli；可用；入口 `pandoc`）

- magick：ImageMagick 6.9.11-60（apt 装）。图片格式转换、缩放、裁剪、拼图；本机 magick 是指向 /usr/bin/convert 的软链。（英文 one_liner 见注册表；调用类型 cli；可用；入口 `magick`）

- mpv：mpv 0.35.1（apt 装），音视频播放器；无头机上主要用来探媒体信息、抽音频，不开画面；转换媒体改用 ffmpeg；播放与探查是它的主业。（英文 one_liner 见注册表；调用类型 cli；可用；入口 `mpv`）

- tesseract：tesseract 5.3.0（apt 装），命令行 OCR，把图片里的文字认成文本；配 --oem 1 --psm 6 认排版文本最稳。（英文 one_liner 见注册表；调用类型 cli；可用；入口 `tesseract`）


### ops / ops-mgmt 运维管理

- docker-init：容器 PID 1 是 docker-init（tini 内核），唯一职责是收尸 + 转发信号，-- 后面才是真正入口。二进制在 /sbin/docker-init。仅作说明；007 用 systemd。（英文 one_liner 见注册表；调用类型 doc；可用；入口 `/sbin/docker-init`）

- gh-token：GitHub CLI 已登录 ip9988001，token 落在持久卷 /data/state/gh/token（40 字节），容器重建自动复登。凭据只落文件，不回显到聊天窗口。（英文 one_liner 见注册表；调用类型 sop；可用；入口 `gh 登录态持久化`）

- lean-ctx：终端命令与文件阅读的降噪/压缩网关（单二进制），rtk 已退役，由 lean-ctx 接管。实测累计省 1.2K token、22 天零使用；第 19 步 v2 过滤版不装，已死。仅存档。（英文 one_liner 见注册表；调用类型 cli；不可用；入口 `lean-ctx`）

- llmtrim：本地 MITM 压缩代理（127.0.0.1:43117），改写发往各家 LLM API 的请求体以少付 input token。实测省 5.43%，11-01 后 16 天零新增；改历史消息会击穿 DeepSeek 前缀缓存。已死，仅存档。（英文 one_liner 见注册表；调用类型 cli；不可用；入口 `llmtrim`）

- logs：查故障先看 /data/logs/，但只有 4/6 个候选日志真的存在，网关日志根本不落盘。仅作位置说明；缺失的日志是已知缺口，不是无错的证明。（英文 one_liner 见注册表；调用类型 doc；可用；入口 `/data/logs 位置表`）

- start-sh：/data/start.sh 是容器唯一引导脚本，负责重建后自愈 + 拉起全部常驻进程 + 前台跑网关。仅作说明；新 Agent 用 systemd，不直接复用。（英文 one_liner 见注册表；调用类型 doc；可用；入口 `start.sh 说明`）

- skill-migration：迁移 skill。跨环境无损迁移、Bootstrap 部署验收。SOP 文档可用；对应 .ts 停放未加载，Go 版需另写实现。规程，非可调工具。（英文 one_liner 见注册表；调用类型 sop；可用；入口 `SKILL.md: steel-will-migration`）

- skill-forge：锻造 skill。自造工具：蓝图 → 沙箱测试 → 列装。SOP 文档可用；对应 .ts 停放未加载，Go 版需另写实现。规程，非可调工具。（英文 one_liner 见注册表；调用类型 sop；可用；入口 `SKILL.md: steel-will-forge`）

- cap-drift：能力目录保鲜体检（108 行 bash）。0 token 机械比对命令在位、条目时效、版本快照，只输出差异。硬编码 CAP=/data/workspace/CAPABILITIES（三号路径），007 上第 2 节为空，只看第 1、3 节。（英文 one_liner 见注册表；调用类型 service；可用；入口 `tools/scripts/cap-drift.sh`）

- cap-scan：能力全集穷举扫描（48 行 bash）。不靠回忆，只看文件系统，把可执行文件扫成候选清单。还扫 /data/tools/*/bin（三号路径，007 上为空）；脚本类能力不在清单里。（英文 one_liner 见注册表；调用类型 service；可用；入口 `tools/scripts/cap-scan.sh`）

- chrome-cdp：启动带 CDP 的 Chromium（16 行 bash），供 browser-tools 技能连 127.0.0.1:9222 调浏览器。硬编码 /data/cache/ms-playwright/chromium-*（007 在 /root/.cache/ms-playwright），须改路径。（英文 one_liner 见注册表；调用类型 service；可用；入口 `tools/scripts/chrome-cdp.sh`）

- chrome-nosandbox：种子里的 chrome-nosandbox 副本（007 版）。自动找 ~/.cache/ms-playwright 下的 chromium，加 --headless=new 与 --timeout=60000 防页面卡住。实跑走 /usr/local/bin/chrome-nosandbox。（英文 one_liner 见注册表；调用类型 service；可用；入口 `tools/scripts/chrome-nosandbox`）

- inventory：环境盘点快照（46 行 bash）。系统与命令在位节有效；网关路由与 /data/bin 节在 007 上为空，别当成工具丢了。读 /data/bot-gateway.js、/data/tools/*（三号路径）。（英文 one_liner 见注册表；调用类型 service；可用；入口 `tools/scripts/inventory.sh`）

- inventory2：环境盘点完整版（55 行 bash）。inventory.sh 加长版，多了路由真实提取、skills 描述、工具版本三节。读三号路径，007 上 skills 在 tools/skills/。（英文 one_liner 见注册表；调用类型 service；可用；入口 `tools/scripts/inventory2.sh`）

- link-selftest：link 中继自测（17 行 node）。三号专属，007 上不可用，只存档。读 link.env、调中继，007 上无 link 服务。（英文 one_liner 见注册表；调用类型 service；不可用；入口 `node tools/scripts/link-selftest.cjs`）

- totp：TOTP 库（85 行 node）。xinxin-link 的 TOTP 算法，60 秒一步；零依赖，只用 node 内置 crypto。是库不是 CLI，须 require 后调 current(secret, unix秒)；secret 来自 link.env。（英文 one_liner 见注册表；调用类型 service；可用；入口 `tools/scripts/totp.js`）


### memory / knowledge 知识

- extensions-overview：/root/.pi/agent/extensions/*.ts 共 37 个 TypeScript 扩展，每个在 session_start（少数挂 session_end/tool_call）时注册工具或后台逻辑。只做注册与挂钩子，无常驻进程。（英文 one_liner 见注册表；调用类型 doc；可用；入口 `37 扩展总览`）

- playbooks：AGENTS.md 里写的 memory/wiki/playbooks/<任务族>/ 在本机不存在，剧本库目前是纸面约定，零实体。宪法里照剧本执行目前落空，先走工具索引。[待核实]（英文 one_liner 见注册表；调用类型 doc；不可用；入口 `—`）

- ext-wiki-upsert-page：创建或更新结构化 Steel Will wiki 页。与 cap-memory-wiki-upsert-page 同一实现、不同条目，二者选其一。（英文 one_liner 见注册表；调用类型 tool；可用；入口 `wiki_upsert_page`）

- ext-wiki-lint：对 Steel Will wiki 做健康检查并出 lint 报告。在 settings.json defaultTools 的 - 名单上；该前缀语义未验证，不据此判禁用。[待核实]（英文 one_liner 见注册表；调用类型 tool；可用；入口 `wiki_lint`）

- skill-adjudicator：裁决 skill。处理内容冲突、替代、过时、未回写。SOP 文档可用；对应 .ts 停放未加载，Go 版需另写实现。规程，非可调工具。（英文 one_liner 见注册表；调用类型 sop；可用；入口 `SKILL.md: steel-will-adjudicator`）


### memory / memory-system 记忆系统

- memory-layers：记忆全在 /root/.pi/agent/memory/（/data/pi-home/agent 是同一份的另一挂载点），按碎片 → 候选 → 长期 → 结构化四层落盘。仅作分层说明。（英文 one_liner 见注册表；调用类型 doc；可用；入口 `memory/ 分层说明`）

- recall-memory：跨 L1/L3/wiki 的统一召回入口，search_mode 可选 semantic / lexical / hybrid，默认 hybrid。主循环每轮调它取相关记忆；与扩展工具同一实现，勿重复注册。（英文 one_liner 见注册表；调用类型 tool；可用；入口 `recall_memory`）

- wiki-upsert-page：把稳定知识写成或更新成 wiki 页（4 类：entities / concepts / sources / synthesis），是记忆的结构化真值层。与扩展工具同一实现，勿重复注册。（英文 one_liner 见注册表；调用类型 tool；可用；入口 `wiki_upsert_page`）

- ext-recall-memory：搜 Steel Will 记忆系统。支持语义检索、词法兜底、混合模式。与 cap-memory-recall-memory 同一实现、不同条目，二者选其一。（英文 one_liner 见注册表；调用类型 tool；可用；入口 `recall_memory`）

- ext-self-status：一体式自述。记忆分布、检索资产、扩展开关、服务状态、定时任务、最新评测指标，一次全取；记忆分布、检索资产、扩展开关、服务状态、定时任务、评测指标都在内。（英文 one_liner 见注册表；调用类型 tool；可用；入口 `self_status`）

- ext-usage-report：记忆使用账本。最常被召回的记忆，以及从未被召回过的沉睡条目。在 settings.json defaultTools 的 - 名单上，语义未验证。[待核实]（英文 one_liner 见注册表；调用类型 tool；可用；入口 `usage_report`）

- ext-detect-conflicts：冲突暴露。给一段新内容，检索出可能与之矛盾的既有记忆，提示复核。不做自动判定，只负责把冲突摆到台面上。- 名单语义未验证。[待核实]（英文 one_liner 见注册表；调用类型 tool；可用；入口 `detect_conflicts`）

- skill-long-term：L3 长期库 skill。收录标准、主题路由、只读存储。SOP 文档可用；对应 .ts 停放未加载，Go 版需另写实现。规程，非可调工具。（英文 one_liner 见注册表；调用类型 sop；可用；入口 `SKILL.md: steel-will-long-term`）

- skill-retrieval：检索 skill。INDEX 导航 + 词法检索找信息。SOP 文档可用；对应 .ts 停放未加载，Go 版需另写实现。规程，非可调工具。（英文 one_liner 见注册表；调用类型 sop；可用；入口 `SKILL.md: steel-will-retrieval`）


## 四、40 个 skill 文件（单独归类，不进工具清单）

来源：2026-11-18 测试机 `tools/skills/` 实查，40 个 SKILL.md。分类按文件名与 SKILL.md 标题归类，内容未逐篇精读；6 个已进注册表的 skill（5 核心 + impeccable）保持注册表条目不变。

### core 核心（5 个）

- **steel-will-long-term**：L3 长期库：绝对固态黑匣与三级永久只读存储列阵。收录标准、主题路由、只读存储。

- **steel-will-adjudicator**：裁决：格式化风暴与无用字节抹除协议。处理内容冲突、替代、过时、未回写。

- **steel-will-retrieval**：检索：全息倒排树与高维特征指针。INDEX 导航 + 词法检索找信息。

- **steel-will-migration**：迁移：协议同化港与跨环境无损平滑迁移簇。跨环境无损迁移、Bootstrap 部署验收。

- **steel-will-forge**：锻造：零号工厂迭代车间与底层代码资产熔铸炉。自造工具：蓝图 → 沙箱测试 → 列装。


### browser 浏览器（3 个）

- **browser-tools**：浏览器工具操作规程：连 CDP 浏览器、截图、交互。

- **lightpanda**：lightpanda 无渲染 JS 执行引擎操作规程。

- **scrapling**：scrapling 抓取框架操作规程：stealthy_fetch、TLS 指纹伪装、会话保持。


### reverse 逆向（1 个）

- **reverse-engineer-anything**：逆向工程操作规程：二进制、固件、游戏文件分析。


### memory 记忆（6 个）

- **steel-will-memory-skeleton**：记忆骨架：降噪皮层与二级高密提炼晶体。L2 提炼层。

- **steel-will-semantic-retrieval**：语义检索：神经反射弧与语义检索技能。

- **steel-will-l1-logger**：L1 日志：游离态突触与一级碎片缓存区。碎片记录。

- **steel-will-backwrite**：回写技能：把结论写回记忆。

- **steel-will-candidate-screener**：候选筛选：逻辑淬火池与无损熵减转化引擎。筛记忆候选。

- **steel-will-vector-radar**：向量雷达：降维捕获网与向量级语义雷达。向量检索。


### platform 平台（3 个）

- **github-get**：GitHub 读取操作规程。

- **tavily-search**：Tavily 搜索操作规程（须 Tavily key）。

- **agent-reach**：agent-reach 平台内容读取操作规程。


### system 系统（22 个）

- **steel-will-autonomic-nervous**：自律神经：包27，认知过载抑制与动态 Token 压榨微调。

- **steel-will-bootstrap**：自举引导序列与依赖注入基座。

- **steel-will-clone-imprint**：克隆印记：包25，自动化克隆印记与任务模式图灵闭环。

- **steel-will-experience-injector**：经验注入：战术外骨骼与动态经验热插拔模块。

- **steel-will-failure-learner**：失败学习：致命反模式与失败经验隔离沙箱。

- **steel-will-gateway**：网关：绝对防火墙与神经网关鉴权层。

- **steel-will-governance**：治理：不可篡改哈希行为审计链与防错回滚轴。

- **steel-will-hallucination-eliminator**：幻觉剔除：包23，幻觉剔除手术与四重冗余逻辑校验器。

- **steel-will-heartbeat**：心跳：永续心跳引擎与进程守望者。

- **steel-will-ontology**：本体论：硅基统一本体论与全局标准数据链。

- **steel-will-reflection**：反思：状态机切片与项目跨度哈希环。

- **steel-will-scheduler**：调度：矩阵之眼与主代理调度控制中枢。

- **steel-will-scheduler-v2**：调度 v2：纳秒级全异步调度宏。

- **steel-will-security**：安全：硬件级锁死与权限安全断路器。

- **steel-will-sensory**：感知：碳基环境嗅探与第三方物理隔离舱。

- **steel-will-subconscious**：潜意识：包26，自发式编排与动态重规划算力栈。

- **steel-will-swarm**：蜂群：纳米蜂群裂变与多维子代理并发矩阵。

- **steel-will-transcendence**：超越：包29，幽灵复刻协议与跨域灾难重生序列。

- **edge-tts**：edge-tts 文字转语音操作规程。

- **hyperframes**：hyperframes 视频合成操作规程。

- **impeccable**：impeccable 设计检测操作规程。

- **pi-web-restart**：Pi-Web 服务重启：Pi-Web 服务重启规程。

