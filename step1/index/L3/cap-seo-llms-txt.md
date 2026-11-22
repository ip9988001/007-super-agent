# cap-seo-llms-txt

## 一句话
给 LLM 读的站点摘要文件（`<domain>/llms.txt`）。容器里没有生成器脚本，只有模板 + 检测 + 规程；成品靠 `/seo --write` 的 prompt 让 pi 现写，或手写。

## 怎么调
# 查别人有没有、长什么样 python3 /data/workspace/seo-toolkit/geo-seo-aeo-skill/scripts/audit.py <url> \ | python3 -c "import json,sys;print(json.load(sys.stdin)['geo']['llms_txt'])" curl -sS https://target/llms.txt | head -20 # exists=true 时直接看正文 # 自己写：抄模板填 5 段 → 放站点根 → 自检 cp /data/workspace/seo-toolkit/geo-seo-aeo-skill/assets/llms-txt-template.md ./llms.txt # 规程细节按需读（别全读） sed -n '1,80p' /data/workspace/se…

## 坑
没有生成器 = 不可复现：同一站点两次让 agent 生成，段落数与措辞都会变；要稳定就落盘成项目里的 `llms.txt` 再迭代，别每次重生成。；检测只认 `<host>/llms.txt` 返回 200（10s 超时、跟重定向）；只放了 `llms-full.txt` 而没 `llms.txt` 会判 false —— 别据此说"这站没有 AI 友好文件"。

## 实际效果
模板：`geo-seo-aeo-skill/assets/llms-txt-template.md`（28 行，全 `{{占位}}` 结构：H1 品牌 → 一句话含可量化指标 → 可选上下文 → `## Core pa…；检测可用：`audit.py` 的 `geo.llms_txt` 返回 `{url, exists}`；example.com 与 python.org 实测均 `exists:false`（判定逻辑正常，不是恒真）

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）触发前缀 /seo；按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：模板：`geo-seo-aeo-skill/assets/llms-txt-template.md`（28 行，全 `{{占位}}` 结构：H1 品牌 → 一句话含可量化指标 →…；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/03-SEO与内容/L-llms-txt.md · 核实：2026-10-26（原文件 实际效果（实测 2026-10-26））
> 注意：容器里没有生成器脚本，只有模板+检测+规程；成品靠 prompt 现写或手写
