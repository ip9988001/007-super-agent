# ext-wiki-lint

## 一句话
Run health checks against the Steel Will wiki and produce a lint report.

## 怎么调
wiki_lint()

## 坑
15 类检查（缺 Metadata/Content 为 high）；向量一致性只抽首/中/末 3 页

## 实际效果
[SteelWill-WikiLint] 加载日志 12 次；二号侧最新报告 768 条（HIGH 157 / MEDIUM 363 / LOW 248）

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：[SteelWill-WikiLint] 加载日志 12 次；工具返回结构化文本；失败返回 code 与错误头

来源：extensions/steel-will-33-lint-system.ts:410 · 核实：2026-11-17（2026-11-17 审计：扩展已加载（日志命中）+ .ts 已读）
> 注意：在 settings.json defaultTools 的 - 名单上；该前缀语义未验证，不据此判禁用
