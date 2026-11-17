# cap-fetch-agent-reach

## 一句话
平台内容只读入口：一个 CLI 覆盖 github / youtube / v2ex / web / bilibili / 小红书 / 推特 / reddit / 雪球 / BOSS 等频道，所有输出都是 JSON。

## 怎么调
agent-reach channels # 全频道+动作+参数的机器可读清单 agent-reach doctor --text # 装了什么、通不通（不发网络请求） agent-reach check [channel] # 真联网测试（--search 只测搜索类） agent-reach <channel> <action> [args] [--timeout 60] agent-reach read <url> # 不知道走哪个频道时用它

## 坑
频道要先 `add` 才有后端；没 add 直接调 → `unknown command or channel`（不是 BAD_URL）。；`agent-reach status` 不存在，状态用 `doctor` / `check`。B站 read/subtitle 目前必挂（412），别重试第二次，直接换 `web read`。

## 实际效果
`agent-reach doctor --text` → ready: github / youtube / v2ex / web；broken: bilibili（read、subtitle 报 UPSTREAM_…；`agent-reach v2ex hot --limit 2` → ✅ 返回中文热帖全文

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`agent-reach doctor --text` → ready: github / youtube / v2ex / web；broken: bilibili（read…；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/01-取数与抓取/A-agent-reach.md · 核实：2026-11-16（no3-tools.md 2026-11-16 盘点（版本+运行））
> 注意：平台内容只读入口
