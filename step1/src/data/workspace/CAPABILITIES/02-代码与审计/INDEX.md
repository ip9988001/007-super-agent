# 02 代码与审计 · 条目索引
> 挑一条，只读那个 md。组内按文件名首字母排序，组之间空一行。

## ① 静态规则扫描
- S-semgrep.md :: 本地规则库静态扫描 1.182.1（30 个规则族 / 2159 条规则，离线可跑）

## ② 代码图谱查询
- C-codegraph.md :: 符号/调用图查询 1.6.2（仅 /data/workspace/rea 已建索引）

## ③ 二进制逆向
- G-ghidra.md :: 逆向平台 12.1.4，headless 分析器可用（JDK 25）
- R-rea.md :: 逆向分析总控 6.8.0（ghidra+jadx provider 已就绪，ida/hopper/kuna 缺）

## ④ 依赖与密钥审计
- G-git.md :: 变更/归属/历史溯源（2.39.5，单提交仓库）
- R-rg.md :: 代码搜索主力 ripgrep 13.0.0（.gitignore 感知，lookahead 要 -P）

> ④ 组内实测**只有 grype 不存在**（未写空壳 md）。ast-grep 0.51.0 / bandit 1.9.4 / pip-audit 2.10.1 / trivy 0.76.1 / gitleaks 8.30.1 **已装，条目在 05-系统与运维 ④ 安全审计**，此处不重复。
