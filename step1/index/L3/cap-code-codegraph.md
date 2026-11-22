# cap-code-codegraph

## 一句话
代码图谱 CLI 1.6.2（node:sqlite 内建后端），把仓库索引成符号 + 调用图后按需查；目前只有 `/data/workspace/rea` 建过索引。

## 怎么调
# 先确认有没有索引，别浪费一轮 codegraph status -p /data/workspace/rea # 找符号 / 按 kind 过滤 codegraph query "hydrateSession" -p /data/workspace/rea -l 20 codegraph query "doctor" -k method -p /data/workspace/rea # 改代码前首选：相关源码 + 调用路径 + 影响面 codegraph explore "how doctor assembles checks" -p /data/workspace/rea # 按任务组上下文 codegraph context "add a new provider" -p /data/workspace/rea # 重建（555MB 库，慢，别随手跑） codegraph ind…

## 坑
无索引就直接 `Not initialized`，不会自动降级到 grep；换目录前先 `status`。；索引是快照：`.codegraph/.gitignore` 是 `*` + `!.gitignore`，所以 db 不进版本库，但目录本身会在 `git status` 里常挂 `?? .codegraph/`。

## 实际效果
`codegraph --version` → `1.6.2` ✅；`codegraph status /data/workspace/rea` → Files 6,889 / Nodes 128,271 / Edges 435,932 / DB 529.46 MB / `node:s…

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）触发前缀 /graph；前置：新仓库先建索引再查。按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`codegraph --version` → `1.6.2` ✅；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/02-代码与审计/C-codegraph.md · 核实：2026-10-26（原文件 实际效果（实测 2026-10-26））
> 注意：仅 /data/workspace/rea 建过索引（6,889 文件 / 128,271 节点）；与另一条 codegraph 同一真身（二进制），Go 版二合一：薄封装即工具。
