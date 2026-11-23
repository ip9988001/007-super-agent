#!/bin/bash
# deploy/build-venv.sh — 给 007 建独立 Python 虚拟环境 /opt/007-venv（公开版）
#
# 解决的问题：Debian 12 系统 Python 里 11 个包是 apt 装的（Brotli、Flask、
# Werkzeug、Jinja2、pyOpenSSL、setuptools、yt-dlp 等），pip 根本升不动
# （PEP 668 直接拦截），79 个漏洞里 14 个包受影响。办法：不动系统 Python，
# 另建一个干净 venv，007 的 Python 工具全走 venv，安全版全部装进 venv。
#
# 做什么：
#   1. 建 /opt/007-venv（不带 --system-site-packages，彻底隔离）
#   2. 按 requirements.in 装包（mcp>=1.30.0、brotli>=1.2.1、httplib2>=0.32.0、
#      yt-dlp>=2026.7.4 等安全版下限；Flask/Werkzeug 等僵尸包不装）
#   3. 给 venv 的 playwright 装匹配的浏览器（playwright install chromium）
#   4. 装 govulncheck（Go 侧漏洞扫描）
#   5. pip check + 导入自检 + pip-audit，导出 requirements.lock
#
# 用法：sudo bash deploy/build-venv.sh [--rebuild]
#   默认：venv 已存在且健康就跳过（可重复跑）；--rebuild 强制重建。
# 不动系统 Python、不动 super-agent 服务。
set -e
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

[ "$(id -u)" -eq 0 ] || { echo "build-venv.sh: must run as root" >&2; exit 1; }
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REQ_IN="$SCRIPT_DIR/requirements.in"
VENV=/opt/007-venv
REBUILD=0
[ "${1:-}" = "--rebuild" ] && REBUILD=1

[ -f "$REQ_IN" ] || { echo "build-venv.sh: $REQ_IN missing" >&2; exit 1; }

# 已存在且健康 -> 跳过（除非 --rebuild）
if [ "$REBUILD" -eq 0 ] && [ -x "$VENV/bin/python3" ]; then
  if "$VENV/bin/pip" check >/dev/null 2>&1 && \
     "$VENV/bin/python3" -c "import sys; assert sys.executable.startswith('$VENV'), sys.executable; import mcp, brotli, httplib2, yt_dlp" 2>/dev/null; then
    echo "build-venv.sh: $VENV already healthy, skip (use --rebuild to force)"
    "$VENV/bin/python3" -c "import mcp, brotli, httplib2; print('mcp:', mcp.__file__); print('brotli:', brotli.__file__)"
    exit 0
  fi
  echo "build-venv.sh: existing venv unhealthy, rebuilding"
fi

echo "=== 1. create venv ==="
rm -rf "$VENV"
python3 -m venv "$VENV"
"$VENV/bin/python3" --version
"$VENV/bin/pip" --version

echo "=== 2. upgrade pip ==="
"$VENV/bin/pip" install -U pip setuptools wheel

echo "=== 3. install requirements ==="
"$VENV/bin/pip" install -r "$REQ_IN"

echo "=== 4. pip check ==="
"$VENV/bin/pip" check

echo "=== 5. pip-audit (venv only, never system Python) ==="
"$VENV/bin/pip-audit" --format json --desc > /tmp/venv_audit.json 2>/tmp/venv_audit.err || echo "pip-audit exit=$? (1 means findings, see /tmp/venv_audit.json)"
python3 -c "import json; d=json.load(open('/tmp/venv_audit.json')); v=[x for x in d['dependencies'] if x.get('vulns')]; print('vuln packages in venv:', len(v)); [print(' ', x['name'], x['version'], len(x['vulns']), 'vulns') for x in v]"

echo "=== 6. import smoke ==="
"$VENV/bin/python3" -c "import sys; print('executable:', sys.executable); import PIL, bs4, lxml, requests, urllib3, trafilatura, htmldate, numpy, matplotlib, openpyxl, pypdf, weasyprint, playwright, camoufox, googleapiclient, google.auth, pytest; print('IMPORT_OK')"
"$VENV/bin/python3" -c "import mcp, brotli, httplib2; print('mcp:', mcp.__file__); print('brotli:', brotli.__file__); print('httplib2:', httplib2.__file__)"
"$VENV/bin/yt-dlp" --version
"$VENV/bin/python3" -c "import yt_dlp; print('yt_dlp:', yt_dlp.__file__)"

echo "=== 7. playwright browsers (match venv playwright version) ==="
"$VENV/bin/playwright" install chromium
"$VENV/bin/python3" -c "from playwright.sync_api import sync_playwright; p=sync_playwright().start(); b=p.chromium.launch(); pg=b.new_page(); pg.goto('about:blank'); print('PLAYWRIGHT_OK'); b.close(); p.stop()"

echo "=== 8. govulncheck (Go side) ==="
if command -v go >/dev/null 2>&1; then
  go install golang.org/x/vuln/cmd/govulncheck@latest
  ~/go/bin/govulncheck -version 2>&1 | head -2 || /root/go/bin/govulncheck -version 2>&1 | head -2
else
  echo "go not found, govulncheck skipped (install Go first, then: go install golang.org/x/vuln/cmd/govulncheck@latest)"
fi

echo "=== 9. lock ==="
"$VENV/bin/pip" freeze > "$VENV/requirements.lock"
wc -l "$VENV/requirements.lock"
cp "$VENV/requirements.lock" "$SCRIPT_DIR/requirements.lock" 2>/dev/null || true
echo "build-venv.sh: ALL_DONE"
