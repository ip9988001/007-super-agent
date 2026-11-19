# rg (ripgrep)
## 一句话
ripgrep **13.0.0**：代码搜索主力，`.gitignore` 感知 + 多线程 + UTF-8，比 grep 快一个量级。

## 实际效果（实测 2026-10-26）
- `rg --version` → `ripgrep 13.0.0`，`-SIMD -AVX (compiled)` ✅
- `rg -c "providerInspections" src/` → 直接给 `file:count`（doctorRuntime.ts、GhidraDoctor.test.ts…）✅
- `rg -n "REA_JADX_MCP_JAR" src/ -g '!*.test.ts'` → 8 处命中，测试文件被正确排除 ✅
- `rg -l -uu "unblob" third_party/` → **160** vs 默认 **150**（多出的 10 个在被 ignore 的 `__pycache__/`、`build/` 里）✅ 证明 `-uu` 真有用
- `rg -a -l "ghidra" .codegraph/codegraph.db` → 命中；**不加 `-a` 返回 0**（二进制被跳过）✅
- `rg -P 'foo(?=\d)'` → 命中 `foo123`；不加 `-P` → `regex parse error` ❌（默认引擎无 lookahead）✅
- `rg --json` 可用，可直接 `python3 -c 'json.loads'` 逐行吃 ✅

## 怎么调
```bash
rg -n "pattern" path                 # 行号 + 内容
rg -l "pattern" path                 # 只要文件路径
rg -c "pattern" path                 # 每文件计数
rg -g '*.ts' -g '!*.test.ts' "p"     # glob 包含 / 排除
rg -t py "pattern"                   # 按文件类型
rg -S "pattern"                      # smart-case（全小写才忽略大小写）
rg -A3 -B3 "pattern"                 # 带上下文
rg -uu "pattern"                     # 连 .gitignore 一起忽略
rg -a "pattern" file                 # 二进制也搜
rg --json "p" | python3 -c "import sys,json;print(json.loads(sys.stdin.readline())['type'])"
```

## 坑
- **搜不到 ≠ 不存在**：默认遵守 `.gitignore`，还跳过隐藏文件和二进制。先 `-uu`，要搜二进制再 `-a`。
- 二进制文件行为不一致：不加 `-a`，`-l` 返回空但 `-c` 会给个大数 —— 别拿 `-c` 判断「这文件能搜」。
- `.codegraph/codegraph.db`（555MB）永远显式排除：`-g '!.codegraph/*'`。
- 默认引擎不支持 lookahead/backreference，要 `-P/--pcre2`。
- 13.0.0 是 2021 版，比当前主线少若干新 flag，抄新文档要现场验。
