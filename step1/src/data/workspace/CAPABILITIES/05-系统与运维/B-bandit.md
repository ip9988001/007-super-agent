# bandit 1.9.4

> **一句话**：Python 源码安全 linter（AST 级），专抓 `eval` / `subprocess(shell=True)` / 硬编码密码这类模式。

## 实际效果（实测）
- `bandit --version` = `bandit 1.9.4`（python 3.11.2）；`/data/tools/bin/bandit`（uv 装的）。
- 实测扫 `t.py`（内容只有 `def f(x): return eval(x)`）：
  命中 `[B307:blacklist] Use of possibly insecure function`，
  `Severity: Medium / Confidence: High / CWE-78`，并打印源码行号 `./t.py:3:11`。
- `-q` 静默模式只留 findings，适合塞管道。

## 怎么调
```
bandit -q t.py                       # 单文件
bandit -r src/ -q                    # 递归整目录（最常用）
bandit -r . -f json -o b.json        # 出 JSON
bandit -r . -ll                      # 只报 MEDIUM 以上（-lll = HIGH）
bandit -r . -s B101,B404             # 跳过指定规则
bandit -r . -c pyproject.toml        # 走配置文件
```

## 坑
- **只认 Python**：扫 `node_modules` / 前端目录只会拖时间。
- 噪声大：`B101 assert_used`、`B404 import_subprocess` 在测试/内部脚本里几乎全是误报 →
  建议开工就 `-s B101,B404,B603`，或 `-ll` 提阈值。
- `-r .` 会跟进 `.venv`/`site-packages`，务必 `-x .venv,node_modules,test`。
- 它是**静态 AST 分析**，`getattr(__builtins__,'eval')` 之类绕法抓不到；不要当唯一防线。
- 与 `02-代码与审计/S-semgrep.md` 分工：语义规则/跨语言用 semgrep，Python 专属 CWE 用 bandit。
