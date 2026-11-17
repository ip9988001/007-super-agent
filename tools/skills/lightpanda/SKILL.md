---
name: lightpanda
description: >-
  JS 执行引擎（无渲染浏览器）。MUST USE 当出现以下任一信号：curl/requests 拿回来是空壳
  （只有 LOADING、骨架屏、空 div）/ 页面数据必须由 JS 执行后才出现 / 需要 JS 算出来的
  参数（签名、csrf token、加密参数）/ 要多步交互（登录→导航→提交）但不需要看页面 /
  同一批 JS 驱动页面要反复抓且内存吃紧（常驻仅 14MB）/ 几十上百页的批量抓取。
  NOT for：纯静态页与公开 API（直接用 curl/Python，更省更快）/
  数据其实内嵌在 HTML 的 <script> 里（正则抠 JSON 即可）/
  要截图、渲染、视觉验证（它没有渲染引擎，用 Chrome）/
  反爬、Cloudflare、验证码（用 scrapling stealthy_fetch 或 BrowserAct）/
  需要滚动或复杂交互（CLI fetch 不带滚动，走 CDP + Playwright）。
summary: 只在「必须执行 JS」且「完全不需要渲染」的交叉点上使用，常驻 14MB / 单次峰值 31MB
metadata:
  binary: /usr/local/bin/lightpanda
  version: 1.1.0-nightly.10033+614c1f56e
  cdp: ws://127.0.0.1:9222/
  docs: https://lightpanda.io/docs
  license: AGPL-3.0
  upstream: https://github.com/lightpanda-io/browser
---

# Lightpanda — JS 执行引擎（无渲染）

## 0. 它唯一的不可替代性

> **在不烧 2GB 内存的前提下执行 JS。**

由此推出铁律：**只要不需要执行 JS，它就毫无价值** —— `curl` / Python 更省内存、更快、更简单。
所以它只活在「必须执行 JS」且「完全不需要渲染」的交叉点上。这不是"另一个爬虫工具"。

---

## 1. 本能触发条件（命中任一，才想起它）

1. **`curl` / `requests` 拿回来是空壳** —— 只有 `LOADING`、骨架屏、空 `<div>`、`<noscript>`
2. **需要 JS 算出来的参数** —— 签名、csrf token、加密参数、时间戳混淆（Python 要逆向 JS 才能构造请求，它直接执行）
3. **多步交互但不看页面** —— 登录 → 导航 → 提交，要真实 DOM + cookies，但不需要视觉
4. **内存吃紧 + 反复跑** —— 常驻轮询、批量、多 agent 并发（常驻 14MB）
5. **批量抓取** —— 几十上百页，Chrome 在这个量级上会 OOM

---

## 2. 反触发（先排除，别用它）

| 情况 | 应该用什么 |
|---|---|
| 纯静态页 / 公开 API | `curl` + `jq` / Python —— **它更省更快，别上浏览器** |
| 数据内嵌在 HTML 的 `<script>` 里 | 正则 / json 抠出来，Python —— 见第 3 节 |
| 要用截图做**视觉验证** | Chrome（它出图但**不应用 CSS**，见 4.D） |
| 反爬 / Cloudflare / 验证码 | `scrapling_stealthy_fetch` / BrowserAct |
| 要滚动 / 复杂交互 | Chrome（CLI fetch 不带滚动） |
| 生产关键路径要求稳定 | 它**只有 nightly**，无稳定版 |

---

## 3. 🚨 动手前的三级探针（别跳，一秒钟省一次浏览器）

```bash
U="https://目标站"; KW="要找的关键词"

# L0 静态就有 → Python 直接干，收工
curl -s "$U" | grep -q "$KW" && echo "L0 静态页，Python 收工" && exit 0

# L1 数据内嵌在 <script> 里 → 正则抠 JSON，收工
curl -s "$U" | grep -oE '<script[^>]*type="application/(ld\+)?json"[^>]*>' | head
curl -s "$U" | grep -cE 'window\.__|__NEXT_DATA__|initialState|application/json'
```

