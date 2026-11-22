# cap-fetch-tvly

## 一句话
Tavily 的官方命令行：search / extract / crawl / map / research 一把梭，`--json` 适合给 agent 用。

## 怎么调
export PATH=/data/tools/uv/bin:$PATH tvly --status # 看是否已登录 tvly search "关键词" --json --max-results 10 # 结构化搜索结果（url/title/content） tvly search "AI" --json --topic news --time-range week --depth advanced tvly search "AI" --json --include-raw-content markdown # 带网页正文 tvly extract <url> [<url>...] # 正文抽取，最多 20 个 URL，可 --query 重排 tvly crawl <url> # 站内爬取 tvly map <url> # 列站点 URL tvly research "课题" # 多…

## 坑
不在 PATH 里：直接 `tvly` 会 command not found，必须先 export 或用绝对路径。；未登录能出结果是设计如此（help 原文：Works without an API key, subject to a rate-limit cap），登录只是去掉限流；但上限未公开，批量别裸跑。

## 实际效果
`tvly --version` → `tavily v0.1.8` ✅（在 `/data/tools/uv/bin/tvly`，不在默认 PATH，需先 export PATH）；`tvly --status` → `Not authenticated Run: tvly login`

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`tvly --version` → `tavily v0.1.8` ✅（在 `/data/tools/uv/bin/tvly`，不在默认 PATH，需先 export PATH）；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/01-取数与抓取/T-tvly.md · 核实：2026-10-26（原文件 实际效果（实测 2026-10-26））
> 注意：未登录也有免费返回
