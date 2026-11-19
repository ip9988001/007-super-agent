# rea
## 一句话
逆向/二进制分析总控 CLI **6.8.0**（175 命令 / 229 MCP 工具）；provider 靠**环境变量**注册，ghidra + jadx 已就绪，ida/hopper/kuna 缺。

## 实际效果（实测 2026-10-26）
- `rea --version` → `6.8.0` ✅；`rea --help` 命令表正常 ✅
- `rea doctor` → `healthy: false` / `environment_healthy: false`，**exit 1**
- 但 `providerInspections[4]` 明确写着：
  - `ghidra` configured:**true** available:**true** providerVersion **12.1.4**
  - `jadx` configured:**true** available:**true**（bridge JAR 已校验）
  - `ida` / `kuna` configured:false available:false
- `rea doctor --provider ghidra` → **`healthy: true`**，scope_checks 9 条全 ok ✅ ← 铁证：ghidra 早就是被识别的 provider
- `rea capabilities` → 只列 4 个**内部能力** provider：`rea-artifact-graph` / `rea-dotnet-static` / `native-macos`(非 mac 不可用) / `sqlite`；跟 doctor 的 provider 不是同一概念 ✅

## 怎么调
```bash
rea doctor                       # 全环境体检（audit-wide 模式）
rea doctor --provider ghidra     # 只验一个 provider；通过就 healthy:true
rea doctor --provider jadx
rea doctor --client pi           # 只验某个 agent 注册
rea capabilities                 # 内部能力矩阵（available / effects）
rea analyze <target>             # 起分析
rea doctor --format json         # 所有命令支持 toon/json/yaml/md
```

## 坑
**`scope.providers: []` 不是"没识别到 provider"** —— 它是「本次调用方要求哪些 provider」，只有 `rea doctor --provider X` 时才填；audit-wide 模式下必然为空。
> **根因结论：ghidra / jadx 什么都不缺，已经注册并被识别。** 靠的是三个环境变量：
> `GHIDRA_INSTALL_DIR=/data/tools/ghidra`、`REA_JADX_MCP_JAR=/data/workspace/rea/third_party/jadx-headless-mcp/build/libs/jadx-headless-mcp-0.7.1-all.jar`、`JAVA_HOME=/data/tools/jdk25`。
> `healthy: false` 的真凶是两条 **core 检查**（与 provider 无关）：
> ① `skill:identity` = config_drift（exposed skill 未安装）；
> ② `registration:pi` = config_drift，因为 `/root/.pi/agent/mcp.json` **文件根本不存在**。
> 修法只有一条：`rea setup`。

- `healthy` 和 `environment_healthy` 是两字段：audit-wide 下 `healthy` 直接等于 `environment_healthy`，见 false 别急着怀疑 provider。
- `--provider hopper` 永远失败（macOS 专用引擎，Linux 上没有）。
- 输出几百行，用 `--filter-output scope.providers` 或 `--token-limit N` 裁。
