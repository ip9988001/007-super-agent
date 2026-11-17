# cap-memory-playbooks

## 一句话
AGENTS.md 里写的 `memory/wiki/playbooks/<任务族>/` 在本机不存在，剧本库目前是纸面约定，零实体。

## 怎么调
现在无可调（原文件原话）。要建的话见来源文件，不在本步建。

## 坑
AGENTS.md 与实盘不一致：宪法里写「剧本库：`memory/wiki/playbooks/<任务族>/`（成功/失败同库，用 `outcome` 字段区分）」；别把 `/data/workspace/reverse-skill-quarantine/.../playbooks` 当成我们的剧本库读（那是隔离区的渗透剧本）。

## 实际效果
`find /root/.pi/agent/memory -type d | grep -i play` → NONE。；全盘 `find / -type d -name "playbooks"` → 只有一处

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`find /root/.pi/agent/memory -type d | grep -i play` → NONE。；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/06-记忆与知识/P-playbooks.md · 核实：2026-11-17（Step 1 文档已读（未独立实测运行时））
> ❌ 本条不可用：见 note，不许调用。
> 注意：原文件实测：playbooks 目录不存在，纸面约定零实体；宪法里‘照剧本执行’目前落空，先走工具索引
