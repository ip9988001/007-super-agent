#!/usr/bin/env bash
# Cloudflare security-audit-skill × Semgrep 双层审计：本地初筛（0 token）→ 由 agent 研判
# 007 通用版：默认路径均为相对路径/家目录；没有本地规则快照时自动走在线 registry 规则。
# 用法: run-audit.sh <目标路径> [--registry] [--refresh-rules] [额外 semgrep 参数...]
#   默认：local  离线本地规则快照（./semgrep-rules，规则更全，不依赖外网；没有则自动走在线规则）
#   --registry   改走 semgrep registry 在线轻量规则（4s），失败自动回落 local
#   --refresh-rules  git pull 更新本地规则快照后再扫描
#   退出码 0=无候选 1=有候选 2=用法错误 3=引擎异常
set -uo pipefail

export XDG_CACHE_HOME="${XDG_CACHE_HOME:-$HOME/.cache}"
mkdir -p "$XDG_CACHE_HOME" 2>/dev/null || true
LOCAL_RULES_DIR="${SEMGREP_RULES_DIR:-./semgrep-rules}"
EXTRA_YAML="${SEMGREP_EXTRA:-}"
if [ -z "$EXTRA_YAML" ] && [ -f ./security-audit-skill/rules/extra.yaml ]; then
  EXTRA_YAML="./security-audit-skill/rules/extra.yaml"
fi

REFRESH=0; MODE="local"; POS=()
for a in "$@"; do
  case "$a" in
    --refresh-rules) REFRESH=1 ;;
    --registry) MODE="registry" ;;
    --local) MODE="local" ;;
    *) POS+=("$a") ;;
  esac
done
TARGET="${POS[0]:-}"
if [ ${#POS[@]} -gt 1 ]; then EXTRA=("${POS[@]:1}"); else EXTRA=(); fi
if [ -z "$TARGET" ] || [ ! -e "$TARGET" ]; then
  echo "用法: run-audit.sh <目标文件或目录> [--registry] [--refresh-rules] [额外 semgrep 参数...]" >&2
  exit 2
fi

OUT_DIR="${OUT_DIR:-./audit-out}"
mkdir -p "$OUT_DIR"
STAMP="$(date +%Y%m%d-%H%M%S)"
RAW="$OUT_DIR/semgrep-$STAMP.json"
ERR="$OUT_DIR/semgrep-$STAMP.err"

if [ "$REFRESH" = "1" ]; then
  echo "REFRESH_RULES 更新本地规则快照"
  if [ -d "$LOCAL_RULES_DIR/.git" ]; then
    git -C "$LOCAL_RULES_DIR" pull --ff-only --depth 1 2>&1 | tail -3
    echo "  本地快照版本: $(git -C "$LOCAL_RULES_DIR" log --oneline -1)"
  else
    echo "  [!] 未找到本地规则快照: $LOCAL_RULES_DIR（请先 git clone semgrep-rules）"
  fi
  rm -f "$XDG_CACHE_HOME/semgrep_version" 2>/dev/null || true
fi

REMOTE_RULES=(--config p/default --config p/secrets --config p/sql-injection)
LOCAL_RULES=(--config "$LOCAL_RULES_DIR/javascript" --config "$LOCAL_RULES_DIR/python" --config "$LOCAL_RULES_DIR/generic")
if [ -n "$EXTRA_YAML" ]; then
  REMOTE_RULES+=(--config "$EXTRA_YAML")
  LOCAL_RULES+=(--config "$EXTRA_YAML")
fi
EXCL=(--exclude node_modules --exclude .git --exclude dist --exclude build --exclude vendor)

run_scan() { # $1 = 标签, 其余 = 规则参数
  local label="$1"; shift
  local t0 t1
  t0=$(date +%s)
  semgrep scan "$@" "${EXCL[@]}" --json --quiet --metrics=off --disable-version-check "${EXTRA[@]}" "$TARGET" > "$RAW" 2> "$ERR"
  t1=$(date +%s)
  echo "SCAN_MODE $label  TIME $((t1-t0))s"
}

if [ "$MODE" = "registry" ]; then
  run_scan "registry" "${REMOTE_RULES[@]}"
  if ! python3 -c "import json,sys; json.load(open('$RAW'))" >/dev/null 2>&1; then
    echo "REMOTE_FALLBACK registry 不可用，自动切换本地规则快照（离线模式）"
    run_scan "local" "${LOCAL_RULES[@]}"
  fi
else
  if [ -d "$LOCAL_RULES_DIR/javascript" ]; then
    run_scan "local" "${LOCAL_RULES[@]}"
  else
    echo "LOCAL_RULES_MISSING 本地快照缺失，回落 registry"
    run_scan "registry" "${REMOTE_RULES[@]}"
  fi
fi

python3 - "$RAW" "$ERR" <<'PY'
import json, sys
raw, err = sys.argv[1], sys.argv[2]
try:
    d = json.load(open(raw, encoding="utf8"))
except Exception as e:
    print("SEMGREP_ERROR 无法解析结果: %s" % e)
    try: print(open(err, encoding="utf8").read()[-800:])
    except Exception: pass
    sys.exit(3)
res = d.get("results") or []
def sev(r):
    m = (r.get("extra") or {}).get("severity") or "INFO"
    return {"ERROR": 0, "WARNING": 1, "INFO": 2}.get(m, 3)
res.sort(key=sev)
counts = {}
for r in res:
    k = ((r.get("extra") or {}).get("severity") or "INFO")
    counts[k] = counts.get(k, 0) + 1
print("SEMGREP_SUMMARY total=%d %s" % (len(res), " ".join("%s=%d" % (k, v) for k, v in sorted(counts.items()))))
print("RAW_JSON %s" % raw)
if not res:
    print("NO_FINDINGS 初筛无候选，无需 agent 介入")
    sys.exit(0)
print("CANDIDATES (按严重度排序，供 agent 对照 Cloudflare 规程研判)")
for i, r in enumerate(res[:40], 1):
    ex = r.get("extra") or {}
    snip = (ex.get("lines") or "").strip().replace("\n", " ⏎ ")[:220]
    print("[%02d] %s | %s | %s:%d" % (i, ex.get("severity", "?"), r.get("check_id", "?").split(".")[-1], r.get("path"), (r.get("start") or {}).get("line", 0)))
    print("     %s" % snip)
    msg = (ex.get("message") or "").strip().replace("\n", " ")[:200]
    if msg: print("     说明: %s" % msg)
if len(res) > 40:
    print("... 另有 %d 条，见 RAW_JSON" % (len(res) - 40))
sys.exit(1)
PY
