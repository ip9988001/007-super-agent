# docker-init（PID 1）

> **一句话**：容器 PID 1 是 `docker-init`（tini 内核），唯一职责是收尸 + 转发信号，`--` 后面才是真正入口。

## 实际效果（实测）
- `/proc/1/cmdline` = `/sbin/docker-init -- docker-entrypoint.sh bash /data/start.sh`。
- 二进制在 **`/sbin/docker-init`**（708KB；`/usr/bin/docker-init` **不存在**，别照老教程写路径）。
- `ps -p 1 -o comm` = `docker-init`；真正的应用进程是它的孙辈 `node /data/bot-gateway.js`（PID 7）。
- 僵尸进程会被 PID 1 回收（实测 `ps` 里偶发 `[git] <defunct>` 会自行消失）。

## 怎么调
```
cat /proc/1/cmdline | tr '\0' ' '     # 看完整启动命令
ps -p 1 -o pid,ppid,comm,args         # 确认 PID1 是 docker-init 而不是你的服务
cat /proc/1/environ | tr '\0' '\n'    # PID1 环境（很少，只有 PATH/JAVA_HOME 等）
```
改不了：`docker-init` 由容器运行时注入，容器内无法替换成 systemd。

## 坑
- **没有 systemd / cron / supervisor**：所有「定时」都得自己写循环（`refine_daily.py` 就是 31 行的 while+sleep 替代 04:00 timer）。
- **PID 1 环境极其干净**：实测只有 `PATH / JAVA_HOME / GHIDRA_INSTALL_DIR`（3 个关键项），
  所以 `start.sh` 里那些 `export` 不会出现在 PID 1 的 environ，查环境变量要去 **具体子进程** `/proc/<pid>/environ`。
- 服务退出后**不会自动重启**，除非自己套 `while true`（`link-agent` 段就是手写守护循环）。
- `docker-init` 只转发 SIGTERM/SIGINT；`kill -9` PID 1 在容器内无效。
