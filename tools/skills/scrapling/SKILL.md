---
name: scrapling
description: >-
  网页抓取与内容提取引擎（MCP 原生 13 工具 + Python 直调）。MUST USE 当任务涉及：
  抓网页 / 爬取 / 提取网页内容 / 抓结构化数据 / 网站改版后还要能抓 / 页面是 JS 渲染的 SPA /
  遇到 Cloudflare、Turnstile 等反爬 / 需要 TLS 指纹伪装 / 要截图 / 要批量抓一批 URL /
  要登录态才能看的页面 / 定时监控某网页变化。
  本 skill 负责「怎么把网页抠干净」；平台化搜索（推特/小红书/B站/RSS/YouTube）请走 agent-reach。
summary: 三级火力（HTTP指纹 / JS渲染 / 隐身反爬）+ 会话复用 + 内存纪律
metadata:
  cli: /root/.local/bin/scrapling
  mcp: scrapling（~/.pi/agent/mcp-adapter.json 已注册）
  python: /root/.local/pipx/venvs/scrapling/bin/python
  docs: https://scrapling.readthedocs.io/en/latest/
  version: 0.4.15
---

# Scrapling — 网页抠取引擎

## 0. 先接决策阶梯

`AGENTS.md` 的「抓取决策阶梯」已经规定**第几阶用什么**。本 skill 只回答**到了那一阶怎么调**。

默认顺序（省内存优先）：

```
curl+jq → r.jina.ai → make_request → fetch → stealthy_fetch
```

**铁律：同一个 URL 失败一次就升级一阶，不许在同一阶试到第三次。**

---

## 1. 三级火力（MCP 工具名速查）

### 🥉 第一级 · HTTP（不吃浏览器，秒级，优先用）

| 工具 | 关键参数 |
|---|---|
| `scrapling_make_request` | `url`（必填）、`method`（GET/POST/PUT/DELETE）、`impersonate`（默认 `chrome`，可选 chrome146 / firefox147 / safari260 / edge145 / tor145 等） |
| `scrapling_bulk_get` | `urls`（数组）、`impersonate` |
| `scrapling_open_request_session` | `session_id`、`impersonate` → 配 `session_make_request` 复用连接 |
| `scrapling_session_make_request` | 配合上面的会话 |

**用途**：TLS 指纹伪装、HTTP/2-3、轻反爬、公开 API、要结构化提取。
**能过就停在这一级 —— 不起浏览器意味着零内存代价。**

### 🥈 第二级 · JS 渲染（Playwright Chromium）

| 工具 | 关键参数 |
|---|---|
| `scrapling_fetch` | `url`（必填）、`extraction_type`=`markdown`/`text`/`html`、`css_selector`、`main_content_only`（默认 true，**只留正文**）、`headless`（默认 true）、`wait`、`useragent`、`real_chrome`、`proxy`、`cdp_url` |
| `scrapling_bulk_fetch` | 同上，批量 URL |

**用途**：SPA、无限滚动、内容由 JS 渲染的页面。
**注意**：`main_content_only=true` 会砍掉导航/页脚 —— 要抓列表或特定区域时记得传 `css_selector` 并考虑关掉它。

### 🥇 第三级 · 隐身（反反爬）

| 工具 | 关键参数 |
|---|---|
| `scrapling_stealthy_fetch` | 同 `fetch`，另加 `hide_canvas`、`block_webrtc`、`allow_webgl` |
| `scrapling_bulk_stealthy_fetch` | 批量版 |

**用途**：Cloudflare / Turnstile / 浏览器指纹检测挡住的页面。

---

## 2. 会话复用（多页同站必用，比反复拉起浏览器省得多）

```
open_session(session_type="dynamic"|"stealthy", session_id="myjob", cookies=[...])
   ↓ 多次
session_fetch(url, session_id="myjob", wait_selector=".item", network_idle=true)
   ↓
screenshot(url, session_id="myjob", full_page=true)      # 需要留证
   ↓
close_session(session_id="myjob")                        # ← 绝不能忘
```

`session_fetch` 常用参数：`wait_selector` + `wait_selector_state`（attached/detached/hidden/visible）、`network_idle`、`load_dom`、`timeout`（默认 30000，毫秒）、`disable_resources`、`blocked_domains`、`extra_headers`。

**忘记关会话的代价**：浏览器进程常驻，2GB 机器会被拖死。用 `scrapling_list_sessions` 查漏。

---

## 3. 内存纪律（这台机器只有 1966MB — 硬约束）

1. **能用 `make_request` 就绝不上浏览器。**
2. **浏览器只按需拉起，干完立刻 `close_session`。**
3. **批量抓串行或小并发** —— `bulk_fetch` 一次别塞超过 3~5 个 URL。
4. **抓完自检残留**：

```bash
pgrep -af "chrome|headless_shell" || echo "(干净)"
free -m | head -2
```

5. 有残留就 `pkill -f headless_shell`。

---

## 4. Python 直调（MCP 没暴露的能力走这里）

MCP 工具没暴露 **自适应元素重定位**（网站改版后还能找到元素）—— 这是 Scrapling 最值钱的特性，需要时用 Python：

```python
# /root/.local/pipx/venvs/scrapling/bin/python
from scrapling.fetchers import Fetcher, StealthyFetcher, DynamicFetcher

p = Fetcher.get("https://example.com", impersonate="chrome")   # 不起浏览器
items = p.css(".item::text")                                   # 首次：auto_save 学习结构
# items = p.css(".item", auto_save=True)                       # 存下元素特征
# 网站改版后：
# items = p.css(".item", adaptive=True)                        # 靠相似度重新定位
```

三级类名：`Fetcher`（HTTP）/ `DynamicFetcher`（JS）/ `StealthyFetcher`（隐身）。
`DynamicFetcher` 起浏览器时记得 `headless=True`。

---

## 5. 自检（怀疑坏了时跑这一条）

```bash
/root/.local/pipx/venvs/scrapling/bin/python - <<'EOF'
from scrapling.fetchers import Fetcher, DynamicFetcher
assert Fetcher.get("https://example.com", impersonate="chrome").status == 200, "HTTP 级挂了"
d = DynamicFetcher.fetch("https://quotes.toscrape.com/js/", headless=True, network_idle=True)
assert len(d.css(".quote .text::text")) >= 5, "JS 渲染级挂了"
print("✅ 三级火力正常")
EOF
```

---

## 6. 已知坑

| 症状 | 原因 / 解法 |
|---|---|
| `ModuleNotFoundError: No module named 'mcp'` | 缺 `[ai]` extra。重装：`pipx install --force "scrapling[fetchers,ai]"` |
| 抓回来是空壳 | 页面靠 JS 渲染 → 升到 `fetch`；或 `main_content_only` 把目标砍了 → 传 `css_selector` |
| MCP 里找不到 scrapling | 配置在 `~/.pi/agent/mcp-adapter.json`（**不是 mcp.json**，适配器 v3.3.0 起改名）。改动只在**下一条消息**生效 |
| 会话工具报 not connected | lazy 模式，首次用 `mcp({ connect: "scrapling" })` 唤醒 |
| 内存飙升 | 忘记 `close_session`，见第 3 节 |

---

## 7. 分工边界（别抢活）

- **平台化搜索**（推特 / 小红书 / B站 / Reddit / RSS / YouTube 字幕）→ `agent-reach`
- **登录态 Cookie 来源**：`agent-reach configure xhs-cookies` 之类
- **纯 JSON API** → `curl` + `jq` 更快
- **普通静态长文** → `r.jina.ai` 更快
