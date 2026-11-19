# agent-reach
## 一句话
平台内容只读入口：一个 CLI 覆盖 github / youtube / v2ex / web / bilibili / 小红书 / 推特 / reddit / 雪球 / BOSS 等频道，所有输出都是 JSON。

## 实际效果（实测 v1.6.0，2026-10-26）
- `agent-reach doctor --text` → ready: github / youtube / v2ex / web；broken: bilibili（read、subtitle 报 UPSTREAM_BROKEN，B站 API 412）
- `agent-reach v2ex hot --limit 2` → ✅ 返回中文热帖全文
- `agent-reach web read https://example.com` → ✅ backend=jina，返回干净 markdown 正文
- `agent-reach web search "..." --limit 2` → ✅ backend=exa，带正文摘要
- `agent-reach youtube read <url>` → ✅ 标题/作者/发布日期/简介全文
- `agent-reach github read torvalds/linux` → ✅ backend=github-api，README 全文
- `agent-reach bilibili search 猫 --limit 1` → ✅ 列表可用；但同频道 read/subtitle ❌（412）
- `agent-reach read <url>` → ✅ 按 URL 自动选频道（v2ex 链接自动走 v2ex-api）
- 未安装频道：twitter / reddit / facebook / instagram / xiaohongshu / linkedin / boss / xueqiu / xiaoyuzhou（doctor 显示 not_added）

## 怎么调
```bash
agent-reach channels                          # 全频道+动作+参数的机器可读清单
agent-reach doctor --text                      # 装了什么、通不通（不发网络请求）
agent-reach check [channel]                    # 真联网测试（--search 只测搜索类）
agent-reach <channel> <action> [args] [--timeout 60]
agent-reach read <url>                         # 不知道走哪个频道时用它
```
典型：`agent-reach v2ex hot --limit 5` / `agent-reach web read <url>` / `agent-reach youtube subtitle <id> --lang zh-Hans,en`
失败用 `--backend public-web` 只走公开网页路由。

## 坑
- 频道要先 `add` 才有后端；没 add 直接调 → `unknown command or channel`（不是 BAD_URL）。
- `agent-reach status` 不存在，状态用 `doctor` / `check`。B站 read/subtitle 目前必挂（412），别重试第二次，直接换 `web read`。
- 输出是 JSON 单行，字段在 `items[]`；`meta.attempts` 会写明哪个 backend 挂了、为什么。
- web read 依赖 jina 代理，长页可能被截断；需要全文换 scrapling。
