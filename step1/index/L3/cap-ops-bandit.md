# cap-ops-bandit

## 一句话
Python 源码安全 linter（AST 级），专抓 `eval` / `subprocess(shell=True)` / 硬编码密码这类模式。

## 怎么调
bandit -q t.py # 单文件 bandit -r src/ -q # 递归整目录（最常用） bandit -r . -f json -o b.json # 出 JSON bandit -r . -ll # 只报 MEDIUM 以上（-lll = HIGH） bandit -r . -s B101,B404 # 跳过指定规则 bandit -r . -c pyproject.toml # 走配置文件

## 坑
只认 Python：扫 `node_modules` / 前端目录只会拖时间。；噪声大：`B101 assert_used`、`B404 import_subprocess` 在测试/内部脚本里几乎全是误报 →

## 实际效果
`bandit --version` = `bandit 1.9.4`（python 3.11.2）；`/data/tools/bin/bandit`（uv 装的）。；实测扫 `t.py`（内容只有 `def f(x): return eval(x)`）

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`bandit --version` = `bandit 1.9.4`（python 3.11.2）；`/data/tools/bin/bandit`（uv 装的）。；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/05-系统与运维/B-bandit.md · 核实：2026-11-16（no3-tools.md 2026-11-16 盘点（版本+运行））
> 注意：1.9.4；扫 Python 安全问题
