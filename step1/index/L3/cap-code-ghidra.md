# cap-code-ghidra

## 一句话
本地逆向平台 12.1.4 (PUBLIC)，装在 `/data/tools/ghidra`，headless 分析器可跑（JDK 25），不用开 GUI。

## 怎么调
# 最小可用：导入 + 自动分析，用完即删项目 /data/tools/ghidra/support/analyzeHeadless /tmp/ghproj myproj \ -import /path/to/binary -deleteProject # 只导入不分析（快速看格式/架构识别） ... -import f -noanalysis ... -import f -processor x86:LE:64:default # 跑自带脚本（脚本名必须真的存在） ... -import f -postScript ExportFunctionInfoScript.java # 批量目录 + 递归 ... -import /path/dir -recursive -analysisTimeoutPerFile 120

## 坑
`-postScript` 脚本找不到 = 整个任务失败，不是跳过。先 `ls /data/tools/ghidra/Ghidra/Features/Base/ghidra_scripts/` 拿真名。；每次启动 JVM 约 2.7s；批量就一次 `-import dir -recursive`，别循环起进程。

## 实际效果
`/data/tools/ghidra/support/analyzeHeadless` 无参 → 打出完整 usage，JVM `openjdk 25.0.5 (Temurin)` ✅；`Ghidra/application.properties` → `application.version=12.1.4` / `application.release.name=PUBLIC` ✅

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`/data/tools/ghidra/support/analyzeHeadless` 无参 → 打出完整 usage，JVM `openjdk 25.0.5 (Temurin…；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/02-代码与审计/G-ghidra.md · 核实：2026-10-26（原文件 实际效果（实测 2026-10-26））
> 注意：headless 分析器可跑（JDK 25），不用开 GUI
