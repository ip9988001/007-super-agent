# cap-ops-script-chrome-nosandbox

## 一句话
种子里的 chrome-nosandbox 副本（三号版，3 行 sh）：直调 chromium 加 `--no-sandbox` 的一次性浏览器入口。

## 怎么调
对比用，不直接跑。007 上用 /usr/local/bin/chrome-nosandbox（第 19 步已按本机 chromium 改写）。

## 坑
三号版硬编码 `/data/cache/ms-playwright/chromium-1248/chrome-linux64/chrome`——007 上无此路径，直接跑必失败；它是存档对照，真正的入口是 /usr/local/bin/chrome-nosandbox（见 cap-fetch-browser-stack）。

## 实际效果
文件在种子内（3 行）；/usr/local/bin/chrome-nosandbox 在 007 上已验证（Chrome 156.0.8078.4）。

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）触发前缀 无前缀，直接调用；按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：对照用；实跑走 /usr/local/bin/chrome-nosandbox；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：tools-seed/scripts/chrome-nosandbox · 核实：2026-11-18（第 19 步种子核验）
