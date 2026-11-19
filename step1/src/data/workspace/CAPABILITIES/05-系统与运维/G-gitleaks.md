# gitleaks 8.30.1

> **一句话**：密钥/凭据硬编码扫描器（正则 + 熵），单二进制，离线即可跑。

## 实际效果（实测）
- `gitleaks version` = `8.30.1`；路径 `/data/tools/bin/gitleaks`（`/usr/local/bin` 软链）。
- 造了一个假 `ghp_` PAT 放进 `k.env`，`gitleaks dir . -v` 命中：
  `RuleID: github-pat · Entropy: 4.803 · File: k.env · Line: 2`。
- **有泄漏时退出码 1**，干净时 0（适合直接挂 CI/脚本 `if`）。
- 空扫描：`scanned ~70 bytes in 7.57ms`，很快。

## 怎么调
```
gitleaks dir <path> --no-banner          # 扫目录（无 git 历史）
gitleaks git <repo> --no-banner -v       # 扫仓库历史（默认模式）
gitleaks dir . -f json -r leak.json      # 出 JSON 报告
gitleaks dir . --baseline-path base.json # 忽略已知存量
```
必加 `--no-banner`，否则先刷一大块 ASCII 头。

## 坑
- **`dir` 与 `git` 是分开的模式**：`dir` 不看历史，只扫工作区；查「以前提交过的 key」必须 `gitleaks git`。
- 规则是**格式敏感**的：我第一版造的 `AKIA` 后面只跟了 10 位，AWS 规则（`AKIA[0-9A-Z]{16}`）没报；
  短假密钥扫不出来 **不代表工具坏了**，是样本不合规。
- 纯熵规则会误报：base64 的图片/压缩包/随机 ID 都可能命中，交付前先 `--report-format json` 人工过一遍。
- 8.30 的 `dir` 子命令是新的（老教程里的 `gitleaks detect --source` 仍可用但已不建议）。
