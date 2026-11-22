# cap-ops-script-cap-scan

## 一句话
能力全集穷举扫描（48 行 bash）：不靠回忆，只看文件系统，把 /usr/local/bin 等处的可执行文件扫成候选清单。

## 怎么调
bash tools/scripts/cap-scan.sh # 候选写 /tmp/cand.txt
SCAN_USRBIN=1 bash tools/scripts/cap-scan.sh # 连 /usr/bin 一起扫（噪声大）

## 坑
三号原版：还扫 `/data/tools/*/bin`（三号路径，007 上为空）；`cand` 只收可执行文件，.py/.js/.sh/.md 直接跳过——脚本类能力不会出现在这份清单里，别拿它当全量。

## 实际效果
文件在种子内（48 行）；扫 /usr/local/bin、/root/.local/bin 两节在 007 上可直接跑。

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）触发前缀 无前缀，直接调用；按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：输出 /tmp/cand.txt（`来源|命令名` 每行一条）；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：tools-seed/scripts/cap-scan.sh · 核实：2026-11-18（第 19 步种子核验）
