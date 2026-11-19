#!/bin/bash
# sa007-redeploy-step2022l.sh — 部署第 23 步锻造核心到测试机
# 用法：bash sa007-redeploy-step2022l.sh
# 步骤：传新二进制到二号 → 二号转测试机 → 停旧守护进程(step2022k) → 换二进制 → 启动 → 校验
set -e
BIN=/tmp/super-agent.new
[ -f "$BIN" ] || { echo "missing binary $BIN"; exit 1; }
md5sum "$BIN"
# 1) 传到二号中转
scp -i /home/hatch/.ssh/id_ed25519 -P 6260 -o BatchMode=yes -o ConnectTimeout=20 -o StrictHostKeyChecking=accept-new -o ProxyCommand="nc -X connect -x hatch-egress-proxy:3130 %h %p" "$BIN" root@100.117.8.35:/tmp/sa007-step2022l-super-agent
echo "scp-to-no2 OK"
# 2) 二号转测试机（在二号上跑 scp；scp 用 < /dev/null，密码走 SSHPASS）
PASS=$(awk '/45.140.88.10/{print $NF}' ~/.config/server-credentials/dogyun)
printf '%s\n' "$PASS" | ~/workspace/pi-mail/ssh_pi.sh 'read P; export SSHPASS="$P"; sshpass -e scp -P 9701 -o StrictHostKeyChecking=accept-new /tmp/sa007-step2022l-super-agent root@45.140.88.10:/root/super-agent-007/super-agent.new < /dev/null && echo "scp-to-test OK"'
# 3) 测试机上部署（base64 管道进 ssh，内层 ssh 后不加 < /dev/null）
cat > /tmp/sa007-deploy-step2022l-run.sh << 'RUN'
set -e
cd /root/super-agent-007
md5sum super-agent.new
# 停旧守护进程
OLDPID=$(pgrep -x super-agent || true)
echo "old daemon pid: $OLDPID"
[ -n "$OLDPID" ] && kill -TERM $OLDPID || true
for i in $(seq 1 15); do pgrep -x super-agent >/dev/null || break; sleep 1; done
pgrep -x super-agent && { echo "OLD DAEMON STILL ALIVE"; exit 1; } || echo "old daemon stopped"
# 换二进制并启动
cp super-agent super-agent.step2022l.bak
mv super-agent.new super-agent
chmod +x super-agent
md5sum super-agent
nohup ./super-agent > /tmp/sa007-deploy-step2022l-20261119.log 2>&1 &
sleep 6
pgrep -x super-agent
grep -E "registry ready|forge ready|self-evolve loop started|gateway ready" /tmp/sa007-deploy-step2022l-20261119.log
(ss -ltnp 2>/dev/null || netstat -ltnp 2>/dev/null) | grep 6262 || echo "6262 ss check inconclusive"
RUN
B64=$(base64 -w0 /tmp/sa007-deploy-step2022l-run.sh)
PASS=$(awk '/45.140.88.10/{print $NF}' ~/.config/server-credentials/dogyun)
printf '%s\n' "$PASS" | ~/workspace/pi-mail/ssh_pi.sh "read P; export SSHPASS=\"\$P\"; echo $B64 | sshpass -e ssh -p 9701 -o StrictHostKeyChecking=accept-new root@45.140.88.10 'base64 -d | bash -s'"
echo "DEPLOY DONE"
