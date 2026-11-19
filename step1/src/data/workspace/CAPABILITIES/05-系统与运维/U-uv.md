# uv 0.13.5

> **一句话**：装 Python 命令行工具的唯一入口（`uv tool install`），工具落 `/data` 持久卷。

## 实际效果（实测）
- `uv --version` = `0.13.5 (x86_64-unknown-linux-gnu)`，软链 `/usr/local/bin/uv` → `/data/tools/uv/bin/uv`。
- `uv tool list` 实测列出 `bandit v1.9.4` / `pip-audit v2.10.1`（含各自子命令）。
- `/data/tools/uv-tools` 占 41MB（bandit 13M + pip-audit 28M）。

## 怎么调
```
export UV_TOOL_DIR=/data/tools/uv-tools     # start.sh 2i 段的口径
export UV_TOOL_BIN_DIR=/data/tools/bin
uv tool install <pkg>     # 装 CLI（自动建隔离 venv）
uv tool list              # 已装清单
uv tool upgrade <pkg>
uv venv && uv pip install # 项目级虚拟环境（比 venv+pip 快）
```

## 坑
- **同一个变量被 start.sh 设了两次**：第 46 行 `UV_TOOL_DIR=/data/tools/uv/tools`（agent-reach 用），
  2i 段又改成 `/data/tools/uv-tools`（审计工具用）→ **后设的赢**。
- **工具存在两份**：`/root/.local/share/uv/tools` 还有一份 47MB 副本（容器重建即丢）。
  不导出环境变量时 `uv tool list` 认的是 `/root` 那份 —— 命令能用，但**不是持久的那份**。
  实际可用性靠 `/usr/local/bin/{bandit,pip-audit}` → `/data/tools/bin/*` 软链兜底。
- `uv venv` 造的 venv **默认不带 pip**，需要 `UV_VENV_SEED=1`（agent-reach 的渠道依赖靠 venv 内 pip）。
- 别把 `pip install --break-system-packages` 和 uv 混着用来装同一个工具。
