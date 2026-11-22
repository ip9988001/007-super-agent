# cap-code-git

## 一句话
git 2.39.5，环境里唯一的 VCS；审计时主要用来查「改了什么 / 谁改的 / 哪次提交引入的」。

## 怎么调
git -C /path status --short # 只看变更 git -C /path diff --stat # 改了哪些文件、多少行 git -C /path log --oneline -20 # 近期提交 git -C /path log -S"REA_JADX_MCP_JAR" --oneline # 哪次提交引入/删掉了这个符号（审计神器） git -C /path blame -L 280,305 src/application/Doctor.ts # 某段行的归属 git -C /path show --stat HEAD # 看单次提交内容 git -C /path diff HEAD~1 --name-only

## 坑
这个仓库只有 1 个 commit、0 个 tag，`git log -S` / `blame` 拿不到历史纵深；「查不到」≠「从没出现过」，别据此下结论。；`git status` 里 `?? .codegraph/` 会一直挂着（555MB），别 `git add .`，会把索引库提上去。

## 实际效果
`git --version` → `2.39.5` ✅；`/data/workspace/rea`：branch `main`，remote `origin https://github.com/morluto/rea.git` ✅

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`git --version` → `2.39.5` ✅；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/02-代码与审计/G-git.md · 核实：2026-10-26（原文件 实际效果（实测 2026-10-26））
> 注意：环境唯一 VCS
