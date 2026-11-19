#!/bin/bash
# sa007-redeploy3.sh — 部署第 20+21 步 + 修复批（browser-stack 入口、headless 包装、证据清单）到测试机
# 用法：bash sa007-redeploy3.sh
# 步骤：传新二进制 + 注册表/索引文件到二号 → 二号转测试机 → 测试机上同步文件、修包装脚本、停旧守护进程、换二进制、启动、校验
set -e
S=~/workspace/super-agent-007
BIN=/tmp/super-agent-007-step2021d
[ -f "$BIN" ] || { echo "missing binary $BIN"; exit 1; }
md5sum "$BIN"
# 1) 传到二号中转
scp -i /home/hatch/.ssh/id_ed25519 -P 6260 -o BatchMode=yes -o ConnectTimeout=20 -o StrictHostKeyChecking=accept-new -o ProxyCommand="nc -X connect -x hatch-egress-proxy:3130 %h %p" "$BIN" root@100.117.8.35:/tmp/sa007-step2021d-super-agent
scp -i /home/hatch/.ssh/id_ed25519 -P 6260 -o BatchMode=yes -o ConnectTimeout=20 -o StrictHostKeyChecking=accept-new -o ProxyCommand="nc -X connect -x hatch-egress-proxy:3130 %h %p" "$S/step1/registry/registry.json" root@100.117.8.35:/tmp/sa007-registry-v12.json
scp -i /home/hatch/.ssh/id_ed25519 -P 6260 -o BatchMode=yes -o ConnectTimeout=20 -o StrictHostKeyChecking=accept-new -o ProxyCommand="nc -X connect -x hatch-egress-proxy:3130 %h %p" "$S/step1/index/L2/fetch.md" root@100.117.8.35:/tmp/sa007-fetch.md
scp -i /home/hatch/.ssh/id_ed25519 -P 6260 -o BatchMode=yes -o ConnectTimeout=20 -o StrictHostKeyChecking=accept-new -o ProxyCommand="nc -X connect -x hatch-egress-proxy:3130 %h %p" "$S/step1/index/L3/cap-fetch-browser-stack.md" root@100.117.8.35:/tmp/sa007-browser-stack.md
echo "scp-to-no2 OK"
# 2) 二号转测试机（在二号上跑 scp；scp 用 < /dev/null，密码走 SSHPASS）
PASS=$(awk '/45.140.88.10/{print $NF}' ~/.config/server-credentials/dogyun)
printf '%s\n' "$PASS" | ~/workspace/pi-mail/ssh_pi.sh 'read P; export SSHPASS="$P"; sshpass -e scp -P 9701 -o StrictHostKeyChecking=accept-new /tmp/sa007-step2021d-super-agent root@45.140.88.10:/root/super-agent-007/super-agent.new < /dev/null && sshpass -e scp -P 9701 -o StrictHostKeyChecking=accept-new /tmp/sa007-registry-v12.json root@45.140.88.10:/tmp/sa007-registry-v12.json < /dev/null && sshpass -e scp -P 9701 -o StrictHostKeyChecking=accept-new /tmp/sa007-fetch.md root@45.140.88.10:/tmp/sa007-fetch.md < /dev/null && sshpass -e scp -P 9701 -o StrictHostKeyChecking=accept-new /tmp/sa007-browser-stack.md root@45.140.88.10:/tmp/sa007-browser-stack.md < /dev/null && echo "scp-to-test OK"'
# 3) 测试机上部署（base64 管道进 ssh，内层 ssh 后不加 < /dev/null）
cat > /tmp/sa007-deploy3-run.sh << 'RUN'
set -e
cd /root/super-agent-007
# 同步注册表 + 索引
cp /tmp/sa007-registry-v12.json data/registry/registry.json
cp /tmp/sa007-fetch.md data/index/L2/fetch.md
cp /tmp/sa007-browser-stack.md data/index/L3/cap-fetch-browser-stack.md
python3 -c "import json; d=json.load(open('data/registry/registry.json')); print('registry version:', d.get('version')); print('browser-stack entry:', [e['entry_point'] for e in d['entries'] if e['tool_id']=='cap-fetch-browser-stack'])"
grep -c "headless" data/index/L3/cap-fetch-browser-stack.md
# 修包装脚本：/usr/local/bin 版加 --headless=new；种子版换成运行时定位版
sed -i 's|exec /root/.cache/ms-playwright/chromium-1248/chrome-linux64/chrome --no-sandbox|exec /root/.cache/ms-playwright/chromium-1248/chrome-linux64/chrome --headless=new --no-sandbox|' /usr/local/bin/chrome-nosandbox
cat /usr/local/bin/chrome-nosandbox
cat > tools/scripts/chrome-nosandbox << 'WR'
#!/bin/sh
# 007 部署版：chromium 在 playwright 默认缓存 ~/.cache/ms-playwright 下；无头模式。
CHROME=$(find "$HOME/.cache/ms-playwright" -name chrome -path "*chrome-linux*/chrome" 2>/dev/null | head -1)
[ -z "$CHROME" ] && { echo "chrome-nosandbox: no chromium found under ~/.cache/ms-playwright" >&2; exit 127; }
exec "$CHROME" --headless=new --no-sandbox --disable-dev-shm-usage --disable-gpu "$@"
WR
chmod +x tools/scripts/chrome-nosandbox
# 冒烟：无头渲染
echo "--- smoke chrome-nosandbox --dump-dom ---"
timeout 60 chrome-nosandbox --dump-dom https://example.com 2>/dev/null | head -c 300; echo
echo "--- smoke tools/scripts/chrome-nosandbox ---"
timeout 60 tools/scripts/chrome-nosandbox --dump-dom https://example.com 2>/dev/null | head -c 300; echo
# 停旧守护进程
OLDPID=$(pgrep -x super-agent || true)
echo "old daemon pid: $OLDPID"
[ -n "$OLDPID" ] && kill -TERM $OLDPID || true
for i in $(seq 1 15); do pgrep -x super-agent >/dev/null || break; sleep 1; done
pgrep -x super-agent && { echo "OLD DAEMON STILL ALIVE"; exit 1; } || echo "old daemon stopped"
# 换二进制并启动
cp super-agent super-agent.step2021c.bak
mv super-agent.new super-agent
chmod +x super-agent
md5sum super-agent
cd /root/super-agent-007 && nohup ./super-agent > /tmp/sa007-deploy4-20261118.log 2>&1 &
sleep 5
pgrep -x super-agent
grep -E "registry ready|planner ready|self-evolve loop started|gateway ready" /tmp/sa007-deploy4-20261118.log
(ss -ltnp 2>/dev/null || netstat -ltnp 2>/dev/null) | grep 6262 || echo "6262 ss check inconclusive"
RUN
B64=$(base64 -w0 /tmp/sa007-deploy3-run.sh)
PASS=$(awk '/45.140.88.10/{print $NF}' ~/.config/server-credentials/dogyun)
printf '%s\n' "$PASS" | ~/workspace/pi-mail/ssh_pi.sh "read P; export SSHPASS=\"\$P\"; echo $B64 | sshpass -e ssh -p 9701 -o StrictHostKeyChecking=accept-new root@45.140.88.10 'base64 -d | bash -s'"
echo "DEPLOY DONE"
