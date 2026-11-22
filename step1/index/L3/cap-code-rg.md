# cap-code-rg

## 一句话
ripgrep 13.0.0：代码搜索主力，`.gitignore` 感知 + 多线程 + UTF-8，比 grep 快一个量级。

## 怎么调
rg -n "pattern" path # 行号 + 内容 rg -l "pattern" path # 只要文件路径 rg -c "pattern" path # 每文件计数 rg -g '*.ts' -g '!*.test.ts' "p" # glob 包含 / 排除 rg -t py "pattern" # 按文件类型 rg -S "pattern" # smart-case（全小写才忽略大小写） rg -A3 -B3 "pattern" # 带上下文 rg -uu "pattern" # 连 .gitignore 一起忽略 rg -a "pattern" file # 二进制也搜 rg --json "p" | python3 -c "import sys,json;print(json.loads(sys.stdin.readline())['type'])"

## 坑
搜不到 ≠ 不存在：默认遵守 `.gitignore`，还跳过隐藏文件和二进制。先 `-uu`，要搜二进制再 `-a`。；二进制文件行为不一致：不加 `-a`，`-l` 返回空但 `-c` 会给个大数 —— 别拿 `-c` 判断「这文件能搜」。

## 实际效果
`rg --version` → `ripgrep 13.0.0`，`-SIMD -AVX (compiled)` ✅；`rg -c "providerInspections" src/` → 直接给 `file:count`（doctorRuntime.ts、GhidraDoctor.test.ts…）✅

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`rg --version` → `ripgrep 13.0.0`，`-SIMD -AVX (compiled)` ✅；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/02-代码与审计/R-rg.md · 核实：2026-10-26（原文件 实际效果（实测 2026-10-26））
> 注意：代码搜索主力；.gitignore 感知
