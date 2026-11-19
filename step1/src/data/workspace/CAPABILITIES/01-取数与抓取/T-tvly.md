# tvly (Tavily CLI)
## 一句话
Tavily 的官方命令行：search / extract / crawl / map / research 一把梭，`--json` 适合给 agent 用。

## 实际效果（实测 2026-10-26）
- `tvly --version` → `tavily v0.1.8` ✅（在 `/data/tools/uv/bin/tvly`，不在默认 PATH，需先 export PATH）
- `tvly --status` → `Not authenticated  Run: tvly login`
- `tvly search "hello" --json` → ✅ **未登录也能拿到结果**（返回 wikipedia 条目 + content 摘要，说明有公共/试用额度）——但成功率和配额未保证
- 库位：`/data/tools/uv/bin/tvly`（同目录还有 scrapling / agent-reach / playwright / bili / edge-tts）

## 怎么调
```bash
export PATH=/data/tools/uv/bin:$PATH
tvly --status                      # 看是否已登录
tvly search "关键词" --json --max-results 10          # 结构化搜索结果（url/title/content）
tvly search "AI" --json --topic news --time-range week --depth advanced
tvly search "AI" --json --include-raw-content markdown  # 带网页正文
tvly extract <url> [<url>...]      # 正文抽取，最多 20 个 URL，可 --query 重排
tvly crawl <url>                   # 站内爬取
tvly map <url>                     # 列站点 URL
tvly research "课题"                # 多步研究汇总
# 认证（当前未做）：tvly login  或  tvly login --api-key tvly-xxx  或 export TAVILY_API_KEY=...
```

## 坑
- **不在 PATH 里**：直接 `tvly` 会 command not found，必须先 export 或用绝对路径。
- 未登录能出结果是**设计如此**（help 原文：Works without an API key, subject to a rate-limit cap），登录只是去掉限流；但上限未公开，批量别裸跑。
- 每次调用消耗 Tavily 配额（登录后按套餐计），批量前先 `--status` 确认。
- 输出默认带人类可读格式，脚本里务必加 `--json`。
- 搜索类需求优先级：pi 自带搜索 > `agent-reach web search`（exa）> tvly。tvly 只作补充/交叉验证。
