# playwright（CLI 侧）
## 一句话
Playwright 1.64.1 的**命令行**：装内核、截图/存 PDF、起 MCP、看 trace 全 ✅；headed 类（`open/codegen/cr/ff/wk`）补装 `xvfb`+`xauth` 后也通了 ✅ —— 前提是加 `xvfb-run -a`。写脚本/三内核 launch 见 `B-browser-stack.md`。

## 实际效果（实测 2026-10-26 14:27–14:28）
- `xvfb-run -a playwright --version` → `Version 1.64.1`，0.526s ✅（xauth 1:1.1.2-1 / xvfb 2:21.1.7-3+deb12u13 已装）
- `xvfb-run -a playwright screenshot --viewport-size=390,844 https://example.com /tmp/pw.png` ✅ 出图 **55706 字节**（0.864s）
- 同一张图**不加 xvfb** 也是 55706 字节 → `screenshot` 本身 headless，不需要 xvfb
- `--full-page` + 390x844 → 59347 字节 ✅；`playwright pdf` 不加 xvfb → 31919 字节 ✅
- `xvfb-run -a playwright codegen --help` ✅ 帮助页正常；真启动 codegen（12s 后 timeout 杀）→ `Xvfb :99 -screen 0 1280x1024x24 -auth /tmp/xvfb-run.*/Xauthority` 在跑、node driver 在跑、**日志 0 字节**
- `xvfb-run -a playwright open|cr|ff|wk` → 四个全 exit=124（被 timeout 杀）、日志 0 字节 = 正常挂起等交互 ✅
- 对照：`playwright open` **不加 xvfb** → `Looks like you launched a headed browser without having a XServer running.`

## 怎么调
```bash
export PLAYWRIGHT_BROWSERS_PATH=/data/cache/ms-playwright   # 不设就找不到内核
playwright screenshot --viewport-size=390,844 https://x /tmp/a.png  # headless，无需 xvfb
playwright pdf https://x /tmp/a.pdf                         # 仅 chromium
xvfb-run -a playwright codegen -o /tmp/rec.py https://x     # 录制脚本（headed）
xvfb-run -a playwright open https://x                       # 手动看页面
xvfb-run -a playwright cr https://x                         # 指定内核 headed
```
**规则一条**：headed 类（`open/codegen/cr/ff/wk`）必须 `xvfb-run -a`；`screenshot/pdf/install/show-trace/mcp` 不用。

## 坑
- 不设 `PLAYWRIGHT_BROWSERS_PATH` → 报 executable doesn't exist（默认路径是空的）。
- 忘套 `xvfb-run` 时 headed 命令报的是 `TargetClosedError` + 一段 XServer 花体提示，不是「DISPLAY 未设」——别被错误名带偏。
- `pdf` 只有 chromium 支持，`ff`/`wk` 不出。
- headed 命令「跑成功」的判据是**日志 0 字节 + 进程挂住**；它会一直等你手关，脚本里记得 `timeout` 包一层。
- `install-deps` 会调 apt + sudo 改系统，别在容器里随手跑，内核已齐。
- CLI 与 Python API 同一份安装但解释器不同：脚本必须用 `/data/tools/uv/tools/playwright/bin/python`（详见 B 条）。
