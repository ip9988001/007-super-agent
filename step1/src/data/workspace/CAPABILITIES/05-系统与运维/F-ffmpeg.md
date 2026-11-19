# ffmpeg 5.1.9

> **一句话**：音视频转码/抽帧/抽音的标配（Debian 12 包，已装）。

## 实际效果（实测）
- `ffmpeg -version` = `5.1.9-0+deb12u1`。
- 实测转码：`ffmpeg -f lavfi -i "sine=frequency=440:duration=1" -c:a libmp3lame tone.mp3`
  → 产出 8586 字节 mp3，`ffprobe` 读回 `1.044898,8586`（时长/字节），退出码 0。
- `libmp3lame` 编码器可用（不是只有原生 aac）。

## 怎么调
```
ffmpeg -loglevel error -i in.mp4 -vn -acodec libmp3lame out.mp3   # 抽音
ffmpeg -loglevel error -i in.mp4 -vf fps=1 out_%03d.png           # 1fps 抽帧
ffprobe -v error -show_entries format=duration,size -of csv=p=0 f # 探元数据
ffmpeg -i in.mp4 -c copy -ss 00:01:00 -t 30 cut.mp4               # 无重编码切片
```

## 坑
- **别省 `-loglevel error`**：默认会刷几百行 banner+stream info，进上下文就是纯浪费。
- 抽帧给视觉模型用时记得限 `fps` 和 `-s`，原分辨率 png 一帧几 MB。
- `-ss` 放 `-i` 前是快速 seek（关键帧对齐），放后是精确定位但慢 —— 要精确切点就别图快。
- 容器里没有 `libx264` 之外的私货编码器时，`-c:v libx265` 可能退化成 ffmpeg 内置实现（极慢）。
- 与 `01-取数与抓取/Y-yt-dlp.md` 配合：yt-dlp 缺 JS runtime 时部分格式拿不到，可让 yt-dlp 只下容器格式再交 ffmpeg 转。
