# gh token 持久化（gh 2.103.2 / ip9988001）

> **一句话**：GitHub CLI 已登录 `ip9988001`，token 落在持久卷 `/data/state/gh/token`（40 字节），容器重建自动复登。
> 用法与渠道细节见 `01-取数与抓取/G-gh-cli.md`（那条写「未登录」**已过期**）；本条只讲运维与自愈。

## 实际效果（实测）
- `gh --version` = `2.103.2`；`/data/tools/gh/bin/gh` → `/usr/local/bin/gh`。
- `gh auth status`：`✓ Logged in to github.com account ip9988001`，
  scopes = `gist, read:org, repo, workflow`，token 前缀 `gho_`。
- `gh api user -q '.login,.public_repos'` → `ip9988001 / 18`。
- `gh api rate_limit -q '.rate.remaining,.rate.limit'` → `4989 / 5000`（未认证只有 60/h）。
- `gh search repos "llmtrim" --limit 2 --json fullName,stargazersCount` → 正常返回 JSON。

## 怎么调
```
export GH_TOKEN=$(cat /data/state/gh/token)   # shell 里想手动用时
gh auth status                                # 先看是否已登录
gh api user -q '.login'                       # 任意 REST 端点
gh api graphql -f query='{viewer{login}}'
gh search repos "kw" --limit 10 --json fullName,stargazersCount
gh auth login --with-token < /data/state/gh/token   # 重建后的复登动作
```
`start.sh` 2h 段：`gh auth status` 失败才用持久 token 复登，然后才跑 rea setup。

## 坑
- **token 明文就在 `/data/state/gh/token`**，任何能读 `/data` 的进程都能用它；文件权限 600，别 `cat` 到聊天里。
- `GH_TOKEN` 只在 `start.sh` 导出的子进程里有效；**新 shell 默认没有**，直接 `gh api` 可能报未认证。
- `gho_` 前缀是 OAuth app token（不是 classic `ghp_`）：能用的 scope 以 `auth status` 输出为准，
  需要新的 scope 得重新授权，改 token 文件无效。
- rate limit 5000/h 是**按 token** 算的；批量 `gh search`/`gh api` 很容易打满，打满后所有 GitHub 渠道一起 403。
- 40 条/分钟的内容类 API（search）另有独立配额，比 5000 小得多。
