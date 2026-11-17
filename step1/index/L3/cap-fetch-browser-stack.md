# cap-fetch-browser-stack

## 一句话
两条真浏览器通道：`chrome-nosandbox`（单次 dump，最省事）和 Playwright 三内核（chromium/firefox/webkit，可交互）。

## 怎么调
# 1) 最省事：渲染后直接吐 DOM，不用写脚本 chrome-nosandbox --headless=new --dump-dom "https://target" | head -50 # 2) 要交互/截图/执行 JS：必须指定浏览器缓存目录 export PLAYWRIGHT_BROWSERS_PATH=~/.cache/ms-playwright python3 -c ' from playwright.sync_api import sync_playwright with sync_playwright() as p: b=p.chromium.launch(args=["--no-sandbox","--disable-dev-shm-usage"]) pg=b.new_p…

## 坑
不设 `PLAYWRIGHT_BROWSERS_PATH` → 去默认路径找浏览器，报 executable doesn't exist。；容器里必须 `--no-sandbox --disable-dev-shm-usage`（`chrome-nosandbox` 包装脚本已内置）。

## 实际效果
`chrome-nosandbox --version` → Google Chrome for Testing 156.0.8078.4 ✅；`chrome-nosandbox --headless=new --dump-dom https://example.com` → ✅ 完整渲染后 DOM

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`chrome-nosandbox --version` → Google Chrome for Testing 156.0.8078.4 ✅；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/01-取数与抓取/B-browser-stack.md · 核实：2026-10-26（原文件 实际效果（实测 2026-10-26））
> 注意：chrome-nosandbox 单次 dump 最省事
