#!/bin/bash
# install/install-007.sh — 007 超级 Agent 一键重装（私密库版）
#
# 在一台全新 Debian 12 服务器上（root 运行）重装 007，从零到投产。
# 前置：已克隆公开库 007-super-agent（/root/007-super-agent）与私密库
# super-agent-007（本脚本所在仓库）。本脚本放在私密库 install/ 下，从私密库里跑。
#
# 流程：公开库拉最新 → 系统依赖+venv（调公开库 deploy/deps.sh）→ 编译
# 二进制（主程序 + ctxread）→ 恢复工具种子 → 落 DeepSeek 密钥 →
# 数据目录（宪法/注册表/索引）→ 补其余环境变量 → 装 systemd unit →
# 装每周漏洞巡检定时器 → 验证。
#
# 不含任何密钥值；GitHub token、Tavily 键不进仓库，在部署机上单独配。
#
# 用法：sudo bash install/install-007.sh
# 可重复跑：已有文件不覆盖（seed-tools、apply-secrets、数据目录自带跳过逻辑）。
set -euo pipefail

[ "$(id -u)" -eq 0 ] || { echo "install-007.sh: must run as root" >&2; exit 1; }
PRIV_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PUBLIC_DIR="${PUBLIC_DIR:-/root/007-super-agent}"
DEPLOY_DIR="${DEPLOY_DIR:-/root/super-agent-007}"
GO_VERSION_WANT="go1.27.4"

[ -d "$PUBLIC_DIR" ] || { echo "install-007.sh: public repo not found at $PUBLIC_DIR (git clone https://github.com/ip9988001/007-super-agent first)" >&2; exit 1; }
[ -f "$PRIV_ROOT/install/super-agent.env.example" ] || { echo "install-007.sh: super-agent.env.example missing" >&2; exit 1; }

echo "=== 0. pull latest public code ==="
if [ -d "$PUBLIC_DIR/.git" ]; then
  git -C "$PUBLIC_DIR" pull --ff-only 2>&1 | tail -2 || echo "public repo pull failed, continuing with local copy"
else
  echo "public dir is not a git repo, using as-is"
fi

echo "=== 1. system deps + toolchain + venv (public deploy/deps.sh) ==="
bash "$PUBLIC_DIR/deploy/deps.sh"

echo "=== 2. build 007 (main + ctxread) ==="
[ "$(go version 2>/dev/null | awk '{print $3}')" = "$GO_VERSION_WANT" ] || { echo "install-007.sh: go $GO_VERSION_WANT not found after deps.sh" >&2; exit 1; }
mkdir -p "$DEPLOY_DIR" "$DEPLOY_DIR/tools"
cd "$PUBLIC_DIR"
go build -o "$DEPLOY_DIR/super-agent" ./cmd/super-agent
go build -o "$DEPLOY_DIR/tools/ctxread" ./cmd/ctxread
"$DEPLOY_DIR/super-agent" --help 2>&1 | head -3 || true
echo "built: $(md5sum "$DEPLOY_DIR/super-agent" | awk '{print $1}')"
echo "ctxread: $(md5sum "$DEPLOY_DIR/tools/ctxread" | awk '{print $1}')"

echo "=== 3. seed tools (tools-seed -> deploy tools/) ==="
TOOLS_DIR="$DEPLOY_DIR/tools" bash "$PRIV_ROOT/scripts/seed-tools.sh"

echo "=== 4. DeepSeek key -> /etc/super-agent/.env ==="
bash "$PRIV_ROOT/scripts/apply-secrets.sh"

echo "=== 5. data dir (constitutions / registry / index) ==="
mkdir -p "$DEPLOY_DIR/data/prompts" "$DEPLOY_DIR/data/registry" "$DEPLOY_DIR/data/index"
for f in "$PRIV_ROOT/prompts/"*.md; do
  base="$(basename "$f")"
  if [ ! -f "$DEPLOY_DIR/data/prompts/$base" ]; then
    cp "$f" "$DEPLOY_DIR/data/prompts/"
    echo "installed data/prompts/$base"
  else
    echo "data/prompts/$base exists, skip"
  fi
