# cap-fetch-bili

## 一句话
bili 0.6.2 —— Bilibili 的命令行客户端：只读热门/排行/搜索/UP主/收藏/历史/动态，外加 `audio` 把视频音频下下来切成 ASR-ready WAV；点赞投币三连发动态等写操作要扫码登录。

## 怎么调
bili hot -n 5 --yaml # 热门 bili rank --yaml # 全站排行 bili search 关键词 --type video -n 5 --yaml bili user-videos <UID> -n 10 --yaml # 某 UP 的视频列表 bili audio BV1ABcsztEcY -s 60 -o /data/tmp # 下音频并切段（ASR 用） bili --json … # 机器可读的另一种输出（与 --yaml 二选一）

## 坑
只有 B 站内容；要视频文件/字幕仍走 `Y-yt-dlp.md` 或 `A-agent-reach.md`（agent-reach 也直连 B 站，两者不冲突，bili 胜在"一键出音频段"）。；参数是 `-n/--max`，`--limit` 不存在（别凭直觉写）。

## 实际效果
`bili --version` → `bili, version 0.6.2` ✅；`bili status` → `ok: false` / `error.code: not_authenticated` / `message: 未登录。使用 bili login 登录。`（只读不受影响）

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`bili --version` → `bili, version 0.6.2` ✅；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/01-取数与抓取/B-bili.md · 核实：2026-10-26（原文件 实际效果（实测 2026-10-26））
> 注意：未登录；只读热门/排行/搜索可用
