# cap-ops-mpv

## 一句话
mpv 0.35.1（apt 装）：音视频播放器；无头服务器上主要用来探媒体信息、抽音频、转格式，不开画面。

## 怎么调
mpv --version # 版本
mpv --no-video --audio-display=no <file> # 只放音频
mpv <url> --ytdl-format=bestaudio --no-video -o out.m4a # 抓在线音频（需 yt-dlp 配合）

## 坑
无头环境别直接 `mpv <file>`：它会尝试开窗口/声卡，卡住直到超时；一律加 `--no-video` 或 `--vo=null --ao=null` 做纯探查；`--ytdl-format` 依赖 yt-dlp 在位。

## 实际效果
`mpv --version` → `0.35.1` ✅（第 19 步 apt 实测）。

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）触发前缀 无前缀，直接调用；按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`mpv --version` → `0.35.1` ✅；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：第 19 步安装核验（--version 实测，2026-11-18）
