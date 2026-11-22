# cap-ops-script-inventory2

## 一句话
环境盘点完整版（55 行 bash）：inventory.sh 的加长版，多了路由真实提取、skills 描述、工具版本三节。

## 怎么调
bash tools/scripts/inventory2.sh # 报告写 /data/state/link/inventory/raw-full-YYYYMMDD-HHMM.txt

## 坑
三号原版：读 `/data/bot-gateway.js`、`/root/.agents/skills/*`、`/root/.pi/agent/skills/*`——全是三号路径。007 上 skills 在 `tools/skills/`，路由节为空；工具版本节（semgrep、codegraph 等 `--version`）可直接跑。

## 实际效果
文件在种子内（55 行）；工具版本节在 007 上可直接跑。

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）触发前缀 无前缀，直接调用；按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：报告文件生成，含工具版本节；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：tools-seed/scripts/inventory2.sh · 核实：2026-11-18（第 19 步种子核验）
