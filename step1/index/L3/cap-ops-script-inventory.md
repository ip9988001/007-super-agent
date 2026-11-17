# cap-ops-script-inventory

## 一句话
环境盘点快照（46 行 bash）：把系统、网关路由、/data/bin、/data/tools、关键 CLI 在位一次写进带时间戳的报告文件。

## 怎么调
bash tools/scripts/inventory.sh # 报告写 /data/state/link/inventory/raw-YYYYMMDD-HHMM.txt

## 坑
三号原版：读 `/data/bot-gateway.js`、`/data/tools/*`、写 `/data/state/link/inventory`——全是三号路径。007 上路由节与 tools 节为空，报告只有系统节与命令在位节有内容；别把空节当成“工具丢了”。

## 实际效果
文件在种子内（46 行）；系统节与命令在位节在 007 上可直接跑。

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）触发前缀 无前缀，直接调用；按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：报告文件生成，含系统节与命令在位节；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：tools-seed/scripts/inventory.sh · 核实：2026-11-18（第 19 步种子核验）
