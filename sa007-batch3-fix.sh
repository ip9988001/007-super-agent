#!/bin/bash
# 007 第 19 步 · 第 3 批补修
# agent-reach：PyPI 只有 0.1.0，1.6.0 在 GitHub Panniantong/agent-reach，打 v1.6.0 标签装
# scrapling：CLI 需要 extras，改装 scrapling[all]==0.5（与三号收据一致）
# httpx：CLI 需要 cli extra（click/pygments/rich），改装 httpx[cli]==0.28.1
# camoufox：补 --break-system-packages
set -u
LOG=/tmp/sa007-batch3-fix.log
exec > >(tee -a "$LOG") 2>&1
echo "=== batch3-fix start $(date -u +%FT%TZ) ==="
export PATH=/usr/local/bin:/root/.local/bin:$PATH

echo "--- agent-reach 1.6.0 (git+https://github.com/Panniantong/agent-reach.git@v1.6.0) ---"
uv tool install git+https://github.com/Panniantong/agent-reach.git@v1.6.0 --force
ln -sfn /root/.local/bin/agent-reach /usr/local/bin/agent-reach

echo "--- scrapling[all]==0.5 ---"
uv tool install "scrapling[all]==0.5" --force
ln -sfn /root/.local/bin/scrapling /usr/local/bin/scrapling

echo "--- httpx[cli]==0.28.1 ---"
uv tool install "httpx[cli]==0.28.1" --force
ln -sfn /root/.local/bin/httpx /usr/local/bin/httpx

echo "--- camoufox ---"
uv pip install --system --break-system-packages camoufox
python3 -c "import camoufox; print('camoufox import ok, version:', getattr(camoufox, '__version__', 'unknown'))"

echo "=== batch3-fix verify ==="
echo "agent-reach: $(agent-reach version 2>&1 | head -1)"
echo "scrapling: $(scrapling --version 2>&1 | head -1)"
echo "httpx: $(httpx --help 2>&1 | head -1)"
echo "camoufox: $(python3 -c "import camoufox; print(getattr(camoufox, '__version__', 'unknown'))" 2>&1 | head -1)"
echo "=== batch3-fix done $(date -u +%FT%TZ) ==="
