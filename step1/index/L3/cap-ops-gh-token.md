# cap-ops-gh-token

## 一句话
GitHub CLI 已登录 `ip9988001`，token 落在持久卷 `/data/state/gh/token`（40 字节），容器重建自动复登。

## 怎么调
export GH_TOKEN=$(cat /data/state/gh/token) # shell 里想手动用时 gh auth status # 先看是否已登录 gh api user -q '.login' # 任意 REST 端点 gh api graphql -f query='{viewer{login}}' gh search repos "kw" --limit 10 --json fullName,stargazersCount gh auth login --with-token < /data/state/gh/token # 重建后的复登动作

## 坑
token 明文就在 `/data/state/gh/token`，任何能读 `/data` 的进程都能用它；文件权限 600，别 `cat` 到聊天里。；`GH_TOKEN` 只在 `start.sh` 导出的子进程里有效；新 shell 默认没有，直接 `gh api` 可能报未认证。

## 实际效果
`gh --version` = `2.103.2`；`/data/tools/gh/bin/gh` → `/usr/local/bin/gh`。；`gh auth status`：`✓ Logged in to github.com account ip9988001`

## SOP
- 输入：任务目标 + 待处理对象（文件/URL/文本/命令参数）。
- 步骤：1）按“怎么调”执行入口；2）核对输出与“实际效果”一致；3）遇“坑”按坑内办法处理，处理不了就上报，不硬编结果。
- 验收：`gh --version` = `2.103.2`；`/data/tools/gh/bin/gh` → `/usr/local/bin/gh`。；退出码 0 为成功；失败返回 code 与 stderr 头 600 字；输出截断 8MB

来源：CAPABILITIES/05-系统与运维/G-gh-token.md · 核实：2026-11-17（2026-11-17 复核 gh auth status 已登录 ip99880）
> 注意：凭据只落文件，不回显到聊天窗口
