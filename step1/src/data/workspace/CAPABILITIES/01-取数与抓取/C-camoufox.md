# camoufox（Camoufox）
## 一句话
**Firefox(Gecko) 内核的反检测浏览器**：Camoufox v0.5.8 + 指纹生成器 fpgen + 配对 Playwright 1.62.0。`camoufox` 这个 CLI 只管"下载/切换/查看浏览器"，真正抓活得在 Python 里 `with Camoufox(...)`。

## 实际效果（实测 2026-10-26）
- `camoufox version` → `Camoufox v0.5.8` / `fpgen v1.3.0` / `Playwright v1.62.0`；Browser Active = `official/stable`，Current browser = `v156.0.1-beta.36 (72637885)`，Install path `/data/cache/camoufox`，磁盘 2.4 GB，GeoIP 数据库 114.9 MB ✅
- `camoufox path` → `/data/cache/camoufox`；`camoufox active` → `official/stable/156.0.1-beta.36 (72637885) (paired with this release)`；`camoufox list` → 只有这一个版本 + geoip aio
- 入口文件头 `#!/data/tools/uv/tools/camoufox/bin/python`；**系统 python3 里没有 camoufox 包**（`ModuleNotFoundError: No module named 'camoufox'`）
- 实跑 `https://example.com`（headless）：`TITLE: Example Domain` / `UA: Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:156.0) Gecko/20100101 Firefox/156.0` / `navigator.webdriver: False` / content 1989 bytes ✅ —— UA 是**伪装成 macOS 的 Firefox**，不是本机 Linux
- `camoufox server` ❌ `Error launching server: Failed to launch the browser process.` + `Looks like you launched a headed browser without having a XServer running.`

## 怎么调
```bash
# 管理（不启动浏览器）
camoufox version | path | active | list
camoufox set official/stable          # 切版本并下载
camoufox sync                          # 同步远端可用版本

# 真抓取：必须用 camoufox 自己的 venv python
/data/tools/uv/tools/camoufox/bin/python -c '
from camoufox.sync_api import Camoufox
with Camoufox(headless=True) as b:
    p = b.new_page(); p.goto("https://target", timeout=60000)
    print(p.title()); print(p.content()[:300])'
```
CF/Turnstile 场景定位：**先试 scrapling 的 stealthy**（Chromium 系，自带 `solve_cloudflare` 专解 Turnstile/Interstitial）；被 Cloudflare 判定"Chromium 指纹可疑"时，再换 camoufox 用 Gecko 指纹过一遍——两者是**互补的第二身份**，不是替代。

## 坑
- 必须用 `/data/tools/uv/tools/camoufox/bin/python`；`python3 -c "import camoufox"` 直接 ModuleNotFoundError（我第一轮就踩了）。
- 配对 Playwright 是 **1.62.0**，与 `/data/cache/ms-playwright` 下 1.64.1 的内核不是一套；别给 camoufox 设 `PLAYWRIGHT_BROWSERS_PATH` 指到 playwright 的缓存目录。
- `camoufox server` / `gui` 要 X server：本机 `DISPLAY` 未设，`xvfb-run` 也因缺 `xauth` 起不来 → 这条通道在本机 ❌；抓取一律用 Python API + `headless=True`。
- 浏览器 2.4 GB 落在 `/data/cache/camoufox`（持久卷），`camoufox remove` 会连版本一起删，之后要重新 `fetch`。
- 和 `S-scrapling.md` 的 stealthy **内核不同**（Gecko vs Chromium），指纹、TLS、header 顺序全不一样；同一目标不要两个混着刷，容易被记成同源异常。
- 2 GB 内存机器上一次只开一个实例，`with` 块结束即关；别写常驻脚本。
