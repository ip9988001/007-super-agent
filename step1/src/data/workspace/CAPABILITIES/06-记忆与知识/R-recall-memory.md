# recall_memory（检索记忆）

> **一句话**：跨 L1/L3/wiki 的统一召回入口，`search_mode` 可选 semantic / lexical / hybrid，默认 hybrid。

## 实际效果（实测）
- 调用：`recall_memory({query:"llmtrim 压缩代理 lean-ctx 降噪 工具", search_mode:"hybrid", top_k:3})`
  → 返回 3 条，每条带 `File / Score / Lane / Source / Content`。
- 真实命中示例：`~/.pi/agent/memory/l1/2026-10-26.md` `Score: 67%` `Source: L1 Fragment`。
- 但另两条 `Score: 23%` 是**几乎无关**的 L3 case（内容只是含"不要"二字）→ 召回精度偏低。
- 工具在 `steel-will-recall-tool.ts` 注册（另注册 `reflections`/`lessons`/`cases` 三个分类视图参数）。

## 怎么调
```
recall_memory({query:"任务指纹或问题原文", search_mode:"hybrid", top_k:3})
recall_memory({query:"模糊探索型问题", search_mode:"hybrid", top_k:5})   # 模糊查询放大到 5
recall_memory({query:"精确文件/符号名", search_mode:"lexical"})          # 求准不求全
recall_memory({query:"意思相近但用词不同", search_mode:"semantic"})      # 需要向量库有料
```
铁律（写在 AGENTS.md）：**没有 recall 就不许引用历史，也不许断言"没有相关记忆"**。

## 坑
- **hybrid 实际是词法兜底**：`system/vector_db/index.json` 的 `items` 为空数组，
  `wiki_lint` 也报 `vector-index-empty` → `semantic` 模式**没有向量底料**，返回质量取决于字符串匹配。
- **底料本身是 prompt 原文**：L1 存的是用户消息前 200 字（见 `L-l1-watcher.md`），
  所以"命中"往往只是命中了当时那句指令，不构成可复用知识。
- 分数不可跨查询比较：同一 23% 在这个查询里是噪声，换个查询可能是最佳答案；**别设固定阈值过滤**。
- `top_k` 太大会把 16 条 L3 全捞上来，反而稀释上下文（读预算由 `vector-radar.check_read_budget` 控）。
- 兜底：若工具不存在（`"Tool not found"`），用 `rg -il '关键词' ~/.pi/agent/memory/` 替代。
