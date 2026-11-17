# cap-ops-uv

## 一句话
装 Python 命令行工具的唯一入口（`uv tool install`），工具落 `/data` 持久卷。

## 怎么调
export UV_TOOL_DIR=/data/tools/uv-tools # start.sh 2i 段的口径 export UV_TOOL_BIN_DIR=/data/tools/bin uv tool install <pkg> # 装 CLI（自动建隔离 venv） uv tool list # 已装清单 uv tool upgrade <pkg> uv venv && uv pip install # 项目级虚拟环境（比 venv+pip 快）

## 坑
同一个变量被 start.sh 设了两次：第 46 行 `UV_TOOL_DIR=/data/tools/uv/tools`（agent-reach 用）；工具存在两份：`/root/.local/share/uv/tools` 还有一份 47MB 副本（容器重建即丢）。

## 实际效果
`uv --version` = `0.13.5 (x86_64-unknown-linux-gnu)`，软链 `/usr/local/bin/uv` → `/data/tools/uv/bin/uv`。；`uv tool list` 实测列出 `bandit v1.9.4` / `pip-audit v2.10.1`（含各自子命令）。

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`uv --version` = `0.13.5 (x86_64-unknown-linux-gnu)`，软链 `/usr/local/bin/uv` → `/data/tool…；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/05-系统与运维/U-uv.md · 核实：2026-11-16（no3-tools.md 2026-11-16 盘点（版本+运行））
> 注意：0.13.5
