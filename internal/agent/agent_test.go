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
	"007-super-agent/internal/registry"
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
	}
	var ents strings.Builder
	ents.WriteString(`{"version":"v1.1-test","generated":"2026-11-18","total":4,"categories":[{"id":"fetch","name":"Fetch","description":"Fetch tools for tests."},{"id":"code","name":"Code","description":"Code tools for tests."},{"id":"seo","name":"SEO","description":"SEO tools for tests."},{"id":"design","name":"Design","description":"Design tools for tests."},{"id":"security","name":"Security","description":"Security tools for tests."},{"id":"runtime","name":"Runtime","description":"Runtime tools for tests."},{"id":"media","name":"Media","description":"Media tools for tests."},{"id":"ops","name":"Operations","description":"Operations tools for tests. Use this category for shell commands and test tools."},{"id":"memory","name":"Memory","description":"Memory tools for tests."}],"subcategories":[{"category":"ops","id":"shell","description":"Shell commands and test tools. Use echo and true/false here."}],"entries":[`)
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
	if res.Status != "done" || res.CompletedSteps != 1 {
		t.Fatalf("应 done 且 1 步成功，实为 status=%s steps=%d", res.Status, res.CompletedSteps)
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
	if res.Status != "done" || res.CompletedSteps != 1 {
		t.Fatalf("应 done 且 1 步成功（坏工具不计），实为 status=%s steps=%d", res.Status, res.CompletedSteps)
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
	if res.Status != "done" || res.CompletedSteps != 0 {
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

// 两次都不是合法 JSON 时，把原话当答案收下（不丢掉已调查的成果），交给监工去审。
func TestRunNonJSONTreatedAsAnswer(t *testing.T) {
	ag := newTestAgent(t, stubSeq([]string{
		`{"action":"tool","tool_id":"test-echo","inputs":["hello"]}`,
		`<p>这是直接输出的答案，不是 JSON</p>`,
		`<p>这是直接输出的答案，不是 JSON</p>`,
	}))
	res, err := ag.Run(context.Background(), "t6", "调 echo 然后直接给答案", "", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "done" {
		t.Fatalf("非 JSON 答案应被收下并 done，实为 status=%s", res.Status)
	}
	if !strings.Contains(res.ReplyDraft, "直接输出的答案") {
		t.Fatalf("回复应含模型原话，实为 %q", res.ReplyDraft)
	}
}

// 熔断后缺交付报告时，要综合一份 report.md 出来，不能只回步骤汇总。
func TestRunMaxIterationsSynthesizesReport(t *testing.T) {
	ag := newTestAgent(t, stubSeq([]string{
		`{"action":"tool","tool_id":"cap-ops-echo","inputs":["hello"]}`,
		`{"action":"tool","tool_id":"cap-ops-echo","inputs":["world"]}`,
		"# 综合报告\n\n这是综合出来的分析结论。",
	}))
	ag.MaxIter = 2
	res, err := ag.Run(context.Background(), "t5", "调查一个东西", "", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "done" {
		t.Fatalf("综合出报告后应为 done，实为 status=%s", res.Status)
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
// 但记为成功步骤（导航是进展，不触发卡住判定）。
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
	if res.Status != "done" || res.CompletedSteps != 1 {
		t.Fatalf("Status=%s CompletedSteps=%d, want done/1", res.Status, res.CompletedSteps)
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
	if res.Status != "done" {
		t.Fatalf("Status=%s, want done", res.Status)
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
