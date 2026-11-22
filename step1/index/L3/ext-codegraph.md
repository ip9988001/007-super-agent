# ext-codegraph

## 一句话
查询 pi 内置的代码图谱数据库，支持符号搜索、依赖分析、调用链追踪。

## 怎么调
codegraph({query_type, keyword, top_k=10})

## 坑
Go 版只写薄封装调二进制真身，不重写解析器；db=false 未重验证

## 实际效果
[SteelWill-CodeGraph] 加载日志 12 次；db=false 未重验证

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）触发前缀 /graph；按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：[SteelWill-CodeGraph] 加载日志 12 次；工具返回结构化文本；失败返回 code 与错误头

来源：extensions/steel-will-codegraph.ts:190 · 核实：2026-11-17（2026-11-17 审计：扩展已加载（日志命中）+ .ts 已读）
> 注意：；与另一条 codegraph 同一真身（二进制），Go 版二合一：薄封装即工具。
