# cap-ops-start-sh

## 一句话
`/data/start.sh` 是容器唯一引导脚本，负责「重建后自愈 + 拉起全部常驻进程 + 前台跑网关」。

## 怎么调
bash -n /data/start.sh # 语法自检（改完必做） grep -nE '^# [0-9]' /data/start.sh # 列出所有编号段 tail -40 /data/logs/boot.log # 看上次引导的真实动作与结果

## 坑
编号顺序是乱的：`2g`(204 行) / `2f`(210) / `2j`(159) 排在 `4`(194) 之后 —— 按行号读会串，按编号查。；`ps` 会误判成二次引导：脚本 `exec` 前 fork 的子 shell 保留 `bash /data/start.sh` 的 cmdline

## 实际效果
入口链：`docker-init(PID1)` → `docker-entrypoint.sh bash /data/start.sh` → 最后 `exec node /data/bot-gateway.js`（ex…；全脚本 225 行，实测存在的编号段共 14 个

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：入口链：`docker-init(PID1)` → `docker-entrypoint.sh bash /data/start.sh` → 最后 `exec node /dat…；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/05-系统与运维/S-start-sh.md · 核实：2026-11-17（Step 1 文档已读（未独立实测运行时））
> 注意：容器引导脚本说明；新 Agent 用 systemd，不直接复用
