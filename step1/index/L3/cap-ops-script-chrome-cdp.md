# cap-ops-script-chrome-cdp

## 一句话
启动带 CDP 的 Chromium（16 行 bash）：供 browser-tools 技能连 127.0.0.1:9222 调浏览器。

## 怎么调
CDP_PORT=9222 bash tools/scripts/chrome-cdp.sh # 已在跑时直接报 OK，不重复起

## 坑
三号原版：硬编码 `CHROME=$(ls -d /data/cache/ms-playwright/chromium-*/chrome-linux64/chrome)`。007 的 chromium 在 `/root/.cache/ms-playwright/chromium-1248`，直接跑会 `ERR: chromium not found`；须先改 CHROME 路径或在 /data/cache 下建同名链接；另写 `/data/state/chrome-cdp` 与 `/data/logs/chrome-cdp.log`（root 会建 /data）。

## 实际效果
文件在种子内（16 行）；路径未改前在 007 上跑会报 chromium not found（已说明原因）。

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）触发前缀 无前缀，直接调用；按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`OK: CDP ready on :9222` 或 `OK: CDP already on :9222`；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：tools-seed/scripts/chrome-cdp.sh · 核实：2026-11-18（第 19 步种子核验）
