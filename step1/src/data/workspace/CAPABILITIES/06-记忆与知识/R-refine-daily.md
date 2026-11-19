# refine_daily.py（每日精炼）

> **一句话**：31 行守护脚本，替容器里不存在的 cron/systemd，每天 04:00（北京时间）调一次 `l1_watcher.py --refine-only`。

## 实际效果（实测）
- 进程在跑：`python3 /data/bin/refine_daily.py`（PID 96）。
- 逻辑：`while True` 每轮看 `now.hour==4 and now.minute<15`；**先写 MARK 文件**再 `subprocess.call`，环境 `HOME=/root`。
- 04:00 那次真实输出（`/data/logs/l1-refine.log`）：
  `[04:00:20] 开始精炼 ...` → `[精炼] 今日(2026-10-25)已检查过，跳过` → `[精炼] 完成，退出`。
- 即 **一次都没真精炼**：`memory/system/refiner_state.json` 仍是 `{"lastRefinedDate": "", "lastCheckDate": "2026-10-25"}`。
- 日志里 25+ 条「启动，等待每日 04:00」= 容器反复重建的化石。

## 怎么调
```
python3 /data/bin/refine_daily.py                 # 常驻（start.sh 4 段拉起）
cat /data/bin/.refine_last                        # 今天有没有被标记过
rm /data/bin/.refine_last                         # 想手动补跑：删标记再等窗口
python3 /root/.pi/agent/bin/l1_watcher.py --refine-only   # 直接跑精炼（不用等 04:00）
```
精炼引擎在 `l1_watcher.py` 里：GLM-4.7 主线 + DeepSeek 兜底（配置 `l1_watcher.config.json`）。

## 坑
- **自锁**：脚本先写 MARK 再执行，而 `--refine-only` 自己又检查 `lastCheckDate` →
  04:00 窗口内一旦"已检查过"就跳过，MARK 又已落盘，**当天彻底没有第二次机会**。
- **密钥是空的**：`l1_watcher.config.json` 里 `glm_api_key` / `deepseek_api_key` 均为 `""`，
  进程环境里也没有 `GLM_*`/`DEEPSEEK_*`（`--sync-key` 可同步 pi 的 key，但没人调用）→
  即使不跳过也调不通。**这是 L3 精炼链路目前 0 产出的根因**。
- 现有 `memory/agent/{cases,lessons,decisions,patterns}` 里那 16 条 L3 是 **auto-promoter（TS 扩展）** 晋升的，
  **不是**这个精炼器产的，别混为一谈。
- 04:00–04:15 是**硬窗口**：容器在那个时间段没跑，当天就永久错过。
- `MARK` 在 `/data/bin/.refine_last`（持久卷），手工删了会重复精炼。
