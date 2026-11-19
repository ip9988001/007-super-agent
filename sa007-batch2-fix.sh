#!/bin/bash
# 007 第 19 步 · 第 2 批补修：semgrep/bandit/pip-audit（uv 需 --break-system-packages）
# ast-grep（正确 asset 名 app-x86_64-unknown-linux-gnu.zip）
# ghidra（实际解压目录 ghidra_12.2_PUBLIC，修正 wrapper 后重跑 headless 测试）
set -u
LOG=/tmp/sa007-batch2-fix.log
exec > >(tee -a "$LOG") 2>&1
echo "=== batch2-fix start $(date -u +%FT%TZ) ==="
export PATH=/usr/local/bin:$PATH

echo "--- semgrep 1.182.1 / bandit 1.9.4 / pip-audit 2.10.1 ---"
uv pip install --system --break-system-packages semgrep==1.182.1 bandit==1.9.4 pip-audit==2.10.1

echo "--- ast-grep 0.51.0 ---"
cd /tmp
curl -fsSL -o ast-grep.zip https://github.com/ast-grep/ast-grep/releases/download/0.51.0/app-x86_64-unknown-linux-gnu.zip
rm -rf /tmp/ast-grep-ex && mkdir -p /tmp/ast-grep-ex
unzip -o -q ast-grep.zip -d /tmp/ast-grep-ex
find /tmp/ast-grep-ex -type f | head
if [ -f /tmp/ast-grep-ex/ast-grep ]; then cp /tmp/ast-grep-ex/ast-grep /usr/local/bin/ast-grep;
elif [ -f /tmp/ast-grep-ex/sg ]; then cp /tmp/ast-grep-ex/sg /usr/local/bin/ast-grep;
else BIN=$(find /tmp/ast-grep-ex -type f -executable | head -1); echo "fallback binary: $BIN"; cp "$BIN" /usr/local/bin/ast-grep; fi
chmod +x /usr/local/bin/ast-grep
rm -rf /tmp/ast-grep-ex ast-grep.zip

echo "--- ghidra wrapper fix to ghidra_12.2_PUBLIC ---"
ls -d /data/tools/ghidra/ghidra_12.2_PUBLIC/support/analyzeHeadless
cat > /usr/local/bin/ghidra <<'WRAPPER_EOF'
#!/bin/sh
exec /data/tools/ghidra/ghidra_12.2_PUBLIC/support/analyzeHeadless "$@"
WRAPPER_EOF
chmod +x /usr/local/bin/ghidra
cat /usr/local/bin/ghidra

echo "--- ghidra headless test on /bin/true ---"
mkdir -p /tmp/ghidra-test
cp /bin/true /tmp/ghidra-test/sample 2>/dev/null || true
timeout 600 /usr/local/bin/ghidra /tmp/ghidra-test/proj GhidraTest -import /tmp/ghidra-test/sample -postScript "Decompile" -deleteProject > /tmp/ghidra-test/out.log 2>&1
echo "ghidra test exit: $?"
grep -i -m3 "decompil" /tmp/ghidra-test/out.log || tail -5 /tmp/ghidra-test/out.log

echo "=== batch2-fix verify ==="
echo "semgrep: $(semgrep --version 2>&1 | tail -1)"
echo "ast-grep: $(ast-grep --version 2>&1 | head -1)"
echo "bandit: $(bandit --version 2>&1 | head -1)"
echo "pip-audit: $(pip-audit --version 2>&1 | head -1)"
echo "=== batch2-fix done $(date -u +%FT%TZ) ==="
