# cap-ops-ast-grep

## 一句话
按 AST 结构做搜索与改写（`sg`），比正则稳、比 semgrep 轻，天然支持 `--update-all` 批量重写。

## 怎么调
ast-grep run -p 'pattern' -l <lang> <path> # 结构搜索 ast-grep run -p 'eval($A)' -r 'safe($A)' -l py --update-all . # 批量改写 ast-grep run -p 'console.log($$$)' -l js -A # -A/-B/-C 带上下文 ast-grep run -p 'pattern' -l ts --json=stream # 机器可读 ast-grep new # 建项目（scan 才需要） ast-grep scan # 按 sgconfig.yml 跑规则集

## 坑
`scan` 必须先是「项目」：实测直接 `ast-grep scan` 报；语言参数写 `python`/`py` 都行，但 `-l ts` 不认 `.tsx`（要用 `tsx`），混写会静默 0 命中。

## 实际效果
`ast-grep --version` = `ast-grep 0.51.0`（全局 npm 包 `@ast-grep/cli`）。；结构搜索：`ast-grep run -p 'eval($A)' -l python .` → 精确命中 `t.py:3: return eval(x)`。

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`ast-grep --version` = `ast-grep 0.51.0`（全局 npm 包 `@ast-grep/cli`）。；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/05-系统与运维/A-ast-grep.md · 核实：2026-11-16（no3-tools.md 2026-11-16 盘点（版本+运行））
> 注意：0.51.0；结构搜索与改写
