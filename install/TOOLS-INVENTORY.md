# 007 工具与服务清单（生产机实测版）

> 生产机：狗云美国洛杉矶 VM124655（45.140.88.10），Debian 12，root。
> 清点时间：2026-11-19 21:39（007 已接管进 systemd 后复核）。
> 清点方式：逐项在生产机上实测版本号，不许凭记忆写；写“未核对”的就是没核对。
> 用途：换新服务器时按此清单重装。重装入口：`install/install-007.sh`。
>
> ⚠️ 本清单没有在干净服务器上真跑过全流程。`install-007.sh` 的步骤
> 是按生产机现状反推的，等有空闲服务器时必须真跑一遍再改这句话。
> 本文件不含任何密钥值、密码原文，只有键名。

## 新服务器重装步骤（按顺序）

1. 全新 Debian 12，root 登录。
2. 克隆公开库：`git clone https://github.com/ip9988001/007-super-agent`，
   克隆私密库：`git clone https://github.com/ip9988001/super-agent-007`
   （私密库需 GitHub token，token 不进仓库，在部署机上单独配置）。
3. 跑公开库的 `deploy/deps.sh`：系统依赖 + Go 1.27.4 + Ollama +
   完整工具链（`install-toolchain.sh`，版本钉死）。
4. 在公开库里 `go build -o super-agent ./cmd/super-agent`，
   二进制放到 `/root/super-agent-007/super-agent`。
5. 私密库 `scripts/seed-tools.sh`：把工具种子（含 47 个 skill）拷进
   `/root/super-agent-007/tools/`（已有文件不覆盖）。
6. 私密库 `scripts/apply-secrets.sh`：把 `secrets/deepseek.env` 落到
   `/etc/super-agent/.env`（0600，只落一次）；其余键按
   `install/super-agent.env.example` 补齐。
7. 首次部署才跑 `scripts/seed-memory.sh` 恢复种子记忆（已有数据时别跑）。
8. 装 systemd：`cp install/super-agent.service /etc/systemd/system/`，
   `systemctl daemon-reload && systemctl enable --now super-agent`。
9. 验证：`systemctl is-active super-agent` 为 active；`ss -ltnp` 见
   127.0.0.1:6262；`journalctl -u super-agent` 见 `registry ready: 88 entries`；
   `go version` 为 go1.27.4；`ollama list` 见 qwen3-embedding:0.6b。

## 0. 主机与 007 本体

| 项 | 版本 / 值 | 来源 | 路径 | 验证命令 |
|---|---|---|---|---|
| 服务器 | Debian 12，狗云 VM124655，美国洛杉矶 | 狗云 | — | `cat /etc/os-release` |
| 007 本体 | step2022t，二进制 md5 `22258d3472b82befb7997efd7b555924` | 本地交叉编译后传包 | `/root/super-agent-007/super-agent` | `md5sum super-agent` |
| 007 服务 | systemd unit `super-agent.service`，enabled，Restart=always | 本清单 install/ | `/etc/systemd/system/super-agent.service` | `systemctl status super-agent` |
| 007 端口 | 只监听 127.0.0.1:6262（POST /wake），不对外 | — | — | `ss -ltnp \| grep 6262` |
| 注册表 | 88 条（见第 7 节） | 007 自带 | `data/registry/registry.json` | `python3 -c "import json;print(json.load(open('data/registry/registry.json'))['total'])"` |
| 备份 | 旧二进制 `super-agent.bak-pre-systemd`（与现行同 md5） | 部署时备份 | `/root/super-agent-007/` | `md5sum super-agent.bak-pre-systemd` |

## 1. 语言运行时

| 项 | 版本 | 来源 | 路径 | 验证命令 |
|---|---|---|---|---|
| Go | go1.27.4 linux/amd64 | 官方 tarball → /usr/local/go | `/usr/local/go`，软链 `/usr/local/bin/go`、`/usr/local/bin/gofmt` | `go version` |
| Python | 3.11.2 | apt（Debian 12） | `/usr/bin/python3` | `python3 --version` |
| Node | v22.23.4（npm 10.9.9） | Node.js 官方二进制包解压到 /usr/local | `/usr/local/bin/node`、`/usr/local/bin/npm` | `node --version` |
| Java | openjdk 25.0.5 LTS | Temurin 二进制包 | `/data/tools/jdk25/bin/java` | `java --version` |
| uv | 0.13.5 | PyPI/官方包 | PATH | `uv --version` |
| yarn | 1.22.22 | npm 全局 | `/usr/local/lib/node_modules` | `yarn --version` |

## 2. 基础工具（apt / 官方包）

