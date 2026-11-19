# ghidra
## 一句话
本地逆向平台 **12.1.4 (PUBLIC)**，装在 `/data/tools/ghidra`，**headless 分析器可跑**（JDK 25），不用开 GUI。

## 实际效果（实测 2026-10-26）
- `/data/tools/ghidra/support/analyzeHeadless` 无参 → 打出完整 usage，JVM `openjdk 25.0.5 (Temurin)` ✅
- `Ghidra/application.properties` → `application.version=12.1.4` / `application.release.name=PUBLIC` ✅
- 真跑一次导入+分析：
  `analyzeHeadless /tmp/ghidra_proj testproj -import /bin/true -deleteProject`
  → `REPORT: Analysis succeeded for file: file:///usr/bin/true`
  → `REPORT: Save succeeded` → `REPORT: Import succeeded`，总耗时 **23.6s**（JVM 启动 2.7s + 分析 ~21s）✅
- `-postScript PrintProgramInfo.java`（脚本名不存在）→ `ERROR REPORT SCRIPT ERROR: Script not found` + `IllegalArgumentException`，**整个任务 abort** ❌

## 怎么调
```bash
# 最小可用：导入 + 自动分析，用完即删项目
/data/tools/ghidra/support/analyzeHeadless /tmp/ghproj myproj \
  -import /path/to/binary -deleteProject

# 只导入不分析（快速看格式/架构识别）
... -import f -noanalysis
... -import f -processor x86:LE:64:default

# 跑自带脚本（脚本名必须真的存在）
... -import f -postScript ExportFunctionInfoScript.java

# 批量目录 + 递归
... -import /path/dir -recursive -analysisTimeoutPerFile 120
```

## 坑
- **`-postScript` 脚本找不到 = 整个任务失败**，不是跳过。先 `ls /data/tools/ghidra/Ghidra/Features/Base/ghidra_scripts/` 拿真名。
- 每次启动 JVM 约 2.7s；批量就一次 `-import dir -recursive`，别循环起进程。
- 输出极吵（几十行 `INFO HEADLESS Script Paths`），脚本里务必 `| grep -E "REPORT|ERROR"` 收口。
- 不加 `-deleteProject` 会在目标目录留 GB 级项目文件，`/tmp` 很快就满。
- 依赖 `JAVA_HOME=/data/tools/jdk25`（当前值已是它）；没它连 usage 都打不出来。
