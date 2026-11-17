# cap-code-semgrep

## 一句话
本地静态规则扫描：semgrep 1.182.1（pip 装），规则库已在 `/data/workspace/semgrep-rules`，离线可跑、不用登录。

## 怎么调
# 单语言扫（最常用） semgrep scan --config /data/workspace/semgrep-rules/python ./src # 全部规则族一起扫 semgrep scan --config /data/workspace/semgrep-rules ./src # 机器可读，喂自己的脚本 semgrep scan --config /data/workspace/semgrep-rules/python --json ./src > out.json # 带过滤 semgrep scan --config /data/workspace/semgrep-rules ./src --severity ERROR --exclude 'tests/*'

## 坑
别用 `--config auto`：要联网上报，离线会卡；本地规则库已够用。；规则族是 30 不是 35（`ls -d */` 去掉 `scripts/`、`stats/` 后 30），写文档别抄旧数。

## 实际效果
`semgrep --version` → `1.182.1` ✅；规则族实测 30 个（不是文档说的 35）：ai apex bash c clojure csharp dockerfile elixir generic go html java javascript json ko…

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）触发前缀 /audit；按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`semgrep --version` → `1.182.1` ✅；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/02-代码与审计/S-semgrep.md · 核实：2026-10-26（原文件 实际效果（实测 2026-10-26））
> 注意：规则库离线可跑，不用登录
