# cap-fetch-scrapling

## 一句话
抓取框架 v0.5，两条通道：CLI（`/data/tools/uv/bin/scrapling`）和 MCP server（16 个工具，已挂在 pi 里）。反爬/stealth 渲染/会话保持都在这。

## 怎么调
# CLI：get=普通 HTTP，fetch=stealthy 浏览器，post/put/delete 同理 scrapling extract get <url> out.md # .md/.html/.txt 由后缀决定格式 scrapling extract fetch <url> out.md # 渲染 + 反爬（慢但强） scrapling extract get <url> out.txt -s "article" --impersonate chrome # 只取 CSS 选中内容 scrapling extract get <url> out.md -H "Referer: https://x.com/" --timeout 60 --proxy http://ip:port scrapling mcp # 自己起 MCP server

## 坑
阶梯顺序别搞反：先 `make_request` / `extract get`（不吃浏览器），再 `extract fetch`（渲染），最后 `solve_cloudflare`。；`browser_*` 一定要 `scrapling_close_session`，本机 2GB 内存，漏关会累积。

## 实际效果
`scrapling --version` → `Scrapling, version 0.5` ✅；`scrapling extract get https://example.com /tmp/x.md` → ✅ `Fetched (200)`，落盘 markdown 2 行

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`scrapling --version` → `Scrapling, version 0.5` ✅；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/01-取数与抓取/S-scrapling.md · 核实：2026-10-26（原文件 实际效果（实测 2026-10-26））
> 注意：CLI + MCP 双通道
