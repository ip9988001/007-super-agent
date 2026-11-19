set -e
PID=$(pgrep -x super-agent | head -1 || true)
echo "old daemon PID: $PID"
if [ -n "$PID" ]; then kill -TERM "$PID"; fi
for i in $(seq 1 10); do pgrep -x super-agent >/dev/null || break; sleep 1; done
pgrep -x super-agent && echo "STILL RUNNING" || echo "daemon stopped"
ss -ltn | grep 6262 || echo "6262 free"
cd /root/super-agent-007
md5sum super-agent.new
cp super-agent super-agent.step2021b.bak
mv super-agent.new super-agent
chmod +x super-agent
md5sum super-agent
nohup ./super-agent > /tmp/sa007-deploy3-20261118.log 2>&1 &
echo "new daemon PID: $!"
sleep 8
grep -E "planner ready|self-evolve loop started|llm ready|advisor ready|gateway ready|panic" /tmp/sa007-deploy3-20261118.log
ss -ltn | grep 6262 || echo "6262 NOT listening"
