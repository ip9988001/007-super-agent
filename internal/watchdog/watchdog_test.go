package watchdog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"007-super-agent/internal/ledger"
	"007-super-agent/internal/memory"
	"007-super-agent/internal/registry"

	_ "modernc.org/sqlite"
)

// fixtureRegistry 在 t.TempDir() 里搭一个 4 条的小总账 + L3 文件。
func fixtureRegistry(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	entries := []map[string]any{
		{"tool_id": "cap-good", "category": "fetch", "subcategory": "s1",
			"call_type": "cli", "entry_point": "good-tool",
			"output_contract": "ok", "source_file": "s.md", "one_liner": "好的",
			"available": true},
		{"tool_id": "cap-bad-probe", "category": "fetch", "subcategory": "s1",
			"call_type": "cli", "entry_point": "missing-tool",
			"output_contract": "ok", "source_file": "s.md", "one_liner": "探针坏的",
			"available": true},
		{"tool_id": "cap-bad-sop", "category": "fetch", "subcategory": "s1",
			"call_type": "cli", "entry_point": "good-tool",
			"output_contract": "ok", "source_file": "s.md", "one_liner": "SOP 缺的",
			"available": true},
		{"tool_id": "cap-off", "category": "fetch", "subcategory": "s1",
			"call_type": "cli", "entry_point": "missing-tool",
			"output_contract": "ok", "source_file": "s.md", "one_liner": "不可用的",
			"available": false},
	}
	raw, err := json.Marshal(map[string]any{
		"version": "test", "generated": "2026-11-17", "total": 4, "entries": entries,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "registry"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "registry", "registry.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	l3dir := filepath.Join(dir, "index", "L3")
	if err := os.MkdirAll(l3dir, 0o700); err != nil {
		t.Fatal(err)
	}
	full := "# t\n\n## SOP\n- 输入：x\n- 步骤：y\n- 验收：z\n"
	noSOP := "# t\n\n## SOP\n- 输入：x\n- 步骤：y\n"
	for id, content := range map[string]string{
		"cap-good": full, "cap-bad-probe": full, "cap-bad-sop": noSOP, "cap-off": full,
	} {
		if err := os.WriteFile(filepath.Join(l3dir, id+".md"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// stubProbe 只对 cap-bad-probe 返回错误，其余都通过。
func stubProbe(e registry.Entry) error {
	if e.ToolID == "cap-bad-probe" {
		return errors.New("命令 missing-tool 不在 PATH")
	}
	return nil
}

func TestCheckRegistryFindsBrokenAndRecords(t *testing.T) {
	dir := fixtureRegistry(t)
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	led, err := ledger.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close(led)
	w := New(led, dbPath, stubProbe)
	res, err := w.CheckRegistry(dir)
	if err != nil {
		t.Fatalf("CheckRegistry err: %v", err)
	}
	if res.Checked != 3 || res.OK != 1 || res.Skipped != 1 || len(res.Broken) != 2 {
		t.Fatalf("result = %+v, want checked=3 ok=1 skipped=1 broken=2", res)
	}
	rows, err := led.ListExecutions("watchdog-daily-check", 10)
	if err != nil {
		t.Fatal(err)
	}
	// 第 13 步：体检只写一行汇总回执（checked/ok/broken 数 + 前 5 个坏条目名），
	// 不再逐条写 failure 淹没账本。
	if len(rows) != 1 {
		t.Fatalf("execution_ledger rows = %d, want 1 summary row", len(rows))
	}
	r := rows[0]
	if r.Status != "failure" || r.ExitCode != 1 || r.ToolID != "watchdog-daily-check" {
		t.Fatalf("bad summary row: %+v", r)
	}
	for _, want := range []string{"checked=3", "ok=1", "broken=2", "cap-bad-probe", "cap-bad-sop"} {
		if !strings.Contains(r.ErrHead, want) {
			t.Fatalf("summary ErrHead %q missing %q", r.ErrHead, want)
		}
	}
}

func TestCheckRegistryNoLedger(t *testing.T) {
	dir := fixtureRegistry(t)
	w := New(nil, "", stubProbe)
	res, err := w.CheckRegistry(dir)
	if err != nil {
		t.Fatalf("CheckRegistry err: %v", err)
	}
	if len(res.Broken) != 2 {
		t.Fatalf("broken = %d, want 2", len(res.Broken))
	}
}

// seedIndexMetrics 直接往 index_metrics 插当日行（账本包没有写这张表的方法）。
func seedIndexMetrics(t *testing.T, dbPath string, injected, saved int64) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`INSERT INTO index_metrics
		(ts, session_id, task_id, level, tool_id, hit, tokens_injected, tokens_saved_vs_full)
		VALUES (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), 's1', 't1', 'L3', 'cap-good', 1, ?, ?)`,
		injected, saved)
	if err != nil {
		t.Fatal(err)
	}
}

func TestDailySavingsNegativeWarns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	led, err := ledger.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close(led)
	if _, err := led.RecordTriage(ledger.TriageRecord{
		SessionID: "s1", TaskID: "t1", Route: "TOOL", Tier: 0, ToolID: "cap-good",
		TokensIn: 100, TokensOut: 50}); err != nil {
		t.Fatal(err)
	}
	if _, err := led.RecordTriage(ledger.TriageRecord{
		SessionID: "s1", TaskID: "t2", Route: "DIRECT", Tier: 1,
		TokensIn: 200, TokensOut: 80}); err != nil {
		t.Fatal(err)
	}
	if _, err := led.RecordExpense(ledger.ExpenseRecord{
		SessionID: "s1", TaskID: "t1", Model: "deepseek-flash",
		PromptTokens: 300, CompletionTokens: 50, CostCNYMilli: 500}); err != nil {
		t.Fatal(err)
	}
	seedIndexMetrics(t, dbPath, 1000, 100) // 省 100、注入 1000，净省 −900
	sv, err := New(led, dbPath, nil).DailySavings()
	if err != nil {
		t.Fatalf("DailySavings err: %v", err)
	}
	if sv.TriageCount != 2 || sv.Tier0Count != 1 || sv.Tier0Ratio != 0.5 {
		t.Fatalf("triage = count %d tier0 %d ratio %v, want 2/1/0.5",
			sv.TriageCount, sv.Tier0Count, sv.Tier0Ratio)
	}
	if sv.TokensInjectedTotal != 1300 || sv.NetSavedTokens != -900 {
		t.Fatalf("injected total %d net %d, want 1300/-900",
			sv.TokensInjectedTotal, sv.NetSavedTokens)
	}
	if sv.ExpenseCalls != 1 || sv.CostCNYMilli != 500 {
		t.Fatalf("expense calls %d cost %d, want 1/500", sv.ExpenseCalls, sv.CostCNYMilli)
	}
	if !sv.Warn || sv.WarnReason == "" {
		t.Fatalf("Warn = %v reason %q, want true + 非空原因", sv.Warn, sv.WarnReason)
	}
}

func TestDailySavingsPositiveNoWarn(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	led, err := ledger.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close(led)
	seedIndexMetrics(t, dbPath, 100, 5000) // 省 5000、注入 100，净省 +4900
	sv, err := New(led, dbPath, nil).DailySavings()
	if err != nil {
		t.Fatalf("DailySavings err: %v", err)
	}
	if sv.NetSavedTokens != 4900 || sv.Warn {
		t.Fatalf("net %d warn %v, want 4900/false", sv.NetSavedTokens, sv.Warn)
	}
}

func TestDailySavingsNoDBPath(t *testing.T) {
	if _, err := New(nil, "", nil).DailySavings(); err == nil {
		t.Fatal("want error for empty dbPath")
	}
}

func TestCheckQueue(t *testing.T) {
	cases := []struct {
		wm    QueueWatermark
		alert bool
	}{
		{QueueWatermark{Seen: 10, Acked: 8, OldestUnackedS: 2000}, true},  // 超 1800 秒告警
		{QueueWatermark{Seen: 10, Acked: 8, OldestUnackedS: 1800}, false}, // 正好 1800 不告警
		{QueueWatermark{Seen: 10, Acked: 8, OldestUnackedS: 100}, false},  // 未超不告警
		{QueueWatermark{Seen: 8, Acked: 8, OldestUnackedS: 9999}, false},  // 全部已确认不告警
	}
	for i, c := range cases {
		alert, reason := CheckQueue(c.wm)
		if alert != c.alert || reason == "" {
			t.Fatalf("case %d: alert=%v reason=%q, want alert=%v", i, alert, reason, c.alert)
		}
	}
}

// TestLastLinesFix 钉住第 7 步发现的账本毛病：带结尾换行的输出，
// RecordExecution 入库后必须留真正的末尾 20 行，不能丢一行。
func TestLastLinesFix(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	led, err := ledger.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close(led)
	var b strings.Builder
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&b, "line%d\n", i)
	}
	if _, err := led.RecordExecution(ledger.ExecutionRecord{
		SessionID: "s1", TaskID: "t-lastlines", ToolID: "cap-good",
		Command: "seq 30", ExitCode: 0, StdoutTail: b.String()}); err != nil {
		t.Fatal(err)
	}
	rows, err := led.ListExecutions("t-lastlines", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	lines := strings.Split(rows[0].StdoutTail, "\n")
	if len(lines) != 20 || lines[0] != "line11" || lines[19] != "line30" {
		t.Fatalf("stdout_tail = %d lines, 首行 %q 末行 %q, want 20/line11/line30",
			len(lines), lines[0], lines[len(lines)-1])
	}
}

// 精炼：桩模型提炼出 2 条，1 条走 WriteWiki 落盘，1 条非法词条名被拒；
// 结果写进 execution_ledger。
func TestRefineDailyWritesWiki(t *testing.T) {
	dir := t.TempDir()
	led, err := ledger.Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close(led)
	mem, err := memory.Open(filepath.Join(dir, "memory"))
	if err != nil {
		t.Fatal(err)
	}
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	l1 := "2026-11-17T10:00:00Z\ttask t1 route=DIRECT status=ok duration_ms=5\n" +
		"2026-11-17T10:01:00Z\ttask t2 route=TOOL tool=cap-code-semgrep status=executed duration_ms=1200\n" +
		"2026-11-17T10:02:00Z\ttask t3 route=META status=ok duration_ms=3\n"
	if err := os.WriteFile(filepath.Join(dir, "memory", "l1", yesterday+".log"), []byte(l1), 0o600); err != nil {
		t.Fatal(err)
	}
	stub := func(_ context.Context, taskID, text string) ([]memory.WikiEntry, error) {
		if taskID != "refine-"+yesterday {
			t.Errorf("refine taskID = %q, want refine-%s", taskID, yesterday)
		}
		if !strings.Contains(text, "task t1") || !strings.Contains(text, "task t3") {
			t.Errorf("精炼收到的 L1 文本不完整")
		}
		return []memory.WikiEntry{
			{Kind: memory.KindConcepts, Title: "测试词条", Summary: "摘要", Body: "正文"},
			{Kind: memory.KindConcepts, Title: "坏/词条", Summary: "摘要", Body: "正文"},
		}, nil
	}
	w := New(led, filepath.Join(dir, "ledger.db"), nil)
	res, err := w.RefineDaily(context.Background(), mem, yesterday, stub)
	if err != nil {
		t.Fatalf("RefineDaily: %v", err)
	}
	if res.Lines != 3 || res.Written != 1 || res.Failed != 1 || res.Skipped {
		t.Fatalf("res = %+v, want Lines=3 Written=1 Failed=1 Skipped=false", res)
	}
	if _, err := os.Stat(filepath.Join(dir, "memory", "wiki", "concepts", "测试词条.md")); err != nil {
		t.Fatalf("Wiki 词条未落盘: %v", err)
	}
	rows, err := led.ListExecutions("refine-"+yesterday, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ToolID != "refine-daily" || rows[0].Status != "success" {
		t.Fatalf("精炼回执行不对: %+v", rows)
	}
}

// 精炼跳过：当天无 L1 日志、模型未接（refine 为 nil）都不报错、不记账。
func TestRefineDailySkips(t *testing.T) {
	dir := t.TempDir()
	led, err := ledger.Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close(led)
	mem, err := memory.Open(filepath.Join(dir, "memory"))
	if err != nil {
		t.Fatal(err)
	}
	never := func(_ context.Context, _, _ string) ([]memory.WikiEntry, error) {
		t.Errorf("跳过时不应调模型")
		return nil, nil
	}
	w := New(led, filepath.Join(dir, "ledger.db"), nil)
	res, err := w.RefineDaily(context.Background(), mem, "2026-11-16", never)
	if err != nil || !res.Skipped {
		t.Fatalf("无 L1 日志应跳过: res=%+v err=%v", res, err)
	}
	res, err = w.RefineDaily(context.Background(), mem, "2026-11-16", nil)
	if err != nil || !res.Skipped {
		t.Fatalf("无模型应跳过: res=%+v err=%v", res, err)
	}
	rows, err := led.ListExecutions("refine-2026-11-16", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("跳过时不应记账，实际 %d 行", len(rows))
	}
}

// 精炼窗口：北京时间 04:00–04:15 且当天未跑过才跑。
func TestShouldRefine(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	at := func(h, m int) time.Time { return time.Date(2026, 11, 17, h, m, 0, 0, loc) }
	cases := []struct {
		now    time.Time
		marker string
		want   bool
	}{
		{at(4, 0), "", true},
		{at(4, 14), "", true},
		{at(4, 15), "", false},
		{at(3, 59), "", false},
		{at(5, 0), "", false},
		{at(4, 5), "2026-11-17", false},
		{at(4, 5), "2026-11-16", true},
	}
	for _, c := range cases {
		if got := shouldRefine(c.now, c.marker); got != c.want {
			t.Errorf("shouldRefine(%v, %q) = %v, want %v", c.now.Format("15:04"), c.marker, got, c.want)
		}
	}
}

// TestBackupLedger 是 B-07 的备份：热备文件能生成、能用 sqlite 打开、
// 旧文件按 7 天轮转被删、结果写进 execution_ledger。
func TestBackupLedger(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ledger.db")
	l, err := ledger.Open(dbPath)
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	defer ledger.Close(l)
	if _, err := l.RecordExecution(ledger.ExecutionRecord{
		SessionID: "s1", TaskID: "t1", ToolID: "tool-a",
		Command: "echo hi", ExitCode: 0, Status: "success",
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	// 放两个假旧备份：8 天前的该删，3 天前的该留。
	backups := filepath.Join(dir, "backups")
	if err := os.MkdirAll(backups, 0o700); err != nil {
		t.Fatal(err)
	}
	today := beijingNow().Format("2006-01-02")
	oldDate := beijingNow().AddDate(0, 0, -8).Format("2006-01-02")
	newDate := beijingNow().AddDate(0, 0, -3).Format("2006-01-02")
	for _, d := range []string{oldDate, newDate} {
		if err := os.WriteFile(filepath.Join(backups, backupFileName(d)), []byte("fake"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w := New(l, dbPath, nil)
	path, err := w.BackupLedger(dir)
	if err != nil {
		t.Fatalf("BackupLedger: %v", err)
	}
	if want := filepath.Join(backups, backupFileName(today)); path != want {
		t.Fatalf("backup path = %s, want %s", path, want)
	}
	// 备份文件能用 sqlite 打开，且有一行 execution_ledger。
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM execution_ledger`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("backup execution rows = %d, err = %v, want 1", n, err)
	}
	// 轮转：8 天前删了，3 天前还在。
	if _, err := os.Stat(filepath.Join(backups, backupFileName(oldDate))); !os.IsNotExist(err) {
		t.Fatalf("old backup %s should be pruned", oldDate)
	}
	if _, err := os.Stat(filepath.Join(backups, backupFileName(newDate))); err != nil {
		t.Fatalf("recent backup %s should be kept: %v", newDate, err)
	}
	// 记账行：backup-ledger 成功 1 行。
	rows, err := l.ListExecutions("backup-"+today, 10)
	if err != nil || len(rows) != 1 || rows[0].ToolID != "backup-ledger" || rows[0].Status != "success" {
		t.Fatalf("backup ledger rows = %+v, err = %v, want one success", rows, err)
	}
	// 同一天重复跑：覆盖旧文件，不报错。
	if _, err := w.BackupLedger(dir); err != nil {
		t.Fatalf("second BackupLedger same day: %v", err)
	}
}

// TestShouldBackup 是备份窗口：只在北京时间 03:30–03:45 且当天未跑过时跑。
func TestShouldBackup(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	mk := func(h, m int) time.Time {
		return time.Date(2026, 11, 17, h, m, 0, 0, loc)
	}
	if !shouldBackup(mk(3, 30), "") {
		t.Fatal("03:30 should backup")
	}
	if !shouldBackup(mk(3, 44), "2026-11-16") {
		t.Fatal("03:44 yesterday marker should backup")
	}
	if shouldBackup(mk(3, 45), "") {
		t.Fatal("03:45 should not backup")
	}
	if shouldBackup(mk(3, 29), "") {
		t.Fatal("03:29 should not backup")
	}
	if shouldBackup(mk(4, 0), "") {
		t.Fatal("04:00 should not backup")
	}
	if shouldBackup(mk(3, 35), "2026-11-17") {
		t.Fatal("already backed up today should not backup")
	}
}
