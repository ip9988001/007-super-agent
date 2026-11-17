// Command planner-verify：第 21 步沙箱验证——
// 用桩模型跑方案第 7 节的四项测试集（两步 / 十五步 / 臃肿计划 / 坏工具），
// 打印每项 PASS/FAIL 与汇总，并实测一次完整规划+核验的 stub 模拟费用。
//
// 桩模型模拟用量（每次调用都记一条 expense_ledger 行）：
//
//	规划（flash）：prompt 1500（1200 命中 + 300 未命中）+ completion 400 = 2 毫分；
//	核验（reasoner，按 Flash 价）：prompt 2500（2000 命中 + 500 未命中）+ completion 500 = 3 毫分。
//
// 数字是桩的模拟用量，不是真实 API 用量；真实用量等上线后从 expense_ledger 读。
// 不真调 API、不联网、不读任何密钥。
package main

import (
	"crypto/md5"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"007-super-agent/internal/ledger"
	"007-super-agent/internal/llm"
	"007-super-agent/internal/planner"
	"007-super-agent/internal/registry"
)

func fail(format string, args ...any) {
	log.Printf("FAIL: "+format, args...)
	os.Exit(1)
}

// miniRegistry 建一个 3 条目的迷你注册表（与 planner_test.go 同口径）。
func miniRegistry(dir string) (*registry.Registry, string, string) {
	entries := []struct{ id, ep, contract string }{
		{"test-echo", "echo", "exit 0"},
		{"test-true", "true", "exit 0"},
		{"test-false", "false", "exit 1"},
	}
	var ents strings.Builder
	ents.WriteString(`{"version":"v1.1-test","generated":"2026-11-18","total":3,"entries":[`)
	for i, e := range entries {
		if i > 0 {
			ents.WriteString(",")
		}
		fmt.Fprintf(&ents, `{"tool_id":%q,"category":"ops","subcategory":"shell","call_type":"cli","trigger_prefix":"","pre_command":"","entry_point":%q,"param_schema":{},"output_contract":%q,"status_triad":{"installed":true,"runnable":true,"verified":false},"available":true,"last_verified_at":"","source_file":"test","one_liner":%q,"verified_basis":"","note":""}`,
			e.id, e.ep, e.contract, e.id+" 占位")
	}
	ents.WriteString("]}")
	if err := os.MkdirAll(filepath.Join(dir, "registry"), 0o755); err != nil {
		log.Fatal(err)
	}
	regJSONPath := filepath.Join(dir, "registry", "registry.json")
	if err := os.WriteFile(regJSONPath, []byte(ents.String()), 0o600); err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "index", "L2"), 0o755); err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "index", "L3"), 0o755); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index", "L1.md"), []byte("fetch\ncode\nseo\ndesign\nops\nmemory\n"), 0o600); err != nil {
		log.Fatal(err)
	}
	var l2 strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&l2, "- %s: %s 占位\n", e.id, e.id)
	}
	if err := os.WriteFile(filepath.Join(dir, "index", "L2", "ops.md"), []byte(l2.String()), 0o600); err != nil {
		log.Fatal(err)
	}
	for _, e := range entries {
		content := "# " + e.id + "\n\n输入：无\n\n步骤：跑命令\n\n验收：" + e.contract + "\n"
		if err := os.WriteFile(filepath.Join(dir, "index", "L3", e.id+".md"), []byte(content), 0o600); err != nil {
			log.Fatal(err)
		}
	}
	rg, err := registry.Load(dir)
	if err != nil {
		log.Fatal(err)
	}
	raw, err := os.ReadFile(regJSONPath)
	if err != nil {
		log.Fatal(err)
	}
	md5sum := fmt.Sprintf("%x", md5.Sum(raw))
	return rg, regJSONPath, md5sum
}

func stepsJSON(n int) string {
	steps := make([]planner.Step, n)
	for i := range steps {
		steps[i] = planner.Step{StepNo: i + 1, ToolID: "test-echo", Inputs: []string{"hello"},
			ExpectedOutput: "输出 hello", EstimatedTokens: 100}
	}
	b, _ := json.Marshal(map[string]any{"steps": steps})
	return string(b)
}

