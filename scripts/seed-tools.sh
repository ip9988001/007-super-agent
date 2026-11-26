#!/bin/bash
# 007 私密构建：首次部署把工具种子接进 <deploy>/tools/。
# 按子目录逐个拷：目标子目录已存在且非空时跳过，绝不覆盖已有文件。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TOOLS_DIR="${TOOLS_DIR:-$ROOT/tools}"
SEED="$ROOT/tools-seed"

if [ ! -d "$SEED/skills" ]; then
  echo "seed-tools: no seed at $SEED, skip" >&2
  exit 0
fi

mkdir -p "$TOOLS_DIR"
for sub in skills capabilities design-md seo-toolkit scripts parked-extensions; do
  if [ -d "$TOOLS_DIR/$sub" ] && [ -n "$(ls -A "$TOOLS_DIR/$sub" 2>/dev/null)" ]; then
    echo "seed-tools: $TOOLS_DIR/$sub already present, skip"
  else
    cp -a "$SEED/$sub" "$TOOLS_DIR/"
    echo "seed-tools: seeded $TOOLS_DIR/$sub"
  fi
done
