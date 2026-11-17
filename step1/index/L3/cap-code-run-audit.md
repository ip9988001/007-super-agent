# cap-code-run-audit

## 一句话
安全审计入口（run-audit.sh，/audit 前缀）：Cloudflare security-audit-skill × Semgrep 双层审计——本地初筛（0 token）出候选，再由 agent 对照规程研判。

## 怎么调
bash tools/scripts/run-audit.sh <目标文件或目录> # 默认 local 模式
bash tools/scripts/run-audit.sh <目标> --registry # 改走 semgrep 在线规则（失败自动回落 local）
SEMGREP_RULES_DIR=<本地规则目录> OUT_DIR=<输出目录> bash tools/scripts/run-audit.sh <目标>

## 坑
本地规则快照 `/data/workspace/semgrep-rules` 在 007 上缺失，默认 local 模式会回落 registry 在线规则（需联网）；`OUT_DIR` 默认 `/data/audit-out`、`XDG_CACHE_HOME` 默认 `/data/cache`，root 运行会在根目录建 /data，建议显式设 OUT_DIR 到部署目录；退出码 0=无候选、1=有候选、2=用法错误、3=引擎异常。

## 实际效果
文件在种子内；semgrep 1.182.1 已装（第 19 步 pip 实测）；registry 模式需联网。

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）触发前缀 /audit；按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`SEMGREP_SUMMARY total=N …` + `NO_FINDINGS`（0）或 `CANDIDATES`（1）；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：tools-seed/scripts/run-audit.sh · 核实：2026-11-18（第 19 步种子核验）
