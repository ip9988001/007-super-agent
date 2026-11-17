# cap-fetch-scrapling-mcp

## 一句话
scrapling 的 MCP 门：stdio server（`/data/tools/uv/bin/scrapling-mcp` → `from scrapling.cli import mcp`），把同一套 scrapling 引擎包成 MCP 工具给 agent 用；和在用的 `scrapling` CLI 是同一个包的两个入口。

## 怎么调
scrapling-mcp # stdio（agent 就是这样挂的，手动别在前台跑） scrapling-mcp --http --port 8000 # 改 streamable-http，便于远程/多客户端/自测 scrapling-mcp --http --auth-token xxx --allowed-host example.com # 对外开放时加门

## 坑
`lifecycle: lazy` + `idleTimeout: 30`：首调用要等进程冷启动；30 秒空闲就回收，`open_session` 建立的长会话跟着一起断，长流程要么连续调用要么改用 `--http`…；stdio 模式在前台手跑会占住终端并等 JSON-RPC，看起来像"卡死"——调试一律 `--http`。

## 实际效果
`which scrapling-mcp` → `/usr/local/bin/scrapling-mcp` → 软链 `/data/tools/uv/bin/scrapling-mcp`；shebang `#!/da…；`scrapling-mcp --help` → 选项：`--http`（stdio ↔ streamable-http）、`--host/--port`（默认 127.0.0.1:8000）、`--executabl…

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`which scrapling-mcp` → `/usr/local/bin/scrapling-mcp` → 软链 `/data/tools/uv/bin/scrapling…；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/01-取数与抓取/S-scrapling-mcp.md · 核实：2026-10-26（原文件 实际效果（实测 2026-10-26））
> 注意：agent 侧 mcp__scrapling__*；与 CLI 同包
