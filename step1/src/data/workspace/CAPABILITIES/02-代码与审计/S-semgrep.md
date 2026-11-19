# semgrep
## 一句话
本地静态规则扫描：semgrep **1.182.1**（pip 装），规则库已在 `/data/workspace/semgrep-rules`，**离线可跑、不用登录**。

## 实际效果（实测 2026-10-26）
- `semgrep --version` → `1.182.1` ✅
- 规则族实测 **30 个**（不是文档说的 35）：ai apex bash c clojure csharp dockerfile elixir generic go html java javascript json kotlin libsonnet ocaml package_managers php problem-based-packs python ruby rust scala solidity swift terraform trusted_python typescript yaml
- 规则文件：`find /data/workspace/semgrep-rules -name '*.yaml' | wc -l` → **2159**
- python 单目录扫 1 文件 → `Ran 371 rules on 1 file` ✅
- 造坏样本（`subprocess.call(shell=True)` + md5）→ 三连中，全部 `❰Blocking❱`：
  `dangerous-subprocess-use-audit` / `subprocess-shell-true`（带 `▶▶ Autofix ▶ False`）/ `insecure-hash-algorithm-md5` ✅
- 干净文件 → `0 findings`，无假阳性 ✅

## 怎么调
```bash
# 单语言扫（最常用）
semgrep scan --config /data/workspace/semgrep-rules/python ./src

# 全部规则族一起扫
semgrep scan --config /data/workspace/semgrep-rules ./src

# 机器可读，喂自己的脚本
semgrep scan --config /data/workspace/semgrep-rules/python --json ./src > out.json

# 带过滤
semgrep scan --config /data/workspace/semgrep-rules ./src --severity ERROR --exclude 'tests/*'
```

## 坑
- **别用 `--config auto`**：要联网上报，离线会卡；本地规则库已够用。
- 规则族是 **30** 不是 35（`ls -d */` 去掉 `scripts/`、`stats/` 后 30），写文档别抄旧数。
- 规则 id 前缀是 `data.workspace.semgrep-rules.…`，因为从本地绝对路径加载 —— 正常现象，不是 bug。
- `❰Blocking❱` 是规则的 severity 标签，**不代表扫描中断了进程**；扫描永远退出 0（有问题也 0，要看 json/`--error`）。
- 无 `--config` 只打欢迎语，一个文件都不扫。
- 一次扫全部规则族在 2000+ yaml 下启动很慢，日常只给对应语言目录。
