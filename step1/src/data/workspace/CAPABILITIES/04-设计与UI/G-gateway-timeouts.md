# 网关超时矩阵（20s 空转核查）
## 一句话
03 批报的 `/stats` 20s 丢工具版本行**今天没能复现**（实测 1.63s 全通），但机制确认成立：那条工具链是**一个** bash 复合命令吃满 20s 预算，超时即 SIGKILL、输出只剩已打印的行；同一类"单点超时 + 静默截断"还散布在 `/ui`、`/audit` 等处。

## 实际效果（实测 2026-10-26）
- 全路由清单（`bot-gateway.js`）：`/help /audit /stats /graph /rea /seo /ui /design` + 默认兜底给 pi。**没有 `/status`** —— 发 `/status` 会落到默认分支、原样交给 pi（等于白问一次模型）。
- 超时表（行号 → 预算）：

| 路由 | 子步骤 | 预算 | 超时后表现 |
|---|---|---|---|
| `/stats` | `sh()` 6 条并发（含工具链）| **20s**（:307）| SIGKILL，该段空白/只剩前几行 |
| `/audit` | `run-audit.sh` | 420s | 报"审计脚本异常"＋退出码（显式） |
| `/audit` | `codegraph sync / init` | 90s / 120s | `sync-fail` / `init-fail`（有字段） |
| `/audit` | `find \| wc -l` | 30s | 计数 0 → 直接跳过建图（静默） |
| `/graph` | codegraph | 180s | 原样贴输出 |
| `/rea` | rea.mjs | 300s | 原样贴输出 |
| `/seo` | `audit.py` | 120s | 显示"audit.py 未产出" |
| `/seo` | `claude-seo doctor` | **默认 180s** | 失败不阻断 |
| `/seo` | pi | 180s / 420s | 贴部分输出 |
| `/ui` | `detect` | 150s / 240s | **静默 0 条**（伪成功） |
| `/ui` | pi | 180s / 420s | 贴部分输出 |
| `/design` | pi | 200s / 420s | 贴部分输出，**可能半改文件** |
| 默认 | pi | `CFG.limits.timeoutMs`，缺省 150s | 贴部分输出 |

- `/stats` 复现测试（照抄 :307-313 的 6 条命令并发跑）：总耗时 **1.63s**，工具块 10 行**全部输出**（codegraph 1.6.2 / semgrep 1.182.1 / lean-ctx 3.11.4 / llmtrim 0.13.4 / claude-seo v2.5.0 / impeccable v0.1.16 / 84 brands / rea 6.8.0 / ghidra 12.1.4 / jadx）。→ 余量 12 倍，**当前不能复现丢失**
- 故障注入验证（`PATH` 塞一个 `sleep 30` 的假 `semgrep`，预算压到 3s）：`EXITCODE -9`，捕获输出**只有 `codegraph 1.6.2` 一行**，后面 9 行全丢 → 与 03 批现象一致 ✅
- 单条命令实测：`lean-ctx gain` 152ms、`llmtrim status` 50ms、`df` 10ms、`claude-seo doctor` 68ms（03 批曾见它空转 180s，故它是这条链上唯一的炸弹）；工具链整条 1.63s（10 次 fork，最慢是 semgrep）

## 怎么调
```bash
cd /data && node -c bot-gateway.js            # 改完先语法校验
grep -n "timeoutMs" bot-gateway.js            # 一眼看全部预算点
# 单独给 /stats 的工具链加余量：把 :307 的 20000 提到 40000（一行改动）
```
- 排查"某段消失"：看 `code` 与 `out.length`。SIGKILL 时 `code=-9` 且 `out` 是**截断的**、不含错误行 —— 这是唯一判据。更稳的做法：把工具链从复合 for 循环拆成 N 条 `sh()`，单条失败只丢一行。 想让失败可见：`/stats` 组装 text 只取 `toolR.out`、**不看 code**，加一句 `if (toolR.code !== 0) …` 才会显形。

## 坑
- **`/stats` 的 20s 是 6 条命令共用的墙钟预算**，但工具链那条本身是 7 次版本探测 + 3 次文件测试的串行复合体，10 次 fork 挤在 20s 里；容器一忙（semgrep 扫描 / 磁盘抖动）就整段消失。 （`/seo` 的 `doctor` 走 `run()` 默认 180s，与 `/stats` 里 20s 的同一条命令是两套预算，别只改一处。）
- **静默性才是真问题**：`/stats` 与 `/ui` 两处都是"超时→空字符串→被当成正常结果"，`/ui` 更狠，直接把 `[]` 渲染成"命中 0 条"。加一行 code 判断即可消掉。
- 串行 `enqueue` 让最坏时延叠加：`/ui --deep` 660s、`/seo --deep` 660s，前面排一个就顶到企微 stream 存活边界（发不出时 `finalReply` 退化成主动推送）。`/help`、`/design list` 无子进程，不涉超时。
