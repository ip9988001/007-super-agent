import sqlite3
c = sqlite3.connect("/root/super-agent-007/data/ledger.db")
print("inbox_queue 总任务数:", c.execute("SELECT COUNT(*) FROM inbox_queue").fetchone()[0])
print("近7天 distinct 任务数:", c.execute("SELECT COUNT(DISTINCT task_id) FROM inbox_queue WHERE ts >= datetime('now','-7 days')").fetchone()[0])
print("近30天 distinct 任务数:", c.execute("SELECT COUNT(DISTINCT task_id) FROM inbox_queue WHERE ts >= datetime('now','-30 days')").fetchone()[0])
print("近7天任务文本分组(前40字):")
for r in c.execute("SELECT substr(text,1,40), COUNT(*) FROM inbox_queue WHERE ts >= datetime('now','-7 days') GROUP BY substr(text,1,40) ORDER BY 2 DESC LIMIT 10"):
    print("  ", r)
print("execution_ledger 总行:", c.execute("SELECT COUNT(*) FROM execution_ledger").fetchone()[0])
print("execution_ledger 失败行:", c.execute("SELECT COUNT(*) FROM execution_ledger WHERE status='failure'").fetchone()[0])
print("advisor HALT 行:", c.execute("SELECT COUNT(*) FROM execution_ledger WHERE advisor_verdict='HALT'").fetchone()[0])
print("expense_ledger 总毫分:", c.execute("SELECT COALESCE(SUM(cost_cny_milli),0) FROM expense_ledger").fetchone()[0])
print("expense_ledger 按模型:")
for r in c.execute("SELECT model, COUNT(*), COALESCE(SUM(cost_cny_milli),0) FROM expense_ledger GROUP BY model"):
    print("  ", r)
print("selfevolve_proposals:", c.execute("SELECT COUNT(*) FROM selfevolve_proposals").fetchone()[0])
print("index_metrics 行:", c.execute("SELECT COUNT(*) FROM index_metrics").fetchone()[0])
