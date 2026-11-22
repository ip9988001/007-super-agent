# cap-fetch-lightpanda

## 一句话
无渲染的 JS 执行引擎（Zig 写的浏览器内核），能跑 JS 但不画界面，直接把页面吐成 markdown / html / semantic_tree。

## 怎么调
lightpanda help fetch # 全部参数 lightpanda fetch <url> --dump markdown # 正文 markdown（最常用） lightpanda fetch <url> --dump html # 渲染后的序列化 DOM lightpanda fetch <url> --dump semantic_tree # 给 LLM 用的语义树 JSON lightpanda fetch <url> --dump markdown --json # 带状态码/headers 的结构化输出 lightpanda fetch <url> --dump markdown --dump-max-bytes 20000 lightpanda fetch URL1 URL2 --json # 多 URL 必须加 --json lightpanda serve…

## 坑
一次多 URL 不加 `--json` 直接报错（单 URL 才允许纯文本输出）。；没有渲染引擎：依赖 `getBoundingClientRect`、可见性、IntersectionObserver 懒加载的页面会拿不到内容。

## 实际效果
`lightpanda version` → `1.1.0-nightly.10033+614c1f56e` ✅；`lightpanda fetch https://example.com --dump markdown` → ✅ 输出干净正文（多语言段落，无标签噪音）

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`lightpanda version` → `1.1.0-nightly.10033+614c1f56e` ✅；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/01-取数与抓取/L-lightpanda.md · 核实：2026-10-26（原文件 实际效果（实测 2026-10-26））
> 注意：无渲染 JS 执行，出 markdown/html
