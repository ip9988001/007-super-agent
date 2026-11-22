# cap-design-gateway-timeouts

## 一句话
03 批报的 `/stats` 20s 丢工具版本行今天没能复现（实测 1.63s 全通），但机制确认成立：那条工具链是一个 bash 复合命令吃满 20s 预算，超时即 SIGKILL、输出只剩已打印的行；同一类"单点超时 + 静默截断"还散布在 `/ui`、`/audit` 等处。

## 怎么调
cd /data && node -c bot-gateway.js # 改完先语法校验 grep -n "timeoutMs" bot-gateway.js # 一眼看全部预算点 # 单独给 /stats 的工具链加余量：把 :307 的 20000 提到 40000（一行改动）

## 坑
`/stats` 的 20s 是 6 条命令共用的墙钟预算，但工具链那条本身是 7 次版本探测 + 3 次文件测试的串行复合体，10 次 fork 挤在 20s 里；容器一忙（semgrep 扫描 / 磁盘抖动）就整段…；静默性才是真问题：`/stats` 与 `/ui` 两处都是"超时→空字符串→被当成正常结果"，`/ui` 更狠，直接把 `[]` 渲染成"命中 0 条"。加一行 code 判断即可消掉。

## 实际效果
全路由清单（`bot-gateway.js`）：`/help /audit /stats /graph /rea /seo /ui /design` + 默认兜底给 pi。没有 `/status` —— 发 `/sta…；超时表（行号 → 预算）

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：全路由清单（`bot-gateway.js`）：`/help /audit /stats /graph /rea /seo /ui /design` + 默认兜底给 pi。没有…；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/04-设计与UI/G-gateway-timeouts.md · 核实：2026-10-26（原文件 实际效果（实测 2026-10-26））
> 注意：20s 丢行未复现（实测 1.63s 全通），机制成立
