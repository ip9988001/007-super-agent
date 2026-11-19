#!/usr/bin/env python3
"""验证三层懒加载：模型应 expand fetch -> http -> 调 curl-jq 抓 example.com 与 example.org，比标题。"""
import json, subprocess, sys, time, os

TASK_ID = "agent-verify-lazyload-20261118a"
TASK_TEXT = "抓取 example.com 和 example.org 两个页面，比较它们的 <title> 是否相同，把结论写进工作目录的 report.md，并用一句话回答我。"
pw = subprocess.run(["awk", "/45.140.88.10/{print $NF}", os.path.expanduser("~/.config/server-credentials/dogyun")], capture_output=True, text=True).stdout.strip()
assert pw, "no dogyun password for 45.140.88.10"

def ssh(cmd):
    b64 = __import__("base64").b64encode(cmd.encode()).decode()
    full = f"printf '%s\\n' '{pw}' | ~/workspace/pi-mail/ssh_pi.sh \"read P; export SSHPASS=\\\"$P\\\"; echo {b64} | sshpass -e ssh -p 9701 -o StrictHostKeyChecking=accept-new root@45.140.88.10 'base64 -d | bash -s'\""
    r = subprocess.run(full, shell=True, capture_output=True, text=True, timeout=120)
    return r.stdout + r.stderr

# 1. 发任务
env = json.dumps({"task_id": TASK_ID, "text": TASK_TEXT, "source": "muse-test", "seq": 1})
post = f"""curl -s -m 15 -X POST http://127.0.0.1:6262/wake -H 'Content-Type: application/json' -d '{env}'"""
print("POST:", ssh(post).strip()[:300])

# 2. 轮询 inbox 状态，最多 12 分钟
deadline = time.time() + 720
status = "?"
while time.time() < deadline:
    time.sleep(20)
    q = ssh(f"""python3 -c "
import sqlite3
c=sqlite3.connect('file:/root/super-agent-007/data/ledger.db?mode=ro', uri=True, timeout=10)
r=c.execute(\"select status from inbox_queue where task_id='{TASK_ID}'\").fetchall()
print(r)
\"""")
    print("poll:", q.strip()[:200])
    if "DONE" in q:
        status = "DONE"
        break
print("final status:", status)

# 3. 拉结果：reply.md、report.md、费用、步骤数
out = ssh(f"""cd /root/super-agent-007
echo '== reply.md =='; cat data/artifacts/{TASK_ID}/reply.md 2>/dev/null | head -30
echo '== report.md =='; ls data/workspace/{TASK_ID}/ 2>/dev/null; cat data/workspace/{TASK_ID}/report.md 2>/dev/null | head -20
echo '== steps =='; ls data/artifacts/{TASK_ID}/ 2>/dev/null
echo '== cost =='; python3 -c "
import sqlite3
c=sqlite3.connect('file:/root/super-agent-007/data/ledger.db?mode=ro', uri=True, timeout=10)
print(c.execute(\"select sum(cost_cny_milli) from expense_ledger where task_id='{TASK_ID}'\").fetchall())
print(c.execute(\"select model, count(*) from expense_ledger where task_id='{TASK_ID}' group by model\").fetchall())
"
""")
print(out[:4000])
