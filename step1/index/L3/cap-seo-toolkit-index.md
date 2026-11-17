# cap-seo-toolkit-index

## 一句话
`/data/workspace/seo-toolkit/TOOLKIT-INDEX.md`（85 行）是整条 seo 工具链的唯一路由表；`/seo` 路由每次都把它的前 140 行塞进 prompt。它是事实来源，但有几处已过期。

## 怎么调
sed -n '1,140p' /data/workspace/seo-toolkit/TOOLKIT-INDEX.md # 85 行，一次读完正好 grep -n "seo-geo\|seo-technical" /data/workspace/seo-toolkit/TOOLKIT-INDEX.md # 找 skill 的 SKILL.md 路径

## 坑
静态快照：仓库 HEAD 已到 `219ff48 docs(security): installer verification moves to v2.6.0`，索引仍按 v2.5.0 写；plugin.json 本身…；表里 skill 描述是从 SKILL.md frontmatter 机械截取的，部分行以 `>` 开头（如 `seo-agentic`、`seo-cluster`），照抄会把 `>` 带进输出。

## 实际效果
26 个 skill 的「skill → SKILL.md 路径」列是准的，按需读单文件即可 ✅；结论：路由表可用、入口命令不可用；把它当目录，别当执行手册。

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）触发前缀 /seo；按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：26 个 skill 的「skill → SKILL.md 路径」列是准的，按需读单文件即可 ✅；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/03-SEO与内容/T-toolkit-index.md · 核实：2026-11-17（Step 1 文档已读（未独立实测运行时））
> 注意：seo 工具链唯一路由表