**L0/L1 都空 → 才是真正的 L2（JS 驱动），才允许进第 4 节。**

> 📌 **实测依据**：`quotes.toscrape.com/js/` 看着是 JS 渲染，引文其实**全在 HTML 的 `<script>` 标签里**，
> Python 直取 5806 字节就能拿到全部数据。这类"伪 JS 渲染"页面是**大多数**。
> 别看到 "js" 就上浏览器。

---

## 4. 实测可用的三种用法

### A. 一次性抓取（CLI，最简）

```bash
lightpanda fetch --dump markdown --log-level error --wait-until networkidle "$URL"
```

| 参数 | 说明 |
|---|---|
| `--dump markdown \| html \| png \| pdf` | png/pdf 是**文字排版图**，不是截图 |
| `--wait-until networkidle` | 🚨 **异步 XHR 页面必须加**，否则返回空 |
| `--dump-selector <css>` | 只吐匹配的第一个元素 |
| `--dump-max-bytes <n>` | 限制输出体积 |
| `--obey-robots` | 要守规矩时加 |

> ⚠️ **实测坑**：`--wait-ms 1500` 对异步 XHR 页面**无效（返回空）**，
> 必须用 `--wait-until networkidle`。这是我踩过的，别重复踩。

### B. 交给 Scrapling / Playwright（CDP，功能最全）

```bash
lightpanda serve --host 127.0.0.1 --port 9222 --log-level error &
```

CDP 端点：`ws://127.0.0.1:9222/`

```
scrapling_fetch(url="...", cdp_url="ws://127.0.0.1:9222")
```

```python
# Playwright 直连（已实测通过）
from playwright.sync_api import sync_playwright
with sync_playwright() as p:
    b = p.chromium.connect_over_cdp("ws://127.0.0.1:9222")
    pg = b.new_page()
    pg.goto(url, wait_until="networkidle")
    txt = pg.text_content("#out")     # JS 渲染后的 DOM ✅
    pg.close(); b.close()
```

**要走滚动、点击、多步交互 → 一律走这条路**，CLI 做不到。

**实测（2026-10-03 本机）**：
- `scrapling_fetch(url=..., cdp_url="ws://127.0.0.1:9222")` ✅ **已验证**，拿到 bilibili 真实内容（视频标题 / UP 主 / 播放量）
- 无限滚动 ✅（`quotes.toscrape.com/scroll` 滚动前 10 条 → 滚动后 40 条）
- 表单登录 ✅（`#username`/`#password` fill + submit → 页面出现 Logout）

### C. 零 token 固化重复流程（官方特性，未实测）

```bash
lightpanda agent --task "抓取 X 站的价格"   # 用模型探路
# 会话内 /save 导出 PandaScript
lightpanda run session.js                   # 之后零 token、确定性重放
```
适合「这个站我每周都要抓一次」——模型只在探索阶段出现一次。

---

### D. ⚠️ 截图的真相（官方 README 说"无渲染引擎"，实测更微妙）

**它能出图，但不应用 CSS。** 实测证据链：

| 探针 | 实测值 | 含义 |
|---|---|---|
| `document.body.getBoundingClientRect().height` | `2190` | ✅ **有布局引擎** |
| `getComputedStyle(el).display` | `block` | ✅ 盒子模型存在 |
| `getComputedStyle(el).fontSize` | **空** | ❌ **无字体度量** |
| `page.screenshot()` | ✅ 成功，1280×720 / 65KB | 出的是**无样式裸文档** |

**结论**：截图返回的是一张"没加载 CSS 的网页"（衬线字体、默认蓝链接），
**站点自身的 CSS 完全没生效**。所以：

- ✅ 能用来验"**内容在不在**"（文字有没有渲染出来）
- ❌ **绝不能**用来做视觉/UI/响应式验证 —— 会得到完全错误的结论

