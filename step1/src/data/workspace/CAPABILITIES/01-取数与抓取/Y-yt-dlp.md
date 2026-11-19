# yt-dlp
## 一句话
视频元数据/字幕/下载利器，版本 2026.10.21；YouTube 主流程可用，但缺 JS runtime，部分格式与站点受限。

## 实际效果（实测 2026-10-26）
- `yt-dlp --version` → `2026.10.21` ✅（`/usr/local/bin/yt-dlp`）
- `yt-dlp --skip-download --print "%(title)s | %(duration)s | %(uploader)s" <YouTube URL>` → ✅ `Rick Astley - Never Gonna Give You Up ... | 213 | Rick Astley`
- 每次都打 WARNING：`No supported JavaScript runtime could be found. Only deno is enabled by default` → 部分流格式缺失（视频下载可能拿不全，元数据/字幕不受影响）
- 已下架视频 → `ERROR: [youtube] xxx: This video is unavailable`（是视频问题，不是工具问题）
- 环境里没有 deno（MISSING），所以那个 WARNING 会一直存在

## 怎么调
```bash
# 元数据（最常用，不下载）
yt-dlp --skip-download --print "%(title)s | %(duration)s | %(uploader)s | %(upload_date)s" "<url>"

# 一次性拿完整 JSON 元数据
yt-dlp --skip-download --dump-json "<url>" | python3 -m json.tool | head -40

# 列字幕 / 下字幕（不碰视频流）
yt-dlp --list-subs --skip-download "<url>"
yt-dlp --skip-download --write-auto-subs --sub-langs "zh-Hans,en" --sub-format vtt -o "/tmp/%(id)s" "<url>"

# 下载（受限流可加 --extractor-args 绕过，格式可能缺）
yt-dlp -f "bv*+ba/b" -o "/tmp/%(title)s.%(ext)s" "<url>"
```
更高层封装（已实测通）：`agent-reach youtube read|subtitle|search <target> --lang zh-Hans,en`

## 坑
- **缺 JS runtime**：YouTube 新版需要，否则部分 format 抓不到；要补就装 deno（本次未装，不改环境）。元数据和字幕路径不受影响。
- YouTube 频控狠，短时间重复请求同一视频会出现 "Sign in to confirm you're not a bot" / 412，别连打。
- `BaW_jenozKc`（官方测试视频）现在是 unavailable，别拿它验证工具坏了。
- B站/其他站点规则随站点改版常失效，失败先 `yt-dlp -U` 想升级（本次禁止改动环境，只记录）。
- 需要登录的内容要 `--cookies cookies.txt`（浏览器导出的 Netscape 格式）。
