# cap-seo-seo-gateway

## 一句话
TG/企微 里打 `/seo [--deep] [--write] <URL|本地路径>` 走这条链：本地 `audit.py` 出确定性 JSON → 拼进 prompt → pi 出修复包；quick 180s / deep 420s，`--write` 才碰源码。

## 怎么调
# 想复现网关那两段（不经过网关） cd /data/workspace/seo-toolkit python3 geo-seo-aeo-skill/scripts/audit.py https://target # 段一 export CLAUDE_SEO_DATA_DIR=/data/state/claude-seo python3 claude-seo/scripts/runtime.py doctor # 段二的正品，51ms # 网关侧（需要 TG/企微 入口） /seo https://example.com # quick：audit + 一次 pi /seo --deep https://example.com # 现在等于 quick 加长版，别指望多跑规程 /seo --write /data/workspace/proj # 会改源码，先复制 .bak-YYYYMM…

## 坑
doctor 那步 180s 是纯浪费，不是"首次重建 venv 慢"——venv 早就在（`/data/state/claude-seo/.venv`，771MB），坏的是启动器本身（根因见 `S-claude-se…；目标站开 gzip/brotli 时 audit 全零（python.org 实测 title/h1/links 全 null），网关不会兜底，pi 仍然会基于空数据写修复包——先自己 curl 一版看是否正常。

## 实际效果
代码位置 `/data/bot-gateway.js:375-467`，常量 `SEO_TOOLKIT=/data/workspace/seo-toolkit`（:33）。网关进程在跑（`ps` 有 1 个 node）✅；第一段（打分）：URL 目标 → `python3 geo-seo-aeo-skill/scripts/audit.py <url>`，超时 120s → 单独复跑已验证能吐完整 JSON ✅

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）触发前缀 /seo；按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：代码位置 `/data/bot-gateway.js:375-467`，常量 `SEO_TOOLKIT=/data/workspace/seo-toolkit`（:33）。网关进…；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/03-SEO与内容/S-seo-gateway.md · 核实：2026-10-26（原文件 实际效果（实测 2026-10-26））
> 注意：quick 180s / deep 420s；--write 才碰源码
