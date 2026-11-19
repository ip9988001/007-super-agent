# ast-grep 0.51.0

> **一句话**：按 AST 结构做搜索与改写（`sg`），比正则稳、比 semgrep 轻，天然支持 `--update-all` 批量重写。

## 实际效果（实测）
- `ast-grep --version` = `ast-grep 0.51.0`（全局 npm 包 `@ast-grep/cli`）。
- 结构搜索：`ast-grep run -p 'eval($A)' -l python .` → 精确命中 `t.py:3: return eval(x)`。
- 结构改写：`ast-grep run -p 'eval($A)' -r 'ast.literal_eval($A)' -l python --update-all .`
  → `Applied 1 changes`，文件真被改成 `return ast.literal_eval(x)`。

## 怎么调
```
ast-grep run -p 'pattern' -l <lang> <path>              # 结构搜索
ast-grep run -p 'eval($A)' -r 'safe($A)' -l py --update-all .   # 批量改写
ast-grep run -p 'console.log($$$)' -l js -A             # -A/-B/-C 带上下文
ast-grep run -p 'pattern' -l ts --json=stream           # 机器可读
ast-grep new                                            # 建项目（scan 才需要）
ast-grep scan                                           # 按 sgconfig.yml 跑规则集
```

## 坑
- **`scan` 必须先是「项目」**：实测直接 `ast-grep scan` 报
  `Error: No ast-grep project configuration is found`（要 `ast-grep new` 生成 `sgconfig.yml`）。
  只有 `run -p` 是开箱即用的。
- 语言参数写 `python`/`py` 都行，但 `-l ts` 不认 `.tsx`（要用 `tsx`），混写会静默 0 命中。
- 元变量 `$A` 匹配单节点、`$$$` 匹配多节点，写错会「0 命中但不报错」。
- `--update-all` **直接改盘无备份**，动手前 `git status` 干净或先 commit。
- 本机是 npm 全局装（`/usr/local/lib`，**不持久**），容器重建靠 `start.sh` 2i 段 `npm i -g @ast-grep/cli` 重装。
