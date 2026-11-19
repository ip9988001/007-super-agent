#!/bin/bash
# 007 第 19 步 · 第 1 批补修：uv / rg / magick
set -u
LOG=/tmp/sa007-batch1-fix.log
exec > >(tee -a "$LOG") 2>&1
echo "=== batch1-fix start $(date -u +%FT%TZ) ==="
export PATH=/usr/local/bin:$PATH
cd /tmp

echo "--- rg 13.0.0 (correct asset name ripgrep_13.0.0_amd64.deb) ---"
curl -fsSL -o ripgrep.deb https://github.com/BurntSushi/ripgrep/releases/download/13.0.0/ripgrep_13.0.0_amd64.deb
dpkg -i ripgrep.deb
rm -f ripgrep.deb

echo "--- uv 0.13.5 (find binaries inside tarball) ---"
curl -fsSL -o uv-0.13.5.tar.gz https://github.com/astral-sh/uv/releases/download/0.13.5/uv-x86_64-unknown-linux-gnu.tar.gz
rm -rf /tmp/uv-extract && mkdir -p /tmp/uv-extract
tar -xzf uv-0.13.5.tar.gz -C /tmp/uv-extract
find /tmp/uv-extract -type f | head
UVBIN=$(find /tmp/uv-extract -name uv -type f | head -1)
UVXBIN=$(find /tmp/uv-extract -name uvx -type f | head -1)
cp "$UVBIN" /usr/local/bin/uv
cp "$UVXBIN" /usr/local/bin/uvx
chmod +x /usr/local/bin/uv /usr/local/bin/uvx
rm -rf /tmp/uv-extract uv-0.13.5.tar.gz

echo "--- magick symlink (same as #3: /usr/local/bin/magick -> /usr/bin/convert) ---"
ln -sfn /usr/bin/convert /usr/local/bin/magick

echo "=== batch1-fix verify ==="
echo "uv: $(uv --version 2>&1 | head -1)"
echo "uvx: $(uvx --version 2>&1 | head -1)"
echo "rg: $(rg --version 2>&1 | head -1)"
echo "magick: $(magick --version 2>&1 | head -1)"
echo "=== batch1-fix done $(date -u +%FT%TZ) ==="
