# cap-design-hyperframes

## 一句话
HTML 即源码的视频合成框架：组合、动画、标题卡、字幕、配音、音频反应视觉与转场全用带 data-* 时序属性的 HTML + GSAP 时间线写，npx hyperframes 做 lint/inspect/preview/render。

## 怎么调
npx hyperframes init <组合名> # 建组合骨架 npx hyperframes lint # 校验组合 npx hyperframes inspect [--json] # 查溢出与时序 npx hyperframes preview # 预览 npx hyperframes render --variables '{...}' --output out.mp4 # 渲染；辅助脚本 scripts/animation-map.mjs、contrast-report.mjs、package-loader.mjs

## 坑
首用 npx 拉取 hyperframes 包，需联网；未在 #3 上实测跑通过（无能力文件），只核过技能源文件。渲染底层走 ffmpeg（cap-ops-ffmpeg）与 edge-tts（cap-ops-edge-tts）。

## 实际效果
未在 #3 上实测（无能力文件）。技能源文件齐全：SKILL.md + scripts/ 三个 .mjs + templates/ + palettes/ + references/。

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`npx hyperframes lint` 与 `npx hyperframes inspect` 通过；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：skills/hyperframes/SKILL.md · 核实：2026-11-17（技能源文件已读，未独立实测运行时）
> 注意：首用 npx 拉包，需联网；未在 #3 上实测
