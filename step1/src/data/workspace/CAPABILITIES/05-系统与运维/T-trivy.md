# trivy 0.76.1

> **一句话**：依赖漏洞 + 配置错误扫描器（CVE 库 + misconfig policy），单二进制。

## 实际效果（实测）
- `trivy --version` = `Version: 0.76.1`；`/data/tools/bin/trivy`。
- **第一次跑依赖扫描必须先下 DB**。下完后 `trivy fs --scanners vuln .`（一个 `requirements.txt`：
  `requests==2.19.0 / flask==0.12.2`）实测报 **9 条**：`HIGH 4 / MEDIUM 4 / LOW 1`，
  带 CVE 号 + 修复版本（如 `CVE-2018-1000656 flask 0.12.2 → 0.12.3`）。
- DB 落 `/data/cache/trivy/db`（`XDG_CACHE_HOME=/data/cache`），当天 `updated_at` 就是最新。

## 怎么调
```
trivy fs --download-db-only .                       # 首次/更新库（要通 ghcr.io）
trivy fs --scanners vuln --quiet <path>             # 只查依赖漏洞
trivy fs --scanners misconfig --quiet <path>        # 只查 IaC/Dockerfile/k8s 配置
trivy fs --severity HIGH,CRITICAL -f json -o r.json .
trivy config <dir>                                  # 配置体检（不需要 DB）
```

## 坑
- **没 DB 时会静默放行**：DB 不存在时输出 `Target: - / Vulnerabilities: -`、
  Legend 写 `Not scanned`，**退出码仍是 0** → 脚本里不判 Legend 就会以为「干净」。
  这是本工具最大的坑，生产用前先 `--download-db-only` 并检查 `/data/cache/trivy/db`。
- 下库走 `ghcr.io`（实测 `https://ghcr.io/v2/` 返回 401 = 通），断网则永远拿不到库。
- 只扫「声明了版本」的清单：`requirements.txt` / `package-lock.json` / `Pipfile.lock` 等；
  裸源码目录扫不出东西（我第一次扫一个只有一个 `t.py` 的目录就是这样）。
- misconfig 规则集依赖 `policy` 目录（`/data/cache/trivy/policy`），更新与 DB 分开。
