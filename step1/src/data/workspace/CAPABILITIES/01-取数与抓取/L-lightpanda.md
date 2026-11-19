# lightpanda
## 一句话
无渲染的 JS 执行引擎（Zig 写的浏览器内核），能跑 JS 但不画界面，直接把页面吐成 markdown / html / semantic_tree。

## 实际效果（实测 2026-10-26）
- `lightpanda version` → `1.1.0-nightly.10033+614c1f56e` ✅
- `lightpanda fetch https://example.com --dump markdown` → ✅ 输出干净正文（多语言段落，无标签噪音）
- 加 `--json` → ✅ 返回 `{url, http_status, headers[], content}` 结构化结果，适合脚本消费
- 速度比 chrome 快、内存占用低（常驻十几 MB 级），适合批量
- 限制：不能截图、不能视觉验证；需要滚动/复杂交互的页面搞不定

## 怎么调
```bash
lightpanda help fetch                 # 全部参数
lightpanda fetch <url> --dump markdown          # 正文 markdown（最常用）
lightpanda fetch <url> --dump html              # 渲染后的序列化 DOM
lightpanda fetch <url> --dump semantic_tree     # 给 LLM 用的语义树 JSON
lightpanda fetch <url> --dump markdown --json   # 带状态码/headers 的结构化输出
lightpanda fetch <url> --dump markdown --dump-max-bytes 20000
lightpanda fetch URL1 URL2 --json               # 多 URL 必须加 --json
lightpanda serve --port 9222                    # 起 CDP，给 Playwright/puppeteer 连
lightpanda mcp                                  # 当 MCP server（stdio）
# 需要登录态：
lightpanda fetch <url> --cookie cookies.txt --cookie-jar /tmp/jar.json --dump markdown
```

## 坑
- 一次多 URL 不加 `--json` 直接报错（单 URL 才允许纯文本输出）。
- 没有渲染引擎：依赖 `getBoundingClientRect`、可见性、IntersectionObserver 懒加载的页面会拿不到内容。
- 不吃 Cloudflare / Turnstile / 验证码 —— 遇到这类直接换 `scrapling ... extract fetch`（stealthy）。
- `--dump png/pdf` 只是"文本渲染成图片"，不是真实截图，别当截图工具用。
- 默认超时短，慢站加 `--timeout`（见 `lightpanda help fetch` 的 common options）。
