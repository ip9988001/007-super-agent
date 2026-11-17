package ledger

import (
	"database/sql"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

// sqlOpen 是测试用的裸连接（绕过 ledger.Open 的建表/迁移，专用于造旧库）。
func sqlOpen(path string) (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+path)
}

func TestLedgerRoundTrip(t *testing.T) {
	l, err := Open(t.TempDir() + "/ledger.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer Close(l)

	// 五张表都已建好（建表失败会在 Open 里直接报错）。
	for _, table := range []string{"triage_metrics", "index_metrics", "execution_ledger", "expense_ledger", "queue_watermarks"} {
		var name string
		if err := l.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name); err != nil {
			t.Fatalf("table %s missing: %v", table, err)
		}
	}
	// WAL 已生效。
	var mode string
	if err := l.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal: %v", mode, err)
	}

	if _, err := l.RecordTriage(TriageRecord{
		SessionID: "s1", TaskID: "t1", Route: "TOOL", Tier: 1,
		ToolID: "cap-code-codegraph", TokensIn: 100, TokensOut: 50, LatencyMs: 120,
	}); err != nil {
		t.Fatalf("RecordTriage: %v", err)
	}

	// 中文截断：600 字以上的错误头和便签必须按字符截断，不能劈开汉字。
	longCN := strings.Repeat("监工叫停测试字段未覆盖", 100) // 1200 字
	if _, err := l.RecordExecution(ExecutionRecord{
		SessionID: "s1", TaskID: "t1", ToolID: "cap-code-codegraph",
		Command: "codegraph query foo", ExitCode: 1,
		StdoutTail: strings.Repeat("line\n", 30), // 只留末尾 20 行
		ErrHead:    longCN,
		DurationMs: 300, AdvisorVerdict: "HALT", AdvisorRetryCount: 1,
		AdvisorNote: longCN,
	}); err != nil {
		t.Fatalf("RecordExecution: %v", err)
	}
	rows, err := l.ListExecutions("t1", 10)
	if err != nil {
		t.Fatalf("ListExecutions: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	e := rows[0]
	if e.Status != "failure" || e.AdvisorVerdict != "HALT" || e.AdvisorRetryCount != 1 {
		t.Fatalf("row mismatch: %+v", e)
	}
	if got := len([]rune(e.ErrHead)); got != 600 {
		t.Fatalf("ErrHead = %d runes, want 600", got)
	}
	if got := len([]rune(e.AdvisorNote)); got != 600 {
		t.Fatalf("AdvisorNote = %d runes, want 600", got)
	}
	if got := len(strings.Split(e.StdoutTail, "\n")); got != 20 {
		t.Fatalf("StdoutTail = %d lines, want 20", got)
	}
	if !utf8.ValidString(e.ErrHead) || !utf8.ValidString(e.AdvisorNote) {
		t.Fatalf("truncation split a multi-byte rune")
	}

	if _, err := l.RecordExpense(ExpenseRecord{
		SessionID: "s1", TaskID: "t1", Model: "deepseek-flash",
		PromptTokens: 2000, CacheHitTokens: 1500, CacheMissTokens: 500,
		CompletionTokens: 300, CostCNYMilli: 12,
	}); err != nil {
		t.Fatalf("RecordExpense: %v", err)
	}
	sum, err := l.SummarizeExpense("s1")
	if err != nil {
		t.Fatalf("SummarizeExpense: %v", err)
	}
	if sum.Calls != 1 || sum.PromptTokens != 2000 || sum.CacheHitTokens != 1500 ||
		sum.CacheMissTokens != 500 || sum.CompletionTokens != 300 || sum.CostCNYMilli != 12 {
		t.Fatalf("summary mismatch: %+v", sum)
	}
}

// 并发写 20 路：串行化 Mutex + 单连接下不许出现 SQLITE_BUSY。
func TestLedgerConcurrentWrites(t *testing.T) {
	l, err := Open(t.TempDir() + "/ledger.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer Close(l)
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := l.RecordExecution(ExecutionRecord{
				SessionID: "s1", TaskID: "t1", Command: "echo hi",
				ExitCode: 0, Status: "success",
			})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent RecordExecution: %v", err)
		}
	}
	rows, err := l.ListExecutions("t1", 100)
	if err != nil {
		t.Fatalf("ListExecutions: %v", err)
	}
	if len(rows) != 20 {
		t.Fatalf("got %d rows, want 20", len(rows))
	}
}

