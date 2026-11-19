# 06 记忆与知识 · 条目索引
> 挑一条，只读那个 md。组内按文件名首字母排序，组之间空一行。

## ① 扩展总览（37 个 ts，1 条汇总不拆）
- E-extensions-总览.md :: 37 个 steel-will 扩展分 7 组（记忆/检索/自治/晋升/成就/工厂/采集），实际暴露约 40 个工具

## ② 采集与精炼链路
- L-l1-watcher.md :: L1 实时采集 1543 行（200ms 轮询，5 条信号词；存的是 **prompt 原文前 200 字**，信噪比低）
- R-refine-daily.md :: 每日 04:00 精炼 31 行（**自锁跳过 + 密钥为空 → L3 精炼 0 产出**）

## ③ 分层存储
- M-memory-layers.md :: `l1/` 5 条 → `candidates/` 16 条 → `agent/` L3 16 条 → `wiki/` **仅 2 页**；向量库 `items:[]` 为空

## ④ 工具用法
- R-recall-memory.md :: 跨层召回（实测 top3 命中 67%/23%/23%，**hybrid 实为词法兜底**）
- W-wiki-upsert-page.md :: 写 wiki 页 + `wiki_capture_candidate` + `wiki_lint`（实测 6 条 findings）

## ⑤ 缺件
- P-playbooks.md :: ❌ 剧本库 `memory/wiki/playbooks/` **不存在**（AGENTS.md 写了但实盘无此目录）

> 关联：`wiki/` 与 `l1/` 的物理路径在 `/root/.pi/agent/memory` 与 `/data/pi-home/agent/memory` 两个视角下是同一份，别双写。
