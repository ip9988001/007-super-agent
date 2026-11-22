# cap-fetch-gh-cli

## 一句话
GitHub 官方 CLI 2.103.2 已装，但未登录且无 GH_TOKEN，所有 API 命令当前不可用。

## 怎么调
# 当前唯一能用的是"不需要 API"的子命令（本地/纯 git） gh --version gh --help gh completion -s bash # 登录后才可用（现在跑会报错） gh auth login --with-token < token.txt # 或 export GH_TOKEN=ghp_xxx gh api repos/OWNER/REPO --jq '.stargazers_count' gh search repos "关键词" --limit 10 gh repo clone OWNER/REPO # 不想登录时的等价替代（已验证可用） agent-reach github read OWNER/REPO # 走 github-api，无需 token agent-reach github search "关键词" --limit 5 curl -s…

## 坑
gh 的 `--jq` 是内置过滤，不依赖系统 jq —— 这是它比 curl 管道唯一占优的地方（但得先能认证）。；报错信息统一是"please run gh auth login"，不区分没装 token 还是 token 失效。

## 实际效果
`gh --version` → `gh version 2.103.2 (2026-10-23)` ✅；`gh auth status` → `You are not logged into any GitHub hosts` ❌

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）前置：登录态已验证（2026-11-17 gh auth status: ip9988001）。按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`gh --version` → `gh version 2.103.2 (2026-10-23)` ✅；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/01-取数与抓取/G-gh-cli.md · 核实：2026-10-26（原文件 实际效果（实测 2026-10-26））
> 注意：原文件 2026-10-26 称未登录；2026-11-17 复核已登录，以复核为准
