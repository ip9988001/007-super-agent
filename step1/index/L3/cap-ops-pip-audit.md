# cap-ops-pip-audit

## 一句话
按 PyPI 漏洞库（OSV/PyPI advisory）审 Python 依赖的 CVE，官方 PyPA 出品。

## 怎么调
pip-audit -r requirements.txt --desc on # 审清单（推荐，不碰环境） pip-audit --desc on # 审当前环境 pip-audit -r req.txt -f json -o a.json # 出 JSON pip-audit -r req.txt --fix # 尝试自动升级修复（会改文件，慎用） pip-audit --skip-editable # 跳过本地可编辑包

## 坑
无参 = 当前环境，很容易得出「一切正常」的假安全感（就像本次实测）。；只查 PyPI 生态，与 `T-trivy.md`（也查 pip，但库不同、可离线）结果常有差异，两者互补。

## 实际效果
`pip-audit --version` = `pip-audit 2.10.1`；`/data/tools/bin/pip-audit`（uv 装的）。；实测 `pip-audit --desc on`（无参 = 审当前解释器环境）→ `No known vulnerabilities found`，退出码 0。

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`pip-audit --version` = `pip-audit 2.10.1`；`/data/tools/bin/pip-audit`（uv 装的）。；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/05-系统与运维/P-pip-audit.md · 核实：2026-11-16（no3-tools.md 2026-11-16 盘点（版本+运行））
> 注意：2.10.1；审当前解释器环境
