#!/bin/bash
# 007 第 19 步 · 第 3 批：抓取浏览 14 个（测试机 VM124655，Debian 12）
# agent-reach 1.6.0 playwright 1.64.1 lightpanda nightly scrapling 0.5 httpx
# yt-dlp 2026.10.21 chrome-nosandbox camoufox edge-tts 7.3.1 hyperframes
# impeccable 4.7.0 tesseract 5.3.0 magick 6.9.11 mpv 0.35.1（后三者第 1 批 apt 已装，本批只核对）
# 来源：uv tool / npm / pip / GitHub release，不走三号。不碰 xray/nginx。
set -u
LOG=/tmp/sa007-batch3.log
exec > >(tee -a "$LOG") 2>&1
echo "=== batch3 start $(date -u +%FT%TZ) ==="
export PATH=/usr/local/bin:/root/.local/bin:$PATH

echo "--- agent-reach 1.6.0 (uv tool) ---"
uv tool install agent-reach==1.6.0
ln -sfn /root/.local/bin/agent-reach /usr/local/bin/agent-reach

echo "--- scrapling 0.5 (uv tool) ---"
uv tool install scrapling==0.5
ln -sfn /root/.local/bin/scrapling /usr/local/bin/scrapling

echo "--- httpx (uv tool, latest) ---"
uv tool install httpx
ln -sfn /root/.local/bin/httpx /usr/local/bin/httpx

echo "--- edge-tts 7.3.1 (uv tool) ---"
uv tool install edge-tts==7.3.1
ln -sfn /root/.local/bin/edge-tts /usr/local/bin/edge-tts

echo "--- camoufox (uv pip --system, Python API) ---"
uv pip install --system camoufox
python3 -c "import camoufox; print('camoufox import ok, version:', getattr(camoufox, '__version__', 'unknown'))"

echo "--- playwright 1.64.1 (npm) + chromium ---"
npm install -g playwright@1.64.1
npx playwright@1.64.1 install chromium
npx playwright@1.64.1 install-deps chromium

echo "--- chrome-nosandbox wrapper (points at playwright chromium) ---"
CHROME=$(find /root/.cache/ms-playwright -name chrome -path "*chrome-linux*/chrome" 2>/dev/null | head -1)
echo "chromium at: $CHROME"
if [ -n "$CHROME" ]; then
  printf '#!/bin/sh\nexec %s --no-sandbox --disable-dev-shm-usage --disable-gpu "$@"\n' "$CHROME" > /usr/local/bin/chrome-nosandbox
  chmod +x /usr/local/bin/chrome-nosandbox
else
  echo "CHROME_NOT_FOUND"
fi

echo "--- yt-dlp 2026.10.21 (GitHub release) ---"
curl -fsSL -o /usr/local/bin/yt-dlp https://github.com/yt-dlp/yt-dlp/releases/download/2026.10.21/yt-dlp_linux
chmod +x /usr/local/bin/yt-dlp

echo "--- lightpanda nightly (lightpanda-io/browser) ---"
curl -fsSL -o /usr/local/bin/lightpanda https://github.com/lightpanda-io/browser/releases/download/nightly/lightpanda-x86_64-linux
chmod +x /usr/local/bin/lightpanda

echo "--- hyperframes (npm, latest) ---"
npm install -g hyperframes

echo "--- impeccable 4.7.0 (skill dir copied from #3 in advance, see batch3 prep) ---"
ln -sfn /root/super-agent-007/tools/skills/impeccable/scripts/impeccable /usr/local/bin/impeccable
echo "impeccable absolute-path call:"
/root/super-agent-007/tools/skills/impeccable/scripts/impeccable --version 2>&1 | head -1

echo "=== batch3 verify ==="
echo "agent-reach: $(agent-reach version 2>&1 | head -1)"
echo "playwright: $(playwright --version 2>&1 | head -1)"
echo "lightpanda: $(lightpanda version 2>&1 | head -1)"
echo "scrapling: $(scrapling --version 2>&1 | head -1)"
echo "httpx: $(httpx --help 2>&1 | head -1)"
echo "yt-dlp: $(yt-dlp --version 2>&1 | head -1)"
echo "chrome-nosandbox: $(chrome-nosandbox --version 2>&1 | head -1)"
echo "edge-tts: $(edge-tts --version 2>&1 | head -1)"
echo "hyperframes: $(hyperframes --version 2>&1 | head -1)"
echo "impeccable: $(/root/super-agent-007/tools/skills/impeccable/scripts/impeccable --version 2>&1 | head -1)"
echo "tesseract: $(tesseract --version 2>&1 | head -1)"
echo "magick: $(magick --version 2>&1 | head -1)"
echo "mpv: $(mpv --version 2>&1 | head -1)"
echo "=== batch3 done $(date -u +%FT%TZ) ==="
