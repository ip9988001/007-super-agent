# start.sh（容器引导 225 行）

> **一句话**：`/data/start.sh` 是容器唯一引导脚本，负责「重建后自愈 + 拉起全部常驻进程 + 前台跑网关」。

## 实际效果（实测）
- 入口链：`docker-init(PID1)` → `docker-entrypoint.sh bash /data/start.sh` → 最后 `exec node /data/bot-gateway.js`（**exec 所以 PID 7 就是 node**，不再是 bash）。
- 全脚本 225 行，**实测存在的编号段共 14 个**：
  `1` 基础包自愈 · `2` pi 软链 · `2d` impeccable · `2e` design-md 库 · `2f` xinxin-link 守护 ·
  `2g` rea wrapper · `2h` gh 登录 + rea MCP · `2i` 审计工具 · `2j` ghidra/magick 软链 ·
  `3` semgrep · `3a` 状态目录持久化 · `3b` llmtrim · `4` L1 采集 + 每日精炼 · `5` 网关前台。
- **没有 2a / 2b / 2c**（编号是历史补丁留下的空洞）。

## 怎么调
```
bash -n /data/start.sh          # 语法自检（改完必做）
grep -nE '^# [0-9]' /data/start.sh   # 列出所有编号段
tail -40 /data/logs/boot.log    # 看上次引导的真实动作与结果
```
纯 bash，无框架；每段自带「命令存在性检查 → 缺失才装/才拉」。

## 坑
- **编号顺序是乱的**：`2g`(204 行) / `2f`(210) / `2j`(159) 排在 `4`(194) 之后 —— 按行号读会串，按编号查。
- **`ps` 会误判成二次引导**：脚本 `exec` 前 fork 的子 shell 保留 `bash /data/start.sh` 的 cmdline
  （实测 PID 93 是 L1 段的残留 subshell，父进程是 node PID 7），**不是**又跑了一遍引导。
- 一堆段是**后台 `( ... ) &` 异步**（semgrep/design-md/审计工具/rea setup）：引导「完成」不等于这些装完，
  要看 `boot.log` 里对应的 `就绪` 行。
- `1` 段是**大锤**：任一校验项缺失就整段 `apt-get install` 全套基础包（含 jdk/chromium 依赖，几百 MB）。
- `2i` 段把 `UV_TOOL_DIR` 改成 `/data/tools/uv-tools`，**覆盖**了第 46 行给 agent-reach 设的路径（见 `U-uv.md`）。
- `4` 段每次执行都会重新 spawn `l1_watcher.py` / `refine_daily.py`，**没有 PID 文件或互斥**；容器反复重建就会多开（见 `06/L-l1-watcher.md`）。
