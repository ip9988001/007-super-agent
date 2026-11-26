package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"007-super-agent/internal/executor"
	"007-super-agent/internal/ledger"
	"007-super-agent/internal/llm"
	"007-super-agent/internal/memory"
	"007-super-agent/internal/registry"
	"007-super-agent/internal/session"
)

// miniRegistry 造一个最小总账：test-echo / test-true / test-false 三个可执行工具。
func miniRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	dst := t.TempDir()
	entries := []struct{ id, ep, contract string }{
		{"test-echo", "echo", "exit 0"},
		{"test-true", "true", "exit 0"},
		{"test-false", "false", "exit 1"},
		{"cap-ops-echo", "echo", "exit 0"},
		{"cap-fetch-echo", "echo", "exit 0"},
		// cap-fetch-tvly：真实工具，名字口径下因 "cap-fetch-" 前缀算只读
		// （网关口径单测用）；entry_point echo，跑起来只回显输入。
		{"cap-fetch-tvly", "echo", "exit 0"},
		// cap-fetch-new-unknown：名字口径下命中 "cap-fetch-" 前缀算只读
		// （网关口径单测用）；entry_point echo，跑起来只回显输入。
		{"cap-fetch-new-unknown", "echo", "exit 0"},
		// cap-ops-python：读文档主力（M4 中期单测专用，2026-11-26）——
		// 输出上限放宽到 20000 字；entry_point echo，跑起来只回显输入，
		// 单测靠回显的长文本验证截断分档。
		{"cap-ops-python", "echo", "exit 0"},
		// test-rm：不可逆动作标记（entry_point rm），专供 batch_rejected
		// 单测——打包里含它即整包拒掉；它从不被真执行（拒在执行前）。
		{"test-rm", "rm", "exit 0"},
	}
	var ents strings.Builder
	ents.WriteString(`{"version":"v1.1-test","generated":"2026-11-18","total":9,"categories":[{"id":"fetch","name":"Fetch","description":"Fetch tools for tests."},{"id":"code","name":"Code","description":"Code tools for tests."},{"id":"seo","name":"SEO","description":"SEO tools for tests."},{"id":"design","name":"Design","description":"Design tools for tests."},{"id":"security","name":"Security","description":"Security tools for tests."},{"id":"runtime","name":"Runtime","description":"Runtime tools for tests."},{"id":"media","name":"Media","description":"Media tools for tests."},{"id":"ops","name":"Operations","description":"Operations tools for tests. Use this category for shell commands and test tools."},{"id":"memory","name":"Memory","description":"Memory tools for tests."}],"subcategories":[{"category":"ops","id":"shell","description":"Shell commands and test tools. Use echo and true/false here."}],"entries":[`)
	for i, e := range entries {
		if i > 0 {
			ents.WriteString(",")
		}
		fmt.Fprintf(&ents, `{"tool_id":%q,"category":"ops","subcategory":"shell","call_type":"cli","trigger_prefix":"","pre_command":"","entry_point":%q,"param_schema":{},"output_contract":%q,"status_triad":{"installed":true,"runnable":true,"verified":false},"available":true,"last_verified_at":"","source_file":"test","one_liner":%q,"verified_basis":"","note":""}`,
			e.id, e.ep, e.contract, e.id+" 占位")
	}
	ents.WriteString("]}")
	if err := os.MkdirAll(filepath.Join(dst, "registry"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "registry", "registry.json"), []byte(ents.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dst, "index", "L2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "index", "L1.md"), []byte("fetch\ncode\nseo\ndesign\nops\nmemory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var l2 strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&l2, "- %s: %s 占位\n", e.id, e.id)
	}
	if err := os.WriteFile(filepath.Join(dst, "index", "L2", "ops.md"), []byte(l2.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	// L3：每个条目一个同名 md（checkL3Correspondence 要求 1:1）。
	if err := os.MkdirAll(filepath.Join(dst, "index", "L3"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.WriteFile(filepath.Join(dst, "index", "L3", e.id+".md"), []byte("# "+e.id+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rg, err := registry.Load(dst)
	if err != nil {
		t.Fatalf("registry.Load: %v", err)
	}
	return rg
}

func openTestLedger(t *testing.T) *ledger.Ledger {
	t.Helper()
	l, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close(l) })
	return l
}

// stubSeq 按顺序返回多份决定（用完后重复最后一份）。
func stubSeq(replies []string) ModelCallFunc {
	i := 0
	return func(taskID, frozen, tail string) (string, int64, int64, error) {
		if i >= len(replies) {
			i = len(replies) - 1
		}
		r := replies[i]
		i++
		return r, 100, 20, nil
	}
}

func newTestAgent(t *testing.T, call ModelCallFunc) *Agent {
	t.Helper()
	rg := miniRegistry(t)
	led := openTestLedger(t)
	ex := &executor.Executor{Ledger: led}
	return &Agent{Call: call, Reg: rg, Led: led, Ex: ex, DataDir: t.TempDir(), SessionID: "test-session", MaxIter: 20}
}

func TestParseAction(t *testing.T) {
	if a, err := parseAction(`{"action":"tool","tool_id":"test-echo","inputs":["hi"]}`); err != nil || a.ToolID != "test-echo" {
		t.Fatalf("tool 解析失败: %v %+v", err, a)
	}
	if a, err := parseAction(`{"action":"answer","text":"好了"}`); err != nil || a.Text != "好了" {
		t.Fatalf("answer 解析失败: %v %+v", err, a)
	}
	// 模型常在 JSON 外包解释文字：取最外层对象。
	if a, err := parseAction(`先调工具：{"action":"tool","tool_id":"test-true","inputs":[]} 就这样`); err != nil || a.ToolID != "test-true" {
		t.Fatalf("包解释文字的 JSON 应能解析: %v %+v", err, a)
	}
	if _, err := parseAction(`{"action":"tool"}`); err == nil {
		t.Fatalf("缺 tool_id 应报错")
	}
	if _, err := parseAction(`不是 JSON`); err == nil {
		t.Fatalf("非法 JSON 应报错")
	}
	if _, err := parseAction(`{"action":"wait"}`); err == nil {
		t.Fatalf("未知 action 应报错")
	}
}

func TestRunToolThenAnswer(t *testing.T) {
	ag := newTestAgent(t, stubSeq([]string{
		`{"action":"tool","tool_id":"test-echo","inputs":["hello"]}`,
		`{"action":"answer","text":"echo 说了 hello"}`,
	}))
	res, err := ag.Run(context.Background(), "t1", "让 echo 说 hello", "", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "stopped" || res.CompletedSteps != 1 {
		t.Fatalf("应 stopped 且 1 步成功，实为 status=%s steps=%d", res.Status, res.CompletedSteps)
	}
	if !strings.Contains(res.ReplyDraft, "hello") {
		t.Fatalf("回复应含 hello，实为 %q", res.ReplyDraft)
	}
	if b, err := os.ReadFile(filepath.Join(ag.DataDir, "artifacts", "t1", "step-1.txt")); err != nil || !strings.Contains(string(b), "hello") {
		t.Fatalf("step-1.txt 应含 hello：%v %q", err, string(b))
	}
}

func TestRunResolveErrorThenRecover(t *testing.T) {
	ag := newTestAgent(t, stubSeq([]string{
		`{"action":"tool","tool_id":"test-nope","inputs":[]}`,
		`{"action":"tool","tool_id":"test-echo","inputs":["ok"]}`,
		`{"action":"answer","text":"换工具后成功"}`,
	}))
	res, err := ag.Run(context.Background(), "t2", "先调个不存在的工具再调 echo", "", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "stopped" || res.CompletedSteps != 1 {
		t.Fatalf("应 stopped 且 1 步成功（坏工具不计），实为 status=%s steps=%d", res.Status, res.CompletedSteps)
	}
}

func TestRunMaxIterations(t *testing.T) {
	ag := newTestAgent(t, stubSeq([]string{
		`{"action":"tool","tool_id":"test-echo","inputs":["x"]}`,
	}))
	ag.MaxIter = 2
	res, err := ag.Run(context.Background(), "t3", "一直调工具不交活", "", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "max_iterations" || res.CompletedSteps != 2 {
		t.Fatalf("应 max_iterations 且 2 步，实为 status=%s steps=%d", res.Status, res.CompletedSteps)
	}
}

func TestRunNoOutputCountsAsFailure(t *testing.T) {
	ag := newTestAgent(t, stubSeq([]string{
		`{"action":"tool","tool_id":"test-true","inputs":[]}`,
		`{"action":"answer","text":"true 没输出"}`,
	}))
	res, err := ag.Run(context.Background(), "t4", "调 true（exit 0 但无输出）", "", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "stopped" || res.CompletedSteps != 0 {
		t.Fatalf("无输出的成功不应计入完成步数，实为 status=%s steps=%d", res.Status, res.CompletedSteps)
	}
}

// 时间预算：到点就停下来，不让开放式任务无限烧下去。
func TestRunTimeBudgetStops(t *testing.T) {
	ag := newTestAgent(t, stubSeq([]string{
		`{"action":"tool","tool_id":"test-echo","inputs":["hello"]}`,
		`{"action":"tool","tool_id":"test-echo","inputs":["world"]}`,
		`{"action":"tool","tool_id":"test-echo","inputs":["again"]}`,
		`{"action":"answer","text":"答完了"}`,
	}))
	ag.TimeBudget = time.Nanosecond // 几乎立刻到点
	res, err := ag.Run(context.Background(), "t8", "一个要跑很久的任务", "", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 到点后停下：不应跑完所有步骤（CompletedSteps 应小于模型想做的轮数）。
	// 这里第一轮 echo 成功后第二轮就到点了，所以完成步数至多 1。
	if res.CompletedSteps > 1 {
		t.Fatalf("时间预算应让任务早停，完成步数实为 %d", res.CompletedSteps)
	}
}

// 连续 5 轮失败就卡住停下，不再硬撑；且不受 20 步旧上限影响。
func TestRunStuckAfterConsecutiveFailures(t *testing.T) {
	// 每次都调 test-false（exit 1），模型一直不交活。
	// 5 次失败后应停下，而不是跑满 MaxIter。
	replies := []string{}
	for i := 0; i < 10; i++ {
		replies = append(replies, `{"action":"tool","tool_id":"test-false","inputs":[]}`)
	}
	ag := newTestAgent(t, stubSeq(replies))
	ag.MaxIter = 200
	res, err := ag.Run(context.Background(), "t7", "一直失败的任务", "", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 5 次失败后停下，CompletedSteps 应为 0（全失败），且不应跑满 200 轮。
	if res.CompletedSteps != 0 {
		t.Fatalf("全失败任务完成步数应为 0，实为 %d", res.CompletedSteps)
	}
	// 回复应为步骤汇总（无报告可综合，因为没用过调查类工具）。
	if !strings.Contains(res.ReplyDraft, "任务未完成") {
		t.Fatalf("卡住后应回步骤汇总，实为 %q", res.ReplyDraft)
	}
}

// CF 006 复现（P0-1）：两次都不是合法 JSON 时，任务直接失败，
// 不再把原话当答案收下标 done；reply.md 不应落盘。
func TestRunNonJSONFailsTask(t *testing.T) {
	ag := newTestAgent(t, stubSeq([]string{
		`{"action":"tool","tool_id":"test-echo","inputs":["hello"]}`,
		`<p>这是直接输出的答案，不是 JSON</p>`,
		`<p>这是直接输出的答案，不是 JSON</p>`,
	}))
	_, err := ag.Run(context.Background(), "t6", "调 echo 然后直接给答案", "", "")
	if err == nil {
		t.Fatal("两次解析失败应返回错误，实为 nil")
	}
	if !strings.Contains(err.Error(), "两次解析失败") {
		t.Fatalf("错误应提“两次解析失败”，实为 %v", err)
	}
	if _, ferr := os.ReadFile(filepath.Join(ag.DataDir, "artifacts", "t6", "reply.md")); ferr == nil {
		t.Fatal("解析失败的任务不应落盘 reply.md")
	}
}

// 熔断后缺交付报告时，要综合一份 report.md 出来，不能只回步骤汇总。
// P0-3：只有 research 任务才综合报告；综合出来的报告同样只算停下（stopped），
// 完成与否由网关验货决定。
func TestRunMaxIterationsSynthesizesReport(t *testing.T) {
	ag := newTestAgent(t, stubSeq([]string{
		`{"action":"tool","tool_id":"cap-ops-echo","inputs":["hello"]}`,
		`{"action":"tool","tool_id":"cap-ops-echo","inputs":["world"]}`,
		"# 综合报告\n\n这是综合出来的分析结论。",
	}))
	ag.MaxIter = 2
	ag.TaskType = "research"
	res, err := ag.Run(context.Background(), "t5", "调查一个东西", "", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "stopped" {
		t.Fatalf("综合出报告后应为 stopped，实为 status=%s", res.Status)
	}
	if !strings.Contains(res.ReplyDraft, "综合报告") {
		t.Fatalf("回复应为综合报告，实为 %q", res.ReplyDraft)
	}
	if b, err := os.ReadFile(filepath.Join(ag.DataDir, "workspace", "t5", "report.md")); err != nil || !strings.Contains(string(b), "综合报告") {
		t.Fatalf("workspace/t5/report.md 应含综合报告：%v %q", err, string(b))
	}
	if b, err := os.ReadFile(filepath.Join(ag.DataDir, "artifacts", "t5", "reply.md")); err != nil || !strings.Contains(string(b), "综合报告") {
		t.Fatalf("artifacts/t5/reply.md 应含综合报告：%v %q", err, string(b))
	}
}

// 三层懒加载：expand 导航后调工具再交活；expand 本身不算干活，
// 但产出新列表，算进展（重置 noProgress）；只读空转由 readOnlyStreak 拦截。
func TestRunExpandThenToolThenAnswer(t *testing.T) {
	ag := newTestAgent(t, stubSeq([]string{
		`{"action":"expand","category":"ops"}`,
		`{"action":"expand","category":"ops","subcategory":"shell"}`,
		`{"action":"tool","tool_id":"test-echo","inputs":["hello"]}`,
		`{"action":"answer","text":"echo 回来了"}`,
	}))
	res, err := ag.Run(context.Background(), "t9", "跑个 echo", "", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "stopped" || res.CompletedSteps != 1 {
		t.Fatalf("Status=%s CompletedSteps=%d, want stopped/1", res.Status, res.CompletedSteps)
	}
	if res.ReplyDraft != "echo 回来了" {
		t.Fatalf("ReplyDraft=%q", res.ReplyDraft)
	}
}

// expand 未知大类记为 expand_error，模型换招后仍能继续。
func TestRunExpandUnknownCategoryRecovers(t *testing.T) {
	ag := newTestAgent(t, stubSeq([]string{
		`{"action":"expand","category":"nope"}`,
		`{"action":"tool","tool_id":"test-echo","inputs":["hi"]}`,
		`{"action":"answer","text":"好了"}`,
	}))
	res, err := ag.Run(context.Background(), "t10", "跑个 echo", "", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "stopped" {
		t.Fatalf("Status=%s, want stopped", res.Status)
	}
}

// parseAction 接受 expand，缺 category 报错。
func TestParseActionExpand(t *testing.T) {
	if a, err := parseAction(`{"action":"expand","category":"ops"}`); err != nil || a.Category != "ops" {
		t.Fatalf("expand 解析失败: %v %+v", err, a)
	}
	if a, err := parseAction(`{"action":"expand","category":"ops","subcategory":"shell"}`); err != nil || a.Subcategory != "shell" {
		t.Fatalf("expand 子类解析失败: %v %+v", err, a)
	}
	if _, err := parseAction(`{"action":"expand"}`); err == nil {
		t.Fatal("expand 缺 category 应报错")
	}
}

// 展开列表只展示一次：展开后的下一轮能看到完整列表，再往后只留摘要。
// 否则每轮都把几千字的列表重发一遍，提示词爆炸（2026-11-18 实测：
// 同类比标题任务提示词 16,280 → 168,774 token，主因就是列表在 history 里全文累积）。
func TestExpandListingShownOnce(t *testing.T) {
	var tails []string
	call := func(taskID, frozen, tail string) (string, int64, int64, error) {
		tails = append(tails, tail)
		switch len(tails) {
		case 1:
			return `{"action":"expand","category":"ops"}`, 100, 20, nil
		case 2:
			return `{"action":"expand","category":"ops","subcategory":"shell"}`, 100, 20, nil
		case 3:
			return `{"action":"tool","tool_id":"test-echo","inputs":["hi"]}`, 100, 20, nil
		default:
			return `{"action":"answer","text":"好了"}`, 100, 20, nil
		}
	}
	ag := newTestAgent(t, call)
	res, err := ag.Run(context.Background(), "t11", "跑个 echo", "", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "stopped" {
		t.Fatalf("Status=%s, want stopped", res.Status)
	}
	if len(tails) < 4 {
		t.Fatalf("只收到 %d 轮尾巴，want >= 4", len(tails))
	}
	// 第 2 轮：大类说明出现一次。
	if !strings.Contains(tails[1], "Operations tools for tests") {
		t.Fatal("第 2 轮尾巴应含展开的大类说明")
	}
	// 第 3 轮：大类说明已消失，工具列表出现一次。
	if strings.Contains(tails[2], "Operations tools for tests") {
		t.Fatal("第 3 轮尾巴不应再出现大类说明")
	}
	if !strings.Contains(tails[2], "test-echo 占位") {
		t.Fatal("第 3 轮尾巴应含展开的工具列表")
	}
	// 第 4 轮：工具列表也消失，只剩带 ID 的摘要。
	if strings.Contains(tails[3], "test-echo 占位") {
		t.Fatal("第 4 轮尾巴不应再出现工具列表")
	}
	if !strings.Contains(tails[3], "展开 ops/shell") {
		t.Fatal("第 4 轮尾巴应保留展开摘要")
	}
}

// ---- 只读补搜内置工具（线 B 第二阶段第 4 棒） ----

// testMemoryStore 建最小记忆 + 简介主库（1 篇 CF 注册简介，
// 词法腿可命中；embed 为 nil，语义腿跳过，与生产停用状态一致）。
func testMemoryStore(t *testing.T) *memory.Store {
	t.Helper()
	root := t.TempDir()
	st, err := memory.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	content := "# CF 注册流程\n\nSummary: 用 Camoufox 过 Cloudflare 注册验证。\n\n用 Camoufox 打开 Cloudflare 注册页，过 Turnstile 验证，邮箱验证后取 Global API Key。Cloudflare 新账号注册流程完整记录。\n"
	if err := os.WriteFile(filepath.Join(root, "wiki", "entities", "cf-test.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := memory.OpenSummaryStore(filepath.Join(t.TempDir(), "summaries.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sum.Close() })
	if err := sum.UpsertDoc(memory.SummaryDoc{
		DocID: "W-0001", Layer: "wiki", Category: "entities", Title: "CF 注册流程",
		Summary:     "用 Camoufox 过 Cloudflare 注册验证，取 Global API Key。",
		Triggers:    "CF注册，Turnstile",
		SummaryPath: "简介/wiki/entities/cf-test.md", SourcePath: "wiki/entities/cf-test.md",
		SourceHash: "aa", SummaryHash: "bb", Status: "ok",
	}); err != nil {
		t.Fatal(err)
	}
	st.WithSummaryStore(sum, nil)
	return st
}

// TestRunMemoryToolsBuiltin：search_memory + read_memory 在主循环里
// 进程内执行（不在注册表里，ResolveTool 不应拦），卡片与全文依次
// 进下一轮尾巴；冻结提示词里应有这两个工具的用法说明。
func TestRunMemoryToolsBuiltin(t *testing.T) {
	mem := testMemoryStore(t)
	var tails, frozens []string
	call := func(taskID, frozen, tail string) (string, int64, int64, error) {
		tails = append(tails, tail)
		frozens = append(frozens, frozen)
		switch len(tails) {
		case 1:
			return `{"action":"tool","tool_id":"search_memory","inputs":["Cloudflare 注册"]}`, 100, 20, nil
		case 2:
			return `{"action":"tool","tool_id":"read_memory","inputs":["W-0001"]}`, 100, 20, nil
		default:
			return `{"action":"answer","text":"已查到 CF 注册流程"}`, 100, 20, nil
		}
	}
	ag := newTestAgent(t, call)
	ag.Mem = mem
	res, err := ag.Run(context.Background(), "t-mem", "查 CF 注册流程", "", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "stopped" {
		t.Fatalf("应 stopped，实为 %s", res.Status)
	}
	if len(tails) < 3 {
		t.Fatalf("应至少 3 轮，实为 %d 轮", len(tails))
	}
	if !strings.Contains(frozens[0], "search_memory") || !strings.Contains(frozens[0], "read_memory") {
		t.Fatalf("冻结提示词应说明两个内置工具")
	}
	// 第 2 轮的尾巴应含 search 回来的卡片（W-0001）。
	if !strings.Contains(tails[1], "W-0001") {
		t.Fatalf("第 2 轮尾巴应含 search_memory 卡片 W-0001：%q", tails[1])
	}
	// 第 3 轮的尾巴应含 read 回来的全文关键句。
	if !strings.Contains(tails[2], "邮箱验证后取 Global API Key") {
		t.Fatalf("第 3 轮尾巴应含 read_memory 全文：%q", tails[2])
	}
}

// TestRunMemoryToolsBudgetCap：同一任务内第 4 次 search_memory 返回
// 超限提示（不报错、不静默），模型看到提示后交活。
func TestRunMemoryToolsBudgetCap(t *testing.T) {
	mem := testMemoryStore(t)
	var tails []string
	call := func(taskID, frozen, tail string) (string, int64, int64, error) {
		tails = append(tails, tail)
		if len(tails) <= 4 {
			return `{"action":"tool","tool_id":"search_memory","inputs":["Cloudflare 注册"]}`, 100, 20, nil
		}
		return `{"action":"answer","text":"查够了"}`, 100, 20, nil
	}
	ag := newTestAgent(t, call)
	ag.Mem = mem
	res, err := ag.Run(context.Background(), "t-mem-cap", "反复查 CF 注册", "", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "stopped" {
		t.Fatalf("应 stopped，实为 %s", res.Status)
	}
	if len(tails) < 5 {
		t.Fatalf("应至少 5 轮，实为 %d 轮", len(tails))
	}
	// 第 5 轮的尾巴（含第 4 次调用结果）应有超限提示。
	if !strings.Contains(tails[4], "已用完 3 次") {
		t.Fatalf("第 5 轮尾巴应含超限提示：%q", tails[4])
	}
}

// TestRunMemoryToolsNoStore：Mem 未接线时两个工具返回失败信息，
// 模型看到错误换招，不崩、不卡死。
func TestRunMemoryToolsNoStore(t *testing.T) {
	var tails []string
	call := func(taskID, frozen, tail string) (string, int64, int64, error) {
		tails = append(tails, tail)
		if len(tails) == 1 {
			return `{"action":"tool","tool_id":"search_memory","inputs":["Cloudflare"]}`, 100, 20, nil
		}
		return `{"action":"answer","text":"记忆未接线，我换别的办法"}`, 100, 20, nil
	}
	ag := newTestAgent(t, call)
	res, err := ag.Run(context.Background(), "t-mem-nil", "查 CF 注册", "", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "stopped" {
		t.Fatalf("应 stopped，实为 %s", res.Status)
	}
	if len(tails) < 2 || !strings.Contains(tails[1], "记忆未接线") {
		t.Fatalf("第 2 轮尾巴应含“记忆未接线”：%v", tails)
	}
}

// P0-1 截断拦截：模型输出被截断（finish_reason=length）时，本轮整轮作废——
// 已发出的工具调用不执行，截断原因回填给模型，下一轮重发。
func TestTruncationVoidsRound(t *testing.T) {
	var tails []string
	calls := 0
	ag := newTestAgent(t, stubSeq([]string{`{"action":"answer","text":"兜底"}`}))
	ag.CallWithMeta = func(taskID, frozen, tail string) (string, string, int64, int64, error) {
		calls++
		tails = append(tails, tail)
		if calls == 1 {
			// 第一轮：带工具调用的输出被截断。
			return `{"action":"tool","tool_id":"test-echo","inputs":["hello"]}`, "length", 100, 20, nil
		}
		return `{"action":"answer","text":"重发后交活"}`, "stop", 100, 20, nil
	}
	res, err := ag.Run(context.Background(), "t-trunc", "跑个 echo", "", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "stopped" {
		t.Fatalf("应 stopped，实为 %s", res.Status)
	}
	if res.CompletedSteps != 0 {
		t.Fatalf("被截断的工具调用不应执行，完成步数应为 0，实为 %d", res.CompletedSteps)
	}
	if _, ferr := os.ReadFile(filepath.Join(ag.DataDir, "artifacts", "t-trunc", "step-1.txt")); ferr == nil {
		t.Fatal("被截断的轮次不应落盘 step-1.txt")
	}
	if len(tails) < 2 || !strings.Contains(tails[1], "截断") {
		t.Fatalf("第 2 轮尾巴应含截断反馈：%v", tails)
	}
}

func TestRepeatGuardRefusesThirdRun(t *testing.T) {
	ag := newTestAgent(t, stubSeq([]string{
		`{"action":"tool","tool_id":"test-false","inputs":[]}`,
	}))
	res, err := ag.Run(context.Background(), "t-rep", "一直调一个坏工具", "", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "stalled" {
		t.Fatalf("应 stalled，实为 status=%s", res.Status)
	}
	if n := strings.Count(res.ReplyDraft, "status=failure"); n != 2 {
		t.Fatalf("坏工具应只真实执行 2 次，ReplyDraft 里 status=failure 出现 %d 次：%q", n, res.ReplyDraft)
	}
	if n := strings.Count(res.ReplyDraft, "status=repeat_rejected"); n < 1 {
		t.Fatalf("第 3 次起应被拒绝执行，ReplyDraft 里应有 repeat_rejected：%q", res.ReplyDraft)
	}
}

// P0-3：执行任务即使调过调查类工具（cap-ops-echo），交活时也不综合报告——
// 报告是调研任务的交付，不是执行任务的遮羞布。
func TestExecuteTaskDoesNotSynthesizeReport(t *testing.T) {
	ag := newTestAgent(t, stubSeq([]string{
		`{"action":"tool","tool_id":"cap-ops-echo","inputs":["hello"]}`,
		`{"action":"tool","tool_id":"cap-ops-echo","inputs":["world"]}`,
		`{"action":"answer","text":"查完了"}`,
	}))
	res, err := ag.Run(context.Background(), "t-nosyn", "查一个东西", "", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "stopped" {
		t.Fatalf("应 stopped，实为 %s", res.Status)
	}
	if _, ferr := os.ReadFile(filepath.Join(ag.DataDir, "workspace", "t-nosyn", "report.md")); ferr == nil {
		t.Fatal("执行任务不应综合出 report.md")
	}
}

// ---- P1 原生工具调用测试 ----

// toolCallMsg 造一条带一次原生工具调用的 assistant 消息。
func toolCallMsg(id, name, args string) llm.Message {
	var tc llm.ToolCall
	tc.ID = id
	tc.Type = "function"
	tc.Function.Name = name
	tc.Function.Arguments = args
	return llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{tc}}
}

// textMsg 造一条纯正文 assistant 消息（无 tool_calls = 请求停下）。
func textMsg(content string) llm.Message {
	return llm.Message{Role: "assistant", Content: content}
}

// toolsCallRecord 是原生 stub 一次调用收到的东西（断言用）。
type toolsCallRecord struct {
	tools   []llm.ToolDefinition
	history []llm.Message
	tail    string
}

// stubToolsSeq 按顺序返回多条 assistant 消息（用完后重复最后一条），
// 并记录每次调用收到的 tools/history/tail。frs 与 msgs 等长时指定
// 每轮的 finish_reason（可空）。
func stubToolsSeq(t *testing.T, msgs []llm.Message, frs []string, rec *[]toolsCallRecord) ModelCallToolsFunc {
	t.Helper()
	i := 0
	return func(taskID, frozen, tail string, tools []llm.ToolDefinition, history []llm.Message) (llm.Message, string, int64, int64, error) {
		cpTools := append([]llm.ToolDefinition(nil), tools...)
		cpHist := append([]llm.Message(nil), history...)
		*rec = append(*rec, toolsCallRecord{tools: cpTools, history: cpHist, tail: tail})
		if i >= len(msgs) {
			i = len(msgs) - 1
		}
		m := msgs[i]
		fr := ""
		if i < len(frs) {
			fr = frs[i]
		}
		i++
		return m, fr, 100, 20, nil
	}
}

func newNativeTestAgent(t *testing.T, call ModelCallToolsFunc, toolIDs []string) *Agent {
	t.Helper()
	rg := miniRegistry(t)
	led := openTestLedger(t)
	ex := &executor.Executor{Ledger: led}
	ag := &Agent{CallWithTools: call, Reg: rg, Led: led, Ex: ex, DataDir: t.TempDir(), SessionID: "test-session", MaxIter: 20}
	ag.ToolIDs = toolIDs
	return ag
}

// TestNativeToolThenAnswer：原生路径调一个工具再回正文交活，
// 状态是 stopped（不是 done），工具真跑了，正文落盘 reply.md。
func TestNativeToolThenAnswer(t *testing.T) {
	var rec []toolsCallRecord
	ag := newNativeTestAgent(t, stubToolsSeq(t, []llm.Message{
		toolCallMsg("call-1", "test-echo", `{"inputs":["hi"]}`),
		textMsg("好了，hi 已回显。"),
	}, nil, &rec), []string{"test-echo"})
	res, err := ag.Run(context.Background(), "t-native-1", "回显 hi", "", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "stopped" {
		t.Fatalf("Status=%s, want stopped", res.Status)
	}
	if res.CompletedSteps != 1 {
		t.Fatalf("CompletedSteps=%d, want 1", res.CompletedSteps)
	}
	if b, err := os.ReadFile(filepath.Join(ag.DataDir, "artifacts", "t-native-1", "step-1.txt")); err != nil || !strings.Contains(string(b), "hi") {
		t.Fatalf("step-1.txt 应含 hi: %v %q", err, string(b))
	}
	if b, err := os.ReadFile(filepath.Join(ag.DataDir, "artifacts", "t-native-1", "reply.md")); err != nil || !strings.Contains(string(b), "好了") {
		t.Fatalf("reply.md 应是正文答案: %v %q", err, string(b))
	}
	// 派单工具 test-echo 声明给了模型（另有 expand 与记忆工具）。
	found := false
	for _, td := range rec[0].tools {
		if td.Function.Name == "test-echo" {
			found = true
		}
	}
	if !found {
		t.Fatalf("首轮声明的工具里没有 test-echo: %+v", rec[0].tools)
	}
}

// TestNativeToolResultFedBack：工具结果以 tool 消息原样回填进下一轮，
// 模型在第二轮能看到 exit 码、状态与输出。
func TestNativeToolResultFedBack(t *testing.T) {
	var rec []toolsCallRecord
	ag := newNativeTestAgent(t, stubToolsSeq(t, []llm.Message{
		toolCallMsg("call-1", "test-echo", `{"inputs":["hello"]}`),
		textMsg("收到 hello。"),
	}, nil, &rec), []string{"test-echo"})
	if _, err := ag.Run(context.Background(), "t-native-2", "回显 hello", "", ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rec) < 2 {
		t.Fatalf("模型应被调 2 轮，实际 %d 轮", len(rec))
	}
	hist := rec[1].history
	// 首轮 user 消息（任务正文）也在历史里：user → assistant → tool。
	if len(hist) < 3 {
		t.Fatalf("第二轮历史应有 user+assistant+tool 三条，实际 %d 条", len(hist))
	}
	if hist[0].Role != "user" || !strings.Contains(hist[0].Content, "回显 hello") {
		t.Fatalf("历史第一条应是首轮 user 消息（任务正文）: %+v", hist[0])
	}
	if len(hist[1].ToolCalls) != 1 || hist[1].ToolCalls[0].ID != "call-1" {
		t.Fatalf("历史第二条应是带 tool_calls 的 assistant 消息: %+v", hist[1])
	}
	if hist[2].Role != "tool" || hist[2].ToolCallID != "call-1" {
		t.Fatalf("历史第三条应是 tool 消息（tool_call_id=call-1）: %+v", hist[2])
	}
	if !strings.Contains(hist[2].Content, "hello") || !strings.Contains(hist[2].Content, "exit=0") {
		t.Fatalf("tool 消息应含结果 hello 与 exit=0: %q", hist[2].Content)
	}
	// 原生路径的尾巴不含已跑步骤文字（结果在结构化历史里，不重发）。
	if strings.Contains(rec[1].tail, "已跑步骤") {
		t.Fatalf("原生尾巴不应含已跑步骤文字: %q", rec[1].tail)
	}
}

// TestNativeTruncationVoidsRound：原生路径截断同样整轮作废——
// 带 tool_calls 的截断回复不执行，工具不跑，模型用更短的回复重发。
func TestNativeTruncationVoidsRound(t *testing.T) {
	var rec []toolsCallRecord
	ag := newNativeTestAgent(t, stubToolsSeq(t, []llm.Message{
		toolCallMsg("call-1", "test-echo", `{"inputs":["hi"]}`),
		textMsg("重发：好了。"),
	}, []string{"length", ""}, &rec), []string{"test-echo"})
	res, err := ag.Run(context.Background(), "t-native-3", "回显 hi", "", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "stopped" {
		t.Fatalf("Status=%s, want stopped", res.Status)
	}
	if res.CompletedSteps != 0 {
		t.Fatalf("截断轮作废，CompletedSteps 应为 0，实际 %d", res.CompletedSteps)
	}
	if _, err := os.Stat(filepath.Join(ag.DataDir, "artifacts", "t-native-3", "step-1.txt")); err == nil {
		t.Fatalf("截断轮的工具调用不应执行，不应有 step-1.txt")
	}
	// 截断提醒进第二轮尾巴（用一次即清），第二轮模型能看到；
	// 首轮 user 消息（任务正文）已在历史里，记忆摘要不会因截断丢失。
	if !strings.Contains(rec[1].tail, "截断") {
		t.Fatalf("第二轮尾巴应含截断提醒: %q", rec[1].tail)
	}
	found := false
	for _, m := range rec[1].history {
		if m.Role == "user" && strings.Contains(m.Content, "回显 hi") {
			found = true
		}
	}
	if !found {
		t.Fatalf("第二轮历史应有首轮 user 消息（任务正文）: %+v", rec[1].history)
	}
}

// TestNativeBadArgsFedBack：tool_calls 参数解析失败不进执行，
// 直接回填一条错误 tool 消息，模型看到错误换招。
func TestNativeBadArgsFedBack(t *testing.T) {
	var rec []toolsCallRecord
	ag := newNativeTestAgent(t, stubToolsSeq(t, []llm.Message{
		toolCallMsg("call-1", "test-echo", `not-json`),
		textMsg("参数错了，算了。"),
	}, nil, &rec), []string{"test-echo"})
	res, err := ag.Run(context.Background(), "t-native-4", "回显", "", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "stopped" {
		t.Fatalf("Status=%s, want stopped", res.Status)
	}
	if res.CompletedSteps != 0 {
		t.Fatalf("参数错误的调用不应执行，CompletedSteps 应为 0，实际 %d", res.CompletedSteps)
	}
	if len(rec) < 2 {
		t.Fatalf("模型应被调 2 轮，实际 %d 轮", len(rec))
	}
	found := false
	for _, m := range rec[1].history {
		if m.Role == "tool" && m.ToolCallID == "call-1" && strings.Contains(m.Content, "解析失败") {
			found = true
		}
	}
	if !found {
		t.Fatalf("第二轮历史应有参数错误的 tool 消息: %+v", rec[1].history)
	}
}

// TestToolCallsToActions：tool_calls 转动作——tool、expand、
// 缺名、坏参数各走各的路。
func TestToolCallsToActions(t *testing.T) {
	mkTC := func(id, name, args string) llm.ToolCall {
		var tc llm.ToolCall
		tc.ID = id
		tc.Type = "function"
		tc.Function.Name = name
		tc.Function.Arguments = args
		return tc
	}
	tcs := []llm.ToolCall{
		mkTC("c1", "test-echo", `{"inputs":["a","b"]}`),
		mkTC("c2", "expand", `{"category":"ops","subcategory":"shell"}`),
		mkTC("c3", "test-echo", `broken`),
		mkTC("c4", "", `{}`),
	}
	actions, errs := toolCallsToActions(tcs)
	if len(actions) != 2 {
		t.Fatalf("actions=%d, want 2", len(actions))
	}
	if actions[0].ToolID != "test-echo" || len(actions[0].Inputs) != 2 || actions[0].ToolCallID != "c1" {
		t.Fatalf("第一个动作应是 test-echo(a,b)，带 c1: %+v", actions[0])
	}
	if actions[1].Action != "expand" || actions[1].Category != "ops" || actions[1].Subcategory != "shell" {
		t.Fatalf("第二个动作应是 expand ops/shell: %+v", actions[1])
	}
	if len(errs) != 2 || errs["c3"] == "" || errs["c4"] == "" {
		t.Fatalf("c3/c4 应各有一条参数错误: %+v", errs)
	}
}

// TestNativeExpandThenTool：原生路径展开导航后调工具——
// expand 的完整列表回填进 tool 消息（不是摘要），模型凭它挑工具。
func TestNativeExpandThenTool(t *testing.T) {
	var rec []toolsCallRecord
	ag := newNativeTestAgent(t, stubToolsSeq(t, []llm.Message{
		toolCallMsg("call-1", "expand", `{"category":"ops","subcategory":"shell"}`),
		toolCallMsg("call-2", "test-echo", `{"inputs":["from-expand"]}`),
		textMsg("展开后调到了。"),
	}, nil, &rec), nil)
	res, err := ag.Run(context.Background(), "t-native-5", "展开 ops 再回显", "", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "stopped" || res.CompletedSteps != 1 {
		t.Fatalf("Status=%s CompletedSteps=%d, want stopped/1", res.Status, res.CompletedSteps)
	}
	if len(rec) < 2 {
		t.Fatalf("模型应被调至少 2 轮，实际 %d 轮", len(rec))
	}
	// 第二轮历史里的 expand tool 消息应含完整列表（有 test-echo）。
	found := false
	for _, m := range rec[1].history {
		if m.Role == "tool" && m.ToolCallID == "call-1" && strings.Contains(m.Content, "test-echo") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expand 的 tool 消息应含完整工具列表: %+v", rec[1].history)
	}
	// 派单没给工具时，只声明 expand 与记忆工具（不声明 test-echo）。
	for _, td := range rec[0].tools {
		if td.Function.Name == "test-echo" {
			t.Fatalf("派单没给工具时不应声明 test-echo: %+v", rec[0].tools)
		}
	}
	// 展开子类成功后，第二轮的 tools 声明里应动态补上该子类的工具
	// （Gemini 审阅高危：不补的话模型调不了未声明的函数名）。
	found = false
	for _, td := range rec[1].tools {
		if td.Function.Name == "test-echo" {
			found = true
		}
	}
	if !found {
		t.Fatalf("展开 ops/shell 后，第二轮 tools 应动态补上 test-echo: %+v", rec[1].tools)
	}
}

// toolCallsMsg 造一条带多个 tool_calls 的 assistant 消息。
func toolCallsMsg(calls ...llm.ToolCall) llm.Message {
	return llm.Message{Role: "assistant", ToolCalls: calls}
}

// mkToolCall 造一个 tool_call。
func mkToolCall(id, name, args string) llm.ToolCall {
	var tc llm.ToolCall
	tc.ID = id
	tc.Type = "function"
	tc.Function.Name = name
	tc.Function.Arguments = args
	return tc
}

// TestNativeBatchRejectedBackfills：原生路径整包被拒（打包里含不可逆动作）
// 也要给每个 tool_call 回填结果再 continue——否则 msgHistory 里悬空
// tool_calls，下一轮调 API 直接报 400（Gemini 审阅高危）。
func TestNativeBatchRejectedBackfills(t *testing.T) {
	var rec []toolsCallRecord
	ag := newNativeTestAgent(t, stubToolsSeq(t, []llm.Message{
		toolCallsMsg(
			mkToolCall("call-1", "test-echo", `{"inputs":["hi"]}`),
			mkToolCall("call-2", "test-rm", `{"inputs":["somefile"]}`),
		),
		textMsg("打包被拒了，我单发。"),
	}, nil, &rec), []string{"test-echo", "test-rm"})
	res, err := ag.Run(context.Background(), "t-native-batch", "回显并删文件", "", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "stopped" {
		t.Fatalf("Status=%s, want stopped", res.Status)
	}
	// 两个调用都没执行（整包拒掉），不落盘 step 文件。
	if _, err := os.Stat(filepath.Join(ag.DataDir, "artifacts", "t-native-batch", "step-1.txt")); err == nil {
		t.Fatal("整包被拒，任何工具都不应执行，不应有 step-1.txt")
	}
	// 第二轮历史里两个 tool_call 都有回填（拒因），没有悬空调用。
	if len(rec) < 2 {
		t.Fatalf("模型应被调至少 2 轮，实际 %d 轮", len(rec))
	}
	seen := map[string]bool{}
	for _, m := range rec[1].history {
		if m.Role == "tool" && strings.Contains(m.Content, "不可逆") {
			seen[m.ToolCallID] = true
		}
	}
	if !seen["call-1"] || !seen["call-2"] {
		t.Fatalf("两个 tool_call 都应回填拒因，实际: %+v", rec[1].history)
	}
}

// TestNativeContentFilterBlocked：原生路径模型回复被内容过滤拦截或为空，
// 不当作交活——不写空 reply.md，记一轮无进展后重发。
func TestNativeContentFilterBlocked(t *testing.T) {
	var rec []toolsCallRecord
	ag := newNativeTestAgent(t, stubToolsSeq(t, []llm.Message{
		textMsg(""),
		textMsg("换个说法：好了。"),
	}, []string{"content_filter", ""}, &rec), []string{"test-echo"})
	res, err := ag.Run(context.Background(), "t-native-cf", "回显", "", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "stopped" {
		t.Fatalf("Status=%s, want stopped", res.Status)
	}
	if len(rec) < 2 {
		t.Fatalf("内容过滤后应重发，模型应被调至少 2 轮，实际 %d 轮", len(rec))
	}
	if b, err := os.ReadFile(filepath.Join(ag.DataDir, "artifacts", "t-native-cf", "reply.md")); err != nil || !strings.Contains(string(b), "换个说法") {
		t.Fatalf("reply.md 应是重发后的正文: %v %q", err, string(b))
	}
}

func TestRunResumedRedoReasonAppended(t *testing.T) {
	var rec []toolsCallRecord
	ag := newNativeTestAgent(t, stubToolsSeq(t, []llm.Message{
		textMsg("已按重干原因改好，文件已重写。"),
	}, nil, &rec), []string{"test-echo"})
	// 预置本步历史：1 条 user + 3 次工具调用（assistant+tool）= 7 条。
	sess := session.New(t.TempDir(), ag.Led)
	ag.Sess = sess
	ag.RunSessionID = "test-session"
	ag.RunStepNo = 1
	ag.RunRedoReason = "文件前缀不对：应为 Current date/time:，实际写了中文“日期：”"
	seed := []llm.Message{
		{Role: "user", Content: "写问候文件"},
		toolCallMsg("c1", "test-echo", `{"inputs":["a"]}`),
		{Role: "tool", ToolCallID: "c1", Content: "exit=0 a"},
		toolCallMsg("c2", "test-echo", `{"inputs":["b"]}`),
		{Role: "tool", ToolCallID: "c2", Content: "exit=0 b"},
		toolCallMsg("c3", "test-echo", `{"inputs":["c"]}`),
		{Role: "tool", ToolCallID: "c3", Content: "exit=0 c"},
	}
	if err := sess.AppendMessages("test-session", 1, "t-h1-redo", seed); err != nil {
		t.Fatal(err)
	}
	res, err := ag.Run(context.Background(), "t-h1-redo", "写问候文件", "", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "stopped" {
		t.Fatalf("Status=%s, want stopped", res.Status)
	}
	if !res.Resumed {
		t.Fatal("读回了 7 条历史，RunResult.Resumed 应为 true")
	}
	if len(rec) < 1 {
		t.Fatal("模型应至少被调 1 轮")
	}
	// 首轮历史里必须有一条含重干原因的 user 消息（在 7 条旧历史之后）。
	found := false
	for _, m := range rec[0].history {
		if m.Role == "user" && strings.Contains(m.Content, "文件前缀不对") {
			found = true
		}
	}
	if !found {
		t.Fatalf("首轮历史里应有含重干原因的 user 消息，实际历史：%+v", rec[0].history)
	}
	// 续跑后历史应增长：7 条旧历史 + 1 条重干提醒 + 1 条答复 = 9 条。
	restored, _, err := sess.LoadMessages("test-session", 1, "t-h1-redo")
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 9 {
		t.Fatalf("续跑后历史应为 9 条（7 旧 + 重干提醒 + 答复），实际 %d 条", len(restored))
	}
}

// TestRunNativeAnswerPersisted（H-1 回归）：原生路径模型不调工具
// 只回正文交活时，答复消息必须追加进 msgHistory 并落盘——
// 否则答复从不存盘，下次续跑读回的条数纹丝不动（prod-real-002
// 两次重试均 7 条的根因之一）。
func TestRunNativeAnswerPersisted(t *testing.T) {
	var rec []toolsCallRecord
	ag := newNativeTestAgent(t, stubToolsSeq(t, []llm.Message{
		textMsg("做完了，文件在指定位置。"),
	}, nil, &rec), []string{"test-echo"})
	sess := session.New(t.TempDir(), ag.Led)
	ag.Sess = sess
	ag.RunSessionID = "test-session"
	ag.RunStepNo = 1
	res, err := ag.Run(context.Background(), "t-h1-ans", "写问候文件", "", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "stopped" {
		t.Fatalf("Status=%s, want stopped", res.Status)
	}
	// 历史应有 2 条：首轮 user（任务正文）+ 答复 assistant。
	restored, _, err := sess.LoadMessages("test-session", 1, "t-h1-ans")
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 2 {
		t.Fatalf("交活后历史应为 2 条（user + 答复），实际 %d 条：%+v", len(restored), restored)
	}
	if restored[1].Role != "assistant" || !strings.Contains(restored[1].Content, "做完了") {
		t.Fatalf("历史第 2 条应是答复消息，实际 %+v", restored[1])
	}
}

func TestGatewayReadOnlyByNamePreserved(t *testing.T) {
	readonly := []string{"expand", "search_memory", "read_memory", "cap-ops-python", "verify-cmd", "cap-fetch-tvly", "cap-fetch-yt-dlp", "cap-ops-lean-ctx", "cap-design-ui-visual", "cap-seo-geo-seo-aeo"}
	for _, id := range readonly {
		if !IsReadOnlyToolByName(id) {
			t.Fatalf("IsReadOnlyToolByName(%s) 应为 true（网关口径），实为 false", id)
		}
	}
	// cap-code-git 不在名字口径的只读集合里（前缀表是 "cap-code-rg" 而非
	// "cap-code-"），网关口径下算实质调用。
	notReadonly := []string{"cap-ops-node", "cap-ops-ffmpeg", "cap-ops-docker-init", "cap-code-git", "test-echo"}
	for _, id := range notReadonly {
		if IsReadOnlyToolByName(id) {
			t.Fatalf("IsReadOnlyToolByName(%s) 应为 false，实为 true", id)
		}
	}
	// 未知工具仍按老办法判（不匹配前缀/开关）。
	if IsReadOnlyToolByName("some-new-tool-xyz") {
		t.Fatalf("未知工具 some-new-tool-xyz 应不算只读，实为 true")
	}
}

// TestLongOutputToolNotTruncatedAt3000（M4 中期 + 2026-11-26 放宽）：
// cap-ops-python 的 5000 字输出应全文进下一轮尾巴的输出部分——
// 旧上限 3000 字会把它截断，新上限 20000 字放行；输出部分不应出现截断提示。
func TestLongOutputToolNotTruncatedAt3000(t *testing.T) {
	long := strings.Repeat("甲", 5000) + "ENDMARKER-20000"
	var tails []string
	ag := newTestAgent(t, nil)
	ag.Call = func(taskID, frozen, tail string) (string, int64, int64, error) {
		tails = append(tails, tail)
		if len(tails) == 1 {
			return `{"action":"tool","tool_id":"cap-ops-python","inputs":["` + long + `"]}`, 100, 20, nil
		}
		return `{"action":"answer","text":"读完了"}`, 100, 20, nil
	}
	res, err := ag.Run(context.Background(), "t-m4-long", "读长文档", "", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "stopped" {
		t.Fatalf("应 stopped，实为 %s", res.Status)
	}
	if len(tails) < 2 {
		t.Fatalf("模型应至少被调 2 轮，实际 %d 轮", len(tails))
	}
	out := outputPart(t, tails[1])
	if !strings.Contains(out, "ENDMARKER-20000") {
		t.Fatalf("cap-ops-python 的 5000 字输出应全文进下一轮尾巴的输出部分，输出部分长度 %d 字", len([]rune(out)))
	}
	if strings.Contains(out, "输出已截到") {
		t.Fatalf("cap-ops-python 输出 5000 字 < 20000 上限，输出部分不应出现截断提示")
	}
}

// TestOrdinaryToolStillTruncatedAt6000（M4 中期对照 + 2026-11-26 放宽）：
// 普通工具保持 6000 字上限——test-echo 的 7000 字回显应被截断，尾标进不了
// 下一轮尾巴的输出部分，且应出现“输出已截到 6000 字”提示。
func TestOrdinaryToolStillTruncatedAt6000(t *testing.T) {
	long := strings.Repeat("乙", 7000) + "ENDMARKER-6000"
	var tails []string
	ag := newTestAgent(t, nil)
	ag.Call = func(taskID, frozen, tail string) (string, int64, int64, error) {
		tails = append(tails, tail)
		if len(tails) == 1 {
			return `{"action":"tool","tool_id":"test-echo","inputs":["` + long + `"]}`, 100, 20, nil
		}
		return `{"action":"answer","text":"读完了"}`, 100, 20, nil
	}
	res, err := ag.Run(context.Background(), "t-m4-ord", "普通工具长输出", "", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "stopped" {
		t.Fatalf("应 stopped，实为 %s", res.Status)
	}
	if len(tails) < 2 {
		t.Fatalf("模型应至少被调 2 轮，实际 %d 轮", len(tails))
	}
	out := outputPart(t, tails[1])
	if strings.Contains(out, "ENDMARKER-6000") {
		t.Fatalf("test-echo 的 7000 字输出应被截到 6000 字，尾标不应进下一轮尾巴的输出部分")
	}
	if !strings.Contains(out, "输出已截到 6000 字") {
		t.Fatalf("test-echo 输出被截时，输出部分应有“输出已截到 6000 字”提示")
	}
}

// outputPart 取尾巴里第一处“结果：”后面的输出部分（M4 单测用）：
// 尾巴同时印着工具输入参数，直接整条搜会把输入里的尾标也算进来。
func outputPart(t *testing.T, tail string) string {
	t.Helper()
	idx := strings.Index(tail, "结果：")
	if idx < 0 {
		t.Fatalf("尾巴里找不到“结果：”，尾巴：%q", tail)
	}
	return tail[idx+len("结果："):]
}

// TestRunMemoryToolSearchTip：search_memory 返回的卡片末尾应带读全文 Tip，
// 告诉模型简介只是卡片、动手前要调 read_memory 读全文。
func TestRunMemoryToolSearchTip(t *testing.T) {
	mem := testMemoryStore(t)
	ag := newTestAgent(t, nil)
	ag.Mem = mem
	res := ag.runMemoryTool(Action{Action: "tool", ToolID: "search_memory", Inputs: []string{"Cloudflare 注册"}})
	if res.ExitCode != 0 {
		t.Fatalf("search_memory 应成功，实为 exit=%d: %s", res.ExitCode, res.StdoutFull)
	}
	if !strings.Contains(res.StdoutFull, "W-0001") {
		t.Fatalf("结果应含卡片 W-0001：%q", res.StdoutFull)
	}
	if !strings.Contains(res.StdoutFull, "以上仅为简介卡片，动手前请调用 read_memory(doc_id) 读取完整正文") {
		t.Fatalf("结果末尾应有读全文 Tip：%q", res.StdoutFull)
	}
}

// TestInjectMustReadMemories：must_read_memories 里的编号开工时由代码
// 直接读全文拼进摘要，计入同一 3 次读预算。
func TestInjectMustReadMemories(t *testing.T) {
	mem := testMemoryStore(t)
	budget := &memory.SearchBudget{}
	summary := "相关记忆摘要：\n- 【W-0001】CF 注册流程：用 Camoufox 过 Cloudflare 注册验证，取 Global API Key。\n"
	out := injectMustReadMemories(mem, budget, []string{"W-0001"}, summary)
	if !strings.Contains(out, "必读记忆全文（W-0001）") {
		t.Fatalf("应含必读记忆全文标记：%q", out)
	}
	if !strings.Contains(out, "邮箱验证后取 Global API Key") {
		t.Fatalf("应含 W-0001 全文关键句：%q", out)
	}
	if _, read := budget.Counts(); read != 1 {
		t.Fatalf("读预算应记 1 次，实为 %d", read)
	}
}

// TestInjectMustReadMemoriesSkipsMissing：读不到的编号（无此文档）
// 不注入、原样返回摘要，不报错、不崩。
func TestInjectMustReadMemoriesSkipsMissing(t *testing.T) {
	mem := testMemoryStore(t)
	budget := &memory.SearchBudget{}
	out := injectMustReadMemories(mem, budget, []string{"W-9999"}, "摘要")
	if strings.Contains(out, "必读记忆全文") {
		t.Fatalf("读不到的编号不应注入全文：%q", out)
	}
	if out != "摘要" {
		t.Fatalf("读不到时应原样返回摘要：%q", out)
	}
}

// TestInjectMustReadMemoriesCapsAtTwo：最多只读前 2 个编号。
func TestInjectMustReadMemoriesCapsAtTwo(t *testing.T) {
	mem := testMemoryStore(t)
	budget := &memory.SearchBudget{}
	out := injectMustReadMemories(mem, budget, []string{"W-0001", "W-0001", "W-0001"}, "摘要")
	if _, read := budget.Counts(); read != 2 {
		t.Fatalf("最多读 2 次，实为 %d", read)
	}
	if strings.Count(out, "必读记忆全文（W-0001）") != 2 {
		t.Fatalf("应注入 2 次全文，实际：%q", out)
	}
}

// TestRunMustReadMemoriesInjected：Run 开工时 MustReadMemories 里的
// 编号由代码自动读全文，首轮尾巴里就有全文，模型不用自己调工具。
func TestRunMustReadMemoriesInjected(t *testing.T) {
	mem := testMemoryStore(t)
	var tails []string
	call := func(taskID, frozen, tail string) (string, int64, int64, error) {
		tails = append(tails, tail)
		return `{"action":"answer","text":"已按剧本做完"}`, 100, 20, nil
	}
	ag := newTestAgent(t, call)
	ag.Mem = mem
	ag.MustReadMemories = []string{"W-0001"}
	res, err := ag.Run(context.Background(), "t-mustread", "注册 CF 账号", "", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "stopped" {
		t.Fatalf("应 stopped，实为 %s", res.Status)
	}
	if len(tails) < 1 || !strings.Contains(tails[0], "必读记忆全文（W-0001）") {
		t.Fatalf("首轮尾巴应含必读记忆全文：%v", tails)
	}
	if !strings.Contains(tails[0], "邮箱验证后取 Global API Key") {
		t.Fatalf("首轮尾巴应含 W-0001 全文关键句：%q", tails[0])
	}
}
