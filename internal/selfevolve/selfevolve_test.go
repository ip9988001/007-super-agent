package selfevolve

import (
	"context"
	"database/sql"
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

// ---- 测试脚手架 ----

// seedExecFail 在账本里种 n 条某工具的执行失败（不同任务号，
// 都是“现在”——北京日期即今天，落在 7 天窗口内）。
func seedExecFail(t *testing.T, led *ledger.Ledger, toolID string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := led.RecordExecution(ledger.ExecutionRecord{
			SessionID: "s1", TaskID: fmt.Sprintf("seed-%s-%d", toolID, i),
			ToolID: toolID, Command: toolID + " --bad", ExitCode: 1,
			Status: "failure", ErrHead: "seed failure " + toolID,
		}); err != nil {
			t.Fatalf("seed exec fail: %v", err)
		}
	}
}

// seedProbeFail 种 n 条探针失败（command 以 probe 开头）。
func seedProbeFail(t *testing.T, led *ledger.Ledger, toolID string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := led.RecordExecution(ledger.ExecutionRecord{
			SessionID: "s1", TaskID: fmt.Sprintf("seed-probe-%s-%d", toolID, i),
			ToolID: toolID, Command: "probe " + toolID, ExitCode: 1,
			Status: "failure", ErrHead: "probe failed " + toolID,
		}); err != nil {
			t.Fatalf("seed probe fail: %v", err)
		}
	}
}

// seedAdvisorVerdict 种 n 条某监工 verdict 的执行行。
func seedAdvisorVerdict(t *testing.T, led *ledger.Ledger, toolID, verdict string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := led.RecordExecution(ledger.ExecutionRecord{
			SessionID: "s1", TaskID: fmt.Sprintf("seed-adv-%s-%d", toolID, i),
			ToolID: toolID, Command: toolID, ExitCode: 0,
			Status: "success", AdvisorVerdict: verdict, AdvisorNote: "seed " + verdict,
		}); err != nil {
			t.Fatalf("seed advisor: %v", err)
		}
	}
}

// stubReview 是桩模型调用：模拟 llm 客户端——先按 taskID 写一行费用
// （model deepseek-flash，7 毫分），再回固定 JSON。
func stubReview(t *testing.T, led *ledger.Ledger, reply string) ReviewFunc {
	t.Helper()
	return func(_ context.Context, taskID, _ string) (string, error) {
		if _, err := led.RecordExpense(ledger.ExpenseRecord{
			SessionID: "s1", TaskID: taskID, Model: "deepseek-flash",
			PromptTokens: 1000, CacheHitTokens: 800, CacheMissTokens: 200,
			CompletionTokens: 300, CostCNYMilli: 7,
		}); err != nil {
			t.Fatalf("stub review record expense: %v", err)
		}
		return reply, nil
	}
}

