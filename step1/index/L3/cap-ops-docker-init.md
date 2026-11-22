# cap-ops-docker-init

## 一句话
容器 PID 1 是 `docker-init`（tini 内核），唯一职责是收尸 + 转发信号，`--` 后面才是真正入口。

## 怎么调
cat /proc/1/cmdline | tr '\0' ' ' # 看完整启动命令 ps -p 1 -o pid,ppid,comm,args # 确认 PID1 是 docker-init 而不是你的服务 cat /proc/1/environ | tr '\0' '\n' # PID1 环境（很少，只有 PATH/JAVA_HOME 等）

## 坑
没有 systemd / cron / supervisor：所有「定时」都得自己写循环（`refine_daily.py` 就是 31 行的 while+sleep 替代 04:00 timer）。；PID 1 环境极其干净：实测只有 `PATH / JAVA_HOME / GHIDRA_INSTALL_DIR`（3 个关键项）

## 实际效果
`/proc/1/cmdline` = `/sbin/docker-init -- docker-entrypoint.sh bash /data/start.sh`。；二进制在 `/sbin/docker-init`（708KB；`/usr/bin/docker-init` 不存在，别照老教程写路径）。

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`/proc/1/cmdline` = `/sbin/docker-init -- docker-entrypoint.sh bash /data/start.sh`。；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/05-系统与运维/D-docker-init.md · 核实：2026-11-17（Step 1 文档已读（未独立实测运行时））
> 注意：二进制在 /sbin/docker-init；/usr/bin/docker-init 不存在
