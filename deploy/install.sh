#!/bin/bash
# deploy/install.sh — 007 超级 Agent 一键部署（公开版）
#
# 在一台全新 Debian 12 服务器上（root 运行），从零装到能投产。
# 流程：系统依赖 → Python venv → 编译二进制 → 数据目录（宪法/注册表/索引）
#       → 工具目录 → DeepSeek 密钥 → systemd 守护 → 漏洞巡检定时器 → 验证。
#
# 用法：
#   sudo bash deploy/install.sh                       # 交互式，会问你要 DeepSeek 密钥
#   sudo DEEPSEEK_API_KEY=sk-... bash deploy/install.sh   # 非交互式，直接给密钥
#
# 可重复跑：已有文件不覆盖（.env、数据目录、工具种子都有跳过逻辑）。
# 密钥只进 /etc/super-agent/.env（0600），绝不进仓库、不打印。
# GitHub/Tavily 键是可选的（备份和搜索功能用），不填也能跑，事后补。
set -euo pipefail

[ "$(id -u)" -eq 0 ] || { echo "install.sh: must run as root" >&2; exit 1; }
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DEPLOY_DIR="${DEPLOY_DIR:-/root/super-agent-007}"
ENV_FILE="${ENV_FILE:-/etc/super-agent/.env}"

echo "=== 1. system deps + toolchain + venv ==="
bash "$REPO_ROOT/deploy/deps.sh"

echo "=== 2. build 007 (main + ctxread) ==="
[ "$(go version 2>/dev/null | awk '{print $3}')" = "go1.27.4" ] || { echo "install.sh: go 1.27.4 not found after deps.sh" >&2; exit 1; }
mkdir -p "$DEPLOY_DIR"
cd "$REPO_ROOT"
go build -o "$DEPLOY_DIR/super-agent" ./cmd/super-agent
go build -o "$DEPLOY_DIR/tools/ctxread" ./cmd/ctxread
"$DEPLOY_DIR/super-agent" --help 2>&1 | head -3 || true
echo "built: $(md5sum "$DEPLOY_DIR/super-agent" | awk '{print $1}')"
echo "ctxread: $(md5sum "$DEPLOY_DIR/tools/ctxread" | awk '{print $1}')"

echo "=== 3. data dir (constitutions / registry / index) ==="
mkdir -p "$DEPLOY_DIR/data/prompts" "$DEPLOY_DIR/data/registry" "$DEPLOY_DIR/data/index"
for f in "$REPO_ROOT/prompts/"*.md; do
  base="$(basename "$f")"
  if [ ! -f "$DEPLOY_DIR/data/prompts/$base" ]; then
    cp "$f" "$DEPLOY_DIR/data/prompts/"
    echo "installed data/prompts/$base"
  else
    echo "data/prompts/$base exists, skip"
  fi
done
if [ ! -f "$DEPLOY_DIR/data/registry/registry.json" ]; then
  cp "$REPO_ROOT/step1/registry/registry.json" "$DEPLOY_DIR/data/registry/registry.json"
  echo "installed data/registry/registry.json"
else
  echo "data/registry/registry.json exists, skip"
fi
for sub in L1.md L2 L3; do
  if [ -e "$REPO_ROOT/step1/index/$sub" ]; then
    if [ ! -e "$DEPLOY_DIR/data/index/$sub" ]; then
      cp -a "$REPO_ROOT/step1/index/$sub" "$DEPLOY_DIR/data/index/"
      echo "installed data/index/$sub"
    else
      echo "data/index/$sub exists, skip"
    fi
  fi
done

echo "=== 4. tools dir ==="
mkdir -p "$DEPLOY_DIR/tools"
for sub in skills seo-toolkit scripts; do
  if [ -d "$REPO_ROOT/tools/$sub" ]; then
    if [ -d "$DEPLOY_DIR/tools/$sub" ] && [ -n "$(ls -A "$DEPLOY_DIR/tools/$sub" 2>/dev/null)" ]; then
      echo "tools/$sub exists, skip"
    else
      cp -a "$REPO_ROOT/tools/$sub" "$DEPLOY_DIR/tools/"
      echo "installed tools/$sub"
    fi
  fi
