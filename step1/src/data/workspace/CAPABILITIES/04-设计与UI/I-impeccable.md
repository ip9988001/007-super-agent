# impeccable（detect 引擎）
## 一句话
本地设计反模式检测器（引擎 `0.1.16` / skill `4.7.0`，二进制 21MB，整技能 23MB），**零 API、零联网、离线确定性**；文件 0.06s、URL 2–29s。`/ui` 路由的打分完全靠它。

## 实际效果（实测 2026-10-26）
- `--version` → `4.7.0`；`scripts/VERSION` → `0.1.16`（**两个版本号并存**，网关 `/stats` 打印的是后者）
- 双引擎，规则集不同：
  - **静态引擎**（`.html/.css/.jsx/.tsx`，走 regex + CSS 解析）：`bad.html` 20 条 finding / 14 类，0.061s
  - **浏览器引擎**（`http(s)://` 和 `file://` 自动切换，Puppeteer 渲染）：同文件 14 条 / 11 类，2.14s
  - 静态能抓 `overused-font`、`marketing-buzzword`；浏览器能抓 `script-error`、`straggler`、`marquee`，两者互补，**不能互相替代**
- URL 实测耗时（单 URL、1280x800）：example.com 2.7s / 123ip.cc 4.1s / wikipedia.org 3.8s / **github.com 28.7s** → 宣传的"20–40s"符合，但**站点越重越慢，无上限**
- 复现 03 批笔记：`https://123ip.cc` → 32 条，`low-contrast`×31 + `overused-font`×1（与 `IMPECCABLE-NOTES.md` 记录一致 ✅）
- 目录扫描：`detect --json <dir>` 0.076s，逐文件出 finding，`file` 字段区分来源
- 退出码：`0` 无问题 / `1` 有目标没扫成 / `2` 有问题（多目标时"跑不成"优先）
- **"62 条确定性规则"不成立（无法证实）**：`SKILL.md`、`reference/*.md` 全库 grep 无 62 这个数字，二进制里规则表按字符串表压缩、无法直接枚举。可证实的是：实测跨 5 个目标共触发 **22 个独立规则 id**（各带 name/description/severity/category/snippet），分 `error`/`warning`/`advisory` 三级、`quality`/`slop` 两类，规则包至少有 `border-rules / color-rules / hover-color-rules / motion-rules / quality-rules / layout-rules / text-content` 七族。**结论：规则是真的，62 是网关自填的数字。**

## 怎么调
```bash
IMP=/root/.pi/agent/skills/impeccable/scripts/impeccable
export IMPECCABLE_BROWSER=/usr/local/bin/chrome-nosandbox   # root 跑 chromium 必须走这个 wrapper（内含 --no-sandbox --disable-dev-shm-usage）
$IMP detect --json README.html                  # 静态，0.06s
$IMP detect --json --viewport 390x844 <目标>     # 移动端 pass（同一 URL 结果会变：example.com 桌面 2 条 → 390 宽 0 条）
$IMP detect --json --scope type|layout <目标>    # 只有这两个 scope（无 quality，加了反而漏报）
$IMP detect --json --no-design-system <目标>     # 屏蔽本地 DESIGN.md 的 tokens 比对
$IMP ignores list|add-value overused-font Inter  # 逐条豁免（写 .impeccable/config.json）
$IMP context                                     # 输出 NO_PRODUCT_MD / RESOLVED_CONTEXT 等结构化指令
```

## 坑
- **没设 `IMPECCABLE_BROWSER` 时 URL 直接失败**：`Error: No Chrome…`，退出 1、stdout 空、耗时 0.007s —— 上层若把空 stdout 当 `[]`，就变成"0 条问题"的假绿灯。
- `--scope type|layout` 是**减法**：`--scope type` 只剩 2 条，`low-contrast` 属于 quality 不在任何 scope 内，按 scope 扫会漏掉它。
- 文本模式 finding 走 **stderr**，`--json` 走 stdout；只重定 stdout 会看到空。
- `--quiet` 的计数口径是 primary（bad.html 显示 19）+ advisory 单列，与 `--json` 的数组长度（20）不一致，对账要按 JSON。
- `--version` 报 skill 版本，`VERSION` 文件报引擎版本，别混用。
- 浏览器引擎默认会**主动隐藏 Cookie 弹窗与引导层**（有 `--no-consent-hiding` / `--no-overlay-hiding` 可关）；测"首次进入体验"时要关掉，否则测的是被清理过的页面。
- `--json` 的 `line` 字段在 URL 模式下恒为 `0`，定位只能靠 `selector`。

> 截图 / 视觉回归怎么做 → 见 `B-ui-visual.md`（实现细节在 `01-取数与抓取/B-browser-stack.md`）。
