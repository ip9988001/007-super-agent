# codegraph
## 一句话
代码图谱 CLI **1.6.2**（node:sqlite 内建后端），把仓库索引成符号 + 调用图后按需查；**目前只有 `/data/workspace/rea` 建过索引**。

## 实际效果（实测 2026-10-26）
- `codegraph --version` → `1.6.2` ✅
- `codegraph status /data/workspace/rea` → Files 6,889 / Nodes 128,271 / Edges 435,932 / DB 529.46 MB / `node:sqlite — built-in (full WAL)` ✅
- 库文件 `.codegraph/codegraph.db` 实测 555,184,128 B，**无 `-wal`/`-shm` 伴随文件**（已 checkpoint）✅
- `codegraph status`（cwd=`/data/workspace`）→ `⚠ Not initialized` ❌ 必须给 `-p` 正确路径
- `codegraph query "doctor"` → method/property + `file:line` + 签名 ✅
- `codegraph explore "doctor health check"` → 71 symbols / 6 files + **blast radius（41 callers + 测试文件清单）** ✅ 最值钱的一条
- `codegraph context "how are providers registered"` → 入口点 + 签名 ✅
- `.codegraph/errors.log` 里 1 条 ENOENT（third_party 悬空 symlink），不影响查询 ✅

## 怎么调
```bash
# 先确认有没有索引，别浪费一轮
codegraph status -p /data/workspace/rea

# 找符号 / 按 kind 过滤
codegraph query "hydrateSession" -p /data/workspace/rea -l 20
codegraph query "doctor" -k method -p /data/workspace/rea

# 改代码前首选：相关源码 + 调用路径 + 影响面
codegraph explore "how doctor assembles checks" -p /data/workspace/rea

# 按任务组上下文
codegraph context "add a new provider" -p /data/workspace/rea

# 重建（555MB 库，慢，别随手跑）
codegraph index -p /path -v
```

## 坑
- 无索引就直接 `Not initialized`，**不会自动降级到 grep**；换目录前先 `status`。
- 索引是快照：`.codegraph/.gitignore` 是 `*` + `!.gitignore`，所以 db 不进版本库，但目录本身会在 `git status` 里常挂 `?? .codegraph/`。
- 库 555MB 且二进制，别 `cat`/`rg` 它；要内容走 `codegraph query`。
- `index` 是全量重建，大仓吃内存+时间；小改动用 `sync`。
- `query/explore/context` 都靠 `-p/--path`，漏了就默认 cwd（在 `/data/workspace` 会撞上 Not initialized）。