> 视觉验证 → Chrome（`scrapling_screenshot`）。别被"截图成功了"骗了。

### E. 内置 adblocker 会误伤内容

实测 bilibili 首页正文里混进大量「该内容被 AdGuard/AdBlock 类插件屏蔽」——
Lightpanda 自带 adblocker，会把某些正常的动态内容接口当广告拦掉。
**抓荐类/信息流页面时若发现内容残缺，怀疑它。**

---

## 5. 内存纪律（这台机器 1966MB，硬约束）

| 形态 | 实测内存 |
|---|---|
| CLI 单次抓取 | **峰值 ~31MB，1.4s** |
| CDP server 常驻 | **~14MB** |
| （对照）Chrome 单实例 | 300MB ~ 2GB |

**用完必须杀干净：**

```bash
pkill -x lightpanda
ps -o pid,rss,cmd -C lightpanda --no-headers || echo "✅ 无残留"
```

> `pkill -f lightpanda` 会匹配到执行它的 shell 自身（命令串里含 "lightpanda"）→ **用 `-x`**。

---

## 6. 已知坑（全部实测标注）

| 症状 | 原因 / 解法 |
|---|---|
| `fetch` 返回空 | 没加 `--wait-until networkidle`（异步 XHR 没跑完） |
| 页面要滚动才出数据 | CLI fetch 不带滚动 → 走 CDP + Playwright `evaluate` 滚动 |
| 用截图做视觉验证 | ⚠️ 能出图但**不应用 CSS**（见 4.D）→ 换 Chrome |
| 被反爬拦 | 无指纹伪装生态 → 换 `stealthy_fetch` / BrowserAct |
| 复杂 SPA 静默失败 | WPT 兼容分低，**可能不报错但拿不到数据** → 必须拿真实目标页 A/B 验 |
| 生产不稳定 | 只有 nightly，别押关键路径 |

---

## 7. 自检（怀疑坏了时跑）

```bash
lightpanda version                                    # 期望 1.1.0-nightly.*
ls -l /usr/local/bin/lightpanda                       # 期望 ~180MB 可执行
lightpanda fetch --dump markdown --log-level error --wait-until networkidle \
  https://quotes.toscrape.com/js/ 2>/dev/null | grep -c "Einstein"   # 期望 > 0
ps -o pid,rss,cmd -C lightpanda --no-headers || echo "✅ 无残留进程"
```

---

## 7.5 实测证据：真站点 A/B（2026-10-03）

检验方法：从 Lightpanda 输出里取真实长词，回到**原始 HTML** 里查找。
命中率高 = 静态就能拿到（不需要浏览器）；命中率低 = JS 决定性。

| 站点 | 词命中率 | 判定 |
|---|---|---|
| `news.ycombinator.com` | 25/25 (100%) | ➖ 静态完全可替代 |
| `quotes.toscrape.com/js/` | 8/8 (100%) | ➖ **伪 JS 渲染**（数据在 script 里） |
| `react.dev` | 25/25 (100%) | ➖ SSR，不需要浏览器 |
| `www.bilibili.com` | **0/25 (0%)** | 🔥 **JS 决定性** |

> 📌 **四个站里只有一个真的需要浏览器。** 这就是三级探针存在的意义 ——
> 不做探针就上浏览器，四次里有三次是白干。

单次抓取实测：峰值 RSS **22–160MB**（bilibili 105MB / 知乎 160MB），耗时 **0.8–5.3s**。
（对照 Chrome 单实例 300MB–2GB）

---

## 8. 分工边界（别抢活）

- **不需要 JS** → `curl` + `jq` / Python（**永远优先**）
- **需要渲染/截图** → Chrome（`scrapling_fetch` / `scrapling_screenshot`）
- **反爬 / 验证码 / 人类接力** → `scrapling_stealthy_fetch` / BrowserAct
- **平台化搜索**（推特/小红书/B站/YouTube/RSS）→ `agent-reach`
- **需要 JS 但不要渲染** → **本 skill**