| 项 | 版本 | 来源 | 验证命令 |
|---|---|---|---|
| git | 2.39.5 | apt | `git --version` |
| curl | 7.88.1 | apt | `curl --version` |
| jq | 1.6 | apt | `jq --version` |
| FFmpeg | 5.1.9-0+deb12u1 | apt | `ffmpeg -version` |
| ripgrep（rg） | 13.0.0 | GitHub release | `rg --version` |
| gh | 2.103.2 | GitHub release | `gh --version` |
| pandoc | 2.17.1.1 | apt/官方包 | `pandoc --version` |
| tesseract | 5.3.0 | apt | `tesseract --version` |
| ImageMagick（magick） | 6.9.11-60 | apt | `magick --version` |
| mpv | 0.35.1 | apt | `mpv --version` |

## 3. 代码审计工具

| 项 | 版本 | 来源 | 验证命令 |
|---|---|---|---|
| rea（rea-agents） | 6.8.0 | npm 全局 | `rea --version` |
| codegraph | 1.6.2 | npm 全局 | `grep '"version"' /usr/local/lib/node_modules/@colbymchenry/codegraph/package.json` |
| semgrep | 未确认（`--version` 无输出） | pip | 待核对 |
| ast-grep | 0.51.0 | GitHub release | `ast-grep --version` |
| trivy | 0.76.1 | GitHub release | `trivy --version` |
| gitleaks | 8.30.1 | GitHub release | `gitleaks version` |
| bandit | 1.9.4 | pip | `bandit --version` |
| pip-audit | 2.10.1 | pip | `pip-audit --version` |
| ghidra | 12.2（install-toolchain.sh 钉死值，未单独核对） | NSA/ghidra | `ls /data/tools/ghidra` |

## 4. 浏览器与抓取

| 项 | 版本 | 来源 | 路径 / 备注 | 验证命令 |
|---|---|---|---|---|
| Playwright | 1.64.1（npm 全局）；pip 侧 playwright 1.62.0 | npm / pip | `/usr/local/lib/node_modules/playwright` | `npm ls -g --depth=0` |
| Chromium | chromium-1248 + headless_shell-1248，Chrome for Testing 156.0.8078.4 | Playwright 下载 | `/root/.cache/ms-playwright/chromium-1248/chrome-linux64/chrome` | 上述路径 `--version` |
| lightpanda | 1.1.0-nightly.12226+88fb36db0 | GitHub release | `lightpanda` | `lightpanda version` |
| camoufox | 0.5.9 | pip | — | `pip3 show camoufox` |
| scrapling | CLI 0.5（`/usr/local/bin/scrapling`）；MCP `/root/.local/bin/scrapling-mcp`；Python 在 pipx venv 里（系统 python3 导不进） | pipx | `/root/.local/bin/scrapling*` | `scrapling --version` |
| puppeteer-core | 25.18.1 | npm 全局 | `/usr/local/lib/node_modules/puppeteer-core` | `npm ls -g --depth=0` |
| httpx | 0.28.1（pipx） | pipx | `/root/.local/bin/httpx` | `httpx --help` |
| yt-dlp | 2026.10.21 | pipx/官方包 | PATH | `yt-dlp --version` |
| edge-tts | 7.3.1 | pipx | `/root/.local/bin/edge-tts` | `edge-tts --version` |
| agent-reach | 1.6.0（`--version` 不支持，用 `agent-reach help` 验） | npm/源码 | `/root/.local/bin/agent-reach` | `agent-reach help` |
| chrome-with-nosandbox | 包装脚本（加 --no-sandbox --disable-dev-shm-usage） | 007 自带 | `/root/super-agent-007/tools/scripts/chrome-with-nosandbox` | `echo $CHROME_PATH`（daemon 环境） |
| tvly（Tavily CLI） | 1.0.1 | npm（官方安装脚本 https://cli.tavily.com/install.sh） | `/usr/local/bin/tvly`；已用 /etc/super-agent/.env 里的 TAVILY_API_KEY 登录，凭证存 /root/.tavily/config.json（600） | `tvly --status`（应显示 Authenticated）、`tvly search "x" --json --max-results 3` |

## 5. 设计 skill（2026-11-19 装）

| 项 | 版本 | 来源 | 路径 |
|---|---|---|---|
| Hallmark | v2.0.0，commit 04977e1，50 主题 | Nutlope/hallmark（MIT） | `tools/skills/hallmark` |
| taste-skill | design-taste-frontend v2.5.1，commit f6b7657 | Leonxlnx/taste-skill（MIT） | `tools/skills/design-taste-frontend` |
| HyperFrames | CLI 0.9.63，核心集 6 个 skill（hyperframes、hyperframes-cli、hyperframes-registry、media-use、reverse-any-video、style-sampling） | heygen-com/hyperframes（Apache-2.0） | `tools/skills/hyperframes*` + `/usr/local/bin/hyperframes` |
| impeccable | 无版本文件（私有种子里的技能文件，版本号未记录） | 私有种子 | `tools/skills/impeccable` |

## 6. 007 技能文件（47 个目录）

全部在 `/root/super-agent-007/tools/skills/`，来源为私密库 `tools-seed/skills`
（`scripts/seed-tools.sh` 恢复）：

