# httpx（CLI）
## 一句话
✅ 本机 httpx 0.28.1 **可用**，但必须绕开 env 代理。病因：HTTP_PROXY / HTTPS_PROXY 指向 http://127.0.0.1:43117（llmtrim MITM），httpx 经该代理访问普通站点时 URL 解析畸形，报 `Invalid port ':'`。实测 `httpx.get(url, trust_env=False)` 直接 200。用法：httpx 请求一律加 `trust_env=False`，或在脚本里 unset HTTP_PROXY/HTTPS_PROXY。curl 不需要处理，因为它对畸形代理更宽容。

## 实际效果（实测 2026-10-26 14:26）
- `httpx https://example.com` ✅ `HTTP/1.1 200 OK`，0.241s（不再报 `Invalid port: ':'`）
- `httpx --version` ❌ `Error: No such option '--version'. (Did you mean one of: '--json', '--no-verify', '--verbose'?)` —— 天生没这选项，不是故障
- `-j` 打 POST `https://httpbin.org/post` ✅ 回显 `"json": {"a": 1}`，UA `python-httpx/0.28.1`
- 头/参数必须**空格分两参**：`-h X-Test v1 -p foo bar` ✅；写 `-h 'X-Test: v1'` ❌ `Missing argument 'URL'`
- `--download /tmp/hx2.html https://example.com` ✅ 577 字节，gzip 已自动解码
- `--follow-redirects` ✅ 302 → 200 连打；**默认不跟随**（裸给 302）
- `--http2` ❌ `ImportError: ... the 'h2' package is not installed`（需 `httpx[http2]`）
- ⚠️ **响应头永远打到 stdout**，无 body-only 开关 → 直接 `| jq` 必失败

## 怎么调
```bash
httpx https://api/x                                 # 头+体全打，看连通性
httpx -j '{"a":1}' https://api/x                     # 有 body 就默认 POST
httpx -h X-Token abc -p page 2 -m GET https://api/x  # 空格分参，别用冒号
httpx --download /tmp/o.html https://x               # 要纯净 body 就落盘再读
httpx --follow-redirects --timeout 10 https://x      # 超时默认只有 5s
httpx https://httpbin.org/get | sed -n '/^{/,$p' | jq .   # 管道里剥头
```
**什么时候用它而不是 curl**：① JSON/表单/多文件免引号地狱（`-j -d -f --auth` 全空格分参）——这是它唯一真优势；② 要 curl 那样的 `-w` 模板、纯 body 管道、HTTP/2 —— **回 curl**（本机 curl 的 h2 正常）。

## 坑
- 别再对着 `--version` 找版本：这 CLI 天生没有；查版本用 `uv tool list` / `pip show httpx`。
- 别用 `pip` 装（系统 python 要 `--break-system-packages`、容器重建会回滚）；用 `uv tool install`，实体在 `/data/tools/uv-tools/httpx/`。
- 管道解析 JSON 必须先剥头或 `--download` 落盘，没有第三种开关。
- `--download` 的进度条会打出诡异 `400/0 bytes`，不影响文件内容。
- 它只管发请求，解析 HTML/JSON 仍靠 jq / python。
