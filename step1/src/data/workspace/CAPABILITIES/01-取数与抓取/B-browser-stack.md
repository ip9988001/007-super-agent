# browser-stack
## 一句话
两条真浏览器通道：`/data/bin/chrome-nosandbox`（单次 dump，最省事）和 Playwright 三内核（chromium/firefox/webkit，可交互）。

## 实际效果（实测 2026-10-26）
- `/data/bin/chrome-nosandbox --version` → Google Chrome for Testing 156.0.8078.4 ✅
- `chrome-nosandbox --headless=new --dump-dom https://example.com` → ✅ 完整渲染后 DOM
- Playwright 1.64.1（`/data/tools/uv/bin/playwright`，python 解释器 `/data/tools/uv/tools/playwright/bin/python`）
- 三内核 launch + goto 实测：chromium ✅ / firefox ✅ / webkit ✅（标题均为 Example Domain，content 1989 字节）
- 注意：系统 `python3` 里 **没有** playwright 模块，必须用上面那个 venv 解释器

## 怎么调
```bash
# 1) 最省事：渲染后直接吐 DOM，不用写脚本
/data/bin/chrome-nosandbox --headless=new --dump-dom "https://target" | head -50

# 2) 要交互/截图/执行 JS：必须指定浏览器缓存目录
export PLAYWRIGHT_BROWSERS_PATH=/data/cache/ms-playwright
/data/tools/uv/tools/playwright/bin/python -c '
from playwright.sync_api import sync_playwright
with sync_playwright() as p:
    b=p.chromium.launch(args=["--no-sandbox","--disable-dev-shm-usage"])
    pg=b.new_page(); pg.goto("https://example.com",timeout=30000)
    print(pg.title()); print(pg.inner_text("body")[:500])
    pg.screenshot(path="/tmp/s.png"); b.close()'
```
已装内核：chromium-1208 / chromium-1248 / chromium_headless_shell-1208+1248 / firefox-1555 / webkit-2370 / ffmpeg-1011+1013

## 坑
- 不设 `PLAYWRIGHT_BROWSERS_PATH` → 去默认路径找浏览器，报 executable doesn't exist。
- 容器里必须 `--no-sandbox --disable-dev-shm-usage`（`chrome-nosandbox` 包装脚本已内置）。
- 别用 `locator("h1")` 猜选择器：example.com 现在没有 h1，会白等 30s 然后 TimeoutError（我踩过）。先 `page.content()` 看结构。
- 内存紧（本机 2GB），一次只开一个浏览器，用完 `b.close()`；交互式长跑优先 scrapling MCP 的 session 管理。
- 慢/被封时不要在这上面硬试，降级到 lightpanda 或 scrapling。