agent-reach、browser-tools、design-taste-frontend、edge-tts、github-get、
hallmark、hyperframes、hyperframes-cli、hyperframes-registry、impeccable、
lightpanda、media-use、pi-web-restart、reverse-any-video、
reverse-engineer-anything、scrapling、style-sampling、tavily-search，
以及 29 个 steel-will-*：adjudicator、autonomic-nervous、backwrite、
bootstrap、candidate-screener、clone-imprint、experience-injector、
failure-learner、forge、gateway、governance、hallucination-eliminator、
heartbeat、l1-logger、long-term、memory-skeleton、migration、ontology、
reflection、retrieval、scheduler、scheduler-v2、security、
semantic-retrieval、sensory、subconscious、swarm、transcendence、
vector-radar。

另有 `tools/scripts/`：chrome-with-nosandbox、chrome-nosandbox、cap-drift.sh、
cap-scan.sh、chrome-cdp.sh、run-audit.sh、sa007-camoufox-fetch.py、totp.js、
link-selftest.cjs。注意：`tools/scripts/` 下还有 inventory.sh、inventory2.sh
两份来历不明的临时文件，待清理。

## 7. 注册表（88 条）

按分类：ops 16、memory 14、fetch 13、design 11、code 8、seo 8、runtime 7、
media 6、security 5。按子分类：ops-mgmt 16、memory-system 9、lang 7、
browser 6、doc-media 6、visual-check 5、scan 5、knowledge 5、http 4、
search 4、content 4、seo-tools 4、ui-gen 4、platform 3、reverse 2、audit 2、
video-compose 2。注册表全英文，含中文的条目 0 条。

## 8. 服务与定点任务

| 项 | 状态 | 备注 |
|---|---|---|
| super-agent | systemd，enabled，active，Restart=always | `/etc/systemd/system/super-agent.service` |
| ollama | systemd，enabled，active（2026-11-18 21:03 起） | `/etc/systemd/system/ollama.service`，模型 qwen3-embedding:0.6b（639 MB） |
| 定点任务 | 守护进程内循环（无 crontab） | 02:30 学习、04:00 精炼、04:30 自查（04:30 已在启动日志确认） |
| 记忆备份 | enabled，每天 04:00 精炼后自动推私密库 | 私密库 `memories/VM124655/` |
| 锻造试用期 | forge-seo-site-audit，trial，真实调用 0 次 | `data/forge/trials/forge-seo-site-audit.json` |

## 9. 环境变量（只列键名，无值）

`/etc/super-agent/.env`（0600，root）：DEEPSEEK_API_KEY、WORKER_MODEL、
ADVISOR_MODEL、ADVISOR_MODE、TIMEOUT_SEC、MAX_OUTPUT_CHARS、GITHUB_TOKEN、
TAVILY_API_KEY、TAVILY_PASSWORD、BACKUP_ENABLED、BACKUP_REPO、
BACKUP_SERVER_ID、BACKUP_TOKEN_KEY、BACKUP_REPO_DIR、CHROME_PATH。
密钥来源：DeepSeek 键在私密库 `secrets/deepseek.env`；GitHub token 与
Tavily 键不进仓库，在部署机上单独配置。详见 `install/super-agent.env.example`。

## 10. 数据目录

| 项 | 大小 / 数量 | 路径 |
|---|---|---|
| ledger.db | 1.9M | `data/ledger.db` |
| registry 目录 | 580K | `data/registry/` |
| data/ 下 md 文件总数 | 1845（含 wiki 与索引） | `data/` |
| vectors.db | 路径与大小本次未核对（待补） | 待核对 |

## 11. 同机业务（部署 007 时不动）

| 项 | 状态 | 备注 |
|---|---|---|
| xray | systemd，enabled，active（2026-11-06 起） | 用户直播业务，绝不动 |
| nginx | systemd，enabled，active（2026-11-03 起） | 监听 80/443，用户直播业务，绝不动 |
| python3 监听 8099 | 127.0.0.1:8099 | 同机其他服务，部署 007 时不动 |

## 12. 已知缺口（重装时注意）

- tvly（Tavily CLI）已装（2026-11-19 22:08）：1.0.1，/usr/local/bin/tvly，已用 API key 登录并实测搜索通过；注册表 cap-fetch-tvly 已启用（available=True），/wake 实测 10 秒 DONE，费用 ¥0.003。
- semgrep `--version` 无输出，未确认是否可用。
- ghidra 版本未单独核对，12.2 是 install-toolchain.sh 的钉死值。
- scrapling 的 SKILL.md 标注版本 0.4.15，与 CLI 实测 0.5 不一致。
- impeccable 没有版本文件，版本号未记录。
- `tools/scripts/` 下 inventory.sh、inventory2.sh 两份临时文件来历不明，待清理。
- 重启后自启未实测（不能重启生产机）；下一次虚拟机替换或重启时验证。
