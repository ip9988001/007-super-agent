import sqlite3, os, json
DB='/root/super-agent-007/data/ledger.db'
TASK='ping0-test-20261118c'
def q(sql, args=()):
    c=sqlite3.connect('file:%s?mode=ro' % DB, uri=True, timeout=10)
    try: return c.execute(sql, args).fetchall()
    finally: c.close()
print("== execution full ==")
for r in q("select tool_id, exit_code, status, advisor_verdict, advisor_retry_count, substr(err_head,1,200), substr(advisor_note,1,150), substr(command,1,150) from execution_ledger where task_id=?", (TASK,)):
    print(r)
print("== plan steps (orig 14) ==")
pj=q("select plan_json from plan_runs where task_id=? and plan_id not like '%-r1'", (TASK,))
if pj:
    for s in json.loads(pj[0][0]):
        print(s.get('step_no'), s.get('tool_id'), '| depends:', s.get('depends_on'), '| inputs:', str(s.get('inputs'))[:100])
print("== replan steps (r1, 10) ==")
pj=q("select plan_json from plan_runs where task_id=? and plan_id like '%-r1'", (TASK,))
if pj:
    for s in json.loads(pj[0][0]):
        print(s.get('step_no'), s.get('tool_id'), '| depends:', s.get('depends_on'), '| inputs:', str(s.get('inputs'))[:100])
print("== reply.md ==")
p='/root/super-agent-007/data/artifacts/'+TASK+'/reply.md'
print(open(p, encoding='utf-8', errors='replace').read()[:2500] if os.path.exists(p) else "MISSING")
print("== workspace files ==")
w='/root/super-agent-007/data/workspace/'+TASK
print(sorted(os.listdir(w)) if os.path.isdir(w) else "MISSING")
print("== daemon log (task lines) ==")
os.system("grep -E 'ping0-test-20261118c' /tmp/sa007-deploy3-20261118.log | tail -8")
