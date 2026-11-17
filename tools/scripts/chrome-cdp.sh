#!/usr/bin/env bash
# 启动带 CDP 的 Chromium（供 browser-tools 技能连 :9222）
# 007 通用版：Chromium 从 ~/.cache/ms-playwright 找，状态与日志放 ~/.cache/super-agent-007；可用 CHROME_CDP_STATE / CHROME_CDP_LOG / CDP_PORT 覆盖。
set -uo pipefail
PORT="${CDP_PORT:-9222}"
CHROME="$(ls -d "$HOME"/.cache/ms-playwright/chromium-*/chrome-linux64/chrome 2>/dev/null | tail -1)"
[ -x "$CHROME" ] || { echo "ERR: chromium not found (playwright install chromium)" >&2; exit 3; }
if curl -s -m 2 "http://127.0.0.1:$PORT/json/version" >/dev/null 2>&1; then
  echo "OK: CDP already on :$PORT"; exit 0
fi
STATE_DIR="${CHROME_CDP_STATE:-$HOME/.cache/super-agent-007/chrome-cdp}"
LOG_FILE="${CHROME_CDP_LOG:-$HOME/.cache/super-agent-007/chrome-cdp.log}"
mkdir -p "$STATE_DIR"
nohup "$CHROME" --headless=new --no-sandbox --disable-gpu --remote-debugging-port="$PORT" \
  --remote-debugging-address=127.0.0.1 --user-data-dir="$STATE_DIR/profile" \
  --no-first-run --no-default-browser-check about:blank >"$LOG_FILE" 2>&1 &
echo "START: $CHROME (pid $!, cdp=127.0.0.1:$PORT)"
for i in $(seq 1 30); do sleep 1; curl -s -m 2 "http://127.0.0.1:$PORT/json/version" >/dev/null 2>&1 && { echo "OK: CDP ready on :$PORT"; exit 0; }; done
echo "ERR: CDP not ready in 30s (log $LOG_FILE)" >&2; exit 4
