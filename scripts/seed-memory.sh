#!/bin/bash
# 007 私密构建：首次部署把二号种子记忆接进 data/memory/。
# 只拷一次：目标已存在且非空时直接跳过，绝不覆盖已有记忆。
# 拷完立即写备份标记，避免第 15 步备份在无标记时全量重推种子。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DATA_DIR="${DATA_DIR:-$ROOT/data}"
SEED="$ROOT/memories/no2-seed"
MEM="$DATA_DIR/memory"

if [ ! -d "$SEED/l1" ] || [ ! -d "$SEED/wiki" ] || [ ! -d "$SEED/reference" ]; then
  echo "seed-memory: no complete seed at $SEED, skip" >&2
  exit 0
fi

# 按目录分别检查：缺哪个补哪个。旧版只拷 l1+wiki、守卫只看 l1，
# 导致 reference/（1305 篇）丢了也不补——第 22 步修。
mkdir -p "$MEM"
for sub in l1 wiki reference; do
  if [ -d "$MEM/$sub" ] && [ -n "$(ls -A "$MEM/$sub" 2>/dev/null)" ]; then
    echo "seed-memory: $MEM/$sub 已存在，跳过"
  else
    cp -a "$SEED/$sub" "$MEM/"
    echo "seed-memory: seeded $MEM/$sub"
  fi
done
# 备份标记：写当前时间（RFC3339），种子文件 mtime 是旧的，
# 下次 04:00 备份只推标记之后真正改动的文件，不重推种子。
date -u +"%Y-%m-%dT%H:%M:%SZ" > "$DATA_DIR/backup_memory_last"
echo "seed-memory: seeded $MEM from $SEED"
