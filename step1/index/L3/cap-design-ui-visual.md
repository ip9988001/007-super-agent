# cap-design-ui-visual

## 一句话
设计体检之外想"用眼睛验"时，两条现成通道：`chrome-nosandbox --screenshot`（一次成型 0.8s）和 Playwright（多视口 / 全页 / 回归比对）；浏览器栈本体不在这里讲，实现细节全在 `01-取数与抓取/B-browser-stack.md`。

## 怎么调
# ① 最省事：一屏截图（不写脚本） /data/bin/chrome-nosandbox --headless=new --hide-scrollbars \ --window-size=1280,800 --screenshot=/tmp/shot.png "https://target" # ② 多视口 / 全页（Playwright，必须指定内核路径） export PLAYWRIGHT_BROWSERS_PATH=/data/cache/ms-playwright /data/tools/uv/tools/playwright/bin/python - <<'PY' from playwright.sync_api import sync_playwright with sync_playwright() as p: b = p.chromium.launch(args=["-…

## 坑
`chrome-nosandbox` 的 stderr 永远有 DBus/NetworkManager 噪音，别当失败；成功判据是 `… bytes written to file`。；Playwright 不设 `PLAYWRIGHT_BROWSERS_PATH` → `executable doesn't exist`；系统 `python3` 里没有 playwright，必须用上面那个 ven…

## 实际效果
`/data/bin/chrome-nosandbox --headless=new --hide-scrollbars --window-size=1280,800 --screenshot=/tmp/shot.pn…；Playwright（`PLAYWRIGHT_BROWSERS_PATH=/data/cache/ms-playwright` + `/data/tools/uv/tools/playwright/bin/python…

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）触发前缀 /ui；按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`/data/bin/chrome-nosandbox --headless=new --hide-scrollbars --window-size=1280,800 --scr…；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/04-设计与UI/B-ui-visual.md · 核实：2026-10-26（原文件 实际效果（实测 2026-10-26））
> 注意：截图一次成型 0.8s；多视口/全页/回归走 Playwright
