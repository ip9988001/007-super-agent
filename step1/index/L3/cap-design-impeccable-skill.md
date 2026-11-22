# cap-design-impeccable-skill

## 一句话
`SKILL.md`（119 行）+ `reference/` 41 篇 = 一套给模型看的设计总监工作流提示词，24 个命令只有名字和规程，CLI 里除 `detect` 外一个都没实现。

## 怎么调
S=/root/.pi/agent/skills/impeccable sed -n '1,60p' $S/SKILL.md # 原则 + Modes（Persuade/Operate/Read/Experience） sed -n '100,119p' $S/SKILL.md # Commands 表：24 行，每条指向一个 reference cat $S/reference/craft-floor.md # 动 UI 前的质量地板与硬禁令 cat $S/reference/<命令>.md # 需要哪条命令就读哪篇（如 polish.md / audit.md） $S/scripts/impeccable context # 让引擎自己报项目状态与强制指令

## 坑
别把 24 命令当 CLI：写脚本调 `impeccable polish` 一定失败；能脚本化的只有 `detect` 和 `ignores`。；`craft` 是 deprecated 别名（"adds no behavior"），`teach` 别名 `init`；文档里出现但 `command-metadata.json` 里没有 `teach`。

## 实际效果
CLI 真命令只有 7 个：`detect / ignores / help / install / link / update / check / telemetry`（`--help` 原文）；对照 `scripts/command-metadata.json` 的 24 条：craft init document extract live generate adapt animate audit bolde…

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）触发前缀 /ui；按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：CLI 真命令只有 7 个：`detect / ignores / help / install / link / update / check / telemetry`（`--…；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/04-设计与UI/I-impeccable-skill.md · 核实：2026-10-26（原文件 实际效果（实测 2026-10-26））
> 注意：24 个命令仅 detect 有实现，其余只有名字和规程
