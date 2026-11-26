#!/bin/bash
# 007 私密构建：首次部署把预置 DeepSeek 密钥接进守护进程读的 .env。
# 只落一次：目标文件已存在时直接跳过，绝不覆盖已部署机器上的活密钥。
# 不打印任何密钥内容；目标权限 0600（守护进程配置加载对过松权限会告警）。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SRC="$ROOT/secrets/deepseek.env"
# DEST 可覆盖（模拟验证用）；默认是守护进程 ResolveAPIKey 的查找路径。
DEST="${SUPER_AGENT_ENV_DEST:-/etc/super-agent/.env}"

if [ ! -f "$SRC" ]; then
  echo "apply-secrets: no secret at $SRC, skip" >&2
  exit 0
fi
if [ -f "$DEST" ]; then
  echo "apply-secrets: $DEST exists, skip (refuse to overwrite)"
  exit 0
fi

mkdir -p "$(dirname "$DEST")"
cp "$SRC" "$DEST"
chmod 600 "$DEST"
echo "apply-secrets: installed $DEST (0600)"
