# 01 取数与抓取 · 条目索引
> 挑一条，只读那个 md。工具按文件名首字母排序。

- A-agent-reach.md :: 只读取小红书/B站/推特/YouTube/Reddit 等平台内容
- B-bili.md :: B站 CLI 0.6.2（热门/排行/搜索只读 ✅，未登录；`audio` 出 ASR-ready WAV）
- B-browser-stack.md :: 真浏览器渲染（chrome-nosandbox + playwright 三内核）
- C-camoufox.md :: **Firefox 内核反检测浏览器**（Camoufox 0.5.8 + fpgen；CLI 只管版本，抓取走 Python API）
- C-curl-jq.md :: 纯 HTTP 拿数据（jq 1.6 已装，可直接 jq 管道）
- G-gh-cli.md :: GitHub CLI 2.103.2（已登录 ip9988001，gh api / gh search 可用）
- H-httpx.md :: ⚠️ httpx 0.28.1 CLI 可用（重装过）；但 Python 库默认走 env 代理 (127.0.0.1:43117 llmtrim) 会报 Invalid port，须 `trust_env=False` 或 unset 代理。
- L-lightpanda.md :: 轻量 JS 执行引擎，不起渲染，出 markdown/html
- P-playwright-cli.md :: playwright 1.64.1 **命令行侧**（install / screenshot / pdf；codegen 需 display ❌）
- S-scrapling-mcp.md :: scrapling 的 MCP 入口（stdio，agent 侧 `mcp__scrapling__*`；与 CLI 同包）
- S-scrapling.md :: 抓取框架，CLI + MCP 双通道，反爬/stealth 渲染（Chromium 内核）
- T-tvly.md :: Tavily 搜索/抽取 CLI（未登录也有免费返回）
- Y-yt-dlp.md :: YouTube 等视频元数据与字幕（无 JS runtime，部分格式缺）
