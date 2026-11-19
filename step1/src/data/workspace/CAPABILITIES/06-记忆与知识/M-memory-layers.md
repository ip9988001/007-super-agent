# 记忆分层目录（L1 / L3 / wiki / candidates）

> **一句话**：记忆全在 `/root/.pi/agent/memory/`（`/data/pi-home/agent` 是同一份的另一挂载点），按「碎片 → 候选 → 长期 → 结构化」四层落盘。

## 实际效果（实测，`du -sh`）
| 目录 | 体量 | 装什么 | 实测条数 |
|---|---|---|---|
| `l1/` | 24K | 会话碎片（原文截断），按日期一文件 | 2 文件 / **5 条** |
| `candidates/agent/` | 76K | 待筛候选（fact/lesson/pattern/decision/preference） | **16 条** |
| `wiki_candidates/pending/` | 20K | `wiki_capture_candidate` 的落点 | **2 条 pending** |
| `agent/` | 84K | L3 长期（`cases` `lessons` `decisions` `patterns`） | 8 / 4 / 2 / 2 |
| `wiki/` | 36K | 结构化知识（`entities` `concepts` `sources` `synthesis` + `index.md` `log.md`） | **2 页**（均 `auto-synth-*`） |
| `system/` | 52K | 各扩展状态 JSON（`achievements` `temporal` `token_roi` `usage` `refiner_state` `auto_promoter_state` `vector_db/`） | — |
| 其他 | — | `archive` 424K / `goals` `governance` `reflections` `sensory_cortex` `stages` `swarm_matrix` `bundles` `adjudication` 均 4K（基本空） | — |

## 怎么调
```
tree -L 2 /root/.pi/agent/memory        # 看结构
ls /root/.pi/agent/memory/l1/           # 碎片（按日期）
ls /root/.pi/agent/memory/agent/*/      # L3 长期
cat /root/.pi/agent/memory/wiki/index.md # wiki 目录页（自动维护）
```
层级职责：`l1/` 采集 → `candidates/` 待筛 → `agent/` L3 长期 → `wiki/` 结构化真值。
工具侧对应：`wiki_capture_candidate`（写候选）→ `wiki_upsert_page`（写 wiki）→ `recall_memory`（读）。

## 坑
- **wiki 几乎是空的**：`entities/` 与 `synthesis/` 目录为空，只有 2 页 `auto-synth-lesson / auto-synth-fact`；
  `wiki/index.md` 明确标 `_Empty_`。所以「wiki 是最高事实来源」目前**只是设计意图，没有内容支撑**。
- **向量库是空的**：`system/vector_db/index.json` = `{"items": []}` → 语义检索没有底料（见 `R-recall-memory.md`）。
- `candidates/agent/` 有 16 条但 `wiki_candidates/pending/` 只 2 条 —— **两条写入路径不互通**，别以为候选池只有一个。
- `archive/` 吃掉 424K（占记忆总量一半多），其余 8 个目录基本是空壳（各 4K）。
- 路径两处都有（`/root/.pi/agent/memory` 与 `/data/pi-home/agent/memory`）——**同一份持久卷的两个视角**，不要在两边各写一份。
- 记忆**没有版本/备份**：`wiki/log.md` 只有 99 字节，误删不可恢复。
