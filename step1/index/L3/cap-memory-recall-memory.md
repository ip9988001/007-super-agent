# cap-memory-recall-memory

## 一句话
跨 L1/L3/wiki 的统一召回入口，`search_mode` 可选 semantic / lexical / hybrid，默认 hybrid。

## 怎么调
recall_memory({query:"任务指纹或问题原文", search_mode:"hybrid", top_k:3}) recall_memory({query:"模糊探索型问题", search_mode:"hybrid", top_k:5}) # 模糊查询放大到 5 recall_memory({query:"精确文件/符号名", search_mode:"lexical"}) # 求准不求全 recall_memory({query:"意思相近但用词不同", search_mode:"semantic"}) # 需要向量库有料

## 坑
hybrid 实际是词法兜底：`system/vector_db/index.json` 的 `items` 为空数组；底料本身是 prompt 原文：L1 存的是用户消息前 200 字（见 `L-l1-watcher.md`）

## 实际效果
调用：`recall_memory({query:"llmtrim 压缩代理 lean-ctx 降噪 工具", search_mode:"hybrid", top_k:3})`；真实命中示例：`~/.pi/agent/memory/l1/2026-10-26.md` `Score: 67%` `Source: L1 Fragment`。

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：调用：`recall_memory({query:"llmtrim 压缩代理 lean-ctx 降噪 工具", search_mode:"hybrid", top_k:3})`；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/06-记忆与知识/R-recall-memory.md · 核实：2026-11-17（2026-11-17 审计：扩展已加载（日志命中））
> 注意：与扩展工具同一实现，勿重复注册
