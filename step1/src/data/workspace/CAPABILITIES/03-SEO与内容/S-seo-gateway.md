# /seo 网关路由
## 一句话
TG/企微 里打 `/seo [--deep] [--write] <URL|本地路径>` 走这条链：本地 `audit.py` 出确定性 JSON → 拼进 prompt → pi 出修复包；quick 180s / deep 420s，`--write` 才碰源码。

## 实际效果（实测 2026-10-26）
- 代码位置 `/data/bot-gateway.js:375-467`，常量 `SEO_TOOLKIT=/data/workspace/seo-toolkit`（:33）。网关进程在跑（`ps` 有 1 个 node）✅
- **第一段（打分）**：URL 目标 → `python3 geo-seo-aeo-skill/scripts/audit.py <url>`，超时 120s → 单独复跑已验证能吐完整 JSON ✅
- **第二段（体检）**：`cd claude-seo && ./scripts/claude-seo doctor 2>&1 | head -4` ❌ 该启动器已被覆盖成 2 行自转发脚本 → 无限 exec 循环、零输出、不退出（实测 6s 未退 `rc=124`）。这一句**没传 timeoutMs**，吃满 `run()` 的默认 180s 才被 SIGKILL → 每次 `/seo` 白等 3 分钟，喂给 prompt 的体检段是空字符串
- **第三段（出包）**：quick 180s / deep 420s 的 pi 调用；deep 模式提示词里写着"必要时用 `scripts/claude-seo run <脚本>`"——同样不可用 ❌
- 净结论：**quick 模式能出活**（audit JSON 有、少一行体检不影响），**deep 模式名不副实**（承诺的 6 个 skill 规程串行 + run 脚本都不发生，退化成一次长 prompt）
- `/stats` 里也有同一句 doctor（`timeoutMs:20000`）→ 那 20s 空转后被 Kill，整段工具版本行一起丢

## 怎么调
```bash
# 想复现网关那两段（不经过网关）
cd /data/workspace/seo-toolkit
python3 geo-seo-aeo-skill/scripts/audit.py https://target              # 段一
export CLAUDE_SEO_DATA_DIR=/data/state/claude-seo
python3 claude-seo/scripts/runtime.py doctor                           # 段二的正品，51ms
# 网关侧（需要 TG/企微 入口）
/seo https://example.com            # quick：audit + 一次 pi
/seo --deep https://example.com     # 现在等于 quick 加长版，别指望多跑规程
/seo --write /data/workspace/proj   # 会改源码，先复制 .bak-YYYYMMDD-HHMM
```
本地路径目标：先找 `index.html / public/index.html / dist/index.html / src/index.html / out/index.html`，找到就 `--file`，找不到只把目录清单喂给 pi（不打分）。

## 坑
- doctor 那步 180s 是**纯浪费**，不是"首次重建 venv 慢"——venv 早就在（`/data/state/claude-seo/.venv`，771MB），坏的是启动器本身（根因见 `S-claude-seo.md`）。
- 目标站开 gzip/brotli 时 audit 全零（python.org 实测 title/h1/links 全 null），网关不会兜底，pi 仍然会基于空数据写修复包——**先自己 curl 一版看是否正常**。
- `--write` 的 .bak 是提示词要求 agent 做，不是路由代码强制；agent 不听话就没有备份。
- 输出被 `clip(pr.text, 9000)` 截断，长报告末尾的"下一步"可能被切。
- 本路由与 `/design`（见 `D-design-md.md`）互不影响，同一进程里两个独立分支。
