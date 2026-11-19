# scrapling
## 一句话
抓取框架 v0.5，两条通道：CLI（`/data/tools/uv/bin/scrapling`）和 MCP server（16 个工具，已挂在 pi 里）。反爬/stealth 渲染/会话保持都在这。

## 实际效果（实测 2026-10-26）
- `scrapling --version` → `Scrapling, version 0.5` ✅
- `scrapling extract get https://example.com /tmp/x.md` → ✅ `Fetched (200)`，落盘 markdown 2 行
- `scrapling extract fetch https://example.com /tmp/y.md` → ✅ stealthy 渲染通道，落盘 1358 字节
- MCP：`scrapling_browser_fetch_once` 实调 → ✅ `{status:200, content:[markdown], url}` 
- MCP 已注册 16 工具：browser_open / browser_fetch / browser_fetch_once / browser_extract / browser_actions / browser_evaluate / browser_screenshot / browser_network_requests / make_request / open_request_session / session_make_request / list_sessions / close_session / save_state / load_state / close_session
- 注意：`import scrapling` 在系统 python3 里失败（ModuleNotFoundError），只有 CLI 和 MCP 两条路可用

## 怎么调
```bash
# CLI：get=普通 HTTP，fetch=stealthy 浏览器，post/put/delete 同理
scrapling extract get  <url> out.md            # .md/.html/.txt 由后缀决定格式
scrapling extract fetch <url> out.md           # 渲染 + 反爬（慢但强）
scrapling extract get <url> out.txt -s "article" --impersonate chrome  # 只取 CSS 选中内容
scrapling extract get <url> out.md -H "Referer: https://x.com/" --timeout 60 --proxy http://ip:port
scrapling mcp                                   # 自己起 MCP server
```
MCP 调用（在 pi 里直接调）：
```
scrapling_browser_fetch_once(url=..., extraction_type="markdown", solve_cloudflare=true)
scrapling_make_request(method="GET", url=...)   # 不起浏览器，先走这个
scrapling_browser_open(...) → 拿到 session_id → browser_fetch / browser_actions / browser_screenshot
```

## 坑
- 阶梯顺序别搞反：先 `make_request` / `extract get`（不吃浏览器），再 `extract fetch`（渲染），最后 `solve_cloudflare`。
- `browser_*` 一定要 `scrapling_close_session`，本机 2GB 内存，漏关会累积。
- `extract fetch` 比 get 慢一个量级，静态页用 fetch 是浪费。
- 输出必须给文件名，CLI 不往 stdout 打正文（要看内容 `cat` 那个文件）。
- 系统 python 不能 `import scrapling`，别写 python 脚本直调库。
