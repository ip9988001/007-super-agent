# llmtrim 0.13.4

> **一句话**：本地 MITM 压缩代理（`127.0.0.1:43117`），改写发往各家 LLM API 的请求体以少付 input token。

## 实际效果（实测 · 2026-10-27 复测）
- 进程活着：`PID 84`，`llmtrim serve --port 43117 --supervised`（PPid=1，v0.13.4，`restarts:0`），`up 10h41m`。
- 资源：`VmRSS 89MB` 恒定、9 线程、累计 CPU `7.9s/10h41m`（≈0.02%），无空转。
- 真实流量：`tracking.db` 56 条，全 `deepseek-flash`，最近一次 `2026-10-26 15:23`，input `-6%`；此后静默。
- `llmtrim _alive` → exit 0；`curl -x :43117 https://example.com/` → `connect=200`。**服务健康。**
- 直接 `GET http://127.0.0.1:43117/<任意路径>` 返回 `529 overloaded_error`（**预期行为，见坑 1**）。

## 怎么调
```
/data/bin/llmtrim-health.sh        # 判活正解：端口可连 + CONNECT 端到端，OK/DOWN + exit 0/1
llmtrim _alive; echo $?            # 语义判活：exit 0
curl -s -o /dev/null -w '%{http_connect}\n' -x http://127.0.0.1:43117 https://example.com/   # 期望 200
llmtrim status | head -3           # 看 running / 省了多少
llmtrim start|stop                 # 起停（start.sh 3b 段每次重建都会 start）
export HTTPS_PROXY=http://127.0.0.1:43117 NODE_EXTRA_CA_CERTS=/data/state/llmtrim/ca.pem
```

## 坑
1. **529 不是故障，是协议误用**：它是 CONNECT-only 代理，无 HTTP 路由；`GET /health` 这类 origin-form 请求缺 absolute URI → hyper `UserAbsoluteUriRequired` → 被统一归类为 client error → 映射成 `overloaded_error` + `Retry-After: 1` → **HTTP 529**。529 在此=「你发错了请求（可重试）」，与过载无关。**禁止用 GET 探 43117 判死活。**
2. 判活只认三种：`llmtrim _alive` exit 0 / TCP `exec 3<>/dev/tcp/127.0.0.1/43117` / `curl -x … -w '%{http_connect}'` = 200。`/data/bin/llmtrim-health.sh` 已封装前两条。
3. `--supervised` 是 CLI 内部标志（`start` 派生 `serve` 时用），别手传；本进程 `PPid=1` 说明派生它的父 daemon 已退出，**没有真 supervisor**，靠 `start.sh` 3b 段重建时拉起。
4. **旧结论已过期**：`⚠ degraded — env not wired` 不再成立——`.bashrc/.profile` 已接线（`# >>> llmtrim >>>` 块，受 `llmtrim _alive` 门控），网关 `runPi()` 按次注入 `HTTPS_PROXY`。
5. login shell 里带 `HTTPS_PROXY` 会让 Python `httpx` 报 `Invalid port ':'`（见 01 类 `H-httpx.md`），须 `trust_env=False`。
6. CA 实际落盘 `/data/state/llmtrim/ca.pem`（`/root/.llmtrim` 只是软链，3a 段建的）；`ca.hosts` 只列 35 个已知 API 域名，新域名要补。
7. 网关侧「按次探测，挂掉自动直连」是 fail-open：代理死了不断服，但也**不会报警**——所以要外部探针。
8. 只压 input；`~-73% output` 是估算值，别当账单依据。抓取/浏览器出口链路**不要**叠这一层。
9. 版本落后：本机 0.13.4，上游 0.13.7（`update-check.json`），补丁级，与 529 无关。
