#!/bin/bash
# 第 14 步阶段 2b（在测试机上跑）：重启守护进程（重放旧行）+ B-01/天气/B-03/B-08/B-06/B-07 实测。
# 守护进程跑完后留在运行状态，供阶段 2c（kill 测试 + 收尾）用。
set -u
D=/root/super-agent-007
cd $D
export PATH=$PATH:/usr/local/go/bin
export GOTOOLCHAIN=auto
export GOPATH=$HOME/gopath

echo "== pre: no daemon, port free =="
pkill -x super-agent 2>/dev/null || true
sleep 1
(ss -ltn 2>/dev/null | grep 6262 && echo "port 6262 BUSY") || echo "port 6262 free"

echo "== start daemon (fixed binary) =="
nohup ./super-agent > /tmp/sa007-step14-daemon2.log 2>&1 &
echo $! > /tmp/sa007-step14-daemon2.pid
sleep 5
grep -E "gateway ready|llm ready|advisor ready|backup loop|refine loop" /tmp/sa007-step14-daemon2.log
SESS=$(grep -o "session=sa-[^ ]*" /tmp/sa007-step14-daemon2.log | head -1 | cut -d= -f2)
echo "session=$SESS"

echo "== Phase A: replay of old rows (probe-ok RUNNING / probe-fail PENDING / ping PENDING) =="
python3 - << 'PY'
import sqlite3, time
DB='/root/super-agent-007/data/ledger.db'
def q(sql, args=()):
    c=sqlite3.connect(f'file:{DB}?mode=ro', uri=True, timeout=10)
    try: return c.execute(sql, args).fetchall()
    finally: c.close()
t0=time.time()
while time.time()-t0 < 30:
    rows=q("SELECT task_id, status, retry_count FROM inbox_queue WHERE task_id IN ('e2e14-probe-ok','e2e14-probe-fail','e2e14-ping') ORDER BY id")
    if len(rows)==3 and all(r[1]=='DONE' for r in rows): break
    time.sleep(1)
print("replay inbox rows:", q("SELECT task_id, status, retry_count FROM inbox_queue WHERE task_id LIKE 'e2e14-%' ORDER BY id"))
print("replay triage:", q("SELECT task_id, route, tier, tool_id FROM triage_metrics WHERE task_id LIKE 'e2e14-%' ORDER BY id"))
print("replay execution:", q("SELECT task_id, tool_id, command, exit_code, status, substr(stdout_tail,1,40) FROM execution_ledger WHERE task_id LIKE 'e2e14-%' ORDER BY id"))
PY

echo "== Phase B: B-01 / weather / B-03 fresh =="
python3 - << 'PY'
import json, sqlite3, time, urllib.request
DB='/root/super-agent-007/data/ledger.db'
BASE='http://127.0.0.1:6262/wake'
def post(task, text, seq=0):
    body=json.dumps({"task_id":task,"text":text,"source":"e2e14b","seq":seq}).encode()
    req=urllib.request.Request(BASE, data=body, headers={"Content-Type":"application/json"})
    t0=time.time()
    with urllib.request.urlopen(req, timeout=10) as r:
        resp=json.loads(r.read())
    return time.time()-t0, resp
def q(sql, args=()):
    c=sqlite3.connect(f'file:{DB}?mode=ro', uri=True, timeout=10)
    try: return c.execute(sql, args).fetchall()
    finally: c.close()
def wait(sql, args, timeout=30, want=1):
    t0=time.time()
    while time.time()-t0 < timeout:
        rows=q(sql, args)
        if len(rows) >= want: return rows, time.time()-t0
        time.sleep(0.5)
    return q(sql, args), time.time()-t0

dt, r = post("e2e14b-latency", "你是谁")
print("B-01 POST dt=%.2fs resp=%s" % (dt, r))
rows, el = wait("SELECT route, tier, tool_id FROM triage_metrics WHERE task_id=?", ("e2e14b-latency",), timeout=15)
print("B-01 triage rows=%s observed_after=%.2fs (limit 10s)" % (rows, el))

dt, r = post("e2e14b-weather", "今天天气怎么样")
rows, el = wait("SELECT route, tier, tool_id FROM triage_metrics WHERE task_id=?", ("e2e14b-weather",), timeout=20)
print("weather triage rows=%s observed_after=%.2fs" % (rows, el))
erows, _ = wait("SELECT task_id, model, cost_cny_milli FROM expense_ledger WHERE task_id=?", ("e2e14b-weather",), timeout=10)
print("weather expense rows=%s" % (erows,))

dt, r = post("e2e14b-probe-ok", "/e2e14probeok")
rows, el = wait("SELECT tool_id, exit_code, status, substr(stdout_tail,1,40) FROM execution_ledger WHERE task_id=?", ("e2e14b-probe-ok",), timeout=20)
print("probe-ok execution rows=%s observed_after=%.2fs" % (rows, el))

dt, r = post("e2e14b-probe-fail", "/e2e14probefail")
rows, el = wait("SELECT tool_id, command, exit_code, status, substr(err_head,1,80) FROM execution_ledger WHERE task_id=? AND command LIKE 'probe %'", ("e2e14b-probe-fail",), timeout=20)
print("probe-fail probe rows=%s observed_after=%.2fs" % (rows, el))
PY

