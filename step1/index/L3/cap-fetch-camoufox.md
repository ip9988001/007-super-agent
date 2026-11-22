# cap-fetch-camoufox

## 一句话
Firefox(Gecko) 内核的反检测浏览器：Camoufox v0.5.8 + 指纹生成器 fpgen + 配对 Playwright 1.62.0。`camoufox` 这个 CLI 只管"下载/切换/查看浏览器"，真正抓活得在 Python 里 `with Camoufox(...)`。

## 怎么调
# 管理（不启动浏览器） camoufox version | path | active | list camoufox set official/stable # 切版本并下载 camoufox sync # 同步远端可用版本 # 真抓取：必须用 camoufox 自己的 venv python /data/tools/uv/tools/camoufox/bin/python -c ' from camoufox.sync_api import Camoufox with Camoufox(headless=True) as b: p = b.new_page(); p.goto("https://target", timeout=60000) print(p.title()); print(p.content()[:300])'

## 坑
必须用 `/data/tools/uv/tools/camoufox/bin/python`；`python3 -c "import camoufox"` 直接 ModuleNotFoundError（我第一轮就踩了）。；配对 Playwright 是 1.62.0，与 `/data/cache/ms-playwright` 下 1.64.1 的内核不是一套；别给 camoufox 设 `PLAYWRIGHT_BROWSERS_PATH…

## 实际效果
`camoufox version` → `Camoufox v0.5.8` / `fpgen v1.3.0` / `Playwright v1.62.0`；Browser Active = `official/sta…；`camoufox path` → `/data/cache/camoufox`；`camoufox active` → `official/stable/156.0.1-beta.36 (72637885) (pai…

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`camoufox version` → `Camoufox v0.5.8` / `fpgen v1.3.0` / `Playwright v1.62.0`；Browser Ac…；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/01-取数与抓取/C-camoufox.md · 核实：2026-10-26（原文件 实际效果（实测 2026-10-26））
> 注意：CLI 只管版本，抓取走 Python API
