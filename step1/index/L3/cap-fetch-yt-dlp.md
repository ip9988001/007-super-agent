# cap-fetch-yt-dlp

## 一句话
视频元数据/字幕/下载利器，版本 2026.10.21；YouTube 主流程可用，但缺 JS runtime，部分格式与站点受限。

## 怎么调
# 元数据（最常用，不下载） yt-dlp --skip-download --print "%(title)s | %(duration)s | %(uploader)s | %(upload_date)s" "<url>" # 一次性拿完整 JSON 元数据 yt-dlp --skip-download --dump-json "<url>" | python3 -m json.tool | head -40 # 列字幕 / 下字幕（不碰视频流） yt-dlp --list-subs --skip-download "<url>" yt-dlp --skip-download --write-auto-subs --sub-langs "zh-Hans,en" --sub-format vtt -o "/tmp/%(id)s" "<url>" # 下载（受限流可加 --extract…

## 坑
缺 JS runtime：YouTube 新版需要，否则部分 format 抓不到；要补就装 deno（本次未装，不改环境）。元数据和字幕路径不受影响。；YouTube 频控狠，短时间重复请求同一视频会出现 "Sign in to confirm you're not a bot" / 412，别连打。

## 实际效果
`yt-dlp --version` → `2026.10.21` ✅（`/usr/local/bin/yt-dlp`）；`yt-dlp --skip-download --print "%(title)s | %(duration)s | %(uploader)s" <YouTube URL>` → ✅ `Rick Astley - N…

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`yt-dlp --version` → `2026.10.21` ✅（`/usr/local/bin/yt-dlp`）；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/01-取数与抓取/Y-yt-dlp.md · 核实：2026-10-26（原文件 实际效果（实测 2026-10-26））
> 注意：缺 JS runtime，部分格式与站点受限
