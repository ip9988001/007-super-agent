# cap-memory-l1-watcher

## 一句话
常驻 Python 进程（1543 行），每 200ms 扫一遍 `~/.pi/agent/sessions/*.jsonl`，按命中词把用户发言写进 `memory/l1/<日期>.md`。

## 怎么调
cd /root/.pi/agent/bin && HOME=/root PYTHONUNBUFFERED=1 python3 l1_watcher.py # 常驻 python3 l1_watcher.py --once # 扫一遍就退（调试用） python3 l1_watcher.py --refine-only # 只跑精炼（被 refine_daily 每日调用） python3 l1_watcher.py --sync-key # 把 pi 驱动模型的 key 同步进密钥表

## 坑
采集的是 prompt 原文前 200 字，所以 L1 里全是任务指令片段（含"不要""必须"这种词就必然命中 lesson 类）→；信号词过宽：`lesson` 组里含 `不要 / 不能 / 错误 / 避免`，任何需求描述里的否定句都会被收（实测 5 条 L1 有 3 条是 lesson 且全是任务开头）。

## 实际效果
进程在跑：`python3 l1_watcher.py`（PID 95，父进程是 start.sh 的残留 subshell 93），CPU 0.4%。；心跳日志每 5s 一行：`心跳 | 监控39个文件 | L1已写: 5`（`/data/logs/l1-watcher.log` 4350 行）。

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：进程在跑：`python3 l1_watcher.py`（PID 95，父进程是 start.sh 的残留 subshell 93），CPU 0.4%。；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/06-记忆与知识/L-l1-watcher.md · 核实：2026-11-17（2026-11-17 审计：进程在跑（PID 95/96））
> 注意：PID 95 在跑；L1 采集 0.2 秒扫、7 天过期删
