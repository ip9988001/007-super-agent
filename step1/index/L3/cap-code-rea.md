# cap-code-rea

## 一句话
逆向/二进制分析总控 CLI 6.8.0（175 命令 / 229 MCP 工具）；provider 靠环境变量注册，ghidra + jadx 已就绪，ida/hopper/kuna 缺。

## 怎么调
rea doctor # 全环境体检（audit-wide 模式） rea doctor --provider ghidra # 只验一个 provider；通过就 healthy:true rea doctor --provider jadx rea doctor --client pi # 只验某个 agent 注册 rea capabilities # 内部能力矩阵（available / effects） rea analyze <target> # 起分析 rea doctor --format json # 所有命令支持 toon/json/yaml/md

## 坑
`healthy` 和 `environment_healthy` 是两字段：audit-wide 下 `healthy` 直接等于 `environment_healthy`，见 false 别急着怀疑 provid…；`--provider hopper` 永远失败（macOS 专用引擎，Linux 上没有）。

## 实际效果
`rea --version` → `6.8.0` ✅；`rea --help` 命令表正常 ✅；`rea doctor` → `healthy: false` / `environment_healthy: false`，exit 1

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）触发前缀 /rea；前置：环境变量 GHIDRA_INSTALL_DIR / REA_JADX_MCP_JAR / JAVA_HOME。按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`rea --version` → `6.8.0` ✅；`rea --help` 命令表正常 ✅；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/02-代码与审计/R-rea.md · 核实：2026-10-26（原文件 实际效果（实测 2026-10-26））
> 注意：doctor 总体 healthy:false 是两条 core 检查 drift，与 provider 无关；ghidra/jadx 已就绪
