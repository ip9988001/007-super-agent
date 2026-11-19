# pip-audit 2.10.1

> **一句话**：按 PyPI 漏洞库（OSV/PyPI advisory）审 Python 依赖的 CVE，官方 PyPA 出品。

## 实际效果（实测）
- `pip-audit --version` = `pip-audit 2.10.1`；`/data/tools/bin/pip-audit`（uv 装的）。
- 实测 `pip-audit --desc on`（无参 = 审**当前解释器环境**）→ `No known vulnerabilities found`，退出码 0。
- 说明：本机系统 site-packages 干净，不代表项目依赖干净 —— 项目必须显式给清单。

## 怎么调
```
pip-audit -r requirements.txt --desc on     # 审清单（推荐，不碰环境）
pip-audit --desc on                          # 审当前环境
pip-audit -r req.txt -f json -o a.json       # 出 JSON
pip-audit -r req.txt --fix                   # 尝试自动升级修复（会改文件，慎用）
pip-audit --skip-editable                    # 跳过本地可编辑包
```
要联网查 OSV；离线环境基本不可用。

## 坑
- **无参 = 当前环境**，很容易得出「一切正常」的假安全感（就像本次实测）。
  真正审计项目必须 `-r requirements.txt` 或 `--path` 指向具体锁文件。
- 只查 **PyPI 生态**，与 `T-trivy.md`（也查 pip，但库不同、可离线）结果常有差异，两者互补。
- `--fix` 会真改 `requirements.txt` 且可能连带破坏版本约束 → 审计场景**不要**加。
- 依赖树里 `-e`/本地路径包默认被跳过，需 `--skip-editable` 才不报噪音（等价但显式）。
- 与 `bandit`（源码模式）、`gitleaks`（密钥）、`semgrep`（规则）不是一层，别互相替代。
