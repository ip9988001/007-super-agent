# l1_watcher.py（L1 实时采集）

> **一句话**：常驻 Python 进程（1543 行），每 200ms 扫一遍 `~/.pi/agent/sessions/*.jsonl`，按命中词把用户发言写进 `memory/l1/<日期>.md`。

## 实际效果（实测）
- 进程在跑：`python3 l1_watcher.py`（PID 95，父进程是 start.sh 的残留 subshell 93），CPU 0.4%。
- 心跳日志每 5s 一行：`心跳 | 监控39个文件 | L1已写: 5`（`/data/logs/l1-watcher.log` 4350 行）。
- 产出：`memory/l1/2026-10-25.md`(22 行) + `2026-10-26.md`(22 行)，共 **5 条 L1**。
- 实测 L1 条目长这样：`## [13:41] [教训] 不要` + **用户 prompt 原文截断**（`td["user"][:200]`）——
  即「粗采原文」，不是提炼。

## 怎么调
```
cd /root/.pi/agent/bin && HOME=/root PYTHONUNBUFFERED=1 python3 l1_watcher.py   # 常驻
python3 l1_watcher.py --once          # 扫一遍就退（调试用）
python3 l1_watcher.py --refine-only   # 只跑精炼（被 refine_daily 每日调用）
python3 l1_watcher.py --sync-key      # 把 pi 驱动模型的 key 同步进密钥表
```
信号词表在 `SIGNALS`（第 512 行起）：preference / lesson / pattern / decision / fact 五类正则。
防抖 `DEBOUNCE_SEC = 1.5`；同句去重用 `sha256(user[:200])[:16]`。

## 坑
- **采集的是 prompt 原文前 200 字**，所以 L1 里全是任务指令片段（含"不要""必须"这种词就必然命中 lesson 类）→
  **信噪比很低**，`recall_memory` 命中这些条目时基本只能当"当时说过这句话"的线索。
- 信号词**过宽**：`lesson` 组里含 `不要 / 不能 / 错误 / 避免`，任何需求描述里的否定句都会被收（实测 5 条 L1 有 3 条是 lesson 且全是任务开头）。
- 状态文件 `memory/l1/.watcher_state.json`（8.7K）记录每个 session 的**字节偏移**，删了会重扫全部历史。
- 只扫 `~/.pi/agent/sessions/`，`HOME` 必须对，否则找不到 session。
- 与 TypeScript 扩展 `steel-will-l1-logger.ts` **双轨写 L1**，两边规则不同，可能重复。
- 自动清理 `>7 天` 的旧 L1 文件 → 老记忆会无声消失。
