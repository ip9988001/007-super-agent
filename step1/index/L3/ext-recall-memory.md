# ext-recall-memory

## 一句话
Search the Steel Will memory system for relevant memories. Supports semantic search, lexical fallback, and hybrid mode.

## 怎么调
recall_memory({query, top_k=3, search_mode='hybrid'})

## 坑
hybrid 检索，语义与词法各取 top_k×2=6，去重取 max、乘路径权重、取 top_k=3，每条截断 240 字；wiki 只回 Summary

## 实际效果
加载日志 grep 22 次：recall_memory tool loaded；hybrid 各取 top_k×2=6，去重取 max、乘路径权重、取 top_k=3

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：加载日志 grep 22 次：recall_memory tool loaded；工具返回结构化文本；失败返回 code 与错误头

来源：extensions/steel-will-recall-tool.ts:564 · 核实：2026-11-17（2026-11-17 审计：扩展已加载（日志命中）+ .ts 已读）
