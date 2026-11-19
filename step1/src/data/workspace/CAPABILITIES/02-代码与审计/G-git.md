# git
## 一句话
git **2.39.5**，环境里唯一的 VCS；审计时主要用来查「改了什么 / 谁改的 / 哪次提交引入的」。

## 实际效果（实测 2026-10-26）
- `git --version` → `2.39.5` ✅
- `/data/workspace/rea`：branch `main`，remote `origin https://github.com/morluto/rea.git` ✅
- `git rev-list --count HEAD` → **1**，`git log` 只有 `6381aa0 fix(ghidra): scope rejected-input diagnostics…`（单提交/浅克隆）✅
- `git tag | wc -l` → **0**；`git describe --tags` → `fatal: No names found` ❌
- `git status --short` → ` M package-lock.json` + `?? .codegraph/` + `?? config/` ✅
- `.codegraph/.gitignore` = `*` + `!.gitignore`，所以 555MB 索引库不进版本库，但目录本身以 `??` 常驻 ✅

## 怎么调
```bash
git -C /path status --short        # 只看变更
git -C /path diff --stat           # 改了哪些文件、多少行
git -C /path log --oneline -20     # 近期提交
git -C /path log -S"REA_JADX_MCP_JAR" --oneline   # 哪次提交引入/删掉了这个符号（审计神器）
git -C /path blame -L 280,305 src/application/Doctor.ts   # 某段行的归属
git -C /path show --stat HEAD      # 看单次提交内容
git -C /path diff HEAD~1 --name-only
```

## 坑
- 这个仓库只有 **1 个 commit、0 个 tag**，`git log -S` / `blame` 拿不到历史纵深；「查不到」≠「从没出现过」，别据此下结论。
- `git status` 里 `?? .codegraph/` 会一直挂着（555MB），**别 `git add .`**，会把索引库提上去。
- 一律用 `git -C <path>`，别 `cd` 进去再跑，脚本里不会串目录。
- 2.39.5 是老版本（2022），2.4x 的新选项（如 `--remerge-diff` 的一些增强）不一定有，抄新版文档先小跑验证。
