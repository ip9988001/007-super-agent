import json, sqlite3, time, urllib.request, os
DB='/root/super-agent-007/data/ledger.db'
BASE='http://127.0.0.1:6262/wake'
TASK='agent-verify-file-20261118a'
TEXT='在测试机 /root/super-agent-007/data/workspace/agent-verify-file-20261118a/ 下创建一个 verify.txt，写入 hello 007 三个字，然后读出来确认内容无误，再告诉我文件路径和内容'
def q(sql, args=()):
    c=sqlite3.connect('file:%s?mode=ro' % DB, uri=True, timeout=10)
    try: return c.execute(sql, args).fetchall()
    finally: c.close()
body=json.dumps({"task_id":TASK,"text":TEXT,"source":"muse-test","seq":1}, ensure_ascii=False).encode()
req=urllib.request.Request(BASE, data=body, headers={"Content-Type":"application/json"})
t0=time.time()
with urllib.request.urlopen(req, timeout=10) as r:
    print("receipt:", json.loads(r.read()), "dt=%.2fs" % (time.time()-t0), flush=True)
t0=time.time()
while time.time()-t0 < 600:
    st=q("select status from inbox_queue where task_id=? order by rowid desc limit 1", (TASK,))
    if st and st[0][0] in ('DONE','FAILED'):
        print("inbox status:", st[0][0], "at %.0fs" % (time.time()-t0), flush=True)
        break
    time.sleep(10)
else:
    print("POLL TIMEOUT after 600s", flush=True)
print("triage:", q("select route, tier, tool_id from triage_metrics where task_id=?", (TASK,)), flush=True)
print("execution:", q("select tool_id, exit_code, status, substr(stdout_tail,1,100) from execution_ledger where task_id=?", (TASK,)), flush=True)
print("expense:", q("select model, prompt_tokens, cache_hit_tokens, cache_miss_tokens, completion_tokens, cost_cny_milli from expense_ledger where task_id=?", (TASK,)), flush=True)
print("expense total milli:", q("select sum(cost_cny_milli) from expense_ledger where task_id=?", (TASK,)), flush=True)
art='/root/super-agent-007/data/artifacts/'+TASK
print("artifacts:", sorted(os.listdir(art)) if os.path.isdir(art) else "MISSING", flush=True)
if os.path.isdir(art) and os.path.exists(os.path.join(art,'reply.md')):
    print("---- reply.md ----")
    print(open(os.path.join(art,'reply.md'), encoding='utf-8', errors='replace').read(), flush=True)
# 直接核对文件是否真的建了、内容对不对
target='/root/super-agent-007/data/workspace/agent-verify-file-20261118a/verify.txt'
print("target file exists:", os.path.exists(target), flush=True)
if os.path.exists(target):
    print("target content:", repr(open(target, encoding='utf-8', errors='replace').read()), flush=True)
