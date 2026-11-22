# cap-memory-memory-layers

## 一句话
记忆全在 `/root/.pi/agent/memory/`（`/data/pi-home/agent` 是同一份的另一挂载点），按「碎片 → 候选 → 长期 → 结构化」四层落盘。

## 怎么调
tree -L 2 /root/.pi/agent/memory # 看结构 ls /root/.pi/agent/memory/l1/ # 碎片（按日期） ls /root/.pi/agent/memory/agent/*/ # L3 长期 cat /root/.pi/agent/memory/wiki/index.md # wiki 目录页（自动维护）

## 坑
wiki 几乎是空的：`entities/` 与 `synthesis/` 目录为空，只有 2 页 `auto-synth-lesson / auto-synth-fact`；向量库是空的：`system/vector_db/index.json` = `{"items": []}` → 语义检索没有底料（见 `R-recall-memory.md`）。

## 实际效果
核实：Step 1 文档已读（未独立实测运行时）（2026-11-17）。

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：退出码 0；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/06-记忆与知识/M-memory-layers.md · 核实：2026-11-17（Step 1 文档已读（未独立实测运行时））
> 注意：四层落盘：碎片 → 候选 → 长期 → 结构化
