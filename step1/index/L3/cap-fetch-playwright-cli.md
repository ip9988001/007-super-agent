# cap-fetch-playwright-cli

## 一句话
Playwright 1.64.1 的命令行：装内核、截图/存 PDF、起 MCP、看 trace 全 ✅；headed 类（`open/codegen/cr/ff/wk`）补装 `xvfb`+`xauth` 后也通了 ✅ —— 前提是加 `xvfb-run -a`。写脚本/三内核 launch 见 `B-browser-stack.md`。

## 怎么调
export PLAYWRIGHT_BROWSERS_PATH=/data/cache/ms-playwright # 不设就找不到内核 playwright screenshot --viewport-size=390,844 https://x /tmp/a.png # headless，无需 xvfb playwright pdf https://x /tmp/a.pdf # 仅 chromium xvfb-run -a playwright codegen -o /tmp/rec.py https://x # 录制脚本（headed） xvfb-run -a playwright open https://x # 手动看页面 xvfb-run -a playwright cr https://x # 指定内核 headed

## 坑
不设 `PLAYWRIGHT_BROWSERS_PATH` → 报 executable doesn't exist（默认路径是空的）。；忘套 `xvfb-run` 时 headed 命令报的是 `TargetClosedError` + 一段 XServer 花体提示，不是「DISPLAY 未设」——别被错误名带偏。

## 实际效果
`xvfb-run -a playwright --version` → `Version 1.64.1`，0.526s ✅（xauth 1:1.1.2-1 / xvfb 2:21.1.7-3+deb12u13 已装）；`xvfb-run -a playwright screenshot --viewport-size=390,844 https://example.com /tmp/pw.png` ✅ 出图 55706 字节（0.8…

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`xvfb-run -a playwright --version` → `Version 1.64.1`，0.526s ✅（xauth 1:1.1.2-1 / xvfb 2:2…；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/01-取数与抓取/P-playwright-cli.md · 核实：2026-10-26（原文件 实际效果（实测 2026-10-26））
> 注意：headed 类（open/codegen）需 display，不可用