done

echo "=== 5. DeepSeek key -> /etc/super-agent/.env ==="
mkdir -p "$(dirname "$ENV_FILE")"
if [ -f "$ENV_FILE" ] && grep -q "^DEEPSEEK_API_KEY=sk-" "$ENV_FILE" 2>/dev/null; then
  echo "$ENV_FILE already has a DeepSeek key, skip"
else
  KEY="${DEEPSEEK_API_KEY:-}"
  if [ -z "$KEY" ]; then
    if [ -t 0 ]; then
      printf "请输入 DeepSeek API Key（只存进 %s，不会显示在日志里）: " "$ENV_FILE"
      read -r -s KEY
      echo
    else
      echo "install.sh: no DEEPSEEK_API_KEY provided and not interactive." >&2
      echo "Re-run with: sudo DEEPSEEK_API_KEY=sk-... bash deploy/install.sh" >&2
      exit 1
    fi
  fi
  [ -n "$KEY" ] || { echo "install.sh: empty DeepSeek key" >&2; exit 1; }
  cat > "$ENV_FILE" <<EOF
DEEPSEEK_API_KEY=$KEY
WORKER_MODEL=deepseek-flash
ADVISOR_MODEL=deepseek-flash
ADVISOR_MODE=STANDARD
TIMEOUT_SEC=300
EOF
  chmod 600 "$ENV_FILE"
  echo "installed $ENV_FILE (0600)"
  echo "可选键（备份、搜索功能用）事后补：GITHUB_TOKEN、TAVILY_API_KEY、TAVILY_PASSWORD"
fi

echo "=== 6. systemd unit ==="
cp "$REPO_ROOT/deploy/super-agent.service" /etc/systemd/system/super-agent.service
systemctl daemon-reload
systemctl enable --now super-agent

echo "=== 7. weekly vuln audit timer ==="
if [ -f "$REPO_ROOT/ops/security/weekly_vuln_scan.py" ]; then
  mkdir -p /root/super-agent-007/ops/security /var/lib/super-agent
  cp "$REPO_ROOT/ops/security/weekly_vuln_scan.py" /root/super-agent-007/ops/security/weekly_vuln_scan.py
  cp "$REPO_ROOT/ops/security/vuln_ignore.json" /root/super-agent-007/ops/security/vuln_ignore.json
  cp "$REPO_ROOT/ops/security/007-vuln-audit.service" /etc/systemd/system/007-vuln-audit.service
  cp "$REPO_ROOT/ops/security/007-vuln-audit.timer" /etc/systemd/system/007-vuln-audit.timer
  systemctl daemon-reload
  systemctl enable --now 007-vuln-audit.timer
  echo "vuln audit timer enabled (Sun 03:00)"
else
  echo "ops/security not found in repo, vuln timer skipped"
fi

echo "=== 8. verify ==="
sleep 8
echo "status: $(systemctl is-active super-agent)"
echo "pid: $(systemctl show -p MainPID --value super-agent)"
(ss -ltnp 2>/dev/null || netstat -ltnp 2>/dev/null) | grep 6262 || echo "WARN: 6262 not listening yet"
journalctl -u super-agent --no-pager -n 10 | grep -E "registry|gateway ready|agent ready" || journalctl -u super-agent --no-pager -n 10
echo "venv: $(/opt/007-venv/bin/python3 -c "import sys; print(sys.executable)" 2>/dev/null || echo MISSING)"
echo "install.sh: DONE"
echo "投产检查：curl -s -X POST http://127.0.0.1:6262/wake -H 'Content-Type: application/json' -d '{\"task_id\":\"smoke-001\",\"text\":\"你好，报一下你的名字和当前时间\"}'"
