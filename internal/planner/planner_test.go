package planner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"007-super-agent/internal/executor"
	"007-super-agent/internal/ledger"
	"007-super-agent/internal/registry"
)

// miniRegistry 在临时目录建一个 4 条目的迷你注册表：
// test-echo（echo）、test-true（true）、test-false（false，exit 1）、
// test-rm（rm，不可逆动作测试用，监工 HALT 时不真跑）。
// 含 L1/L2/L3，ResolveTool 能完整走通；只在临时目录写文件，
// 不动 step1/ 任何文件。
func miniRegistry(t *testing.T) (string, *registry.Registry) {
	t.Helper()
	dst := t.TempDir()
	entries := []struct{ id, ep, contract string }{
		{"test-echo", "echo", "exit 0"},
		{"test-true", "true", "exit 0"},
		{"test-false", "false", "exit 1"},
		{"test-rm", "rm", "exit 1"},
	}
	var ents strings.Builder
	ents.WriteString(`{"version":"v1.1-test","generated":"2026-11-18","total":4,"entries":[`)
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
	if err := os.MkdirAll(filepath.Join(dst, "index", "L3"), 0o755); err != nil {
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
	for _, e := range entries {
		content := "# " + e.id + "\n\n输入：无\n\n步骤：跑命令\n\n验收：" + e.contract + "\n"
		if err := os.WriteFile(filepath.Join(dst, "index", "L3", e.id+".md"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rg, err := registry.Load(dst)
	if err != nil {
		t.Fatalf("load mini registry: %v", err)
	}
	return dst, rg
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

// stubAdvisor 是监工桩（与 executor_test.go 同口径）：只按预设 verdict 回。
type stubAdvisor struct {
	verdict executor.Verdict
	note    string
	calls   []executor.Trigger
}

func (s *stubAdvisor) CollectEvidenceBundle(task executor.Task, trigger executor.Trigger) (executor.Evidence, error) {
	return executor.Evidence{TaskText: task.Text, Draft: task.Draft}, nil
}

func (s *stubAdvisor) Check(evidence executor.Evidence, trigger executor.Trigger) (executor.CheckResult, error) {
	s.calls = append(s.calls, trigger)
	return executor.CheckResult{Verdict: s.verdict, Note: s.note}, nil
}

// newTestPlanner 建一个测试用规划器（桩模型 + 迷你注册表 + 临时账本）。
func newTestPlanner(t *testing.T, planCall, verifyCall ModelCallFunc, adv executor.Advisor) (*Planner, *ledger.Ledger) {
	t.Helper()
	_, rg := miniRegistry(t)
	led := openTestLedger(t)
	ex := &executor.Executor{Ledger: led, Advisor: adv}
	dataDir := t.TempDir()
	return New(planCall, verifyCall, rg, led, ex, dataDir, "test-session", false), led
}

// stubJSON 返回固定 JSON 回复的桩（token 数固定 100/20，不记费用）。
func stubJSON(reply string) ModelCallFunc {
	return func(taskID, frozen, tail string) (string, int64, int64, error) {
		return reply, 100, 20, nil
	}
}

// stubSeq 按顺序返回多份回复的桩（重规划测试用）。
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

// approveJSON 生成 APPROVE 核验回复（四个问题齐全）。
func approveJSON() string {
	return `{"verdict":"APPROVE","findings":[
{"question":"necessity","text":"已检查合并可能性：各步不能合并，因为后一步依赖前一步的产物"},
{"question":"rationality","text":"依赖成立，无漏步"},
{"question":"reasons","text":"无不合理处"},
{"question":"suggestion","text":"按原表执行"}], "revised_plan":[]}`
}

// stepsJSON 生成 n 步的规划回复（全用 test-echo，inputs ["hello"]）。
func stepsJSON(n int) string {
	steps := make([]Step, n)
	for i := range steps {
		steps[i] = Step{StepNo: i + 1, ToolID: "test-echo", Inputs: []string{"hello"},
			ExpectedOutput: "输出 hello", EstimatedTokens: 100}
	}
	b, _ := json.Marshal(map[string]any{"steps": steps})
	return string(b)
}

// reviseJSON 生成 REVISE 核验回复（修订版为给定步骤表）。
func reviseJSON(steps []Step) string {
	b, _ := json.Marshal(map[string]any{
		"verdict": "REVISE",
		"findings": []Finding{
			{Question: "necessity", Text: "步骤 3、7、11 是废步，可以合并"},
			{Question: "rationality", Text: "依赖成立，无漏步"},
			{Question: "reasons", Text: "3 步废步：只做重复检查，无产物"},
			{Question: "suggestion", Text: "合并废步后的 9 步表见 revised_plan"},
		},
		"revised_plan": steps,
	})
	return string(b)
}

// TestPlanTwoSteps：两步任务应拆 2 步，核验 APPROVE，账本记一行 APPROVE。
func TestPlanTwoSteps(t *testing.T) {
	pl, led := newTestPlanner(t, stubJSON(stepsJSON(2)), stubJSON(approveJSON()), nil)
	plan, ver, err := pl.PlanAndVerify("t-2step", "先喊一声再喊一声", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 2 || plan.Rounds != 0 || ver.Verdict != VerdictApprove {
		t.Fatalf("plan = %d steps rounds=%d verdict=%s, want 2/0/APPROVE", len(plan.Steps), plan.Rounds, ver.Verdict)
	}
	if plan.PlanID == "" {
		t.Fatal("plan_id 为空")
	}
	rows, err := led.ListPlanRuns("t-2step", 5)
	if err != nil || len(rows) != 1 || rows[0].Verdict != "APPROVE" || rows[0].Steps != 2 {
		t.Fatalf("plan_runs = %+v, %v, want 1 行 APPROVE/2 步", rows, err)
	}
}

// TestVerifyKeepsFifteenSteps：十五步任务必须完整保留 15 步，核验不许砍。
func TestVerifyKeepsFifteenSteps(t *testing.T) {
	pl, _ := newTestPlanner(t, stubJSON(stepsJSON(15)), stubJSON(approveJSON()), nil)
	plan, _, err := pl.PlanAndVerify("t-15step", "十五步的活", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 15 {
		t.Fatalf("plan = %d steps, want 15（核验不许砍）", len(plan.Steps))
	}
	for i, s := range plan.Steps {
		if s.StepNo != i+1 {
			t.Fatalf("步骤编号断了：steps[%d].StepNo=%d", i, s.StepNo)
		}
	}
}

// TestVerifyTrimsBloatedPlan：12 步里塞 3 步废步，核验必须指出合并，
// 修订版 9 步，第二轮核验 APPROVE 后执行 9 步。
func TestVerifyTrimsBloatedPlan(t *testing.T) {
	revised := make([]Step, 9)
	for i := range revised {
		revised[i] = Step{StepNo: i + 1, ToolID: "test-echo", Inputs: []string{"hello"},
			ExpectedOutput: "输出 hello", EstimatedTokens: 100}
	}
	verifySeq := stubSeq([]string{reviseJSON(revised), approveJSON()})
	pl, _ := newTestPlanner(t, stubJSON(stepsJSON(12)), verifySeq, nil)
	plan, ver, err := pl.PlanAndVerify("t-bloated", "臃肿的活", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 9 || plan.Rounds != 1 || ver.Verdict != VerdictApprove {
		t.Fatalf("plan = %d steps rounds=%d verdict=%s, want 9/1/APPROVE", len(plan.Steps), plan.Rounds, ver.Verdict)
	}
}

// TestVerifyRejectsUnknownTool：步骤用了不存在的工具，模型说 APPROVE
// 也要被代码实核打回（降级为 REVISE，点名坏工具）。
func TestVerifyRejectsUnknownTool(t *testing.T) {
	pl, _ := newTestPlanner(t, stubJSON(""), stubJSON(approveJSON()), nil)
	bad := &Plan{TaskID: "t-badtool", TaskText: "坏工具", Steps: []Step{
		{StepNo: 1, ToolID: "test-echo", Inputs: []string{"hi"}, ExpectedOutput: "x", EstimatedTokens: 10},
		{StepNo: 2, ToolID: "cap-no-such-tool", Inputs: []string{"hi"}, ExpectedOutput: "x", EstimatedTokens: 10},
	}}
	ver, err := pl.Verify("t-badtool", bad)
	if err != nil {
		t.Fatal(err)
	}
	if ver.Verdict != VerdictRevise {
		t.Fatalf("verdict = %s, want REVISE（代码实核打回）", ver.Verdict)
	}
	found := false
	for _, f := range ver.Findings {
		if strings.Contains(f.Text, "cap-no-such-tool") {
			found = true
		}
	}
	if !found {
		t.Fatalf("findings 未点名坏工具：%+v", ver.Findings)
	}
}

// TestReviseRoundCap：核验一直 REVISE，2 轮后停下，取最好一版执行，
// 不死循环；账本记 verdict=REVISE、rounds=2。
func TestReviseRoundCap(t *testing.T) {
	revised := []Step{
		{StepNo: 1, ToolID: "test-echo", Inputs: []string{"a"}, ExpectedOutput: "x", EstimatedTokens: 10},
		{StepNo: 2, ToolID: "test-echo", Inputs: []string{"b"}, ExpectedOutput: "x", EstimatedTokens: 10},
	}
	pl, led := newTestPlanner(t, stubJSON(stepsJSON(2)), stubJSON(reviseJSON(revised)), nil)
	plan, ver, err := pl.PlanAndVerify("t-cap", "一直被打回的活", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 2 || plan.Rounds != 2 || ver.Verdict != VerdictRevise {
		t.Fatalf("plan = %d steps rounds=%d verdict=%s, want 2/2/REVISE", len(plan.Steps), plan.Rounds, ver.Verdict)
	}
	rows, err := led.ListPlanRuns("t-cap", 5)
	if err != nil || len(rows) != 1 || rows[0].ReviseRounds != 2 {
		t.Fatalf("plan_runs = %+v, %v, want rounds=2", rows, err)
	}
}

// TestVerifyFindingsCoverage：核验 findings 缺问题（只有 2 个），
// 解析必须报错，不放行。
func TestVerifyFindingsCoverage(t *testing.T) {
	short := `{"verdict":"APPROVE","findings":[
{"question":"necessity","text":"x"},
{"question":"rationality","text":"x"}], "revised_plan":[]}`
	pl, _ := newTestPlanner(t, stubJSON(""), stubJSON(short), nil)
	plan := &Plan{TaskID: "t-short", TaskText: "x", Steps: []Step{
		{StepNo: 1, ToolID: "test-echo", Inputs: []string{"hi"}, ExpectedOutput: "x", EstimatedTokens: 10},
	}}
	if _, err := pl.Verify("t-short", plan); err == nil {
		t.Fatal("findings 缺问题应报错， got nil")
	}
}

// TestPlanValidation：规划器输出非法步骤表时 Plan() 必须报错。
func TestPlanValidation(t *testing.T) {
	badTool := `{"steps":[{"step_no":1,"tool_id":"cap-no-such-tool","inputs":[],"depends_on":[],"expected_output":"x","estimated_tokens":10}]}`
	pl, _ := newTestPlanner(t, stubJSON(badTool), stubJSON(approveJSON()), nil)
	if _, err := pl.Plan("t-v1", "坏工具", "", ""); err == nil {
		t.Fatal("未知工具的计划应报错")
	}
	badDep := `{"steps":[
{"step_no":1,"tool_id":"test-echo","inputs":[],"depends_on":[2],"expected_output":"x","estimated_tokens":10},
{"step_no":2,"tool_id":"test-echo","inputs":[],"depends_on":[],"expected_output":"x","estimated_tokens":10}]}`
	pl2, _ := newTestPlanner(t, stubJSON(badDep), stubJSON(approveJSON()), nil)
	if _, err := pl2.Plan("t-v2", "依赖后面的步骤", "", ""); err == nil {
		t.Fatal("依赖后面步骤的计划应报错")
	}
	badNo := `{"steps":[
{"step_no":2,"tool_id":"test-echo","inputs":[],"depends_on":[],"expected_output":"x","estimated_tokens":10}]}`
	pl3, _ := newTestPlanner(t, stubJSON(badNo), stubJSON(approveJSON()), nil)
	if _, err := pl3.Plan("t-v3", "编号不从 1 开始", "", ""); err == nil {
		t.Fatal("编号不从 1 开始的计划应报错")
	}
}

// synthStub 是综合回复桩：Plan 调用按 frozen 内容分流——
// 含“交活综合器”的是综合调用，回固定回复；否则回规划回复。
func synthStub(planReply, synthReply string) ModelCallFunc {
	return func(taskID, frozen, tail string) (string, int64, int64, error) {
		if strings.Contains(frozen, "交活综合器") {
			return synthReply, 50, 30, nil
		}
		return planReply, 100, 20, nil
	}
}

// handPlan 直接造一张已审过的计划（RunPlan 测试用，不走模型）。
func handPlan(taskID string, steps []Step) *Plan {
	return &Plan{PlanID: "pl-test-001", TaskID: taskID, TaskText: "手工计划", Steps: steps}
}

// TestRunPlanSuccess：两步都成功，产物与 Stage 行、综合回复都落盘。
func TestRunPlanSuccess(t *testing.T) {
	pl, led := newTestPlanner(t, synthStub("", "做完了，两步都成功。"), stubJSON(approveJSON()), nil)
	plan := handPlan("t-run-ok", []Step{
		{StepNo: 1, ToolID: "test-echo", Inputs: []string{"hello"}, ExpectedOutput: "喊一声", EstimatedTokens: 10},
		{StepNo: 2, ToolID: "test-echo", Inputs: []string{"{{step1}}"}, ExpectedOutput: "把上一步产物路径再喊一遍", EstimatedTokens: 10},
	})
	res, err := pl.RunPlan(plan, "手工计划", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "completed" || res.CompletedSteps != 2 {
		t.Fatalf("RunPlan = %+v, want completed/2", res)
	}
	if !strings.Contains(res.ReplyDraft, "做完了") {
		t.Fatalf("综合回复不对：%q", res.ReplyDraft)
	}
	artDir := filepath.Join(pl.DataDir, "artifacts", "t-run-ok")
	for _, f := range []string{"step-1.txt", "step-2.txt", "reply.md"} {
		if _, err := os.Stat(filepath.Join(artDir, f)); err != nil {
			t.Fatalf("产物 %s 缺失：%v", f, err)
		}
	}
	// 步骤 2 的输入代入了步骤 1 的产物路径：它的命令里应有 step-1.txt。
	rows, err := led.ListExecutions("t-run-ok", 10)
	if err != nil {
		t.Fatal(err)
	}
	stageRows := 0
	cmdHasArtifact := false
	for _, r := range rows {
		if r.StageNo > 0 {
			stageRows++
			if r.ArtifactPath != "" {
				if _, err := os.Stat(r.ArtifactPath); err != nil {
					t.Fatalf("Stage 产物路径 %q 不存在：%v", r.ArtifactPath, err)
				}
			}
		}
		if strings.Contains(r.Command, "step-1.txt") {
			cmdHasArtifact = true
		}
	}
	if stageRows != 2 {
		t.Fatalf("Stage 行 %d 条，want 2", stageRows)
	}
	if !cmdHasArtifact {
		t.Fatalf("步骤 2 的命令里没有代入的产物路径：%+v", rows)
	}
}

// TestProgressInsurance：3 步全失败，连续 3 步零进展即停，
// 停在第 3 步；每步重试一次（各跑 2 次）。
func TestProgressInsurance(t *testing.T) {
	pl, led := newTestPlanner(t, synthStub("", "卡住了。"), stubJSON(approveJSON()), nil)
	plan := handPlan("t-noprogress", []Step{
		{StepNo: 1, ToolID: "test-false", Inputs: []string{}, ExpectedOutput: "x", EstimatedTokens: 10},
		{StepNo: 2, ToolID: "test-false", Inputs: []string{}, ExpectedOutput: "x", EstimatedTokens: 10},
		{StepNo: 3, ToolID: "test-false", Inputs: []string{}, ExpectedOutput: "x", EstimatedTokens: 10},
	})
	res, err := pl.RunPlan(plan, "全失败", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "stopped_no_progress" || res.StoppedAtStep != 3 || res.CompletedSteps != 0 {
		t.Fatalf("RunPlan = %+v, want stopped_no_progress/停在 3/完成 0", res)
	}
	if len(res.SkippedSteps) != 2 || res.SkippedSteps[0] != 1 || res.SkippedSteps[1] != 2 {
		t.Fatalf("skipped = %v, want [1 2]", res.SkippedSteps)
	}
	// 每步尝试 2 次（首次 + 重试一次）：3 步共 6 次失败执行行 + 2 行 skipped。
	rows, err := led.ListExecutions("t-noprogress", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 8 {
		t.Fatalf("执行行 %d 条，want 8（6 次失败 + 2 行 skipped）", len(rows))
	}
}

// TestRetryThenSkip：第一步失败两次后跳过，第二步成功，整单完成。
func TestRetryThenSkip(t *testing.T) {
	pl, _ := newTestPlanner(t, synthStub("", "第一步跳过，第二步做完了。"), stubJSON(approveJSON()), nil)
	plan := handPlan("t-retry", []Step{
		{StepNo: 1, ToolID: "test-false", Inputs: []string{}, ExpectedOutput: "x", EstimatedTokens: 10},
		{StepNo: 2, ToolID: "test-true", Inputs: []string{}, ExpectedOutput: "x", EstimatedTokens: 10},
	})
	res, err := pl.RunPlan(plan, "重试后跳过", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "completed" || res.CompletedSteps != 1 || len(res.SkippedSteps) != 1 || res.SkippedSteps[0] != 1 {
		t.Fatalf("RunPlan = %+v, want completed/完成 1/跳过 [1]", res)
	}
}

// TestReplanOnDependencyFailure：第一步失败、第二步依赖它 → 重规划一次，
// 新步骤成功，整单完成；账本里原计划 + 重规划各一行。
func TestReplanOnDependencyFailure(t *testing.T) {
	plan1 := `{"steps":[
{"step_no":1,"tool_id":"test-false","inputs":[],"depends_on":[],"expected_output":"x","estimated_tokens":10},
{"step_no":2,"tool_id":"test-echo","inputs":["hi"],"depends_on":[1],"expected_output":"x","estimated_tokens":10}]}`
	plan2 := `{"steps":[
{"step_no":1,"tool_id":"test-true","inputs":[],"depends_on":[],"expected_output":"x","estimated_tokens":10}]}`
	// Plan 调用顺序：初版 plan1 → 重规划 plan2 → 综合。
	planCall := stubSeq([]string{plan1, plan2, "重规划后做完了。"})
	// 注意：stubSeq 按调用次序回，但综合调用的 frozen 含“交活综合器”，
	// stubSeq 不分流——第三次调用回的正是综合文本，顺序恰好对上。
	pl, led := newTestPlanner(t, planCall, stubJSON(approveJSON()), nil)
	plan, _, err := pl.PlanAndVerify("t-replan", "依赖失败重规划", "")
	if err != nil {
		t.Fatal(err)
	}
	// PlanAndVerify 时 stubSeq 已消费第 1 次调用；RunPlan 里的重规划
	// 消费第 2 次、综合消费第 3 次。
	res, err := pl.RunPlan(plan, "依赖失败重规划", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "completed" || res.CompletedSteps != 1 {
		t.Fatalf("RunPlan = %+v, want completed/完成 1（重规划步）", res)
	}
	// 重规划的新步骤续编号为 2：产物应为 step-2.txt。
	if _, err := os.Stat(filepath.Join(pl.DataDir, "artifacts", "t-replan", "step-2.txt")); err != nil {
		t.Fatalf("重规划步骤产物 step-2.txt 缺失：%v", err)
	}
	rows, err := led.ListPlanRuns("t-replan", 5)
	if err != nil || len(rows) != 2 {
		t.Fatalf("plan_runs = %d 行，want 2（原计划 + 重规划 -r1）", len(rows))
	}
	if !strings.HasSuffix(rows[0].PlanID, "-r1") {
		t.Fatalf("重规划行 plan_id = %q, want 后缀 -r1", rows[0].PlanID)
	}
}

// TestSubstituteInputs：占位符代入与引用不存在步骤时报错。
func TestSubstituteInputs(t *testing.T) {
	artifacts := map[int]string{1: "/tmp/x/step-1.txt"}
	summaries := map[int]string{1: "第一步输出 hello"}
	out, err := substituteInputs([]string{"{{step1}}", "pre-{{step1.summary}}", "plain"}, artifacts, summaries)
	if err != nil {
		t.Fatal(err)
	}
	if out[0] != "/tmp/x/step-1.txt" || out[1] != "pre-第一步输出 hello" || out[2] != "plain" {
		t.Fatalf("substitute = %q", out)
	}
	if _, err := substituteInputs([]string{"{{step9}}"}, artifacts, summaries); err == nil {
		t.Fatal("引用不存在步骤的产物应报错")
	}
	if _, err := substituteInputs([]string{"{{step2.summary}}"}, artifacts, summaries); err == nil {
		t.Fatal("引用不存在步骤的摘要应报错")
	}
}

// TestShadowDoesNotExecute：影子模式只出规划+核验结论，不执行——
// RunPlan 拒绝，产物目录不存在，执行账本零行。
func TestShadowDoesNotExecute(t *testing.T) {
	_, rg := miniRegistry(t)
	led := openTestLedger(t)
	ex := &executor.Executor{Ledger: led}
	pl := New(stubJSON(stepsJSON(2)), stubJSON(approveJSON()), rg, led, ex, t.TempDir(), "test-session", true)
	plan, _, err := pl.PlanAndVerify("t-shadow", "影子计划", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pl.RunPlan(plan, "影子计划", ""); err == nil {
		t.Fatal("影子模式 RunPlan 应拒绝执行")
	}
	if _, err := os.Stat(filepath.Join(pl.DataDir, "artifacts")); !os.IsNotExist(err) {
		t.Fatal("影子模式不应创建产物目录")
	}
	rows, err := led.ListExecutions("t-shadow", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("影子模式执行行 %d 条，want 0", len(rows))
	}
}

// TestHaltBlocksPlanStep：不可逆步骤（rm）执行前过监工，HALT 时不真跑、
// 不重试，直接跳过；监工确实在 pre-action 触发点被调到。
func TestHaltBlocksPlanStep(t *testing.T) {
	adv := &stubAdvisor{verdict: executor.VerdictHalt, note: "删文件先停下"}
	pl, led := newTestPlanner(t, synthStub("", "rm 被拦下了。"), stubJSON(approveJSON()), adv)
	plan := handPlan("t-halt", []Step{
		{StepNo: 1, ToolID: "test-rm", Inputs: []string{}, ExpectedOutput: "删文件", EstimatedTokens: 10},
		{StepNo: 2, ToolID: "test-true", Inputs: []string{}, ExpectedOutput: "x", EstimatedTokens: 10},
	})
	res, err := pl.RunPlan(plan, "删文件测试", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "completed" || len(res.SkippedSteps) != 1 || res.SkippedSteps[0] != 1 {
		t.Fatalf("RunPlan = %+v, want completed/跳过 [1]", res)
	}
	called := false
	for _, tr := range adv.calls {
		if tr == executor.TriggerPreAction {
			called = true
		}
	}
	if !called {
		t.Fatalf("监工未在 pre-action 触发点被调到：calls=%v", adv.calls)
	}
	rows, err := led.ListExecutions("t-halt", 10)
	if err != nil {
		t.Fatal(err)
	}
	haltRow := false
	for _, r := range rows {
		if r.AdvisorVerdict == "HALT" {
			haltRow = true
		}
		// rm 命令本身一次都没真跑：执行行里不许出现成功的 rm。
		if strings.HasPrefix(r.Command, "rm") && r.Status == "success" {
			t.Fatalf("rm 被真跑了：%+v", r)
		}
	}
	if !haltRow {
		t.Fatalf("账本里没有 HALT 行：%+v", rows)
	}
}

// TestSynthesizeIncludesWorkDirListing：综合输入必须带上工作目录实际落盘
// 清单（2026-11-18 ping0 第三轮：综合稿写了“没有落盘文件”，但文件其实已落盘）。
func TestSynthesizeIncludesWorkDirListing(t *testing.T) {
	_, rg := miniRegistry(t)
	led := openTestLedger(t)
	ex := &executor.Executor{Ledger: led}
	dataDir := t.TempDir()
	var gotTail string
	capture := func(taskID, frozen, tail string) (string, int64, int64, error) {
		gotTail = tail
		return "综合回复", 100, 20, nil
	}
	pl := New(capture, capture, rg, led, ex, dataDir, "test-session", false)
	wd := filepath.Join(dataDir, "workspace", "t1")
	if err := os.MkdirAll(wd, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wd, "home.html"), []byte("<html>"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := pl.Synthesize("t1", "抓个站", []string{"步骤1成功"}); err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	for _, want := range []string{"home.html", "workspace", "落盘文件"} {
		if !strings.Contains(gotTail, want) {
			t.Errorf("synthesize tail missing %q:\n%s", want, gotTail)
		}
	}
	// 空目录：清单必须明说无落盘文件，不能含糊。
	gotTail = ""
	emptyWd := filepath.Join(dataDir, "workspace", "t-empty")
	if err := os.MkdirAll(emptyWd, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := pl.Synthesize("t-empty", "抓个站", []string{"步骤1成功"}); err != nil {
		t.Fatalf("Synthesize empty: %v", err)
	}
	if !strings.Contains(gotTail, "无落盘文件") {
		t.Errorf("synthesize tail for empty workdir must say 无落盘文件:\n%s", gotTail)
	}
	// 目录根本不存在：清单必须明说不存在（和“空”是两回事）。
	gotTail = ""
	if _, err := pl.Synthesize("t-missing", "抓个站", []string{"步骤1成功"}); err != nil {
		t.Fatalf("Synthesize missing: %v", err)
	}
	if !strings.Contains(gotTail, "不存在") {
		t.Errorf("synthesize tail for missing workdir must say 不存在:\n%s", gotTail)
	}
}
