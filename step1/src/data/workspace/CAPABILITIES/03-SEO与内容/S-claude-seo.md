# claude-seo
## 一句话
26 个 SEO skill + 19 个 agent + 60 个 py 脚本的技能库（v2.5.0，AgriciDaniel/claude-seo）；所有脚本走 `/data/state/claude-seo/.venv` 托管环境，不污染系统 python。

## 实际效果（实测 2026-10-26）
- **入口链断了**：`/usr/local/bin/claude-seo` → 软链 → `scripts/claude-seo`，而后者已被覆盖成 2 行 `exec 自己` 的转发脚本 → 无限 exec 循环，零输出不退出（6s 未退 `rc=124`）❌
- **真启动器在 git HEAD**（75 行，自动挑 py 3.11–3.14）。取出来跑：`Runtime: ready / Python 3.11 / Chromium: ready` ✅ 说明坏只坏在被覆盖的那个文件
- **绕开坏启动器**：`python3 scripts/runtime.py doctor` 不带 env → `Runtime: setup required`（4 条 reason：missing + schema/sha/python changed）；带 `CLAUDE_SEO_DATA_DIR=/data/state/claude-seo` → `ready`，**51ms** ✅（不是重建，venv 现成 771MB、8 个 playwright 内核）
- `runtime.py run` 实测可用：`fetch_page.py https://example.com --json` → HTTP 200 + 完整 headers（Server: cloudflare，Content-Encoding: gzip）；`parse_html.py` / `content_quality.py` 均正常 ✅
- 规模核对：skills 26 ✅ / agents 19 ✅ / scripts 60 ✅ / extensions 9；plugin.json 版本 2.5.0
- **无外部凭据**：`/data/state/claude-seo` 只有 `.venv`、`ms-playwright`、`runtime-state.json`，全盘无 GSC/gcloud 密钥，MCP 也没装 dataforseo → seo-google / seo-maps / seo-dataforseo / seo-backlinks 只能读规程，出不了实时数据

## 26 个 skill 按能力分组（要细节才读 `<skill>/SKILL.md`，一次只读一个）
- 总控/全站：`seo`（总入口+路由表）· `seo-audit`（全站打分报告）
- 单页诊断：`seo-page`（一页六维）· `seo-technical`（爬/索引/CWV/渲染/IndexNow）· `seo-sxo`（意图错配）
- 结构化与索引：`seo-schema`（JSON-LD 检/验/生成）· `seo-sitemap` · `seo-hreflang`
- AI 可见性：`seo-geo`（AI Overviews/可引用性）· `seo-agentic`（Lighthouse Agentic + WebMCP + robots/Content-Signal）
- 内容：`seo-content`（E-E-A-T/薄内容/去 AI 腔）· `seo-content-brief` · `seo-cluster`（关键词聚类）· `seo-competitor-pages` · `seo-programmatic` · `seo-plan`
- 本地：`seo-local`（GBP/NAP/引文）· `seo-maps`（geo-grid/评论情报）
- 垂类：`seo-ecommerce` · `seo-images`（图片优化）· `seo-image-gen`（Gemini 出图，需 key）
- 外链：`seo-backlinks`（毒性/差距/disavow）
- 数据与监控：`seo-google`（GSC/PSI/CrUX）· `seo-dataforseo`（MCP）· `seo-drift`（基线/回归）· `seo-flow`（FLOW 框架）

## 怎么调
```bash
export CLAUDE_SEO_DATA_DIR=/data/state/claude-seo    # 硬前提，不设永远 setup required
cd /data/workspace/seo-toolkit/claude-seo
python3 scripts/runtime.py doctor                     # 51ms
python3 scripts/runtime.py run fetch_page.py <url> --json
python3 scripts/runtime.py run portability_check.py    # 校验 26 个 SKILL.md 跨 harness 合规
# 想恢复常规入口（本次未做，因要求不改配置）：git show HEAD:scripts/claude-seo > scripts/claude-seo && chmod +x scripts/claude-seo
```

## 坑
- **根因**：`/data/start.sh:93` 用 `printf ... > /usr/local/bin/claude-seo` 写包装脚本，而该路径**已是指向 `scripts/claude-seo` 的软链** → `>` 穿透软链把真启动器覆盖成自转发。每次开机重犯（现文件 mtime 13:10 = 今日开机时刻）。
- 仓库带 `.git` → `_is_plugin()` 判 False → 模式回落 `manual`，data_dir 取 repo 根 → `repo/.venv` 不存在 → 无 env 时永远 `setup required`。
- 三个 env 各管一段：`CLAUDE_PLUGIN_ROOT`（插件内相对路径）· `CLAUDE_SEO_DATA_DIR`（venv 落点）· `PLAYWRIGHT_BROWSERS_PATH` 被 runtime 内部改写为 `<data_dir>/ms-playwright`，与 start.sh 里的 `/data/cache/ms-playwright` 是两份（内容相同）。
- `runtime.py`、`consistency_check.py` 各有 1 行未提交改动（把 `${CLAUDE_PLUGIN_ROOT}` 注释换成绝对路径），不影响运行；但别 `git checkout` 它们。
- 26 个 skill 是**提示词规程**，不是可调用函数：pi 没有 subagent，得主 agent 串行按 SKILL.md 做，别以为调一下就能出全站报告。
