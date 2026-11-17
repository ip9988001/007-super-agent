# cap-seo-design-md

## 一句话
84 个真实站点设计语言的 DESIGN.md（每份 4–44KB，VoltAgent/awesome-design-md · MIT）；`/design` 路由按品牌取 tokens + 铁律，或按它改造目标项目。

## 怎么调
L=/data/workspace/design-md-library ls $L/design-md | wc -l # 84 grep -n "stripe" $L/DESIGN-MD-INDEX.md # 先看一行摘要再决定读不读 sed -n '1,80p' $L/design-md/stripe/DESIGN.md # frontmatter tokens（最值钱的一段） grep -n "^## " $L/design-md/stripe/DESIGN.md # Overview/Components/Shapes… 按需跳读 python3 -c "import json;d=json.load(open('$L/design-md-index.json'));print([x['brand'] for x in d][:10])" # TG 侧：/design list…

## 坑
索引的「字体」列被硬截在 72 字符（airbnb 收在 "…display, body, n"），`airbnb` / `cohere` / `kraken` 的「半径」为空 —— 索引只能当目录，不能当设计依据，要…；子串匹配有歧义风险：`line` 命中 `linear.app`；想找 openai 会 MISS（库里近邻是 `x.ai` / `together.ai` / `claude` / `cohere`），MISS 时只…

## 实际效果
`design-md/` 下 84 个品牌目录，每个都有 DESIGN.md，零缺失；与 `design-md-index.json` 完全对齐（84 vs 84，无孤儿、无缺项）✅；`DESIGN-MD-INDEX.md` = 1 表头 + 84 行（品牌/大小/主色/字体/半径）✅

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）触发前缀 /design；按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`design-md/` 下 84 个品牌目录，每个都有 DESIGN.md，零缺失；与 `design-md-index.json` 完全对齐（84 vs 84，无孤儿、无缺项…；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/03-SEO与内容/D-design-md.md · 核实：2026-10-26（原文件 实际效果（实测 2026-10-26））
> 注意：84 个真实站点设计语言，每份 4–44KB
