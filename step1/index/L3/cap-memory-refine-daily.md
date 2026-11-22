# cap-memory-refine-daily

## 一句话
31 行守护脚本，替容器里不存在的 cron/systemd，每天 04:00（北京时间）调一次 `l1_watcher.py --refine-only`。

## 怎么调
python3 /data/bin/refine_daily.py # 常驻（start.sh 4 段拉起） cat /data/bin/.refine_last # 今天有没有被标记过 rm /data/bin/.refine_last # 想手动补跑：删标记再等窗口 python3 /root/.pi/agent/bin/l1_watcher.py --refine-only # 直接跑精炼（不用等 04:00）

## 坑
自锁：脚本先写 MARK 再执行，而 `--refine-only` 自己又检查 `lastCheckDate` →；密钥是空的：`l1_watcher.config.json` 里 `glm_api_key` / `deepseek_api_key` 均为 `""`

## 实际效果
进程在跑：`python3 /data/bin/refine_daily.py`（PID 96）。；逻辑：`while True` 每轮看 `now.hour==4 and now.minute<15`；先写 MARK 文件再 `subprocess.call`，环境 `HOME=/root`。

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：进程在跑：`python3 /data/bin/refine_daily.py`（PID 96）。；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/06-记忆与知识/R-refine-daily.md · 核实：2026-11-17（2026-11-17 审计：进程在跑（PID 95/96））
> 注意：PID 96 在跑；每天 04:00–04:15 精炼
