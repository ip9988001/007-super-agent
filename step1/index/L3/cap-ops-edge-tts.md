# cap-ops-edge-tts

## 一句话
edge-tts 7.3.1 —— 借微软 Edge 在线 TTS 把文字转 mp3（322 音色，zh-CN 8 个）✅；`edge-playback` 的 mpv 依赖已补齐（`/usr/bin/mpv` 0.35.1），依赖链通了 ✅ —— 但本容器无音频设备，真播出来是无声。

## 怎么调
edge-tts --list-voices | grep zh-CN # 挑音色 edge-tts --text "要念的字" --voice zh-CN-XiaoxiaoNeural --write-media /tmp/a.mp3 # 只要文件 edge-tts -f long.txt --voice zh-CN-YunxiNeural --write-media /tmp/b.mp3 # 长文本 edge-playback --text "hello" # 要现场播，依赖 mpv（本机无声） mpv --no-video --ao=null /tmp/a.mp3 # 验证音频本身能否解码

## 坑
`edge-playback` 退出码恒 0，无声也算成功；要确认真响了得有带声卡的机器。；无 `/dev/snd` 时任何播放器都哑（mpv、ffplay 一起死在 ALSA），装 pulseaudio 也救不回来，除非容器加 `--device /dev/snd`。

## 实际效果
`edge-tts --version` → `edge-tts 7.3.1` ✅；`edge-tts --text "test" --write-media /tmp/x.mp3` ✅ exit=0，生成 11232 字节

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`edge-tts --version` → `edge-tts 7.3.1` ✅；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/05-系统与运维/E-edge-tts.md · 核实：2026-10-26（原文件 实际效果（实测 2026-10-26））
> 注意：借微软 Edge 在线 TTS；mpv 依赖已补齐
