#!/bin/bash
# 第 14 步阶段 2c（在测试机上跑）：B-02 kill 重启重放 + 停干净 + 注册表恢复 73 + 最终核验。
set -u
D=/root/super-agent-007
cd $D
export PATH=$PATH:/usr/local/go/bin
export GOTOOLCHAIN=auto
export GOPATH=$HOME/gopath

echo "== Phase F: B-02 kill test (POST 后 1 秒内 SIGKILL，重启后只处理一次） =="
python3 - << 'PY'
import json, os, signal, sqlite3, time, urllib.request
DB='/root/super-agent-007/data/ledger.db'
BASE='http://127.0.0.1:6262/wake'
pid = int(open('/tmp/sa007-step14-daemon2.pid').read().strip())
body=json.dumps({"task_id":"e2e14b-kill","text":"你是谁","source":"e2e14b"}).encode()
req=urllib.request.Request(BASE, data=body, headers={"Content-Type":"application/json"})
t0=time.time()
with urllib.request.urlopen(req, timeout=10) as r:
    resp=json.loads(r.read())
print("POST dt=%.3fs resp=%s; killing pid %d NOW" % (time.time()-t0, resp, pid))
os.kill(pid, signal.SIGKILL)
print("SIGKILL sent at POST+%.3fs (debounce is 3s, handler cannot have started)" % (time.time()-t0))
def q(sql, args=()):
    c=sqlite3.connect(f'file:{DB}?mode=ro', uri=True, timeout=10)
    try: return c.execute(sql, args).fetchall()
    finally: c.close()
print("triage rows for e2e14b-kill right after kill:", q("SELECT count(*) FROM triage_metrics WHERE task_id=?", ("e2e14b-kill",)))
print("inbox row for e2e14b-kill:", q("SELECT status, retry_count FROM inbox_queue WHERE task_id=?", ("e2e14b-kill",)))
PY
sleep 2
ps aux | grep "[s]uper-agent" || echo "daemon dead after SIGKILL"
(ss -ltn 2>/dev/null | grep 6262 && echo "port 6262 BUSY") || echo "port 6262 free"

echo "== restart daemon (replay) =="
nohup ./super-agent > /tmp/sa007-step14-daemon3.log 2>&1 &
echo $! > /tmp/sa007-step14-daemon3.pid
sleep 5
grep -E "gateway ready|replay" /tmp/sa007-step14-daemon3.log | head -5
python3 - << 'PY'
import sqlite3, time
DB='/root/super-agent-007/data/ledger.db'
def q(sql, args=()):
    c=sqlite3.connect(f'file:{DB}?mode=ro', uri=True, timeout=10)
    try: return c.execute(sql, args).fetchall()
    finally: c.close()
t0=time.time()
while time.time()-t0 < 30:
    rows=q("SELECT status, retry_count FROM inbox_queue WHERE task_id=?", ("e2e14b-kill",))
    if rows and rows[0][0]=='DONE': break
    time.sleep(0.5)
print("kill-test inbox row after restart:", q("SELECT status, retry_count FROM inbox_queue WHERE task_id=?", ("e2e14b-kill",)), "(waited %.1fs)" % (time.time()-t0))
print("kill-test triage count (must be exactly 1):", q("SELECT count(*) FROM triage_metrics WHERE task_id=?", ("e2e14b-kill",)))
print("kill-test triage row:", q("SELECT route, tier FROM triage_metrics WHERE task_id=?", ("e2e14b-kill",)))
PY

echo "== Phase G: stop daemon cleanly =="
kill $(cat /tmp/sa007-step14-daemon3.pid)
sleep 3
ps aux | grep "[s]uper-agent" || echo "no super-agent process left"
(ss -ltn 2>/dev/null | grep 6262 && echo "port 6262 BUSY") || echo "port 6262 free"
ps aux | grep "[s]leep 60" || echo "no leftover sleep processes"
tail -5 /tmp/sa007-step14-daemon3.log

echo "== restore registry 73 =="
python3 - << 'PY'
import json
p='/root/super-agent-007/data/registry/registry.json'
d=json.load(open(p))
before = len(d['entries'])
d['entries'] = [e for e in d['entries'] if not e['tool_id'].startswith('cap-e2e14-')]
d['total'] = len(d['entries'])
json.dump(d, open(p,'w'), ensure_ascii=False, indent=1)
print('registry entries: %d -> %d' % (before, d['total']))
PY
python3 - << 'PY'
p='/root/super-agent-007/data/index/L2/memory.md'
lines = open(p).read().splitlines()
kept = [l for l in lines if 'cap-e2e14-' not in l]
open(p,'w').write('\n'.join(kept)+'\n')
print('L2 memory.md lines: %d -> %d' % (len(lines), len(kept)))
PY
rm -f /root/super-agent-007/data/index/L3/cap-e2e14-*.md
ls /root/super-agent-007/data/index/L3/cap-e2e14-*.md 2>/dev/null || echo "temp L3 files removed"
DATA_DIR=/root/super-agent-007/data go run ./cmd/registry-verify

echo "== final ledger checks =="
python3 - << 'PY'
import sqlite3
DB='/root/super-agent-007/data/ledger.db'
c=sqlite3.connect(f'file:{DB}?mode=ro', uri=True, timeout=10)
print("inbox_queue exists:", c.execute("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='inbox_queue'").fetchone())
print("execution_ledger columns:", [r[1] for r in c.execute("PRAGMA table_info(execution_ledger)")])
print("inbox status counts:", c.execute("SELECT status, count(*) FROM inbox_queue GROUP BY status").fetchall())
print("inbox dedup index:", c.execute("SELECT name FROM sqlite_master WHERE type='index' AND name='idx_inbox_dedup'").fetchall())
print("expense rows with empty task_id (old step13 rows may be empty):", c.execute("SELECT count(*) FROM expense_ledger WHERE task_id='' OR task_id IS NULL").fetchone())
print("e2e14* expense rows (must all have task_id):", c.execute("SELECT task_id, model, cost_cny_milli FROM expense_ledger WHERE task_id LIKE 'e2e14%' ORDER BY id").fetchall())
print("total cost e2e14b (milli CNY):", c.execute("SELECT sum(cost_cny_milli) FROM expense_ledger WHERE task_id LIKE 'e2e14%'").fetchone())
print("triage rows e2e14*:", c.execute("SELECT count(*) FROM triage_metrics WHERE task_id LIKE 'e2e14%'").fetchone())
PY

echo "== cleanup /tmp on test server =="
rm -f /tmp/sa007-step14-src.tar.gz /tmp/sa007-step14-stage2a.sh /tmp/sa007-step14-stage2b.sh /tmp/sa007-step14-diag.sh /tmp/sa007-step14-ledger-copy.db* /tmp/sa007-step14-ledger-backup.db
ls /tmp/sa007-step14-* 2>/dev/null || echo "all /tmp/sa007-step14-* cleaned (daemon logs kept? check)"
echo "== stage2c done =="