// TestInboxEnqueueDedup 是 B-02 的去重：同 (task_id, seq) 且 seq>0 时
// 第二次入队不建新行；seq==0 时每条都是独立投递。
func TestInboxEnqueueDedup(t *testing.T) {
	l, err := Open(t.TempDir() + "/ledger.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer Close(l)

	id1, ok, err := l.EnqueueInbox(InboxRecord{SessionID: "s", TaskID: "t1", Seq: 5, Text: "first"})
	if err != nil || !ok || id1 == 0 {
		t.Fatalf("first enqueue: id=%d ok=%v err=%v", id1, ok, err)
	}
	_, ok, err = l.EnqueueInbox(InboxRecord{SessionID: "s", TaskID: "t1", Seq: 5, Text: "dup"})
	if err != nil || ok {
		t.Fatalf("dup enqueue: ok=%v err=%v, want inserted=false", ok, err)
	}
	_, ok, err = l.EnqueueInbox(InboxRecord{SessionID: "s", TaskID: "t1", Seq: 6, Text: "next seq"})
	if err != nil || !ok {
		t.Fatalf("seq 6 enqueue: ok=%v err=%v", ok, err)
	}
	_, ok, err = l.EnqueueInbox(InboxRecord{SessionID: "s", TaskID: "t1", Seq: 0, Text: "no seq 1"})
	if err != nil || !ok {
		t.Fatalf("seq 0 first: ok=%v err=%v", ok, err)
	}
	_, ok, err = l.EnqueueInbox(InboxRecord{SessionID: "s", TaskID: "t1", Seq: 0, Text: "no seq 2"})
	if err != nil || !ok {
		t.Fatalf("seq 0 second: ok=%v err=%v, want inserted=true", ok, err)
	}
	rows, err := l.ListInboxByStatus(InboxPending)
	if err != nil || len(rows) != 4 {
		t.Fatalf("pending rows = %d, err = %v, want 4", len(rows), err)
	}
}

// TestInboxTransitions 是 B-02/B-06 的状态机：PENDING→RUNNING→DONE，
// 重排只对 retry_count==0 的 RUNNING 行生效一次。
func TestInboxTransitions(t *testing.T) {
	l, err := Open(t.TempDir() + "/ledger.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer Close(l)

	id, _, err := l.EnqueueInbox(InboxRecord{SessionID: "s", TaskID: "t1", Seq: 1, Text: "x"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := l.MarkInboxDone(id); err == nil {
		t.Fatal("PENDING→DONE should fail")
	}
	if err := l.MarkInboxRunning(id); err != nil {
		t.Fatalf("mark RUNNING: %v", err)
	}
	if err := l.MarkInboxRunning(id); err == nil {
		t.Fatal("RUNNING→RUNNING should fail")
	}
	if err := l.RequeueInbox(id); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	rows, err := l.ListInboxByStatus(InboxPending)
	if err != nil || len(rows) != 1 || rows[0].RetryCount != 1 {
		t.Fatalf("after requeue: rows=%+v err=%v, want one PENDING retry=1", rows, err)
	}
	if err := l.RequeueInbox(id); err == nil {
		t.Fatal("second requeue should fail (retry_count=1)")
	}
	if err := l.MarkInboxRunning(id); err != nil {
		t.Fatalf("mark RUNNING again: %v", err)
	}
	if err := l.MarkInboxDone(id); err != nil {
		t.Fatalf("mark DONE: %v", err)
	}
	if err := l.MarkInboxDone(id); err == nil {
		t.Fatal("DONE→DONE should fail")
	}
}

// TestStageRoundTrip 是 B-06 的 Stage 落盘：记两个 Stage，
// LastStage 返回最大的那一个；重跑从下一 Stage 接续。
func TestStageRoundTrip(t *testing.T) {
	l, err := Open(t.TempDir() + "/ledger.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer Close(l)

	if _, _, found, err := l.LastStage("t1", "tool-a"); err != nil || found {
		t.Fatalf("LastStage empty: found=%v err=%v", found, err)
	}
	if _, err := l.RecordStage(StageRecord{SessionID: "s", TaskID: "t1", ToolID: "tool-a", StageNo: 1, ArtifactPath: "/tmp/s1.out"}); err != nil {
		t.Fatalf("RecordStage 1: %v", err)
	}
	if _, err := l.RecordStage(StageRecord{SessionID: "s", TaskID: "t1", ToolID: "tool-a", StageNo: 2, ArtifactPath: "/tmp/s2.out", Command: "run stage 2"}); err != nil {
		t.Fatalf("RecordStage 2: %v", err)
	}
	n, p, found, err := l.LastStage("t1", "tool-a")
	if err != nil || !found || n != 2 || p != "/tmp/s2.out" {
		t.Fatalf("LastStage = (%d, %q, %v), err = %v, want (2, /tmp/s2.out, true)", n, p, found, err)
	}
	rows, err := l.ListExecutions("t1", 10)
	if err != nil {
		t.Fatalf("ListExecutions: %v", err)
	}
	stages := 0
	for _, r := range rows {
		if r.StageNo > 0 {
			stages++
			if r.ArtifactPath == "" {
				t.Fatalf("stage row missing artifact_path: %+v", r)
			}
		}
	}
	if stages != 2 {
		t.Fatalf("stage rows = %d, want 2", stages)
	}
}

// TestMigrateOldSchema 是 B-06/B-02 的迁移：用第 13 步之前的旧表结构
// （五张表，无 inbox_queue、无 stage 列）建库，Open 必须自动补齐，
// 否则测试机上已有的 ledger.db 升级会失败。
func TestMigrateOldSchema(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/ledger.db"
	db, err := sqlOpen(path)
	if err != nil {
		t.Fatalf("sql open: %v", err)
	}
	oldSchema := `
CREATE TABLE triage_metrics (id INTEGER PRIMARY KEY AUTOINCREMENT, ts TEXT NOT NULL, session_id TEXT NOT NULL, task_id TEXT NOT NULL, route TEXT NOT NULL, tier INTEGER NOT NULL, tool_id TEXT NOT NULL DEFAULT '', tokens_in INTEGER NOT NULL DEFAULT 0, tokens_out INTEGER NOT NULL DEFAULT 0, latency_ms INTEGER NOT NULL DEFAULT 0);
CREATE TABLE index_metrics (id INTEGER PRIMARY KEY AUTOINCREMENT, ts TEXT NOT NULL, session_id TEXT NOT NULL, task_id TEXT NOT NULL, level TEXT NOT NULL, tool_id TEXT NOT NULL DEFAULT '', hit INTEGER NOT NULL DEFAULT 0, tokens_injected INTEGER NOT NULL DEFAULT 0, tokens_saved_vs_full INTEGER NOT NULL DEFAULT 0);
CREATE TABLE execution_ledger (id INTEGER PRIMARY KEY AUTOINCREMENT, ts TEXT NOT NULL, session_id TEXT NOT NULL, task_id TEXT NOT NULL, tool_id TEXT NOT NULL DEFAULT '', command TEXT NOT NULL, exit_code INTEGER NOT NULL, stdout_tail TEXT NOT NULL DEFAULT '', err_head TEXT NOT NULL DEFAULT '', duration_ms INTEGER NOT NULL DEFAULT 0, status TEXT NOT NULL, advisor_verdict TEXT NOT NULL DEFAULT 'NONE', advisor_retry_count INTEGER NOT NULL DEFAULT 0, advisor_note TEXT NOT NULL DEFAULT '');
CREATE TABLE expense_ledger (id INTEGER PRIMARY KEY AUTOINCREMENT, ts TEXT NOT NULL, session_id TEXT NOT NULL, task_id TEXT NOT NULL, model TEXT NOT NULL, prompt_tokens INTEGER NOT NULL DEFAULT 0, cache_hit_tokens INTEGER NOT NULL DEFAULT 0, cache_miss_tokens INTEGER NOT NULL DEFAULT 0, completion_tokens INTEGER NOT NULL DEFAULT 0, cost_cny_milli INTEGER NOT NULL DEFAULT 0);
CREATE TABLE queue_watermarks (id INTEGER PRIMARY KEY AUTOINCREMENT, ts TEXT NOT NULL, box TEXT NOT NULL, seen_seq INTEGER NOT NULL, acked_seq INTEGER NOT NULL, oldest_unacked_age_s INTEGER NOT NULL DEFAULT 0, unacked_alert INTEGER NOT NULL DEFAULT 0);
INSERT INTO execution_ledger (ts, session_id, task_id, tool_id, command, exit_code, status) VALUES ('2026-11-17T00:00:00Z', 's-old', 't-old', 'tool-old', 'echo hi', 0, 'success');
`
	if _, err := db.Exec(oldSchema); err != nil {
		db.Close()
		t.Fatalf("build old schema: %v", err)
	}
	db.Close()

	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open old db: %v", err)
	}
	defer Close(l)
	// 旧行还在，新列已补齐，Stage 能写。
	rows, err := l.ListExecutions("t-old", 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("old rows = %d, err = %v, want 1", len(rows), err)
	}
	if _, err := l.RecordStage(StageRecord{SessionID: "s", TaskID: "t-old", ToolID: "tool-old", StageNo: 1}); err != nil {
		t.Fatalf("RecordStage on migrated db: %v", err)
	}
	if _, _, err := l.EnqueueInbox(InboxRecord{SessionID: "s", TaskID: "t-new", Text: "hi"}); err != nil {
		t.Fatalf("EnqueueInbox on migrated db: %v", err)
	}
}