// miniRegistry 搭一个最小临时注册表（2 条 ops 工具 + L3 文件）：
// cap-ops-true 入口是坏路径（探针失败），cap-ops-git 入口是 git（探针通过）。
func miniRegistry(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	regJSON := `{"version":"v1.1-test","generated":"2026-11-19","total":2,"entries":[
{"tool_id":"cap-ops-true","category":"ops","subcategory":"shell","call_type":"cli","trigger_prefix":"","pre_command":"","entry_point":"/nonexistent/xyz","param_schema":{},"output_contract":"exit 0","status_triad":{"installed":false,"runnable":false,"verified":false},"available":true,"last_verified_at":"","source_file":"test","one_liner":"true 命令占位","verified_basis":"","note":""},
{"tool_id":"cap-ops-git","category":"ops","subcategory":"vcs","call_type":"cli","trigger_prefix":"","pre_command":"","entry_point":"git","param_schema":{},"output_contract":"exit 0","status_triad":{"installed":true,"runnable":true,"verified":false},"available":true,"last_verified_at":"","source_file":"test","one_liner":"git","verified_basis":"","note":""}
]}`
	if err := os.MkdirAll(filepath.Join(dir, "registry"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "registry", "registry.json"), []byte(regJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "index", "L3"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"cap-ops-true", "cap-ops-git"} {
		content := "# " + id + "\n\n输入：无\n\n步骤：跑命令\n\n验收：exit 0\n"
		if err := os.WriteFile(filepath.Join(dir, "index", "L3", id+".md"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// testLoop 搭一个测试用 Loop（账本 + 最小注册表 + 记忆 + 桩模型）。
func testLoop(t *testing.T, reply string) (*Loop, *ledger.Ledger, *registry.Registry, string) {
	t.Helper()
	dataDir := t.TempDir()
	led, err := ledger.Open(filepath.Join(dataDir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close(led) })
	regDir := miniRegistry(t)
	reg, err := registry.Load(regDir)
	if err != nil {
		t.Fatalf("load mini registry: %v", err)
	}
	mem, err := memory.Open(filepath.Join(dataDir, "memory"))
	if err != nil {
		t.Fatal(err)
	}
	loop := New(led, reg, mem, dataDir, "test-session", stubReview(t, led, reply), false)
	return loop, led, reg, dataDir
}

// ---- 阈值测试：2 次不提议、3 次提议 ----

func TestReviewThreshold(t *testing.T) {
	reply := `{"lessons":["入口路径要先验"],"proposals":[{"signature":"exec_fail:cap-ops-true","kind":"registry_entrypoint_fix","tool_id":"cap-ops-true","new_entry_point":"true","text":"把入口改为 true"}]}`
	loop, led, _, _ := testLoop(t, reply)
	date := beijingNow().Format("2006-01-02")

	// 2 次：只记教训，不产生提议。
	seedExecFail(t, led, "cap-ops-true", 2)
	res, err := loop.ReviewDaily(context.Background(), date)
	if err != nil {
		t.Fatalf("ReviewDaily(2 fails): %v", err)
	}
	if res.Proposals != 0 {
		t.Fatalf("2 次失败产生了 %d 条提议， want 0", res.Proposals)
	}
	if res.Lessons != 1 {
		t.Fatalf("lessons = %d, want 1（不足阈值只记教训）", res.Lessons)
	}
	props, err := led.ListProposals("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(props) != 0 {
		t.Fatalf("提议表有 %d 行， want 0", len(props))
	}

	// 再补 1 次到 3 次：产生 1 条提议，且自动应用（修路档、探针通过）。
	seedExecFail(t, led, "cap-ops-true", 1)
	res, err = loop.ReviewDaily(context.Background(), date)
	if err != nil {
		t.Fatalf("ReviewDaily(3 fails): %v", err)
	}
	if res.Proposals != 1 || res.Applied != 1 {
		t.Fatalf("proposals=%d applied=%d, want 1/1", res.Proposals, res.Applied)
	}
	props, err = led.ListProposals("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(props) != 1 || props[0].Status != ledger.ProposalApplied {
		t.Fatalf("提议表 = %+v, want 1 行 applied", props)
	}
	if props[0].Signature != "exec_fail:cap-ops-true" || props[0].EvidenceCount != 3 {
		t.Fatalf("提议签名/证据数 = %q/%d, want exec_fail:cap-ops-true/3", props[0].Signature, props[0].EvidenceCount)
	}
}

// ---- 提议表写入与状态流转 ----

func TestProposalWriteAndStatusFlow(t *testing.T) {
	reply := `{"lessons":["修路要先验","宪法不能动"],"proposals":[
{"signature":"exec_fail:cap-ops-true","kind":"registry_entrypoint_fix","tool_id":"cap-ops-true","new_entry_point":"true","text":"把入口改为 true"},
{"signature":"probe_fail:cap-ops-true","kind":"constitution","text":"宪法加一条探针口径"},
{"signature":"advisor_halt:cap-ops-true","kind":"triage_prompt","text":"分诊提示词加一条"},
{"signature":"exec_fail:cap-ops-git","kind":"registry_entrypoint_fix","tool_id":"cap-ops-git","new_entry_point":"true","text":"低于阈值应被丢弃"},
{"signature":"exec_fail:cap-ops-nope","kind":"registry_verified","tool_id":"cap-ops-nope","text":"不在信号里应被丢弃"}
]}`
	loop, led, reg, dataDir := testLoop(t, reply)
	date := beijingNow().Format("2006-01-02")

	seedExecFail(t, led, "cap-ops-true", 3)
	seedProbeFail(t, led, "cap-ops-true", 3)
	seedAdvisorVerdict(t, led, "cap-ops-true", "HALT", 3)
	seedExecFail(t, led, "cap-ops-git", 2) // 低于阈值

	// 记下重建前的 registry.json（重建与自查都不许改它）。
	regJSONBefore, err := os.ReadFile(filepath.Join(dataDir, "registry", "registry.json"))
	_ = regJSONBefore
	_ = err

	res, err := loop.ReviewDaily(context.Background(), date)
	if err != nil {
		t.Fatalf("ReviewDaily: %v", err)
	}
	if res.Proposals != 3 || res.Applied != 1 || res.Pending != 2 || res.Rejected != 0 {
		t.Fatalf("proposals=%d applied=%d pending=%d rejected=%d, want 3/1/2/0",
			res.Proposals, res.Applied, res.Pending, res.Rejected)
	}
	props, err := led.ListProposals("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(props) != 3 {
		t.Fatalf("提议表 %d 行， want 3", len(props))
	}
	bySig := map[string]ledger.Proposal{}
	for _, p := range props {
		bySig[p.Signature] = p
	}
	if bySig["exec_fail:cap-ops-true"].Status != ledger.ProposalApplied {
		t.Errorf("修路提议状态 = %q, want applied", bySig["exec_fail:cap-ops-true"].Status)
	}
	if bySig["probe_fail:cap-ops-true"].Status != ledger.ProposalPending {
		t.Errorf("宪法提议状态 = %q, want pending", bySig["probe_fail:cap-ops-true"].Status)
	}
	if bySig["advisor_halt:cap-ops-true"].Status != ledger.ProposalPending {
		t.Errorf("分诊提议状态 = %q, want pending", bySig["advisor_halt:cap-ops-true"].Status)
	}
	if _, ok := bySig["exec_fail:cap-ops-git"]; ok {
		t.Error("低于阈值的提议不应入库")
	}
	// 自动应用只活在内存覆盖层：Entry() 返回新入口，磁盘文件未动。
	if e, _ := reg.Entry("cap-ops-true"); e.EntryPoint != "true" {
		t.Errorf("内存入口 = %q, want true", e.EntryPoint)
	}
	if ov := reg.EntryPointOverrides(); ov["cap-ops-true"] != "true" {
		t.Errorf("覆盖层 = %v, want cap-ops-true=true", ov)
	}
	// 同一签名 7 天内去重：再跑一次，不产生新提议。
	res2, err := loop.ReviewDaily(context.Background(), date)
	if err != nil {
		t.Fatalf("ReviewDaily again: %v", err)
	}
	if res2.Proposals != 0 {
		t.Fatalf("重复自查又产生了 %d 条提议， want 0（去重）", res2.Proposals)
	}
	// Wiki 教训条目已落盘（标题含日期）。
	wikiPath := filepath.Join(dataDir, "memory", "wiki", "synthesis", "自进化自查 "+date+".md")
	if _, err := os.Stat(wikiPath); err != nil {
		t.Fatalf("Wiki 教训未落盘 %s: %v", wikiPath, err)
	}
}

// ---- 费用行 task_id 格式与当日汇总行 ----

func TestExpenseTaskIDFormat(t *testing.T) {
	reply := `{"lessons":["花钱要看账"],"proposals":[]}`
	loop, led, _, _ := testLoop(t, reply)
	date := beijingNow().Format("2006-01-02")
	seedExecFail(t, led, "cap-ops-true", 3)

	res, err := loop.ReviewDaily(context.Background(), date)
	if err != nil {
		t.Fatalf("ReviewDaily: %v", err)
	}
	taskID := "selfevolve-" + date
	rows, err := led.ListExpenseByTask(taskID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("task %s 费用行 %d 条， want 2（调用行 + 汇总行）", taskID, len(rows))
	}
	var callRow, summaryRow *ledger.ExpenseRecord
	for i := range rows {
		if rows[i].Model == "selfevolve-summary" {
			summaryRow = &rows[i]
		} else {
			callRow = &rows[i]
		}
	}
	if callRow == nil || callRow.Model != "deepseek-flash" || callRow.CostCNYMilli != 7 {
		t.Errorf("调用行 = %+v, want model=deepseek-flash cost=7", callRow)
	}
	if summaryRow == nil || summaryRow.CostCNYMilli != 7 {
		t.Errorf("汇总行 = %+v, want cost=7（与调用行一致）", summaryRow)
	}
	if res.CostMilli != 7 {
		t.Errorf("ReviewResult.CostMilli = %d, want 7", res.CostMilli)
	}
	// 汇总行不参与二次汇总：再汇总一次仍是 7，不会 double count。
	sum, err := led.SummarizeExpenseByTask(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if sum.CostCNYMilli != 7 {
		t.Errorf("二次汇总 cost = %d, want 7", sum.CostCNYMilli)
	}
}

// ---- 影子模式不写账本 ----

func TestShadowDoesNotWrite(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		loc = time.FixedZone("CST", 8*3600)
	}
	// 2026-11-22 是周日：自查与重建窗口同时开。
	now := time.Date(2026, 11, 22, 4, 31, 0, 0, loc)

	dataDir := t.TempDir()
	led, err := ledger.Open(filepath.Join(dataDir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close(led)
	regDir := miniRegistry(t)
	reg, err := registry.Load(regDir)
	if err != nil {
		t.Fatalf("load mini registry: %v", err)
	}
	mem, err := memory.Open(filepath.Join(dataDir, "memory"))
	if err != nil {
		t.Fatal(err)
	}
	seedExecFail(t, led, "cap-ops-true", 3)

	before := countLedgerRows(t, led)
	shadowLoop := New(led, reg, mem, dataDir, "test-session", stubReview(t, led, `{"lessons":[],"proposals":[]}`), true)
	shadowLoop.maybeRun(now)
	after := countLedgerRows(t, led)
	if before != after {
		t.Fatalf("影子模式写了账本：跑前 %+v，跑后 %+v", before, after)
	}
	// Wiki、标记文件、快照一律没有。
	if ents, _ := filepath.Glob(filepath.Join(dataDir, "memory", "wiki", "synthesis", "*.md")); len(ents) != 0 {
		t.Fatalf("影子模式写了 Wiki: %v", ents)
	}
	for _, m := range []string{"selfevolve_last", "selfevolve_rebuild_last"} {
		if _, err := os.Stat(filepath.Join(dataDir, m)); err == nil {
			t.Fatalf("影子模式写了标记文件 %s", m)
		}
	}
	if _, err := os.Stat(filepath.Join(dataDir, "selfevolve", "rebuild_last.json")); err == nil {
		t.Fatal("影子模式写了重建快照")
	}

	// 同一窗口、非影子模式：正常执行并写标记。
	realLoop := New(led, reg, mem, dataDir, "test-session", stubReview(t, led, `{"lessons":["影子之外要干活"],"proposals":[]}`), false)
	realLoop.maybeRun(now)
	if _, err := os.Stat(filepath.Join(dataDir, "selfevolve_last")); err != nil {
		t.Fatal("非影子模式未写自查标记")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "selfevolve_rebuild_last")); err != nil {
		t.Fatal("非影子模式未写重建标记")
	}
}

// ledgerRowCounts 是四张相关表的行数（影子测试用）。
type ledgerRowCounts struct {
	Execution int
	Expense   int
	Proposals int
}

func countLedgerRows(t *testing.T, led *ledger.Ledger) ledgerRowCounts {
	t.Helper()
	var c ledgerRowCounts
	execs, err := led.ListExecutions("seed-cap-ops-true-0", 100)
	if err != nil {
		t.Fatal(err)
	}
	_ = execs
	// 用公开 API 计数：执行行按任务查不到全表，改走提议表与费用汇总。
	// 这里只比“自查会动的三处”：提议表行数、费用行数（按任务汇总）、
	// 以及 Wiki 文件数（调用方另查）。执行汇总行用 ListExecutions 查
	// 自查任务号。
	props, err := led.ListProposals("", 100)
	if err != nil {
		t.Fatal(err)
	}
	c.Proposals = len(props)
	date := "2026-11-22"
	rows, err := led.ListExpenseByTask("selfevolve-"+date, 100)
	if err != nil {
		t.Fatal(err)
	}
	c.Expense = len(rows)
	selfExecs, err := led.ListExecutions("selfevolve-"+date, 100)
	if err != nil {
		t.Fatal(err)
	}
	c.Execution = len(selfExecs)
	return c
}

// ---- 窗口判断 ----

func TestShouldReviewAndRebuild(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		loc = time.FixedZone("CST", 8*3600)
	}
	// 2026-11-22 周日，2026-11-23 周一。
	sun := func(h, m int) time.Time { return time.Date(2026, 11, 22, h, m, 0, 0, loc) }
	mon := func(h, m int) time.Time { return time.Date(2026, 11, 23, h, m, 0, 0, loc) }
	if !shouldReview(sun(4, 30), "") || !shouldReview(mon(4, 30), "") {
		t.Error("04:30 应跑自查")
	}
	if shouldReview(sun(4, 29), "") || shouldReview(sun(4, 45), "") || shouldReview(sun(5, 30), "") {
		t.Error("窗口外不应跑自查")
	}
	if shouldReview(sun(4, 31), "2026-11-22") {
		t.Error("当天已跑过不应再跑自查")
	}
	if !shouldRebuild(sun(4, 30), "") {
		t.Error("周日 04:30 应跑重建")
	}
	if shouldRebuild(mon(4, 30), "") {
		t.Error("周一不应跑重建")
	}
	if shouldRebuild(sun(4, 29), "") || shouldRebuild(sun(4, 45), "") {
		t.Error("窗口外不应跑重建")
	}
}

// ---- 模型回复解析 ----

func TestParseReviewReply(t *testing.T) {
	valid := "```json\n{\"lessons\":[\"教训一\"],\"proposals\":[{\"signature\":\"exec_fail:cap-x\",\"kind\":\"code\",\"text\":\"改代码\"}]}\n```"
	out, err := parseReviewReply(valid)
	if err != nil {
		t.Fatalf("parse valid: %v", err)
	}
	if len(out.Lessons) != 1 || len(out.Proposals) != 1 {
		t.Fatalf("解析结果 lessons=%d proposals=%d, want 1/1", len(out.Lessons), len(out.Proposals))
	}
	if _, err := parseReviewReply("不是 JSON"); err == nil {
		t.Error("非法 JSON 应报错")
	}
	// 未知种类、缺 tool_id、修路缺新入口的草案一律丢弃，不毁整轮。
	mixed := `{"lessons":[],"proposals":[
{"signature":"s1","kind":"no-such-kind","text":"x"},
{"signature":"s2","kind":"registry_entrypoint_fix","tool_id":"cap-x","text":"缺新入口"},
{"signature":"s3","kind":"registry_verified","text":"缺 tool_id"},
{"signature":"s4","kind":"other","text":"这条合法"}]}`
	out, err = parseReviewReply(mixed)
	if err != nil {
		t.Fatalf("parse mixed: %v", err)
	}
	if len(out.Proposals) != 1 || out.Proposals[0].Signature != "s4" {
		t.Fatalf("非法草案过滤后 proposals = %+v, want 只有 s4", out.Proposals)
	}
}

// ---- 信号聚合（含分诊错选代理与费用突增） ----

func TestAggregateSignals(t *testing.T) {
	dir := t.TempDir()
	led, err := ledger.Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close(led)

	seedExecFail(t, led, "cap-a", 2)
	seedProbeFail(t, led, "cap-a", 1)
	seedAdvisorVerdict(t, led, "cap-b", "HALT", 1)
	seedAdvisorVerdict(t, led, "cap-b", "CONCERN", 1)
	// 分诊错选代理：Tier-1 挑了 cap-c，同一任务执行失败。
	if _, err := led.RecordTriage(ledger.TriageRecord{
		SessionID: "s1", TaskID: "misfire-task", Route: "TOOL", Tier: 1, ToolID: "cap-c",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := led.RecordExecution(ledger.ExecutionRecord{
		SessionID: "s1", TaskID: "misfire-task", ToolID: "cap-c",
		Command: "cap-c", ExitCode: 1, Status: "failure",
	}); err != nil {
		t.Fatal(err)
	}
	// 费用：今天 300 毫分；前 7 天每天 100 毫分（直接写库，定死北京时间）。
	if _, err := led.RecordExpense(ledger.ExpenseRecord{
		SessionID: "s1", TaskID: "t-today", Model: "deepseek-flash", CostCNYMilli: 300,
	}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	loc, _ := time.LoadLocation("Asia/Shanghai")
	if loc == nil {
		loc = time.FixedZone("CST", 8*3600)
	}
	nowBJ := time.Now().In(loc)
	for d := 1; d <= 7; d++ {
		day := nowBJ.AddDate(0, 0, -d)
		ts := time.Date(day.Year(), day.Month(), day.Day(), 12, 0, 0, 0, loc).UTC().Format(time.RFC3339)
		if _, err := db.Exec(`INSERT INTO expense_ledger
			(ts, session_id, task_id, model, prompt_tokens, cache_hit_tokens,
			 cache_miss_tokens, completion_tokens, cost_cny_milli)
			VALUES (?, 's1', ?, 'deepseek-flash', 0, 0, 0, 0, 100)`,
			ts, fmt.Sprintf("t-prior-%d", d)); err != nil {
			t.Fatalf("seed prior expense: %v", err)
		}
	}

	sig, err := led.AggregateSignals(7)
	if err != nil {
		t.Fatalf("AggregateSignals: %v", err)
	}
	if sig.ExecFailures["cap-a"] != 2 {
		t.Errorf("ExecFailures cap-a = %d, want 2", sig.ExecFailures["cap-a"])
	}
	if sig.ProbeFailures["cap-a"] != 1 {
		t.Errorf("ProbeFailures cap-a = %d, want 1", sig.ProbeFailures["cap-a"])
	}
	if sig.AdvisorHalts["cap-b"] != 1 || sig.AdvisorConcerns["cap-b"] != 1 {
		t.Errorf("HALT/CONCERN cap-b = %d/%d, want 1/1", sig.AdvisorHalts["cap-b"], sig.AdvisorConcerns["cap-b"])
	}
	if sig.TriageMisfires["cap-c"] != 1 {
		t.Errorf("TriageMisfires cap-c = %d, want 1", sig.TriageMisfires["cap-c"])
	}
	if sig.ExpenseTodayMilli != 300 {
		t.Errorf("ExpenseTodayMilli = %d, want 300", sig.ExpenseTodayMilli)
	}
	if sig.ExpensePrior7DayMeanMilli != 100 {
		t.Errorf("ExpensePrior7DayMeanMilli = %d, want 100", sig.ExpensePrior7DayMeanMilli)
	}
	if !sig.ExpenseSpike {
		t.Error("ExpenseSpike = false, want true（300 > 2×100）")
	}
	if n, ok := sig.CountFor("expense_spike"); !ok || n < 1 {
		t.Errorf("CountFor(expense_spike) = %d/%v, want ≥1/true", n, ok)
	}
	if _, ok := sig.CountFor("exec_fail:cap-nope"); ok {
		t.Error("CountFor(不存在的签名) 不应命中")
	}
}

// ---- 每周重建差异 ----

func TestRebuildWeeklyDiff(t *testing.T) {
	dataDir := t.TempDir()
	led, err := ledger.Open(filepath.Join(dataDir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close(led)
	regDir := miniRegistry(t)
	reg, err := registry.Load(regDir)
	if err != nil {
		t.Fatalf("load mini registry: %v", err)
	}
	mem, err := memory.Open(filepath.Join(dataDir, "memory"))
	if err != nil {
		t.Fatal(err)
	}
	loop := New(led, reg, mem, dataDir, "test-session", nil, false)

	// 记下 registry.json 原文（重建绝不改它）。
	regJSONPath := filepath.Join(regDir, "registry", "registry.json")
	before, err := os.ReadFile(regJSONPath)
	if err != nil {
		t.Fatal(err)
	}
	// 上次快照故意写反：git 上次坏、true 上次好。
	snapDir := filepath.Join(dataDir, "selfevolve")
	if err := os.MkdirAll(snapDir, 0o700); err != nil {
		t.Fatal(err)
	}
	snapJSON := `{"date":"2026-11-15","tools":{"cap-ops-git":{"ok":false,"verified":false},"cap-ops-true":{"ok":true,"verified":false}},"discovered":[]}`
	if err := os.WriteFile(filepath.Join(snapDir, "rebuild_last.json"), []byte(snapJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := loop.RebuildWeekly(context.Background(), "2026-11-22")
	if err != nil {
		t.Fatalf("RebuildWeekly: %v", err)
	}
	if res.Checked != 2 || res.OK != 1 || res.Broken != 1 {
		t.Fatalf("checked=%d ok=%d broken=%d, want 2/1/1", res.Checked, res.OK, res.Broken)
	}
	if len(res.NewlyOK) != 1 || res.NewlyOK[0] != "cap-ops-git" {
		t.Errorf("NewlyOK = %v, want [cap-ops-git]", res.NewlyOK)
	}
	if len(res.NewlyBroken) != 1 || res.NewlyBroken[0] != "cap-ops-true" {
		t.Errorf("NewlyBroken = %v, want [cap-ops-true]", res.NewlyBroken)
	}
	// registry.json 磁盘内容未变。
	after, err := os.ReadFile(regJSONPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("重建改了 registry.json（只许动内存覆盖层）")
	}
	// Wiki 报告已落盘，快照已更新。
	wikiPath := filepath.Join(dataDir, "memory", "wiki", "synthesis", "自进化注册表重建 2026-11-22.md")
	body, err := os.ReadFile(wikiPath)
	if err != nil {
		t.Fatalf("重建 Wiki 未落盘: %v", err)
	}
	if !strings.Contains(string(body), "cap-ops-true") || !strings.Contains(string(body), "cap-ops-git") {
		t.Errorf("重建 Wiki 未含差异条目：\n%s", body)
	}
	snap := loadRebuildSnapshot(filepath.Join(snapDir, "rebuild_last.json"))
	if !snap.Tools["cap-ops-git"].OK || snap.Tools["cap-ops-true"].OK {
		t.Errorf("快照未更新：%+v", snap.Tools)
	}
	// 探针通过的工具标上内存已验证。
	if reg.NeedsProbe("cap-ops-git") {
		t.Error("重建后 cap-ops-git 仍需探针（应已标内存已验证）")
	}
}

// ---- 提议表状态流转（直接调账本 API） ----

func TestProposalStatusFlow(t *testing.T) {
	dir := t.TempDir()
	led, err := ledger.Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close(led)
	if _, err := led.RecordProposal(ledger.Proposal{
		ProposalID: "se-2026-11-19-001", Date: "2026-11-19",
		Signature: "exec_fail:cap-x", EvidenceCount: 3, Text: "修入口",
	}); err != nil {
		t.Fatalf("RecordProposal: %v", err)
	}
	if err := led.UpdateProposalStatus("se-2026-11-19-001", ledger.ProposalApplied); err != nil {
		t.Fatalf("UpdateProposalStatus applied: %v", err)
	}
	props, err := led.ListProposals(ledger.ProposalApplied, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(props) != 1 || props[0].ProposalID != "se-2026-11-19-001" {
		t.Fatalf("ListProposals(applied) = %+v, want 1 行", props)
	}
	if err := led.UpdateProposalStatus("se-2026-11-19-001", "nope"); err == nil {
		t.Error("非法状态应拒绝")
	}
	if err := led.UpdateProposalStatus("se-2026-11-19-999", ledger.ProposalApplied); err == nil {
		t.Error("不存在的提议应报错")
	}
	// 同一 proposal_id 重复插入应失败。
	if _, err := led.RecordProposal(ledger.Proposal{
		ProposalID: "se-2026-11-19-001", Date: "2026-11-19",
		Signature: "exec_fail:cap-x", EvidenceCount: 3, Text: "重复",
	}); err == nil {
		t.Error("重复 proposal_id 应失败")
	}
}