func approveJSON() string {
	return `{"verdict":"APPROVE","findings":[
{"question":"necessity","text":"已检查合并可能性：各步不能合并，因为后一步依赖前一步的产物"},
{"question":"rationality","text":"依赖成立，无漏步"},
{"question":"reasons","text":"无不合理处"},
{"question":"suggestion","text":"按原表执行"}], "revised_plan":[]}`
}

func main() {
	dir, err := os.MkdirTemp("", "plverify")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)
	log.Printf("sandbox dir: %s", dir)

	led, err := ledger.Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer ledger.Close(led)

	rg, regJSONPath, regMD5Before := miniRegistry(dir)

	// ---- 桩模型：按 taskID 分流，记费用行 ----
	recordExpense := func(taskID, model string, prompt, hit, miss, completion int64) {
		cost := llm.CostMilliForModel(model, hit, miss, completion, false)
		if _, err := led.RecordExpense(ledger.ExpenseRecord{
			SessionID: "verify", TaskID: taskID, Model: model,
			PromptTokens: prompt, CacheHitTokens: hit, CacheMissTokens: miss,
			CompletionTokens: completion, CostCNYMilli: cost,
		}); err != nil {
			log.Fatal(err)
		}
	}
	planStub := func(taskID, frozen, tail string) (string, int64, int64, error) {
		recordExpense(taskID, "deepseek-flash", 1500, 1200, 300, 400)
		switch taskID {
		case "verify-2step":
			return stepsJSON(2), 1500, 400, nil
		case "verify-15step":
			return stepsJSON(15), 1500, 400, nil
		case "verify-bloated":
			return stepsJSON(12), 1500, 400, nil
		default:
			return "", 0, 0, fmt.Errorf("plan stub: 未知 task %s", taskID)
		}
	}
	verifyCalls := map[string]int{}
	verifyStub := func(taskID, frozen, tail string) (string, int64, int64, error) {
		recordExpense(taskID, "deepseek-reasoner", 2500, 2000, 500, 500)
		verifyCalls[taskID]++
		if taskID == "verify-bloated" && verifyCalls[taskID] == 1 {
			// 第一轮：REVISE，修订版 9 步。
			revised := make([]planner.Step, 9)
			for i := range revised {
				revised[i] = planner.Step{StepNo: i + 1, ToolID: "test-echo", Inputs: []string{"hello"},
					ExpectedOutput: "输出 hello", EstimatedTokens: 100}
			}
			b, _ := json.Marshal(map[string]any{
				"verdict": "REVISE",
				"findings": []planner.Finding{
					{Question: "necessity", Text: "步骤 3、7、11 是废步，可以合并"},
					{Question: "rationality", Text: "依赖成立，无漏步"},
					{Question: "reasons", Text: "3 步废步：只做重复检查，无产物"},
					{Question: "suggestion", Text: "合并废步后的 9 步表见 revised_plan"},
				},
				"revised_plan": revised,
			})
			return string(b), 2500, 500, nil
		}
		return approveJSON(), 2500, 500, nil
	}

	pl := planner.New(planStub, verifyStub, rg, led, nil, dir, "verify-session", false)

	// ---- ① 两步任务 ----
	plan, ver, err := pl.PlanAndVerify("verify-2step", "先喊一声再喊一声", "")
	if err != nil {
		fail("item 1 两步任务：%v", err)
	}
	if len(plan.Steps) != 2 || ver.Verdict != planner.VerdictApprove {
		fail("item 1 两步任务：steps=%d verdict=%s, want 2/APPROVE", len(plan.Steps), ver.Verdict)
	}
	fmt.Println("item 1 两步任务：PASS（2 步，APPROVE）")

	// ---- ② 十五步任务 ----
	plan, ver, err = pl.PlanAndVerify("verify-15step", "十五步的活", "")
	if err != nil {
		fail("item 2 十五步任务：%v", err)
	}
	if len(plan.Steps) != 15 || ver.Verdict != planner.VerdictApprove {
		fail("item 2 十五步任务：steps=%d verdict=%s, want 15/APPROVE", len(plan.Steps), ver.Verdict)
	}
	fmt.Println("item 2 十五步任务：PASS（15 步完整保留，APPROVE）")

	// ---- ③ 臃肿计划 ----
	plan, ver, err = pl.PlanAndVerify("verify-bloated", "臃肿的活", "")
	if err != nil {
		fail("item 3 臃肿计划：%v", err)
	}
	if len(plan.Steps) != 9 || plan.Rounds != 1 || ver.Verdict != planner.VerdictApprove {
		fail("item 3 臃肿计划：steps=%d rounds=%d verdict=%s, want 9/1/APPROVE", len(plan.Steps), plan.Rounds, ver.Verdict)
	}
	fmt.Println("item 3 臃肿计划：PASS（12 步 → 9 步，1 轮修改）")

	// ---- ④ 坏工具计划（直接调 Verify，模型说 APPROVE 也要被实核打回）----
	bad := &planner.Plan{TaskID: "verify-badtool", TaskText: "坏工具", Steps: []planner.Step{
		{StepNo: 1, ToolID: "test-echo", Inputs: []string{"hi"}, ExpectedOutput: "x", EstimatedTokens: 10},
		{StepNo: 2, ToolID: "cap-no-such-tool", Inputs: []string{"hi"}, ExpectedOutput: "x", EstimatedTokens: 10},
	}}
	ver, err = pl.Verify("verify-badtool", bad)
	if err != nil {
		fail("item 4 坏工具计划：%v", err)
	}
	if ver.Verdict != planner.VerdictRevise {
		fail("item 4 坏工具计划：verdict=%s, want REVISE（代码实核打回）", ver.Verdict)
	}
	named := false
	for _, f := range ver.Findings {
		if strings.Contains(f.Text, "cap-no-such-tool") {
			named = true
		}
	}
	if !named {
		fail("item 4 坏工具计划：findings 未点名坏工具")
	}
	fmt.Println("item 4 坏工具计划：PASS（模型 APPROVE 被实核降级为 REVISE，点名坏工具）")

	// ---- 账本与费用汇总 ----
	totalTokens := int64(0)
	totalMilli := int64(0)
	for _, tid := range []string{"verify-2step", "verify-15step", "verify-bloated", "verify-badtool"} {
		rows, err := led.ListExpenseByTask(tid, 20)
		if err != nil {
			log.Fatal(err)
		}
		for _, r := range rows {
			totalTokens += r.PromptTokens + r.CompletionTokens
			totalMilli += r.CostCNYMilli
			fmt.Printf("expense task=%s model=%s prompt=%d hit=%d miss=%d completion=%d cost=%d 毫分\n",
				r.TaskID, r.Model, r.PromptTokens, r.CacheHitTokens, r.CacheMissTokens, r.CompletionTokens, r.CostCNYMilli)
		}
	}
	// 预期：规划 3 次 × (1500+400) + 核验 5 次 × (2500+500) = 5700 + 15000 = 20700 token；
	// 费用 3×2 + 5×3 = 21 毫分。
	if totalTokens != 20700 || totalMilli != 21 {
		fail("费用汇总 tokens=%d milli=%d, want 20700/21", totalTokens, totalMilli)
	}
	fmt.Printf("measured (stub-simulated): 20700 tokens, 21 毫分 = ¥0.021（4 项：规划 3 次 + 核验 5 次）\n")
	fmt.Printf("一次完整规划+核验（1 次规划 + 1 次核验）：4900 tokens，5 毫分 = ¥0.005\n")

	// plan_runs 应为 3 行（①②③各一；④直接调 Verify，不记 plan_runs）。
	// 注意：ListPlanRuns 按 task 查，这里直接数三行的和。
	n := 0
	for _, tid := range []string{"verify-2step", "verify-15step", "verify-bloated"} {
		rows, err := led.ListPlanRuns(tid, 5)
		if err != nil {
			log.Fatal(err)
		}
		n += len(rows)
	}
	if n != 3 {
		fail("plan_runs = %d 行，want 3", n)
	}
	raw, err := os.ReadFile(regJSONPath)
	if err != nil {
		log.Fatal(err)
	}
	if fmt.Sprintf("%x", md5.Sum(raw)) != regMD5Before {
		fail("registry.json 被改动（只许读，不许写）")
	}
	fmt.Println("planner-verify: ALL PASS")
}
