# ext-usage-report

## 一句话
记忆使用账本：最常被召回的记忆、以及从未被召回过的沉睡条目。

## 怎么调
usage_report({top=10})

## 坑
top 默认 10；沉睡=在向量库但不在 usage.json

## 实际效果
[SteelWill-SelfStatus] 加载日志 6 次；最常被召回的记忆与沉睡条目账本

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：[SteelWill-SelfStatus] 加载日志 6 次；工具返回结构化文本；失败返回 code 与错误头

来源：extensions/steel-will-35-self-status.ts:131 · 核实：2026-11-17（2026-11-17 审计：扩展已加载（日志命中）+ .ts 已读）
> 注意：在 settings.json defaultTools 的 - 名单上；该前缀语义未验证，不据此判禁用
