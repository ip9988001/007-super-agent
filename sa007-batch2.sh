#!/bin/bash
# 007 第 19 步 · 第 2 批：代码审计 9 个（测试机 VM124655，Debian 12）
# rea 6.8.0 codegraph 1.6.2 semgrep 1.182.1 ast-grep 0.51.0 trivy 0.76.1
# gitleaks 8.30.1 bandit 1.9.4 pip-audit 2.10.1 ghidra（最新稳定版）
# 来源：npm / pip（uv）/ GitHub release，不走三号。不碰 xray/nginx。
set -u
LOG=/tmp/sa007-batch2.log
exec > >(tee -a "$LOG") 2>&1
echo "=== batch2 start $(date -u +%FT%TZ) ==="
export PATH=/usr/local/bin:$PATH

echo "--- rea 6.8.0 (npm rea-agents@6.8.0, morluto/rea) ---"
npm install -g rea-agents@6.8.0

echo "--- codegraph 1.6.2 (npm @colbymchenry/codegraph@1.6.2) ---"
npm install -g @colbymchenry/codegraph@1.6.2

echo "--- semgrep 1.182.1 / bandit 1.9.4 / pip-audit 2.10.1 (uv pip --system) ---"
uv pip install --system semgrep==1.182.1 bandit==1.9.4 pip-audit==2.10.1

echo "--- ast-grep 0.51.0 (GitHub release) ---"
cd /tmp
curl -fsSL -o ast-grep.zip https://github.com/ast-grep/ast-grep/releases/download/0.51.0/ast-grep_0.51.0_x86_64-unknown-linux-gnu.zip
mkdir -p /tmp/ast-grep-ex && unzip -o -q ast-grep.zip -d /tmp/ast-grep-ex
if [ -f /tmp/ast-grep-ex/ast-grep ]; then cp /tmp/ast-grep-ex/ast-grep /usr/local/bin/ast-grep;
elif [ -f /tmp/ast-grep-ex/sg ]; then cp /tmp/ast-grep-ex/sg /usr/local/bin/ast-grep; fi
chmod +x /usr/local/bin/ast-grep
rm -rf /tmp/ast-grep-ex ast-grep.zip

echo "--- trivy 0.76.1 (GitHub release) ---"
curl -fsSL -o trivy.tar.gz https://github.com/aquasecurity/trivy/releases/download/v0.76.1/trivy_0.76.1_Linux-64bit.tar.gz
tar -xzf trivy.tar.gz -C /usr/local/bin trivy
chmod +x /usr/local/bin/trivy
rm -f trivy.tar.gz

echo "--- gitleaks 8.30.1 (GitHub release) ---"
curl -fsSL -o gitleaks.tar.gz https://github.com/gitleaks/gitleaks/releases/download/v8.30.1/gitleaks_8.30.1_linux_x64.tar.gz
tar -xzf gitleaks.tar.gz -C /usr/local/bin gitleaks
chmod +x /usr/local/bin/gitleaks
rm -f gitleaks.tar.gz

echo "--- ghidra (GitHub release, latest stable) ---"
GH_JSON=$(curl -fsSL --max-time 30 "https://api.github.com/repos/NationalSecurityAgency/ghidra/releases/latest")
GH_TAG=$(echo "$GH_JSON" | python3 -c "import sys,json; print(json.load(sys.stdin)['tag_name'])")
GH_URL=$(echo "$GH_JSON" | python3 -c "import sys,json; d=json.load(sys.stdin); print([a['browser_download_url'] for a in d['assets'] if a['name'].startswith('ghidra_') and a['name'].endswith('.zip')][0])")
echo "ghidra tag: $GH_TAG url: $GH_URL"
mkdir -p /data/tools/ghidra
curl -fsSL -o /tmp/ghidra.zip "$GH_URL"
unzip -o -q /tmp/ghidra.zip -d /data/tools/ghidra
GH_DIR=$(ls -d /data/tools/ghidra/ghidra_*_PUBLIC_* 2>/dev/null | head -1)
echo "ghidra dir: $GH_DIR"
cat > /usr/local/bin/ghidra <<'WRAPPER_EOF'
#!/bin/sh
exec /data/tools/ghidra/ghidra-11.4.2_PUBLIC_20250826/support/analyzeHeadless "$@"
WRAPPER_EOF
chmod +x /usr/local/bin/ghidra
rm -f /tmp/ghidra.zip
# fix wrapper to the actual extracted dir (tag may differ)
if [ -n "$GH_DIR" ] && [ "$GH_DIR/support/analyzeHeadless" != "/data/tools/ghidra/ghidra-11.4.2_PUBLIC_20250826/support/analyzeHeadless" ]; then
  cat > /usr/local/bin/ghidra <<WRAPPER_EOF
#!/bin/sh
exec $GH_DIR/support/analyzeHeadless "\$@"
WRAPPER_EOF
  chmod +x /usr/local/bin/ghidra
fi
ls -l /usr/local/bin/ghidra; cat /usr/local/bin/ghidra

echo "--- ghidra headless test on /bin/true ---"
mkdir -p /tmp/ghidra-test
cp /bin/true /tmp/ghidra-test/sample 2>/dev/null || true
timeout 600 /usr/local/bin/ghidra /tmp/ghidra-test/proj GhidraTest -import /tmp/ghidra-test/sample -postScript "Decompile" -deleteProject > /tmp/ghidra-test/out.log 2>&1
echo "ghidra test exit: $?"
grep -i -m3 "decompil" /tmp/ghidra-test/out.log || tail -5 /tmp/ghidra-test/out.log

echo "=== batch2 verify ==="
echo "rea: $(rea --version 2>&1 | head -1)"
echo "codegraph: $(codegraph --version 2>&1 | head -1)"
echo "semgrep: $(semgrep --version 2>&1 | tail -1)"
echo "ast-grep: $(ast-grep --version 2>&1 | head -1)"
echo "trivy: $(trivy --version 2>&1 | head -1)"
echo "gitleaks: $(gitleaks version 2>&1 | head -1)"
echo "bandit: $(bandit --version 2>&1 | head -1)"
echo "pip-audit: $(pip-audit --version 2>&1 | head -1)"
echo "ghidra wrapper: $(cat /usr/local/bin/ghidra | tail -1)"
echo "=== batch2 done $(date -u +%FT%TZ) ==="
