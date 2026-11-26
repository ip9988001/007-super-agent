package ledger

import (
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"
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

// TestInboxDeadlineAndTimeout 是线 A 的 deadline 与看门狗置位：
// RUNNING 行写 deadline，未超期不算超期，超期后看门狗可标 TIMEOUT，
// 且 TIMEOUT 之后不能再改成 DONE/FAILED。
func TestInboxDeadlineAndTimeout(t *testing.T) {
	l, err := Open(t.TempDir() + "/ledger.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer Close(l)

	id, _, err := l.EnqueueInbox(InboxRecord{SessionID: "s", TaskID: "t-dl", Text: "x"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// 未取走（PENDING）时写 deadline 应失败。
	if err := l.SetInboxDeadline(id, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("PENDING 行写 deadline 应失败")
	}
	if err := l.MarkInboxRunning(id); err != nil {
		t.Fatalf("mark RUNNING: %v", err)
	}
	// deadline 设在未来：未超期。
	if err := l.SetInboxDeadline(id, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	overdue, err := l.ListOverdueInbox(time.Now(), time.Hour)
	if err != nil || len(overdue) != 0 {
		t.Fatalf("未超期却列出 %d 行，err=%v", len(overdue), err)
	}
	// deadline 拨到过去：超期。
	if err := l.SetInboxDeadline(id, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("set past deadline: %v", err)
	}
	overdue, err = l.ListOverdueInbox(time.Now(), time.Hour)
	if err != nil || len(overdue) != 1 || overdue[0].ID != id {
		t.Fatalf("超期行 = %+v, err=%v, want 1 行", overdue, err)
	}
	if overdue[0].Deadline == "" {
		t.Fatal("Deadline 字段应回读出值")
	}
	if err := l.MarkInboxTimeout(id); err != nil {
		t.Fatalf("mark TIMEOUT: %v", err)
	}
	if err := l.MarkInboxDone(id); err == nil {
		t.Fatal("TIMEOUT→DONE 应失败")
	}
	if err := l.MarkInboxFailed(id); err == nil {
		t.Fatal("TIMEOUT→FAILED 应失败")
	}
	rows, err := l.ListInboxByStatus(InboxTimeout)
	if err != nil || len(rows) != 1 || rows[0].TaskID != "t-dl" {
		t.Fatalf("TIMEOUT rows = %+v, err=%v", rows, err)
	}
}

// TestInboxRejected 是线 A 的前置校验拒绝：RUNNING 行可标 REJECTED，
// 之后不能再改成 DONE/FAILED；PENDING 行不能直接标 REJECTED。
func TestInboxRejected(t *testing.T) {
	l, err := Open(t.TempDir() + "/ledger.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer Close(l)

	id, _, err := l.EnqueueInbox(InboxRecord{SessionID: "s", TaskID: "t-rej", Text: "x"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := l.MarkInboxRejected(id); err == nil {
		t.Fatal("PENDING→REJECTED 应失败")
	}
	if err := l.MarkInboxRunning(id); err != nil {
		t.Fatalf("mark RUNNING: %v", err)
	}
	if err := l.MarkInboxRejected(id); err != nil {
		t.Fatalf("mark REJECTED: %v", err)
	}
	if err := l.MarkInboxDone(id); err == nil {
		t.Fatal("REJECTED→DONE 应失败")
	}
	rows, err := l.ListInboxByStatus(InboxRejected)
	if err != nil || len(rows) != 1 || rows[0].TaskID != "t-rej" {
		t.Fatalf("REJECTED rows = %+v, err=%v", rows, err)
	}
}

// TestOverdueLegacyRow 是线 A 的旧行兜底：没有 deadline 的旧 RUNNING 行，
// updated_ts 早于 now-legacyAge 即算超期（看门狗仍能兜底）。
func TestOverdueLegacyRow(t *testing.T) {
	l, err := Open(t.TempDir() + "/ledger.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer Close(l)

	id, _, err := l.EnqueueInbox(InboxRecord{SessionID: "s", TaskID: "t-old", Text: "x"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := l.MarkInboxRunning(id); err != nil {
		t.Fatalf("mark RUNNING: %v", err)
	}
	// 刚取走的旧行（无 deadline，updated_ts 是现在）：不算超期。
	if overdue, err := l.ListOverdueInbox(time.Now(), time.Hour); err != nil || len(overdue) != 0 {
		t.Fatalf("新行不应超期：%+v, err=%v", overdue, err)
	}
	// 把 updated_ts 拨到 3 小时前：算超期。
	if _, err := l.db.Exec(`UPDATE inbox_queue SET updated_ts = ? WHERE id = ?`,
		time.Now().UTC().Add(-3*time.Hour).Format(time.RFC3339), id); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	overdue, err := l.ListOverdueInbox(time.Now(), time.Hour)
	if err != nil || len(overdue) != 1 || overdue[0].ID != id {
		t.Fatalf("旧超期行 = %+v, err=%v, want 1 行", overdue, err)
	}
}

// TestRecordDispatchEvent 是线 A 的派单预算事件：四种 kind 都能写，
// detail 超 600 字按字符截断。
func TestRecordDispatchEvent(t *testing.T) {
	l, err := Open(t.TempDir() + "/ledger.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer Close(l)

	if _, err := l.RecordDispatchEvent(DispatchEvent{
		SessionID: "s", TaskID: "t-ev", DispatchID: "d1",
		Kind: DispatchEventSoftAlert, DispatchEstTokens: 1800, Detail: "派单 1800 token，触发软告警",
	}); err != nil {
		t.Fatalf("record soft_alert: %v", err)
	}
	longDetail := strings.Repeat("裁减说明", 200) // 800 字
	if _, err := l.RecordDispatchEvent(DispatchEvent{
		SessionID: "s", TaskID: "t-ev", DispatchID: "d1",
		Kind: DispatchEventTrimmed, DispatchEstTokens: 2500, Detail: longDetail,
	}); err != nil {
		t.Fatalf("record trimmed: %v", err)
	}
	var n int
	if err := l.db.QueryRow(`SELECT COUNT(*) FROM dispatch_events WHERE task_id = 't-ev'`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("dispatch_events rows = %d, err=%v, want 2", n, err)
	}
	var detail string
	if err := l.db.QueryRow(`SELECT detail FROM dispatch_events WHERE kind = 'trimmed'`).Scan(&detail); err != nil {
		t.Fatalf("read detail: %v", err)
	}
	if got := len([]rune(detail)); got != 600 {
		t.Fatalf("Detail = %d runes, want 600", got)
	}
	if !utf8.ValidString(detail) {
		t.Fatal("detail 截断劈开了多字节字符")
	}
}

// TestMigrateInboxDeadline 是线 A 的迁移：旧库的 inbox_queue 没有 deadline 列，
// Open 必须自动补齐，旧行还在，新列可写。
func TestMigrateInboxDeadline(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/ledger.db"
	db, err := sqlOpen(path)
	if err != nil {
		t.Fatalf("sql open: %v", err)
	}
	oldInbox := `
CREATE TABLE inbox_queue (id INTEGER PRIMARY KEY AUTOINCREMENT, ts TEXT NOT NULL, session_id TEXT NOT NULL, task_id TEXT NOT NULL, inbound_seq INTEGER NOT NULL DEFAULT 0, source TEXT NOT NULL DEFAULT '', text TEXT NOT NULL, status TEXT NOT NULL, retry_count INTEGER NOT NULL DEFAULT 0, created_ts TEXT NOT NULL, updated_ts TEXT NOT NULL);
INSERT INTO inbox_queue (ts, session_id, task_id, inbound_seq, source, text, status, retry_count, created_ts, updated_ts) VALUES ('2026-11-23T00:00:00Z', 's-old', 't-old', 0, '', 'hi', 'RUNNING', 0, '2026-11-23T00:00:00Z', '2026-11-23T00:00:00Z');
`
	if _, err := db.Exec(oldInbox); err != nil {
		db.Close()
		t.Fatalf("build old inbox: %v", err)
	}
	db.Close()

	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open old db: %v", err)
	}
	defer Close(l)
	rows, err := l.ListInboxByStatus(InboxRunning)
	if err != nil || len(rows) != 1 || rows[0].TaskID != "t-old" {
		t.Fatalf("old rows = %+v, err=%v, want 1 行", rows, err)
	}
	// 旧行 deadline 为空（迁移默认值），现在可以写 deadline。
	if err := l.SetInboxDeadline(rows[0].ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("set deadline on migrated row: %v", err)
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

// TestRecordRecall 是线 B 第一阶段的召回账本：recall_log 表随 Open 建好，
// 写入前查询与结果过脱敏（节点 3），合成假密钥只留掩码、不留原文。
func TestRecordRecall(t *testing.T) {
	l, err := Open(t.TempDir() + "/ledger.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer Close(l)
	var name string
	if err := l.db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='recall_log'`).Scan(&name); err != nil {
		t.Fatalf("recall_log 表缺失: %v", err)
	}
	// 合成假密钥，只用于验证账本脱敏。
	fake := "sk-FAKEKEY1234567890abcdef"
	if _, err := l.RecordRecall(RecallRecord{
		SessionID: "s-recall", TaskID: "t-recall",
		Query:       "怎么注册 Cloudflare，key 是 " + fake,
		Candidates:  []string{"wiki/entities/cf-xxx=42", "l3/lessons/yyy=30"},
		LexicalRank: []string{"wiki/entities/cf-xxx", "l3/lessons/yyy"},
		Delivered:   []string{"wiki/entities/cf-xxx"},
		TokensEst:   120, LatencyMs: 55,
	}); err != nil {
		t.Fatalf("RecordRecall: %v", err)
	}
	var query, cands, delivered string
	var hits int
	if err := l.db.QueryRow(
		`SELECT query, candidates, delivered, hits FROM recall_log WHERE task_id='t-recall'`,
	).Scan(&query, &cands, &delivered, &hits); err != nil {
		t.Fatalf("recall_log 回读: %v", err)
	}
	if strings.Contains(query, fake) {
		t.Fatalf("账本查询里不应留假密钥原文：%q", query)
	}
	if !strings.Contains(query, "[REDACTED]") {
		t.Fatalf("账本查询应含掩码：%q", query)
	}
	if hits != 2 || !strings.Contains(cands, "wiki/entities/cf-xxx=42") ||
		!strings.Contains(delivered, "wiki/entities/cf-xxx") {
		t.Fatalf("账本行不对：query=%q cands=%q delivered=%q hits=%d", query, cands, delivered, hits)
	}
	// ListRecallByTask 读口应回读到同一行。
	rows, err := l.ListRecallByTask("t-recall", 5)
	if err != nil {
		t.Fatalf("ListRecallByTask: %v", err)
	}
	if len(rows) != 1 || rows[0].Hits != 2 || !strings.Contains(rows[0].Delivered, "wiki/entities/cf-xxx") {
		t.Fatalf("ListRecallByTask 回读不对：%+v", rows)
	}
}

// TestRecordPatrol：巡检账本建表 + 写入 + 回读 + 细节脱敏。
func TestRecordPatrol(t *testing.T) {
	l, err := Open(t.TempDir() + "/ledger.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer Close(l)
	var name string
	if err := l.db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='patrol_log'`).Scan(&name); err != nil {
		t.Fatalf("patrol_log 表缺失: %v", err)
	}
	// 合成假密钥，只用于验证账本脱敏（sk- + 20 个 a，命中脱敏规则）。
	fake := "sk-" + strings.Repeat("a", 20)
	if _, err := l.RecordPatrol(PatrolRecord{
		SessionID: "s-patrol", TaskID: "t-patrol", Full: true,
		Scanned: 10, New: 1, Modified: 2, Renamed: 1, Deleted: 0,
		StaleMarked: 2, PendingMarked: 1, SkippedGenerating: 1,
		SecretFiles: 1, SecretHits: 3,
		Detail: "新增未入库：wiki/entities/c.md；含密钥文件 1 处，key 是 " + fake,
	}); err != nil {
		t.Fatalf("RecordPatrol: %v", err)
	}
	var full, scanned, newCount, modified, renamed int
	var detail string
	if err := l.db.QueryRow(
		`SELECT full, scanned, new_count, modified, renamed, detail FROM patrol_log WHERE task_id='t-patrol'`,
	).Scan(&full, &scanned, &newCount, &modified, &renamed, &detail); err != nil {
		t.Fatalf("patrol_log 回读: %v", err)
	}
	if full != 1 || scanned != 10 || newCount != 1 || modified != 2 || renamed != 1 {
		t.Fatalf("账本行不对：full=%d scanned=%d new=%d modified=%d renamed=%d", full, scanned, newCount, modified, renamed)
	}
	if strings.Contains(detail, fake) {
		t.Fatalf("账本细节里不应留假密钥原文：%q", detail)
	}
	if !strings.Contains(detail, "[REDACTED]") {
		t.Fatalf("账本细节应含掩码：%q", detail)
	}
	if !strings.Contains(detail, "wiki/entities/c.md") {
		t.Fatalf("账本细节应保留路径：%q", detail)
	}
}

// TestMigrateDispatchEventsTokens 是 P0 修复的迁移：旧库 dispatch_events
// 用的是 tokens 列，Open 必须把它改名成 dispatch_est_tokens，旧行数据保留。
func TestMigrateDispatchEventsTokens(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/ledger.db"
	db, err := sqlOpen(path)
	if err != nil {
		t.Fatalf("sql open: %v", err)
	}
	oldEvents := `
CREATE TABLE dispatch_events (id INTEGER PRIMARY KEY AUTOINCREMENT, ts TEXT NOT NULL, session_id TEXT NOT NULL, task_id TEXT NOT NULL, dispatch_id TEXT NOT NULL DEFAULT '', kind TEXT NOT NULL, tokens INTEGER NOT NULL DEFAULT 0, detail TEXT NOT NULL DEFAULT '');
INSERT INTO dispatch_events (ts, session_id, task_id, dispatch_id, kind, tokens, detail) VALUES ('2026-11-23T00:00:00Z', 's-old', 't-old', 'd1', 'soft_alert', 1800, '旧事件');
`
	if _, err := db.Exec(oldEvents); err != nil {
		db.Close()
		t.Fatalf("build old dispatch_events: %v", err)
	}
	db.Close()

	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open old db: %v", err)
	}
	defer Close(l)
	events, err := l.ListDispatchEvents("t-old")
	if err != nil || len(events) != 1 || events[0].DispatchEstTokens != 1800 {
		t.Fatalf("migrated events = %+v, err=%v, want 1 行 1800", events, err)
	}
	// 改名后新事件照常写。
	if _, err := l.RecordDispatchEvent(DispatchEvent{
		SessionID: "s", TaskID: "t-old", DispatchID: "d1",
		Kind: DispatchEventTrimmed, DispatchEstTokens: 2500, Detail: "新事件",
	}); err != nil {
		t.Fatalf("record after migrate: %v", err)
	}
	events, err = l.ListDispatchEvents("t-old")
	if err != nil || len(events) != 2 {
		t.Fatalf("events after record = %+v, err=%v, want 2", events, err)
	}
}

// TestInboxTerminalCAS 是 P0 修复的终态 CAS 保护：终态更新只改 RUNNING 行，
// 同一行标过终态后，再标任何终态都必须报错，绝不反转状态。
func TestInboxTerminalCAS(t *testing.T) {
	l, err := Open(t.TempDir() + "/ledger.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer Close(l)
	id, _, err := l.EnqueueInbox(InboxRecord{SessionID: "s", TaskID: "t-cas", Text: "x"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// PENDING 行标 DONE 应报错（CAS：只改 RUNNING 行）。
	if err := l.MarkInboxDone(id); err == nil {
		t.Fatal("PENDING 行标 DONE 应报错")
	}
	if err := l.MarkInboxRunning(id); err != nil {
		t.Fatalf("mark RUNNING: %v", err)
	}
	if err := l.MarkInboxDone(id); err != nil {
		t.Fatalf("RUNNING 行标 DONE 应成功：%v", err)
	}
	// 已是 DONE，再标 DONE / TIMEOUT / FAILED 都必须报错，状态不反转。
	for _, fn := range []struct {
		name string
		f    func(int64) error
	}{
		{"DONE", l.MarkInboxDone},
		{"TIMEOUT", l.MarkInboxTimeout},
		{"FAILED", l.MarkInboxFailed},
		{"REJECTED", l.MarkInboxRejected},
	} {
		if err := fn.f(id); err == nil {
			t.Fatalf("DONE 行再标 %s 应报错", fn.name)
		}
	}
	rows, err := l.ListInboxByStatus(InboxDone)
	if err != nil || len(rows) != 1 || rows[0].ID != id {
		t.Fatalf("CAS 后行应保持 DONE，得到 %+v, err=%v", rows, err)
	}
}

// TestExpenseRoleRoundTrip（规划 v2）：费用行分角色记账——
// RecordExpense 写 role，ListExpenseByTask 读回 role，
// TaskCostByRoles 按角色汇总（熔断只算 front_plan + front_judge）。
func TestExpenseRoleRoundTrip(t *testing.T) {
	l, err := Open(t.TempDir() + "/ledger.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer Close(l)
	recs := []ExpenseRecord{
		{SessionID: "s", TaskID: "t-role", Model: "deepseek-flash", Role: ExpenseRoleFrontPlan, CostCNYMilli: 10},
		{SessionID: "s", TaskID: "t-role", Model: "deepseek-flash", Role: ExpenseRoleFrontJudge, CostCNYMilli: 20},
		{SessionID: "s", TaskID: "t-role", Model: "deepseek-flash", Role: ExpenseRoleExecutor, CostCNYMilli: 1000},
		{SessionID: "s", TaskID: "t-role", Model: "deepseek-flash", Role: "", CostCNYMilli: 5},
	}
	for i, r := range recs {
		if _, err := l.RecordExpense(r); err != nil {
			t.Fatalf("RecordExpense %d: %v", i, err)
		}
	}
	got, err := l.ListExpenseByTask("t-role", 10)
	if err != nil {
		t.Fatalf("ListExpenseByTask: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("费用行应为 4 行，得到 %d", len(got))
	}
	// 最新在前：最后一条（role ""）在最前。
	if got[0].Role != "" || got[1].Role != ExpenseRoleExecutor || got[2].Role != ExpenseRoleFrontJudge || got[3].Role != ExpenseRoleFrontPlan {
		t.Fatalf("role 读回不对：%v", got)
	}
	// 按角色汇总：front_plan + front_judge = 30；executor 单独 1000；空角色 5。
	if c, err := l.TaskCostByRoles("t-role", ExpenseRoleFrontPlan, ExpenseRoleFrontJudge); err != nil || c != 30 {
		t.Fatalf("TaskCostByRoles(front_plan,front_judge)=%d, want 30, err=%v", c, err)
	}
	if c, err := l.TaskCostByRoles("t-role", ExpenseRoleExecutor); err != nil || c != 1000 {
		t.Fatalf("TaskCostByRoles(executor)=%d, want 1000, err=%v", c, err)
	}
	if c, err := l.TaskCostByRoles("t-role"); err != nil || c != 0 {
		t.Fatalf("无角色时应返回 0，得到 %d, err=%v", c, err)
	}
	if c, err := l.TaskCostByRoles("no-such-task", ExpenseRoleFrontPlan); err != nil || c != 0 {
		t.Fatalf("无记录时应返回 0，得到 %d, err=%v", c, err)
	}
}

// TestMigrateExpenseRole（规划 v2）：旧库（无 role 列）打开时自动迁移补列，
// 旧行读回 role 为 ""，新行可正常写 role。
func TestMigrateExpenseRole(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/ledger.db"
	// 造旧库：建一张无 role 列的 expense_ledger 并写一行旧数据。
	db, err := sqlOpen(path)
	if err != nil {
		t.Fatalf("sqlOpen: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE expense_ledger (id INTEGER PRIMARY KEY AUTOINCREMENT, ts TEXT NOT NULL, session_id TEXT NOT NULL, task_id TEXT NOT NULL, model TEXT NOT NULL, prompt_tokens INTEGER NOT NULL DEFAULT 0, cache_hit_tokens INTEGER NOT NULL DEFAULT 0, cache_miss_tokens INTEGER NOT NULL DEFAULT 0, completion_tokens INTEGER NOT NULL DEFAULT 0, cost_cny_milli INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatalf("建旧表: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO expense_ledger (ts, session_id, task_id, model, cost_cny_milli) VALUES ('2026-11-25T00:00:00Z', 's', 't-old', 'deepseek-flash', 7)`); err != nil {
		t.Fatalf("写旧行: %v", err)
	}
	db.Close()
	// Open 应迁移补列，不报错。
	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open 旧库: %v", err)
	}
	defer Close(l)
	got, err := l.ListExpenseByTask("t-old", 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("旧行读回失败：%v %v", got, err)
	}
	if got[0].Role != "" || got[0].CostCNYMilli != 7 {
		t.Fatalf("旧行 role 应为 \"\"、费用 7，得到 %+v", got[0])
	}
	// 新行可正常写 role。
	if _, err := l.RecordExpense(ExpenseRecord{SessionID: "s", TaskID: "t-old", Model: "deepseek-flash", Role: ExpenseRoleFrontPlan, CostCNYMilli: 3}); err != nil {
		t.Fatalf("新行写 role: %v", err)
	}
	if c, err := l.TaskCostByRoles("t-old", ExpenseRoleFrontPlan); err != nil || c != 3 {
		t.Fatalf("新行 role 汇总=%d, want 3, err=%v", c, err)
	}
}

// TestTouchInboxRunning（规划 v2）：按 task_id 续期 RUNNING 行的
// deadline 与 updated_ts；无 RUNNING 行时报错。
func TestTouchInboxRunning(t *testing.T) {
	l, err := Open(t.TempDir() + "/ledger.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer Close(l)
	if err := l.TouchInboxRunning("t-touch", time.Now().Add(1800*time.Second)); err == nil {
		t.Fatalf("无 RUNNING 行时应报错")
	}
	id, _, err := l.EnqueueInbox(InboxRecord{SessionID: "s", TaskID: "t-touch", Text: "hi"})
	if err != nil {
		t.Fatalf("EnqueueInbox: %v", err)
	}
	if err := l.MarkInboxRunning(id); err != nil {
		t.Fatalf("MarkInboxRunning: %v", err)
	}
	before := time.Now().UTC()
	dl := before.Add(1800 * time.Second)
	if err := l.TouchInboxRunning("t-touch", dl); err != nil {
		t.Fatalf("TouchInboxRunning: %v", err)
	}
	rows, err := l.ListInboxByStatus(InboxRunning)
	if err != nil || len(rows) != 1 {
		t.Fatalf("续期后 RUNNING 行应仍为 1 行：%v %v", rows, err)
	}
	if rows[0].Deadline != dl.UTC().Format(time.RFC3339) {
		t.Fatalf("deadline 应为 %s，得到 %s", dl.UTC().Format(time.RFC3339), rows[0].Deadline)
	}
	if rows[0].UpdatedTS < before.Format(time.RFC3339) {
		t.Fatalf("updated_ts 应被刷新，得到 %s", rows[0].UpdatedTS)
	}
}
