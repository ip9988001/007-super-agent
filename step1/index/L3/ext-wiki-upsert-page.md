# ext-wiki-upsert-page

## 一句话
Create or update a structured Steel Will wiki page.

## 怎么调
wiki_upsert_page({title, category, summary, content})

## 坑
写 wiki 必须同步走向量入口，否则向量库停更；写盘五步：写盘→重建 index→写 log→向量同步

## 实际效果
[SteelWill-Wiki] 加载日志 24 次；落点 memory/wiki/<category>/<slug>.md，同时更新 index 与 log

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：[SteelWill-Wiki] 加载日志 24 次；工具返回结构化文本；失败返回 code 与错误头

来源：extensions/steel-will-30-wiki-engine.ts:263 · 核实：2026-11-17（2026-11-17 审计：扩展已加载（日志命中）+ .ts 已读）