done
if [ ! -f "$DEPLOY_DIR/data/registry/registry.json" ]; then
  cp "$PRIV_ROOT/step1/registry/registry.json" "$DEPLOY_DIR/data/registry/registry.json"
  echo "installed data/registry/registry.json"
else
  echo "data/registry/registry.json exists, skip"
fi
for sub in L1.md L2 L3; do
  if [ -e "$PRIV_ROOT/step1/index/$sub" ]; then
    if [ ! -e "$DEPLOY_DIR/data/index/$sub" ]; then
      cp -a "$PRIV_ROOT/step1/index/$sub" "$DEPLOY_DIR/data/index/"
      echo "installed data/index/$sub"
    else
      echo "data/index/$sub exists, skip"
    fi
  fi
done

echo "=== 6. remaining env keys ==="
if [ -f /etc/super-agent/.env ]; then
  echo "check /etc/super-agent/.env against install/super-agent.env.example;"
  echo "missing keys (GITHUB_TOKEN, TAVILY_API_KEY, TAVILY_PASSWORD) must be added by hand;"
  echo "BACKUP_SERVER_ID must be set to this machine's id. Never paste values into chat or git."
  cut -d= -f1 /etc/super-agent/.env
else
  echo "install-007.sh: /etc/super-agent/.env still missing after apply-secrets.sh" >&2
  exit 1
fi

echo "=== 7. systemd unit ==="
cp "$PRIV_ROOT/install/super-agent.service" /etc/systemd/system/super-agent.service
systemctl daemon-reload
systemctl enable --now super-agent

echo "=== 8. weekly vuln audit timer ==="
if [ -f "$PRIV_ROOT/ops/security/weekly_vuln_scan.py" ]; then
  mkdir -p "$DEPLOY_DIR/ops/security" /var/lib/super-agent
  cp "$PRIV_ROOT/ops/security/weekly_vuln_scan.py" "$DEPLOY_DIR/ops/security/weekly_vuln_scan.py"
  cp "$PRIV_ROOT/ops/security/vuln_ignore.json" "$DEPLOY_DIR/ops/security/vuln_ignore.json"
  cp "$PRIV_ROOT/ops/security/007-vuln-audit.service" /etc/systemd/system/007-vuln-audit.service
  cp "$PRIV_ROOT/ops/security/007-vuln-audit.timer" /etc/systemd/system/007-vuln-audit.timer
  systemctl daemon-reload
  systemctl enable --now 007-vuln-audit.timer
  echo "vuln audit timer enabled (Sun 03:00)"
else
  echo "ops/security not found in private repo, vuln timer skipped"
fi

echo "=== 8b. gateway watchdog timer (Line A P0) ==="
if [ -f "$PRIV_ROOT/install/gateway-watchdog.service" ]; then
  cp "$PRIV_ROOT/install/gateway-watchdog.service" /etc/systemd/system/gateway-watchdog.service
  cp "$PRIV_ROOT/install/gateway-watchdog.timer" /etc/systemd/system/gateway-watchdog.timer
  systemctl daemon-reload
  systemctl enable --now gateway-watchdog.timer
  echo "gateway watchdog timer enabled (every 5 min)"
else
  echo "install/gateway-watchdog.service not found in private repo, watchdog timer skipped"
fi

echo "=== 9. verify ==="
sleep 8
echo "status: $(systemctl is-active super-agent)"
echo "pid: $(systemctl show -p MainPID --value super-agent)"
(ss -ltnp 2>/dev/null || netstat -ltnp 2>/dev/null) | grep 6262 || echo "WARN: 6262 not listening yet"
journalctl -u super-agent --no-pager -n 10 | grep -E "registry ready|gateway ready|memory backup" || journalctl -u super-agent --no-pager -n 10
echo "venv: $(/opt/007-venv/bin/python3 -c "import sys; print(sys.executable)" 2>/dev/null || echo MISSING)"
go version
ollama list
echo "install-007.sh: DONE"
echo "NOTE: first deploy only — run scripts/seed-memory.sh to restore the seed memory."
echo "NOTE: reboot survival was never tested on the production host; verify at the next reboot/VM replacement."
echo "投产检查：curl -s -X POST http://127.0.0.1:6262/wake -H 'Content-Type: application/json' -d '{\"task_id\":\"smoke-001\",\"text\":\"你好，报一下你的名字和当前时间\"}'"
