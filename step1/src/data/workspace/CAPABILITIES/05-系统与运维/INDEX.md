# 05 系统与运维 · 条目索引
> 挑一条，只读那个 md。组内按文件名首字母排序，组之间空一行。

## ① 代理与降噪
- L-lean-ctx.md :: 终端/文件上下文降噪 3.11.4（实测 `-c` 省 ~60% token；**rtk 已退役，由 lean-ctx 接管**）
- L-llmtrim.md :: 本地 MITM 压缩代理 0.13.4（`:43117` 健康；**529 = 探针协议误用，非故障**；判活用 `/data/bin/llmtrim-health.sh`）

## ② 运行时与包管理
- J-jdk25.md :: Temurin JDK 25.0.5（Ghidra 必需；**PATH 的 java 是 17**；jar 解包 / javap 反编译 / jwebserver 起静态服）
- N-node.md :: node v22.23.4（网关/扩展底座；全局 npm 包**不持久**）
- P-python.md :: python3 3.11.2（无 pyenv；pip 必须 `--break-system-packages`，pip 层重建即丢）
- U-uv.md :: uv 0.13.5（装 CLI 唯一入口；`UV_TOOL_DIR` 被 start.sh **设了两次**，工具存在两份）
- Y-yarn.md :: yarn 1.22.22 classic（与 npm 10.9.9 共用 node；**本机不用它**，两套 lockfile 会打架）

## ③ 命令行三件套
- E-edge-tts.md :: edge-tts 7.3.1 文字转 mp3（322 音色，zh-CN 8 个 ✅）；**edge-playback ❌ 缺 mpv**，用 ffplay 替代
- F-ffmpeg.md :: 5.1.9（转码/抽帧/抽音，实测出 mp3 正常；务必 `-loglevel error`）
- J-jq.md :: jq-1.6（**已装**，覆盖 01/C 那条"缺 jq"的旧结论；1.6 无 1.7 新特性）
- P-pandoc.md :: 2.17.1.1（md↔html/docx 实测通过；**PDF 需 latex 引擎，未验证**）

## ④ 安全审计（新装，与 02 互补）
- A-ast-grep.md :: 0.51.0 AST 搜索/改写（`run -p/-r` 可用；`scan` **必须**先 `ast-grep new`）
- B-bandit.md :: 1.9.4 Python 安全 AST linter（实测命中 B307 eval；B101/B404 噪声大）
- G-gitleaks.md :: 8.30.1 密钥扫描（实测命中 github-pat，有泄漏退出码 1；规则格式敏感）
- P-pip-audit.md :: 2.10.1 PyPI 依赖审计（无参 = 当前环境，**容易假安全**；项目必给 `-r`）
- T-trivy.md :: 0.76.1 依赖+配置扫描（实测 requirements 报 9 条 CVE；**无 DB 时静默放行且退出码 0**）

## ⑤ 凭据 · 引导 · PID1
- D-docker-init.md :: PID1 = `/sbin/docker-init`（`/usr/bin/docker-init` **不存在**）；无 systemd/cron
- G-gh-token.md :: gh 2.103.2 **已登录 ip9988001**（token 在 `/data/state/gh/token`，5000/h，实测可用）
- S-start-sh.md :: 引导脚本 225 行 / **14 个编号段，无 2a-2c**，2f-2j 排在 4 之后；`exec` 后 ps 会误判二次引导

## ⑥ 日志
- L-logs.md :: `/data/logs/`：boot / l1-watcher / l1-refine / llmtrim **存在**；`bot-gateway.log`、`claude-seo-setup.log` ❌ 不存在（网关只走 stdout）

> 过期纠正：`01-取数与抓取/G-gh-cli.md`「未登录，api 不可用」、`C-curl-jq.md`「jq 缺失」、
> `02-代码与审计/INDEX.md`「ast-grep/bandit/pip-audit/trivy/gitleaks 均不存在」——**三条结论在本机已全部过期**，以本类实测为准。
