package ledger

import (
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

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
