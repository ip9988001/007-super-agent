# lean-ctx 3.11.4

> **一句话**：终端命令与文件阅读的降噪/压缩网关（单二进制），**rtk 已退役，由 lean-ctx 接管**。

## 实际效果（实测）
- `lean-ctx -c "ls -la <dir>"` → 输出尾部标 `277 → 109 tok (↓~60%)`，9 个文件一条不丢。
- `lean-ctx overview` → 直接给项目树（depth 2，含每目录文件数），省掉一串 `ls`。
- `lean-ctx gain` → 累计 `1.2K token difference / 26% representation ratio / Lv1 Apprentice`。
- `--version` = 3.11.4 (official)；PATH `/usr/local/bin/lean-ctx` → `/data/tools/lean-ctx/bin/lean-ctx`。

## 怎么调
```
lean-ctx -c "cmd"        # 跑命令 + 压缩输出（日常主力）
lean-ctx raw "cmd"       # 同上但原始输出（逃生口，等价 LEAN_CTX_RAW=1）
lean-ctx read <file>     # 压缩式读文件
lean-ctx grep <pat>      # 压缩式搜索
lean-ctx overview        # 项目结构速览
lean-ctx gain --deep     # 省钱/省 token 明细
lean-ctx tools health    # 未用工具 / 冗余规则 / 过期知识体检
lean-ctx status|doctor   # 是否接线正常
```
全量命令：`lean-ctx help all`（官网称 79 个 MCP 工具，本机以 help 实际列表为准）。

## 坑
- **状态目录靠软链持久化**：`/root/.local/share/lean-ctx` → `/data/state/lean-ctx/share`，
  `/root/.lean-ctx` → `/data/state/lean-ctx/config`，由 `start.sh` 3a 段重建；手工删软链会丢配置与统计。
- **`lean-ctx serve` 守护进程瞬时 CPU 很高**（实测 `ps` 抓到 536%），独核运行时可感知变慢。
- 压缩只改「你看到的表示」，命令真实 stdout 未变；但**同一行被折叠后，后续 grep/awk 的语义会变** —— 要精确字节就 `raw`/`RTK_DISABLED=1`。
- 有一层 shell hook（`onboard`/`wrap` 时注入），不想被接管：`LEAN_CTX_DISABLED=1`。
- `dashboard` 起在 `localhost:3333`，容器内无对外暴露，只能本地看。
