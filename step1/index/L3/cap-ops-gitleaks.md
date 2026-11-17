# cap-ops-gitleaks

## 一句话
密钥/凭据硬编码扫描器（正则 + 熵），单二进制，离线即可跑。

## 怎么调
gitleaks dir <path> --no-banner # 扫目录（无 git 历史） gitleaks git <repo> --no-banner -v # 扫仓库历史（默认模式） gitleaks dir . -f json -r leak.json # 出 JSON 报告 gitleaks dir . --baseline-path base.json # 忽略已知存量

## 坑
`dir` 与 `git` 是分开的模式：`dir` 不看历史，只扫工作区；查「以前提交过的 key」必须 `gitleaks git`。；规则是格式敏感的：我第一版造的 `AKIA` 后面只跟了 10 位，AWS 规则（`AKIA[0-9A-Z]{16}`）没报

## 实际效果
`gitleaks version` = `8.30.1`；路径 `/data/tools/bin/gitleaks`（`/usr/local/bin` 软链）。；造了一个假 `ghp_` PAT 放进 `k.env`，`gitleaks dir . -v` 命中

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`gitleaks version` = `8.30.1`；路径 `/data/tools/bin/gitleaks`（`/usr/local/bin` 软链）。；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/05-系统与运维/G-gitleaks.md · 核实：2026-11-16（no3-tools.md 2026-11-16 盘点（版本+运行））
> 注意：8.30.1；扫密钥泄露
