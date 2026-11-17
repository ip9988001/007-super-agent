# cap-design-impeccable

## 一句话
本地设计反模式检测器（引擎 `0.1.16` / skill `4.7.0`，二进制 21MB，整技能 23MB），零 API、零联网、离线确定性；文件 0.06s、URL 2–29s。`/ui` 路由的打分完全靠它。

## 怎么调
IMP=/root/.pi/agent/skills/impeccable/scripts/impeccable export IMPECCABLE_BROWSER=/usr/local/bin/chrome-nosandbox # root 跑 chromium 必须走这个 wrapper（内含 --no-sandbox --disable-dev-shm-usage） $IMP detect --json README.html # 静态，0.06s $IMP detect --json --viewport 390x844 <目标> # 移动端 pass（同一 URL 结果会变：example.com 桌面 2 条 → 390 宽 0 条） $IMP detect --json --scope type|layout <目标> # 只有这两个 scope（无 quality，加了反…

## 坑
没设 `IMPECCABLE_BROWSER` 时 URL 直接失败：`Error: No Chrome…`，退出 1、stdout 空、耗时 0.007s —— 上层若把空 stdout 当 `[]`，就变成"0 条…；`--scope type|layout` 是减法：`--scope type` 只剩 2 条，`low-contrast` 属于 quality 不在任何 scope 内，按 scope 扫会漏掉它。

## 实际效果
`--version` → `4.7.0`；`scripts/VERSION` → `0.1.16`（两个版本号并存，网关 `/stats` 打印的是后者）；双引擎，规则集不同

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）触发前缀 /ui；前置：绝对路径调用。按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`--version` → `4.7.0`；`scripts/VERSION` → `0.1.16`（两个版本号并存，网关 `/stats` 打印的是后者）；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/04-设计与UI/I-impeccable.md · 核实：2026-10-26（原文件 实际效果（实测 2026-10-26））
> 注意：软链曾报 no engine binary，绝对路径可用（4.7.0）；引擎 0.1.16
