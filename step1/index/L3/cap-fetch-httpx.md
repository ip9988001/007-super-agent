# cap-fetch-httpx

## 一句话
✅ 本机 httpx 0.28.1 可用，但必须绕开 env 代理。病因：HTTP_PROXY / HTTPS_PROXY 指向 http://127.0.0.1:43117（llmtrim MITM），httpx 经该代理访问普通站点时 URL 解析畸形，报 `Invalid port ':'`。实测 `httpx.get(url, trust_env=…

## 怎么调
httpx https://api/x # 头+体全打，看连通性 httpx -j '{"a":1}' https://api/x # 有 body 就默认 POST httpx -h X-Token abc -p page 2 -m GET https://api/x # 空格分参，别用冒号 httpx --download /tmp/o.html https://x # 要纯净 body 就落盘再读 httpx --follow-redirects --timeout 10 https://x # 超时默认只有 5s httpx https://httpbin.org/get | sed -n '/^{/,$p' | jq . # 管道里剥头

## 坑
别再对着 `--version` 找版本：这 CLI 天生没有；查版本用 `uv tool list` / `pip show httpx`。；别用 `pip` 装（系统 python 要 `--break-system-packages`、容器重建会回滚）；用 `uv tool install`，实体在 `/data/tools/uv-tools/httpx…

## 实际效果
`httpx https://example.com` ✅ `HTTP/1.1 200 OK`，0.241s（不再报 `Invalid port: ':'`）；`httpx --version` ❌ `Error: No such option '--version'. (Did you mean one of: '--json', '--no-verify', '--ver…

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）前置：unset HTTP_PROXY/HTTPS_PROXY 或 trust_env=False。按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`httpx https://example.com` ✅ `HTTP/1.1 200 OK`，0.241s（不再报 `Invalid port: ':'`）；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/01-取数与抓取/H-httpx.md · 核实：2026-10-26（原文件 实际效果（实测 2026-10-26））
> 注意：Python 库默认走 env 代理会报 Invalid port
