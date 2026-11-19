# edge-tts / edge-playback
## 一句话
edge-tts 7.3.1 —— 借微软 Edge 在线 TTS 把文字转 mp3（322 音色，zh-CN 8 个）✅；`edge-playback` 的 mpv 依赖已补齐（`/usr/bin/mpv` 0.35.1），**依赖链通了** ✅ —— 但本容器无音频设备，真播出来是无声。

## 实际效果（实测 2026-10-26 14:27）
- `edge-tts --version` → `edge-tts 7.3.1` ✅
- `edge-tts --text "test" --write-media /tmp/x.mp3` ✅ exit=0，生成 **11232 字节**
- `edge-playback --version` → 先打 `edge-tts 7.3.1`，再 `Failed to recognize file format.`（`--version` 被当文本走完整链路，没产文件，mpv 空手而归）
- `timeout 20 edge-playback --text "hello"` ✅ 不再报 `mpv is not installed.`，一路跑到 mpv：`[ao/alsa] Playback open error: No such file or directory` / `jack server is not running` / `[ao] Failed to initialize audio driver 'sdl'` / 收尾 `Could not open/initialize audio device -> no sound.`
- ⚠️ 上面那堆错误之后 **`REAL_EXIT=0`** —— 播放失败不返错码，别拿退出码当判据
- 设备现状：`/dev/snd` 无、pulse 无、jack 无；`mpv --no-video --ao=null /tmp/x.mp3` ✅ exit=0 干净解码 → 只是没声卡
- `ffplay -nodisp -autoexit /tmp/x.mp3` ❌ 同样 `ALSA: Couldn't open audio device` → 不是播放器的问题

## 怎么调
```bash
edge-tts --list-voices | grep zh-CN            # 挑音色
edge-tts --text "要念的字" --voice zh-CN-XiaoxiaoNeural --write-media /tmp/a.mp3  # 只要文件
edge-tts -f long.txt --voice zh-CN-YunxiNeural --write-media /tmp/b.mp3           # 长文本
edge-playback --text "hello"                   # 要现场播，依赖 mpv（本机无声）
mpv --no-video --ao=null /tmp/a.mp3            # 验证音频本身能否解码
```
**两种用法别混**：要**文件**走 `edge-tts --write-media`（稳、可复用）；要**立刻播**才用 `edge-playback`（内部先 edge-tts 到临时文件再拉 mpv，mp3 落在 `NamedTemporaryFile(delete=False)` 的临时路径，**拿不到手**）。

## 坑
- `edge-playback` 退出码恒 0，无声也算成功；要确认真响了得有带声卡的机器。
- 无 `/dev/snd` 时**任何**播放器都哑（mpv、ffplay 一起死在 ALSA），装 pulseaudio 也救不回来，除非容器加 `--device /dev/snd`。
- 走微软**在线**接口：必须出网，离线直接超时，无本地兜底。
- `--write-media` 不写就只往 stdout 打日志，不产生文件。
- 长文本自动分段，多段语气/语速不完全一致；正式稿建议一两句一段。
