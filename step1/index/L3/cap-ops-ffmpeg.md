# cap-ops-ffmpeg

## 一句话
音视频转码/抽帧/抽音的标配（Debian 12 包，已装）。

## 怎么调
ffmpeg -loglevel error -i in.mp4 -vn -acodec libmp3lame out.mp3 # 抽音 ffmpeg -loglevel error -i in.mp4 -vf fps=1 out_%03d.png # 1fps 抽帧 ffprobe -v error -show_entries format=duration,size -of csv=p=0 f # 探元数据 ffmpeg -i in.mp4 -c copy -ss 00:01:00 -t 30 cut.mp4 # 无重编码切片

## 坑
别省 `-loglevel error`：默认会刷几百行 banner+stream info，进上下文就是纯浪费。；抽帧给视觉模型用时记得限 `fps` 和 `-s`，原分辨率 png 一帧几 MB。

## 实际效果
`ffmpeg -version` = `5.1.9-0+deb12u1`。；实测转码：`ffmpeg -f lavfi -i "sine=frequency=440:duration=1" -c:a libmp3lame tone.mp3`

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`ffmpeg -version` = `5.1.9-0+deb12u1`。；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/05-系统与运维/F-ffmpeg.md · 核实：2026-11-16（no3-tools.md 2026-11-16 盘点（版本+运行））
> 注意：5.1.9