echo "== Phase C: B-08 dual lane (long sleep 60 + short) =="
python3 - << 'PY'
import json, sqlite3, time, urllib.request
DB='/root/super-agent-007/data/ledger.db'
BASE='http://127.0.0.1:6262/wake'
def post(task, text):
    body=json.dumps({"task_id":task,"text":text,"source":"e2e14b"}).encode()
    req=urllib.request.Request(BASE, data=body, headers={"Content-Type":"application/json"})
    t0=time.time()
    with urllib.request.urlopen(req, timeout=10) as r:
        json.loads(r.read())
    return t0
def q(sql, args=()):
    c=sqlite3.connect(f'file:{DB}?mode=ro', uri=True, timeout=10)
    try: return c.execute(sql, args).fetchall()
    finally: c.close()
def wait(sql, args, timeout=60, want=1):
    t0=time.time()
    while time.time()-t0 < timeout:
        rows=q(sql, args)
        if len(rows) >= want: return rows, time.time()-t0
        time.sleep(0.5)
    return q(sql, args), time.time()-t0

t_long_post = post("e2e14b-long", "/e2e14long")
rows, el = wait("SELECT route, tier, tool_id, ts FROM triage_metrics WHERE task_id=?", ("e2e14b-long",), timeout=20)
print("long triage rows=%s observed_after=%.2fs (handler started)" % (rows, el))
time.sleep(5)
t_short_post = post("e2e14b-short", "你是谁")
rows, el = wait("SELECT route, tier, tool_id, ts FROM triage_metrics WHERE task_id=?", ("e2e14b-short",), timeout=15)
print("short triage rows=%s observed_after=%.2fs (must be < long completion)" % (rows, el))
lrows, el2 = wait("SELECT tool_id, exit_code, status, ts FROM execution_ledger WHERE task_id=?", ("e2e14b-long",), timeout=60)
print("long execution rows=%s observed_after=%.2fs" % (lrows, el2))
print("B-08 VERDICT: short triage at POST+%.1fs, long execution at POST+%.1fs -> %s" % (
    (t_short_post + el) - t_long_post, el2 + 5 + el, "SHORT NOT BLOCKED" if (t_short_post + el) - t_long_post < el2 + 5 + el else "BLOCKED?!"))
PY

echo "== Phase D: B-06 stale requeue (fake old RUNNING row) =="
python3 - << 'PY'
import sqlite3, time
from datetime import datetime, timedelta, timezone
DB='/root/super-agent-007/data/ledger.db'
now = datetime.now(timezone.utc)
old = (now - timedelta(hours=3)).strftime('%Y-%m-%dT%H:%M:%SZ')
c=sqlite3.connect(DB, timeout=10)
c.execute("INSERT INTO inbox_queue (ts, session_id, task_id, inbound_seq, source, text, status, retry_count, created_ts, updated_ts) VALUES (?,?,?,?,?,?,?,?,?,?)",
    (now.strftime('%Y-%m-%dT%H:%M:%SZ'), 'e2e14b-stale', 'e2e14b-stale', 99, 'e2e14', '你是谁', 'RUNNING', 0, old, old))
c.commit(); c.close()
print("inserted fake stale RUNNING row e2e14b-stale seq=99 updated_ts=%s" % old)
def q(sql, args=()):
    cc=sqlite3.connect(f'file:{DB}?mode=ro', uri=True, timeout=10)
    try: return cc.execute(sql, args).fetchall()
    finally: cc.close()
t0=time.time()
while time.time()-t0 < 90:
    rows=q("SELECT status, retry_count FROM inbox_queue WHERE task_id=?", ("e2e14b-stale",))
    if rows and rows[0][0]=='DONE': break
    time.sleep(1)
print("stale row after sweep: %s (waited %.1fs)" % (q("SELECT status, retry_count FROM inbox_queue WHERE task_id=?", ("e2e14b-stale",)), time.time()-t0))
print("stale triage: %s" % (q("SELECT task_id, route FROM triage_metrics WHERE task_id=?", ("e2e14b-stale",)),))
print("stale requeue ledger rows: %s" % (q("SELECT tool_id, command, status FROM execution_ledger WHERE tool_id='watchdog-requeue' ORDER BY id DESC LIMIT 3"),))
PY

echo "== Phase E: B-07 backup =="
go test -run TestBackupLedger -count=1 ./internal/watchdog/ 2>&1 | tail -3
ls -la /root/super-agent-007/data/backups/ 2>/dev/null || echo "no backups dir yet (expected: window is 03:30 Beijing)"
grep -c "backup loop started" /tmp/sa007-step14-daemon2.log
python3 - << 'PY'
import sqlite3, shutil, os
shutil.copy('/root/super-agent-007/data/ledger.db', '/tmp/sa007-step14-ledger-copy.db')
for suf in ('-wal', '-shm'):
    p='/root/super-agent-007/data/ledger.db'+suf
    if os.path.exists(p): shutil.copy(p, '/tmp/sa007-step14-ledger-copy.db'+suf)
c=sqlite3.connect('/tmp/sa007-step14-ledger-copy.db', timeout=10)
c.execute("VACUUM INTO '/tmp/sa007-step14-ledger-backup.db'")
c.close()
c2=sqlite3.connect('/tmp/sa007-step14-ledger-backup.db', timeout=10)
print("backup copy tables:", c2.execute("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name").fetchall())
print("backup copy inbox rows:", c2.execute("SELECT count(*) FROM inbox_queue").fetchone())
c2.close()
PY
echo "== daemon2 log tail =="
tail -8 /tmp/sa007-step14-daemon2.log
echo "== stage2b done (daemon left running for stage2c) =="
