# llms.txt
## 一句话
给 LLM 读的站点摘要文件（`<domain>/llms.txt`）。容器里**没有生成器脚本**，只有模板 + 检测 + 规程；成品靠 `/seo --write` 的 prompt 让 pi 现写，或手写。

## 实际效果（实测 2026-10-26）
- **模板**：`geo-seo-aeo-skill/assets/llms-txt-template.md`（28 行，全 `{{占位}}` 结构：H1 品牌 → 一句话含可量化指标 → 可选上下文 → `## Core pages` / `## Documentation` / `## Insights` / `## Optional`（含 llms-full.txt））✅
- **检测可用**：`audit.py` 的 `geo.llms_txt` 返回 `{url, exists}`；example.com 与 python.org 实测均 `exists:false`（判定逻辑正常，不是恒真）
- **无生成实现**：全盘 grep `scripts/*.py` 没有"写 llms.txt"的代码，`agentic_check.py` / `agentic_fix.py` 只做检查与建议
- **规程散在 6 处**：`skills/{seo, seo-audit, seo-geo, seo-technical, seo-agentic, seo-local}/SKILL.md`，最细的是 `skills/seo-geo/references/llmstxt-evidence.md`
- **网关口径**：`/seo` 的输出格式第 3.c 项固定要「`llms.txt` 全文（若有则给修订版）」→ 每次现生成，不同次可能结构不同

## 怎么调
```bash
# 查别人有没有、长什么样
python3 /data/workspace/seo-toolkit/geo-seo-aeo-skill/scripts/audit.py <url> \
  | python3 -c "import json,sys;print(json.load(sys.stdin)['geo']['llms_txt'])"
curl -sS https://target/llms.txt | head -20      # exists=true 时直接看正文

# 自己写：抄模板填 5 段 → 放站点根 → 自检
cp /data/workspace/seo-toolkit/geo-seo-aeo-skill/assets/llms-txt-template.md ./llms.txt
# 规程细节按需读（别全读）
sed -n '1,80p' /data/workspace/seo-toolkit/claude-seo/skills/seo-geo/references/llmstxt-evidence.md
```

## 坑
- **没有生成器 = 不可复现**：同一站点两次让 agent 生成，段落数与措辞都会变；要稳定就落盘成项目里的 `llms.txt` 再迭代，别每次重生成。
- 检测只认 `<host>/llms.txt` 返回 200（10s 超时、跟重定向）；只放了 `llms-full.txt` 而没 `llms.txt` 会判 false —— 别据此说"这站没有 AI 友好文件"。
- `--file` 模式不带 `--base-url` 时 `llms_txt` 恒 false（不是缺失，是没 base 可比）。
- 模板里全是 `{{mustache}}`，直接复制上线会漏填；`## Optional` 段的 `llms-full.txt` 需另外生成，模板不产。
- 中文站没有现成中文模板，模板措辞是英文范式（"what you do + 可量化指标"），中译时别把可量化指标丢了——那正是 LLM 最先抓的一句。
