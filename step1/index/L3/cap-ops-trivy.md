# cap-ops-trivy

## 一句话
依赖漏洞 + 配置错误扫描器（CVE 库 + misconfig policy），单二进制。

## 怎么调
trivy fs --download-db-only . # 首次/更新库（要通 ghcr.io） trivy fs --scanners vuln --quiet <path> # 只查依赖漏洞 trivy fs --scanners misconfig --quiet <path> # 只查 IaC/Dockerfile/k8s 配置 trivy fs --severity HIGH,CRITICAL -f json -o r.json . trivy config <dir> # 配置体检（不需要 DB）

## 坑
没 DB 时会静默放行：DB 不存在时输出 `Target: - / Vulnerabilities: -`；下库走 `ghcr.io`（实测 `https://ghcr.io/v2/` 返回 401 = 通），断网则永远拿不到库。

## 实际效果
`trivy --version` = `Version: 0.76.1`；`/data/tools/bin/trivy`。；第一次跑依赖扫描必须先下 DB。下完后 `trivy fs --scanners vuln .`（一个 `requirements.txt`

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）前置：首次跑先下载漏洞库（1.6G，不进包）。按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`trivy --version` = `Version: 0.76.1`；`/data/tools/bin/trivy`。；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/05-系统与运维/T-trivy.md · 核实：2026-11-16（no3-tools.md 2026-11-16 盘点（版本+运行））
