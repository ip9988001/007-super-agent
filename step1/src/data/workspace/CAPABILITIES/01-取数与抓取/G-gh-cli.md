# gh (GitHub CLI)
## 一句话
GitHub 官方 CLI 2.103.2 已装，但**未登录且无 GH_TOKEN**，所有 API 命令当前不可用。

## 实际效果（实测 2026-10-26）
- `gh --version` → `gh version 2.103.2 (2026-10-23)` ✅
- `gh auth status` → `You are not logged into any GitHub hosts` ❌
- `gh api rate_limit` → 拒绝：`To get started with GitHub CLI, please run: gh auth login` ❌
- `gh api repos/cli/cli --jq '.stargazers_count'` → 同上错误（连公开端点也要认证）❌
- `gh search repos "http client" --limit 2` → 同上错误 ❌
- 环境里 `GH_TOKEN` / `GITHUB_TOKEN` 均未设置（env 查过）

## 怎么调
```bash
# 当前唯一能用的是"不需要 API"的子命令（本地/纯 git）
gh --version
gh --help
gh completion -s bash

# 登录后才可用（现在跑会报错）
gh auth login --with-token < token.txt      # 或 export GH_TOKEN=ghp_xxx
gh api repos/OWNER/REPO --jq '.stargazers_count'
gh search repos "关键词" --limit 10
gh repo clone OWNER/REPO

# 不想登录时的等价替代（已验证可用）
agent-reach github read OWNER/REPO          # 走 github-api，无需 token
agent-reach github search "关键词" --limit 5
curl -s https://api.github.com/repos/OWNER/REPO | python3 -m json.tool | head -30
```

## 坑
- gh 的 `--jq` 是内置过滤，不依赖系统 jq —— 这是它比 curl 管道唯一占优的地方（但得先能认证）。
- 报错信息统一是"please run gh auth login"，不区分没装 token 还是 token 失效。
- 未认证时不要反复重试不同子命令，全部同一条错误。
- `gh repo clone` 走 https 匿名克隆公开库理论可行，但属于 git 能力，不是 gh 的 API 能力，别当数据接口用。
- GitHub 未认证 REST 限 60 次/小时，认证后 5000；批量抓仓库优先 agent-reach github。
