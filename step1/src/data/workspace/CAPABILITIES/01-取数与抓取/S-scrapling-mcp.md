# scrapling-mcp（MCP 入口）
## 一句话
scrapling 的 **MCP 门**：stdio server（`/data/tools/uv/bin/scrapling-mcp` → `from scrapling.cli import mcp`），把同一套 scrapling 引擎包成 MCP 工具给 agent 用；和在用的 `scrapling` CLI 是**同一个包的两个入口**。

## 实际效果（实测 2026-10-26）
- `which scrapling-mcp` → `/usr/local/bin/scrapling-mcp` → 软链 `/data/tools/uv/bin/scrapling-mcp`；shebang `#!/data/tools/uv/tools/scrapling/bin/python`，主体 `from scrapling.cli import mcp` ✅
- `scrapling-mcp --help` → 选项：`--http`（stdio ↔ streamable-http）、`--host/--port`（默认 127.0.0.1:8000）、`--executable-path`（自定义 Chromium 兼容内核）、`--auth-token`、`--allowed-host`（DNS-rebinding 防护）✅
- 注册位置：`/root/.pi/agent/mcp-adapter.json` →
  `"scrapling": {"type":"stdio","command":"/data/tools/uv/bin/scrapling-mcp","lifecycle":"lazy","idleTimeout":30}`
  → agent 侧表现为 `mcp__scrapling__*` 工具族（fetch / bulk_fetch / stealthy_fetch / open_session …）
- 版本共底：`scrapling --version` → `Scrapling, version 0.5`；MCP server 与 CLI 用的是同一份 `site-packages`

## 怎么调
```bash
scrapling-mcp                                  # stdio（agent 就是这样挂的，手动别在前台跑）
scrapling-mcp --http --port 8000               # 改 streamable-http，便于远程/多客户端/自测
scrapling-mcp --http --auth-token xxx --allowed-host example.com   # 对外开放时加门
```
Agent 内直接用工具名：`mcp__scrapling__fetch` / `stealthy_fetch` / `bulk_fetch` / `open_session`（见 `S-scrapling.md` 的引擎差异，CLI 与 MCP 调的是同一批 fetcher）。

## 坑
- `lifecycle: lazy` + `idleTimeout: 30`：首调用要等进程冷启动；**30 秒空闲就回收**，`open_session` 建立的长会话跟着一起断，长流程要么连续调用要么改用 `--http` 常驻。
- stdio 模式在前台手跑会占住终端并等 JSON-RPC，看起来像"卡死"——调试一律 `--http`。
- `--http` 默认只听 `127.0.0.1`；想让别的容器访问必须同时给 `--auth-token` 和 `--allowed-host`，否则默认拒绝。
- 只能用它自己的 python（`/data/tools/uv/tools/scrapling/bin/python`）；系统 `python3 -m scrapling` 没有这个包。
- `--executable-path` 只认 **Chromium 兼容**内核；别指 camoufox（Gecko）——那条腿要走 `C-camoufox.md` 的 Python API。
- MCP 与 CLI 共享浏览器缓存/锁：同时开多个实例抢同一个 Chromium profile 会报 profile in use。
