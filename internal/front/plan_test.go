// 规划循环 v2（前置判回执）单测。
package front

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"007-super-agent/internal/dispatch"
	"007-super-agent/internal/ledger"
	"007-super-agent/internal/llm"
	"007-super-agent/internal/session"
)

// TestValidatePlanStepType：step_type 按步校验——research 步 verify 可空，
// execute 步 verify 也可空（对齐二号三号：verify_cmd 与 expected_artifacts
// 都是可选的，想写就写、不写也行；网关有账本兜底与硬底线把关）。
// 非法 step_type 照样打回。
func TestValidatePlanStepType(t *testing.T) {
	fr := &Front{Reg: miniRegistry(t)}
	// research 步无 verify：通过。
	ok := PlanArgs{Goal: "查一下", TaskType: "research", Steps: []PlanStep{
		{StepNo: 1, ToolID: "test-echo", ExpectedOutput: "看到结果", StepType: "research"},
	}}
	if err := fr.validatePlan(ok); err != nil {
		t.Fatalf("research 步无 verify 应通过：%v", err)
	}
	// execute 步无 verify：也通过（验收全可选，网关账本兜底）。
	bad := PlanArgs{Goal: "做一下", TaskType: "execute", Steps: []PlanStep{
		{StepNo: 1, ToolID: "test-echo", ExpectedOutput: "看到结果", StepType: "execute"},
	}}
	if err := fr.validatePlan(bad); err != nil {
		t.Fatalf("execute 步无 verify 也应通过（验收可选）：%v", err)
	}
	// 步的 step_type 覆盖整单：整单 execute，但某步标 research 可空 verify。
	mixed := PlanArgs{Goal: "混合", TaskType: "execute", Steps: []PlanStep{
		{StepNo: 1, ToolID: "test-echo", ExpectedOutput: "查到", StepType: "research"},
		{StepNo: 2, ToolID: "test-true", ExpectedOutput: "通过", VerifyCmd: []string{"true"}},
	}}
	if err := fr.validatePlan(mixed); err != nil {
		t.Fatalf("步级 research 应通过：%v", err)
	}
	// 非法 step_type 打回。
	illegal := PlanArgs{Goal: "x", Steps: []PlanStep{
		{StepNo: 1, ToolID: "test-echo", ExpectedOutput: "x", StepType: "bogus", VerifyCmd: []string{"true"}},
	}}
	if err := fr.validatePlan(illegal); err == nil {
		t.Fatalf("非法 step_type 应打回")
	}
}

// TestValidatePlanCoarseLimit：计划要粗粒度，超 10 步打回。
func TestValidatePlanCoarseLimit(t *testing.T) {
	fr := &Front{Reg: miniRegistry(t)}
	var steps []PlanStep
	for i := 1; i <= 11; i++ {
		steps = append(steps, PlanStep{StepNo: i, ToolID: "test-true", ExpectedOutput: "通过", VerifyCmd: []string{"true"}})
	}
	p := PlanArgs{Goal: "太多步", Steps: steps}
	if err := fr.validatePlan(p); err == nil {
		t.Fatalf("11 步应打回（粗粒度上限 10）")
	}
	steps = steps[:5]
	p.Steps = steps
	if err := fr.validatePlan(p); err != nil {
		t.Fatalf("5 步应通过：%v", err)
	}
}

// TestValidatePlanInputsTilde：inputs 里 ~/ 相对路径要拦，
// 普通 URL/标志/子命令不误杀。
func TestValidatePlanInputsTilde(t *testing.T) {
	if err := validatePlanInputs("test-echo", []string{"~/x.txt"}); err == nil {
		t.Fatalf("~/ 应拦")
	}
	if err := validatePlanInputs("test-echo", []string{"~"}); err == nil {
		t.Fatalf("~ 应拦")
	}
	if err := validatePlanInputs("test-echo", []string{"-s", "https://example.com", "status", "nginx"}); err != nil {
		t.Fatalf("普通参数不应误杀：%v", err)
	}
	if err := validatePlanInputs("test-echo", []string{"-s", "/abs/path.txt"}); err != nil {
		t.Fatalf("绝对路径不应误杀：%v", err)
	}
}

// redFactList 造一份硬底线红的事实清单（假回执对抗用）：
// 回执说“做完了”，但产物缺失、0 次调用。
func redFactList() *dispatch.FactList {
	return &dispatch.FactList{
		HasMachineCheck: true,
		Artifacts: []dispatch.ArtifactFact{
			{Path: "/tmp/plan-v2-test-out.txt", Exists: false, Problem: "不存在或不可读"},
		},
		SuccessfulCalls: 0, SubstantiveCalls: 0,
		HardFloorRed: true, HardFloorReason: "产物 /tmp/plan-v2-test-out.txt：不存在或不可读",
		AllGreen:   false,
		Confidence: 0, ConfidenceToolPoints: 0, ConfidenceFilePoints: 0,
		ClaimedFiles: 1, VerifiedFiles: 0,
		MissingFiles: []string{"/tmp/plan-v2-test-out.txt"},
	}
}

// greenFactList 造一份全绿的事实清单（快路径用）：置信度 100%
// （工具 50% + 成果 50%，1 个声明文件核对有）。
func greenFactList() *dispatch.FactList {
	return &dispatch.FactList{
		HasMachineCheck: true,
		Artifacts: []dispatch.ArtifactFact{
			{Path: "/tmp/plan-v2-test-out.txt", Exists: true, Size: 12, Fresh: true, NonEmpty: true, Valid: true},
		},
		SuccessfulCalls: 2, SubstantiveCalls: 1,
		AllGreen:   true,
		Confidence: 100, ConfidenceToolPoints: 50, ConfidenceFilePoints: 50,
		ToolCallBreakdown: "test-echo×2",
		ClaimedFiles:      1, VerifiedFiles: 1,
	}
}

func testPlanState() *session.PlanState {
	return &session.PlanState{
		PlanID: "test-plan", SessionID: "test-session", TaskID: "t-plan",
		Status: session.PlanRunning, Phase: session.PhaseRunning,
		Steps: []session.PlanStep{
			{StepNo: 1, ToolID: "test-echo", StepType: session.StepExecute,
				ExpectedArtifacts: []string{"/tmp/plan-v2-test-out.txt"}, Status: session.StepRunning, Attempts: 1},
		},
		CurrentStep: 1, TotalSteps: 1,
	}
}

// TestJudgeReceiptFakeReceiptStillRedo：假回执（0 次调用、置信度 0、
// 产物缺失）仍判重干——不是因为硬底线否决，而是事实证明根本没干实活。
// 硬底线一票否决已去掉（2026-11-26 用户定），这里靠事实本身拦假活。
func TestJudgeReceiptFakeReceiptStillRedo(t *testing.T) {
	fr := &Front{Reg: miniRegistry(t)}
	ps := testPlanState()
	step := ps.Steps[0]
	r := &dispatch.Receipt{
		DispatchID: "d1", TaskID: "t-plan", Status: "done",
		Conclusion: "做完了，一切顺利，文件已生成", FactList: redFactList(),
	}
	v, reason := fr.judgeReceipt(context.Background(), "t-plan", ps, step, r)
	if v != verdictRedo {
		t.Fatalf("假回执（0 次调用、置信度 0）verdict=%s，want redo，reason=%s", v, reason)
	}
}

// TestJudgeReceiptHardFloorRedCanPassWhenConfidence100（否决已去掉）：
// 硬底线红了，但置信度 100%、主要文件核对通过时，前置可以判通过。
// 回执 Status=done、置信度 100% 走快路径，不应再被硬底线拦下。
func TestJudgeReceiptHardFloorRedCanPassWhenConfidence100(t *testing.T) {
	fr := &Front{Reg: miniRegistry(t)} // 无 CallWithToolsJudge：快路径不应调模型
	ps := testPlanState()
	step := ps.Steps[0]
	// 硬底线红（边角产物有问题），但主要成果已核对通过、置信度 100%。
	fl := &dispatch.FactList{
		HasMachineCheck: true,
		Artifacts: []dispatch.ArtifactFact{
			{Path: "/tmp/plan-v2-test-out.txt", Exists: true, Size: 12, Fresh: true, NonEmpty: true, Valid: true},
		},
		SuccessfulCalls: 5, SubstantiveCalls: 3,
		HardFloorRed: true, HardFloorReason: "产物 /tmp/old-side.json：不是本单新生成的，mtime 过早",
		AllGreen:   false,
		Confidence: 100, ConfidenceToolPoints: 50, ConfidenceFilePoints: 50,
		ToolCallBreakdown: "test-echo×5",
		ClaimedFiles:      1, VerifiedFiles: 1,
	}
	r := &dispatch.Receipt{
		DispatchID: "d1", TaskID: "t-plan", Status: "done",
		Conclusion: "做完了，主要成果已拿到", FactList: fl,
	}
	v, reason := fr.judgeReceipt(context.Background(), "t-plan", ps, step, r)
	if v != verdictPass {
		t.Fatalf("硬底线红但置信度 100%% 时 verdict=%s，want pass（否决已去掉），reason=%s", v, reason)
	}
}

// TestJudgeReceiptFastPath：置信度 100% 且回执状态 done 的步，
// 代码直接推进，不叫前置模型（CallWithToolsJudge 保持 nil 也不影响）。
func TestJudgeReceiptFastPath(t *testing.T) {
	fr := &Front{Reg: miniRegistry(t)} // 无 CallWithToolsJudge：快路径不应调模型
	ps := testPlanState()
	step := ps.Steps[0]
	r := &dispatch.Receipt{
		DispatchID: "d1", TaskID: "t-plan", Status: "done",
		Conclusion: "ok", FactList: greenFactList(),
	}
	v, reason := fr.judgeReceipt(context.Background(), "t-plan", ps, step, r)
	if v != verdictPass {
		t.Fatalf("快路径 verdict=%s，want pass，reason=%s", v, reason)
	}
	if !strings.Contains(reason, "置信度 100%") {
		t.Fatalf("快路径原因应提置信度 100%%，实际：%s", reason)
	}
}

// TestJudgeReceiptFastPathRequiresConfidence100：置信度未满 100% 时
// 不走快路径——即使 AllGreen 为真（旧口径），也要交模型判。
// 无模型调用时落入 judgeFallback（按事实兜底），绝不直接放行。
func TestJudgeReceiptFastPathRequiresConfidence100(t *testing.T) {
	fr := &Front{Reg: miniRegistry(t)} // 无 CallWithToolsJudge
	ps := testPlanState()
	step := ps.Steps[0]
	// 2 次调用、1 个文件只核对有 0 个（缺失）→ 置信度 50%，未满。
	fl := greenFactList()
	fl.Confidence = 50
	fl.ConfidenceToolPoints = 50
	fl.ConfidenceFilePoints = 0
	fl.VerifiedFiles = 0
	fl.MissingFiles = []string{"/tmp/plan-v2-test-out.txt"}
	fl.AllGreen = false
	r := &dispatch.Receipt{
		DispatchID: "d1", TaskID: "t-plan", Status: "done",
		Conclusion: "ok", FactList: fl,
	}
	v, reason := fr.judgeReceipt(context.Background(), "t-plan", ps, step, r)
	if v == verdictPass {
		t.Fatalf("置信度 50%% 不应走快路径直接通过，reason=%s", reason)
	}
}

// TestJudgeReceiptNilFactList：事实清单缺失时，前置看回执正文与产物判，
// 不直接标重干。对齐二号三号（2026-11-25）：清单是辅助参考，
// 缺了不算失败；只有“回执正文 + 产物”都看不出成果才重干。
// 无模型调用时走 judgeFallback（回执正文与产物兜底）。
func TestJudgeReceiptNilFactList(t *testing.T) {
	fr := &Front{Reg: miniRegistry(t)}
	ps := testPlanState()
	step := ps.Steps[0]
	// 回执正文四栏齐全、产物在列 → 判通过，不自动重干。
	r := &dispatch.Receipt{
		DispatchID: "d1", TaskID: "t-plan", Status: "done",
		Conclusion: "干了什么：写了问候文件；结果是什么：写好了；" +
			"成果在哪里：/tmp/plan-v2-test-out.txt；用了哪些工具：test-echo",
		Artifacts: []string{"/tmp/plan-v2-test-out.txt"}, FactList: nil,
	}
	v, reason := fr.judgeReceipt(context.Background(), "t-plan", ps, step, r)
	if v != verdictPass {
		t.Fatalf("清单缺失但回执正文+产物齐全应判通过，verdict=%s，reason=%s", v, reason)
	}
	// 回执正文是套话、产物也没有 → 重干。
	r2 := &dispatch.Receipt{
		DispatchID: "d2", TaskID: "t-plan", Status: "done",
		Conclusion: "执行结束，状态：done", FactList: nil,
	}
	v2, _ := fr.judgeReceipt(context.Background(), "t-plan", ps, step, r2)
	if v2 != verdictRedo {
		t.Fatalf("清单缺失且回执正文空洞、产物也没有应判重干，verdict=%s", v2)
	}
	// 回执状态不是 done → 锁死不能通过（哪怕清单缺失、文字再漂亮）。
	r3 := &dispatch.Receipt{
		DispatchID: "d3", TaskID: "t-plan", Status: "failed",
		Conclusion: "干了什么：写了文件；结果是什么：写好了；成果在哪里：/tmp/plan-v2-test-out.txt；用了哪些工具：test-echo",
		Artifacts:  []string{"/tmp/plan-v2-test-out.txt"}, FactList: nil,
	}
	v3, _ := fr.judgeReceipt(context.Background(), "t-plan", ps, step, r3)
	if v3 == verdictPass {
		t.Fatalf("非 done 回执绝不能通过，verdict=%s", v3)
	}
}

// TestParseJudgeVerdict：模型判回执的大白话结论正确映射到
// 继续 / 重干 / 改计划 / 停下。对齐二号三号（2026-11-25）：
// 前置模型用大白话判，不回 JSON；解析要宽松，模型写清楚结论就行，
// 不因格式对不上就判失败。拿不准走 judgeFallback（事实兜底）。
func TestParseJudgeVerdict(t *testing.T) {
	r := &dispatch.Receipt{FactList: greenFactList()}
	for _, tc := range []struct {
		in   string
		want planVerdict
	}{
		// 通过：大白话说清楚“通过 / 做完了 / 已完成”。
		{"通过\n产物已落盘，内容齐全。", verdictPass},
		{"做完了，文件在指定位置，内容正确。", verdictPass},
		{"已完成，两个文件都写好了。", verdictPass},
		// 通过：其他常见肯定说法（模型判卷不一定用“通过”这个词）。
		{"内容正确，文件就是要的问候语。", verdictPass},
		{"没问题，文件写好了。", verdictPass},
		{"确认无误，两个文件都在。", verdictPass},
		{"符合预期，产物齐全。", verdictPass},
		// 重干：大白话说没做完 / 没做好。
		{"重干\n这步没做完，只写了一半。", verdictRedo},
		{"没做好，文件内容不对。", verdictRedo},
		{"未完成，产物还是空的。", verdictRedo},
		// 改计划：大白话说计划有问题。
		{"改计划\n这条路走不通，工具用不了，换个做法。", verdictReplan},
		{"计划有问题，第二步的输入不存在。", verdictReplan},
		// 停下：大白话说做不下去。
		{"停下\n做不下去，账号被锁了。", verdictStop},
		{"做不下去，缺关键信息。", verdictStop},
		// 否定词不能反成通过：事实绿也不行。
		{"没通过，产物还是错的。", verdictRedo},
		{"没有通过，差一个文件。", verdictRedo},
		{"this did not pass the check", verdictRedo},
		// 旧 JSON 格式不再要求，但模型万一回了 JSON 也不报错：
		// 拿不准走事实兜底（全绿→通过）。
		{`{"verdict":"pass","reason":"做完了"}`, verdictPass},
	} {
		v, _ := parseJudgeVerdict(tc.in, r)
		if v != tc.want {
			t.Fatalf("parseJudgeVerdict(%q)=%s, want %s", tc.in, v, tc.want)
		}
	}
	// 完全解析不了的文本 → 事实兜底（全绿→通过）。
	v, _ := parseJudgeVerdict("今天天气不错", r)
	if v != verdictPass {
		t.Fatalf("解析失败应按事实兜底（全绿→通过），实际 %s", v)
	}
	// 事实红时解析失败 → 重干。
	rRed := &dispatch.Receipt{FactList: redFactList()}
	v, _ = parseJudgeVerdict("今天天气不错", rRed)
	if v != verdictRedo {
		t.Fatalf("事实红时解析失败应重干，实际 %s", v)
	}
}

// TestRunPlanFastPathEndToEnd：两步计划走快路径全程通过，
// 最终 reply.md 落盘且列出两步结果。
func TestRunPlanFastPathEndToEnd(t *testing.T) {
	var rec []frontToolsCallRecord
	fr := newNativeTestFront(t, stubFrontToolsSeq(t, []llm.Message{
		frontToolCallMsg("c1", "plan_task", `{"goal":"两步写文件","steps":[
			{"step_no":1,"tool_id":"test-echo","inputs":["hi"],"expected_output":"看到 hi","expected_artifacts":["/tmp/t-plan-a.txt"]},
			{"step_no":2,"tool_id":"test-true","expected_output":"通过","verify_cmd":["true"]}
		]}`),
	}, nil, &rec))
	res, err := fr.Run(context.Background(), "t-plan-e2e", "两步写文件")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "done" {
		t.Fatalf("Status=%s, want done", res.Status)
	}
	b, err := os.ReadFile(filepath.Join(fr.DataDir, "artifacts", "t-plan-e2e", "reply.md"))
	if err != nil {
		t.Fatalf("reply.md 未落盘：%v", err)
	}
	if !strings.Contains(string(b), "第 1 步") || !strings.Contains(string(b), "第 2 步") {
		t.Fatalf("reply.md 应列出两步结果，实际：%s", string(b))
	}
}

// newFakeReceiptTestFront 建一个“假回执”前置单测对象：桩执行层每次
// 都回 Status=done、结论漂亮（“文件已写好”），但事实清单是红的
// （声明产物缺失、0 次成功调用），模拟执行层谎报做完。这是场景 (c)
// 的端到端版：走完整 runPlan 循环，断言绝不标 DONE。
func newFakeReceiptTestFront(t *testing.T, call ModelCallToolsFunc) *Front {
	t.Helper()
	rg := miniRegistry(t)
	led := openTestLedger(t)
	dataDir := t.TempDir()
	ch := dispatch.New(dataDir)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go dispatch.Worker(ctx, ch, func(ctx context.Context, d *dispatch.Dispatch) (*dispatch.Receipt, error) {
		fl := &dispatch.FactList{
			HasMachineCheck:  len(d.ExpectedArtifacts) > 0 || len(d.VerifyCmd) > 0,
			SubstantiveCalls: 0, SuccessfulCalls: 0,
		}
		for _, p := range d.ExpectedArtifacts {
			fl.Artifacts = append(fl.Artifacts, dispatch.ArtifactFact{
				Path: p, Exists: false, Size: 0, Fresh: false, NonEmpty: false, Valid: false, Problem: "产物缺失：" + p,
			})
		}
		fl.HardFloorRed = true
		fl.HardFloorReason = "产物缺失（代码已锁死这步不能通过）"
		fl.AllGreen = false
		return &dispatch.Receipt{
			DispatchID: d.ID, TaskID: d.TaskID, Status: "done",
			Conclusion: "文件已写好，全部做完，请放心",
			CreatedAt:  time.Now().UTC().Format(time.RFC3339),
			FactList:   fl,
		}, nil
	})
	return &Front{CallWithTools: call, Reg: rg, Ch: ch, DataDir: dataDir, SessionID: "test-session", Led: led}
}

// TestRunPlanFakeReceiptNeverPasses：桩执行层每步都回“做完”+漂亮结论，
// 但事实清单全红（产物缺失、0 次调用）。走完整规划循环：前置重干 2 次
// → 改计划失败（桩模型不回新步骤）→ 诚实报失败。断言：最终状态是
// failed（绝不是 done），reply.md 写明失败原因、没说“做完了”。
func TestRunPlanFakeReceiptNeverPasses(t *testing.T) {
	var rec []frontToolsCallRecord
	fr := newFakeReceiptTestFront(t, stubFrontToolsSeq(t, []llm.Message{
		frontToolCallMsg("c1", "plan_task", `{"goal":"写一个文件","steps":[
			{"step_no":1,"tool_id":"test-echo","inputs":["hi"],"expected_output":"看到 hi","expected_artifacts":["/tmp/t-fake-receipt.txt"]}
		]}`),
	}, nil, &rec))
	res, err := fr.Run(context.Background(), "t-fake", "写一个文件")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "failed" {
		t.Fatalf("Status=%s, want failed（假回执绝不能标 DONE）", res.Status)
	}
	b, err := os.ReadFile(filepath.Join(fr.DataDir, "artifacts", "t-fake", "reply.md"))
	if err != nil {
		t.Fatalf("reply.md 未落盘：%v", err)
	}
	if strings.Contains(string(b), "全部做完") || strings.Contains(string(b), "已完成") {
		t.Fatalf("reply.md 不应说做完了，实际：%s", string(b))
	}
	if !strings.Contains(string(b), "没能做完") && !strings.Contains(string(b), "失败") {
		t.Fatalf("reply.md 应写明失败，实际：%s", string(b))
	}
}

// TestStepIsOptionalAndUndepended：可选且无依赖的步可跳过，
// 不可选或有依赖的步不可跳过。
func TestStepIsOptionalAndUndepended(t *testing.T) {
	ps := &session.PlanState{
		Steps: []session.PlanStep{
			{StepNo: 1, ToolID: "test-echo", Optional: true, Status: session.StepFailed},
			{StepNo: 2, ToolID: "test-true", Optional: false, Status: session.StepPending},
			{StepNo: 3, ToolID: "test-true", Optional: true, Status: session.StepPending},
			{StepNo: 4, ToolID: "test-true", Optional: false, Status: session.StepPending, DependsOn: []int{3}},
		},
	}
	if !stepIsOptionalAndUndepended(ps, 1) {
		t.Fatalf("第 1 步可选且无依赖，应可跳过")
	}
	if stepIsOptionalAndUndepended(ps, 2) {
		t.Fatalf("第 2 步不可选，不应可跳过")
	}
	if stepIsOptionalAndUndepended(ps, 3) {
		t.Fatalf("第 3 步虽可选但被第 4 步依赖，不应可跳过")
	}
}

func TestParseJudgeVerdictNoPassSubstring(t *testing.T) {
	rRed := &dispatch.Receipt{FactList: redFactList()}
	for _, in := range []string{
		"this did not pass the check",
		"missing password, please retry",
		"the password is wrong",
	} {
		v, _ := parseJudgeVerdict(in, rRed)
		if v == verdictPass {
			t.Fatalf("parseJudgeVerdict(%q) 不应误判通过，实际 %s", in, v)
		}
		if v != verdictRedo {
			t.Fatalf("parseJudgeVerdict(%q)=%s，want redo（事实红 → 重干）", in, v)
		}
	}
	// 事实全绿时 "did not pass" 也不能靠子串通过——应走事实兜底
	// （全绿→通过是事实兜底给的，不是子串给的；reason 里不应含“关键词兜底”）。
	rGreen := &dispatch.Receipt{FactList: greenFactList()}
	_, reason := parseJudgeVerdict("this did not pass", rGreen)
	if strings.Contains(reason, "关键词兜底") {
		t.Fatalf("不应走关键词兜底，reason=%q", reason)
	}
}

// TestJudgeReceiptNonDoneCanPassWhenDeliverableDone（非 done 锁已松开）：
// 回执状态是 max_iterations，但事实证明主要成果已拿到（置信度 100%、
// 主要文件核对通过）时，前置可以判通过，不再锁死改重干。
func TestJudgeReceiptNonDoneCanPassWhenDeliverableDone(t *testing.T) {
	fr := &Front{Reg: miniRegistry(t)}
	// 桩模型：看事实后判通过。
	fr.CallWithToolsJudge = func(taskID, frozen, tail string, tools []llm.ToolDefinition, history []llm.Message) (llm.Message, string, int64, int64, error) {
		return llm.Message{Role: "assistant", Content: "通过\n主要成果已拿到，文件在盘上。"}, "", 10, 5, nil
	}
	ps := testPlanState()
	step := ps.Steps[0]
	r := &dispatch.Receipt{
		DispatchID: "d1", TaskID: "t-plan", Status: "max_iterations",
		Conclusion: "主要成果已拿到，文件在盘上", FactList: greenFactList(),
	}
	v, reason := fr.judgeReceipt(context.Background(), "t-plan", ps, step, r)
	if v != verdictPass {
		t.Fatalf("状态 max_iterations 但主要成果已拿到时 verdict=%s，want pass（非 done 锁已松开），reason=%s", v, reason)
	}
}

// TestJudgeByModelPromptContainsMissingFilesRule：判卷提示词里有用户定的
// 那句“认账”规则（2026-11-26）：核对结果判“成果文件缺失”只是代码
// 按时间戳算的，前置要看缺失原因再判——都是这单里早一步做的且内容
// 对就通过；有旧文件/不存在/空的或内容不对、置信度低于 100% 时，
// 先读回执核实再决定是否重干。
func TestJudgeByModelPromptContainsMissingFilesRule(t *testing.T) {
	fr := &Front{Reg: miniRegistry(t)}
	var prompt string
	fr.CallWithToolsJudge = func(taskID, frozen, tail string, tools []llm.ToolDefinition, history []llm.Message) (llm.Message, string, int64, int64, error) {
		prompt = tail
		return llm.Message{Role: "assistant", Content: "通过\n文件是这单里早一步做的，内容核对无误。"}, "", 10, 5, nil
	}
	ps := testPlanState()
	step := ps.Steps[0]
	// 置信度 50%（不走快路径），缺失文件都是这单里早一步做的。
	fl := greenFactList()
	fl.Confidence = 50
	fl.ConfidenceToolPoints = 50
	fl.ConfidenceFilePoints = 0
	fl.VerifiedFiles = 0
	fl.MissingFiles = []string{"/tmp/plan-v2-test-out.txt"}
	fl.MissingEarlyTask = 1
	fl.AllGreen = false
	r := &dispatch.Receipt{
		DispatchID: "d1", TaskID: "t-plan", Status: "done",
		Conclusion: "做完了，文件是上一步写的，内容已核对", FactList: fl,
	}
	v, _ := fr.judgeReceipt(context.Background(), "t-plan", ps, step, r)
	if v != verdictPass {
		t.Fatalf("桩模型判通过时 verdict=%s，want pass", v)
	}
	for _, want := range []string{
		"核对结果里“判为 成果文件缺失”只是代码按时间戳算的",
		"都是这单里早一步做的",
		"就判通过，不要重干",
		"有以前的旧文件",
		"有不存在的",
		"有是空的",
		"置信度低于 100%",
		"直接根据回执内容核实之后，再根据你核实的实际结果决定是否重干",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("判卷提示词应含用户那句规则里的 %q，实际提示词：\n%s", want, prompt)
		}
	}
}

// TestJudgeFallbackMiddleCase：事实清单不红也不绿的中间态，不自动重干。
// 对齐 E2E 任务 A（2026-11-25）：research 步天生无机器检查、全绿不了，
// 没声明产物/校验命令的 execute 步也是；兜底“没全绿就重干”会把好步
// 反复重干、烧光 ¥0.05 预算。中间态看回执本身：done + 结论实在 +
// 成功调用 ≥1 就通过，否则重干。0 次调用与产物问题早被硬底线拦了。
func TestJudgeFallbackMiddleCase(t *testing.T) {
	// research 步事实清单：无机器检查、1 次成功调用（只读）、不红不绿。
	middle := func() *dispatch.FactList {
		return &dispatch.FactList{HasMachineCheck: false, SuccessfulCalls: 1, SubstantiveCalls: 0, AllGreen: false}
	}
	good := "干了什么：读回 hello.txt；结果是什么：确认是非空问候语；成果在哪里：/tmp/x/hello.txt；用了哪些工具：cap-ops-python"
	// done + 结论实在 + 成功调用 → 通过。
	r := &dispatch.Receipt{Status: "done", Conclusion: good, FactList: middle()}
	if v, _ := judgeFallback(r); v != verdictPass {
		t.Fatalf("中间态 + 回执实在应判通过，实际 %s", v)
	}
	// done 但结论是空话 → 重干。
	r2 := &dispatch.Receipt{Status: "done", Conclusion: "执行结束，状态：done", FactList: middle()}
	if v, _ := judgeFallback(r2); v != verdictRedo {
		t.Fatalf("中间态 + 结论空洞应判重干，实际 %s", v)
	}
	// done 但结论为空 → 重干。
	r3 := &dispatch.Receipt{Status: "done", Conclusion: "", FactList: middle()}
	if v, _ := judgeFallback(r3); v != verdictRedo {
		t.Fatalf("中间态 + 结论为空应判重干，实际 %s", v)
	}
	// 状态不是 done → 重干（结论再实在也不行）。
	r4 := &dispatch.Receipt{Status: "failed", Conclusion: good, FactList: middle()}
	if v, _ := judgeFallback(r4); v != verdictRedo {
		t.Fatalf("中间态 + 非 done 应判重干，实际 %s", v)
	}
	// execute 步中间态（无机器检查、有实质调用）+ 回执实在 → 通过。
	execMiddle := &dispatch.FactList{HasMachineCheck: false, SuccessfulCalls: 2, SubstantiveCalls: 1, AllGreen: false}
	r5 := &dispatch.Receipt{Status: "done", Conclusion: good, FactList: execMiddle}
	if v, _ := judgeFallback(r5); v != verdictPass {
		t.Fatalf("execute 中间态 + 回执实在应判通过，实际 %s", v)
	}
}

// TestParseJudgeVerdictFirstLinePriority（H-1 回归）：第一行结论优先，
// 第二行的解释文字绝不能反向劫持第一行的结论。
func TestParseJudgeVerdictFirstLinePriority(t *testing.T) {
	r := &dispatch.Receipt{FactList: greenFactList()}
	// 第一行“通过”，第二行含“主动停下”→ 必须通过，不能被第二行停下。
	v, reason := parseJudgeVerdict("通过\n防重守卫脚本已写好，检测到重复任务时会主动停下。", r)
	if v != verdictPass {
		t.Fatalf("第一行通过、第二行含停下应判通过，实际 %s", v)
	}
	if !strings.Contains(reason, "主动停下") {
		t.Fatalf("reason 应保留第二行具体原因，得到 %q", reason)
	}
	// 第一行“通过”，第二行含“无需重干”与 failed 日志 → 必须通过。
	v, reason = parseJudgeVerdict("通过\n上次重干后这次文件已落盘，无需再重干（日志里有一条 failed 重试记录但最终成功）。", r)
	if v != verdictPass {
		t.Fatalf("第一行通过、第二行含重干/failed 应判通过，实际 %s", v)
	}
	if !strings.Contains(reason, "无需再重干") {
		t.Fatalf("reason 应保留第二行具体原因，得到 %q", reason)
	}
	// 第一行“通过”带行尾标点与 Markdown 加粗 → 同样采纳。
	v, _ = parseJudgeVerdict("**通过**。\n产物已落盘。", r)
	if v != verdictPass {
		t.Fatalf("第一行加粗通过应判通过，实际 %s", v)
	}
	// 第一行“结论：通过”前缀 → 同样采纳。
	v, _ = parseJudgeVerdict("结论：通过\n产物已落盘。", r)
	if v != verdictPass {
		t.Fatalf("第一行结论：通过应判通过，实际 %s", v)
	}
}

// TestParseJudgeVerdictNoFalsePass（H-1 回归）：介词“通过”、复合否定
// 与“只完成了一半”绝不能判通过（事实全绿也不行）。
func TestParseJudgeVerdictNoFalsePass(t *testing.T) {
	r := &dispatch.Receipt{FactList: greenFactList()}
	for _, in := range []string{
		"执行层试图通过 python 写入但失败",
		"不符合预期",
		"未能通过",
		"只完成了一半",
		"执行层试图通过 python 写文件，但文件内容为空，建议重新执行。",
		"回执声称已完成，但查目录为空",
	} {
		v, _ := parseJudgeVerdict(in, r)
		if v == verdictPass {
			t.Fatalf("parseJudgeVerdict(%q) 绝不能判通过，实际 %s", in, v)
		}
	}
}

// TestParseJudgeVerdictRedoPreservesReason（M-1 回归）：重干原因要原样
// 传给执行层避坑，不能丢成死字符串。
func TestParseJudgeVerdictRedoPreservesReason(t *testing.T) {
	r := &dispatch.Receipt{FactList: greenFactList()}
	v, reason := parseJudgeVerdict("重干\n缺第 3 列数据", r)
	if v != verdictRedo {
		t.Fatalf("第一行重干应判重干，实际 %s", v)
	}
	if !strings.Contains(reason, "缺第 3 列数据") {
		t.Fatalf("reason 必须保留“缺第 3 列数据”，得到 %q", reason)
	}
	// 结论与原因在同一行时，原因同样保留。
	v, reason = parseJudgeVerdict("重干，文件内容不对，缺第 3 列数据", r)
	if v != verdictRedo {
		t.Fatalf("同行重干应判重干，实际 %s", v)
	}
	if !strings.Contains(reason, "缺第 3 列数据") {
		t.Fatalf("reason 必须保留“缺第 3 列数据”，得到 %q", reason)
	}
}

// TestJudgeFallbackVerifyCmdFailedNeverPasses（H-3 回归）：校验命令明确
// 失败的步，模型判不了落入兜底时不得放行，必须重干。
func TestJudgeFallbackVerifyCmdFailedNeverPasses(t *testing.T) {
	fl := &dispatch.FactList{
		HasMachineCheck: true, SuccessfulCalls: 1, SubstantiveCalls: 1,
		HardFloorRed: false, AllGreen: false,
		VerifyCmd: &dispatch.VerifyCmdFact{Ran: true, Passed: false, ExitCode: 1},
	}
	r := &dispatch.Receipt{
		Status: "done",
		Conclusion: "干了什么：跑了校验脚本；结果是什么：产出了报告；" +
			"成果在哪里：/tmp/x/report.txt；用了哪些工具：cap-ops-python",
		FactList: fl,
	}
	if v, _ := judgeFallback(r); v != verdictRedo {
		t.Fatalf("校验命令失败落入兜底必须重干，实际 %s", v)
	}
}

// TestJudgeFallbackEmptyTemplateOrFailureConclusion（M-2 回归）：四栏全
// 写“无”或结果栏自述失败的 done 回执，兜底不得放行，必须重干。
func TestJudgeFallbackEmptyTemplateOrFailureConclusion(t *testing.T) {
	middle := &dispatch.FactList{HasMachineCheck: false, SuccessfulCalls: 1, SubstantiveCalls: 1, AllGreen: false}
	// 四栏全写“无”：剥掉栏目名只剩 4 个“无”，不到 8 个有效字符。
	r := &dispatch.Receipt{
		Status:     "done",
		Conclusion: "干了什么：无；结果是什么：无；成果在哪里：无；用了哪些工具：无",
		FactList:   middle,
	}
	if v, _ := judgeFallback(r); v != verdictRedo {
		t.Fatalf("四栏全无应判重干，实际 %s", v)
	}
	// 结果栏写“注册失败”：含自我认输词。
	r2 := &dispatch.Receipt{
		Status: "done",
		Conclusion: "干了什么：注册 CF 账号；结果是什么：注册失败；" +
			"成果在哪里：无；用了哪些工具：cap-ops-python",
		FactList: middle,
	}
	if v, _ := judgeFallback(r2); v != verdictRedo {
		t.Fatalf("结果自述失败应判重干，实际 %s", v)
	}
}

// TestJudgeFallbackResumedZeroNewCallsNeverAutoPasses（方向 b）：
// Resumed=true、PriorStepCalls=3、SuccessfulCalls=0、HardFloorRed==false、
// 回执 done 且结论实在 → judgeFallback 必须返回 verdictRedo
// （模型挂了绝不靠历史调用自动通过，fail-closed）。
func TestJudgeFallbackResumedZeroNewCallsNeverAutoPasses(t *testing.T) {
	fl := &dispatch.FactList{
		HasMachineCheck: false, SuccessfulCalls: 0, SubstantiveCalls: 0,
		HardFloorRed: false, AllGreen: false,
		Resumed: true, PriorStepCalls: 3, PriorStepSubstantiveCalls: 3,
	}
	r := &dispatch.Receipt{
		Status: "done",
		Conclusion: "干了什么：写了问候文件；结果是什么：文件已落盘；" +
			"成果在哪里：/tmp/x/greeting.txt；用了哪些工具：cap-ops-python",
		FactList: fl,
	}
	if v, _ := judgeFallback(r); v != verdictRedo {
		t.Fatalf("续跑 0 新调用落入兜底必须重干（绝不靠历史调用自动通过），实际 %s", v)
	}
}

// TestJudgeFallbackVerifyCmdFailedStillRedoWhenResumed（H-3 不退化）：
// 续跑 + PriorStepCalls=3 + VerifyCmd.Passed==false → HardFloorRed==false，
// 但 judgeFallback 必须返回 verdictRedo。
func TestJudgeFallbackVerifyCmdFailedStillRedoWhenResumed(t *testing.T) {
	fl := &dispatch.FactList{
		HasMachineCheck: true, SuccessfulCalls: 0, SubstantiveCalls: 0,
		HardFloorRed: false, AllGreen: false,
		Resumed: true, PriorStepCalls: 3, PriorStepSubstantiveCalls: 3,
		VerifyCmd: &dispatch.VerifyCmdFact{Ran: true, Passed: false, ExitCode: 1},
	}
	r := &dispatch.Receipt{
		Status: "done",
		Conclusion: "干了什么：写了问候文件；结果是什么：文件已落盘；" +
			"成果在哪里：/tmp/x/greeting.txt；用了哪些工具：cap-ops-python",
		FactList: fl,
	}
	if v, _ := judgeFallback(r); v != verdictRedo {
		t.Fatalf("续跑 + 校验命令失败落入兜底必须重干，实际 %s", v)
	}
}

func TestStepToActionDynamicEstSteps(t *testing.T) {
	p := PlanArgs{Goal: "测试", TaskType: "execute"}
	// research 步缺省 → 12。
	act := stepToAction(p, PlanStep{StepNo: 1, ToolID: "test-echo", ExpectedOutput: "查到", StepType: "research"}, nil, nil)
	if act.EstSteps != 12 {
		t.Fatalf("research 缺省 EstSteps=%d, want 12", act.EstSteps)
	}
	// execute 步缺省 → 10。
	act2 := stepToAction(p, PlanStep{StepNo: 2, ToolID: "test-echo", ExpectedOutput: "做完", StepType: "execute"}, nil, nil)
	if act2.EstSteps != 10 {
		t.Fatalf("execute 缺省 EstSteps=%d, want 10", act2.EstSteps)
	}
	// 规划给了 20 → 用 20。
	act3 := stepToAction(p, PlanStep{StepNo: 3, ToolID: "test-echo", ExpectedOutput: "查到", StepType: "research", EstSteps: 20}, nil, nil)
	if act3.EstSteps != 20 {
		t.Fatalf("规划 EstSteps=20 时应用 20，实际 %d", act3.EstSteps)
	}
	// 会话步的 EstSteps 经 planStepFromArgs 与 frontStepFromSession 透传。
	sess := session.PlanStep{StepNo: 1, ToolID: "test-echo", StepType: session.StepResearch, EstSteps: 15, ExpectedOutput: "查到"}
	if got := frontStepFromSession(sess).EstSteps; got != 15 {
		t.Fatalf("frontStepFromSession 应还原 EstSteps=15，实际 %d", got)
	}
}

// TestJudgeByModelFinalWrapUpAfterSelfChecks（CF 诊断 H-1）：自查用满
// 2 次仍不给结论时，追加 1 次 tools=nil 的收口调用强制出结论，
// 只有收口也失败才落入 judgeFallback。
func TestJudgeByModelFinalWrapUpAfterSelfChecks(t *testing.T) {
	calls := 0
	fr := &Front{Reg: miniRegistry(t)}
	fr.CallWithToolsJudge = func(taskID, frozen, tail string, tools []llm.ToolDefinition, history []llm.Message) (llm.Message, string, int64, int64, error) {
		calls++
		if len(tools) == 0 {
			// 收口调用：直接给结论。
			return llm.Message{Role: "assistant", Content: "通过\n做完了，产物齐全。"}, "", 10, 5, nil
		}
		// 自查调用：调 read 查文件。
		var tc llm.ToolCall
		tc.ID = fmt.Sprintf("c%d", calls)
		tc.Type = "function"
		tc.Function.Name = frontToolRead
		tc.Function.Arguments = `{"path":"/tmp/plan-v2-test-out.txt"}`
		return llm.Message{Role: "assistant", Content: "", ToolCalls: []llm.ToolCall{tc}}, "", 10, 5, nil
	}
	ps := testPlanState()
	step := ps.Steps[0]
	// 中间态事实清单：不红不绿，必须走 judgeByModel。
	fl := &dispatch.FactList{HasMachineCheck: false, SuccessfulCalls: 1, SubstantiveCalls: 0, AllGreen: false}
	r := &dispatch.Receipt{Status: "done", Conclusion: "干了什么：读了文件；结果是什么：确认了；成果在哪里：/tmp/x；用了哪些工具：test-echo", FactList: fl}
	v, reason := fr.judgeReceipt(context.Background(), "t-plan", ps, step, r)
	if v != verdictPass {
		t.Fatalf("自查 2 次后收口应判通过，verdict=%s，reason=%s，模型调用 %d 次", v, reason, calls)
	}
	if calls != 4 {
		t.Fatalf("应调模型 4 次（3 次自查循环 + 1 次收口），实际 %d 次", calls)
	}
}

// TestSubstantiveConclusionHonestReportWithErrorFile（CF 诊断高优）：
// 回执诚实提到“报错文件”不算自认失败（cf-011 误杀）；明确的自认
// 失败短语才拦。
func TestSubstantiveConclusionHonestReportWithErrorFile(t *testing.T) {
	// 诚实提到报错文件 + 四栏实在 → 实在。
	r := &dispatch.Receipt{Conclusion: "干了什么：读了 playbook 与报错文件；结果是什么：确认了流程；成果在哪里：/tmp/cf/check.log；用了哪些工具：cap-ops-python"}
	if !substantiveConclusion(r) {
		t.Fatalf("提到“报错文件”的实在回执不应被判不实在")
	}
	// “无报错”也不拦。
	r2 := &dispatch.Receipt{Conclusion: "干了什么：跑了检查脚本；结果是什么：无报错，全部通过；成果在哪里：/tmp/cf/check.log；用了哪些工具：cap-ops-python"}
	if !substantiveConclusion(r2) {
		t.Fatalf("“无报错”的实在回执不应被判不实在")
	}
	// 明确自认失败 → 不实在。
	r3 := &dispatch.Receipt{Conclusion: "干了什么：跑了检查；结果是什么：任务失败，没拿到验证码；成果在哪里：无；用了哪些工具：cap-ops-python"}
	if substantiveConclusion(r3) {
		t.Fatalf("“任务失败”应判不实在")
	}
	r4 := &dispatch.Receipt{Conclusion: "干了什么：跑了检查；结果是什么：未能完成注册；成果在哪里：无；用了哪些工具：cap-ops-python"}
	if substantiveConclusion(r4) {
		t.Fatalf("“未能完成”应判不实在")
	}
	// 空壳（四栏全“无”）→ 不实在。
	r5 := &dispatch.Receipt{Conclusion: "干了什么：无；结果是什么：无；成果在哪里：无；用了哪些工具：无"}
	if substantiveConclusion(r5) {
		t.Fatalf("四栏全“无”的空壳应判不实在")
	}
	// “之前失败的重试也过了”是事后总结（失败已克服），不是自认失败 → 实在。
	r6 := &dispatch.Receipt{Conclusion: "干了什么：按新方法重试注册；结果是什么：之前失败的重试也过了，账号已建好；成果在哪里：/tmp/cf/account.txt；用了哪些工具：cap-ops-python"}
	if !substantiveConclusion(r6) {
		t.Fatalf("“之前失败的重试也过了”的事后总结不应被判不实在")
	}
}

func TestFrontNeverDispatchesCamoufoxCliForBrowser(t *testing.T) {
	fr := &Front{Reg: miniRegistry(t)}
	// 派 cap-fetch-camoufox 干浏览器活：必须打回，且错误里指明换 Python。
	_, err := fr.validateDispatchAction(Action{
		Action:   "dispatch",
		ToolIDs:  []string{"cap-fetch-camoufox"},
		TaskType: "execute",
	})
	if err == nil {
		t.Fatal("派 cap-fetch-camoufox 应被打回，实际放行了")
	}
	if !strings.Contains(err.Error(), "cap-ops-python") {
		t.Fatalf("打回错误应指明换 cap-ops-python，实际错误：%v", err)
	}
	if !strings.Contains(err.Error(), "cap-fetch-camoufox") {
		t.Fatalf("打回错误应点名 cap-fetch-camoufox，实际错误：%v", err)
	}
	// 派 cap-ops-python 干浏览器活：必须放行。
	taskType, err := fr.validateDispatchAction(Action{
		Action:   "dispatch",
		ToolIDs:  []string{"cap-ops-python"},
		TaskType: "execute",
	})
	if err != nil {
		t.Fatalf("派 cap-ops-python 应放行，实际被打回：%v", err)
	}
	if taskType != "execute" {
		t.Fatalf("task_type 应为 execute，实为 %q", taskType)
	}
	// 混派（一个正确工具 + 一个禁派工具）：照样打回。
	_, err = fr.validateDispatchAction(Action{
		Action:   "dispatch",
		ToolIDs:  []string{"cap-ops-python", "cap-fetch-camoufox"},
		TaskType: "execute",
	})
	if err == nil {
		t.Fatal("混派含 cap-fetch-camoufox 应被打回，实际放行了")
	}
}

// TestJudgeReceiptLedgerOverrulesReport（CF-014）：回执结论写着“未产出/
// 失败”，但账本显示确实干了实活、声明产物都在盘上——以账本为准判通过，
// 不许只凭报告那句话判失败、改计划。
func TestJudgeReceiptLedgerOverrulesReport(t *testing.T) {
	fr := &Front{Reg: miniRegistry(t)} // 无模型调用：判回执走 judgeFallback
	ps := testPlanState()
	step := ps.Steps[0]
	// 声明产物文件真实存在（落盘在测试临时目录）。
	art := filepath.Join(t.TempDir(), "flow-notes.md")
	if err := os.WriteFile(art, []byte("# 清单\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fl := &dispatch.FactList{
		HasMachineCheck: true,
		Artifacts: []dispatch.ArtifactFact{
			{Path: art, Exists: true, Size: 8, Fresh: true, NonEmpty: true, Valid: true},
		},
		SuccessfulCalls: 30, SubstantiveCalls: 30,
		HardFloorRed: false, AllGreen: false,
	}
	r := &dispatch.Receipt{
		DispatchID: "d1", TaskID: "t-plan", Status: "done",
		Conclusion: "本步只做调研，未产出，邮箱断了，失败了",
		FactList:   fl,
	}
	// 账本压过报告：判卷结果必须是“通过”，不许是失败/改计划。
	if v, reason := fr.judgeReceipt(context.Background(), "t-plan", ps, step, r); v != verdictPass {
		t.Fatalf("账本证明活已干完、产物在盘上，判卷应通过，实际 %s，reason=%s", v, reason)
	}
	// 兜底本身也要过（不走模型时 judgeFallback 直接判）。
	if v, _ := judgeFallback(r); v != verdictPass {
		t.Fatalf("judgeFallback 账本压过报告应判通过，实际 %s", v)
	}
	// 护栏函数本身。
	if !ledgerOverrulesReport(r) {
		t.Fatal("ledgerOverrulesReport 应为真")
	}
}

// TestJudgeReceiptLedgerOverrulesReportNegative（CF-014 护栏的窄口径）：
// 没有产物证据、校验命令失败、硬底线红时，护栏绝不能把“失败”翻成通过。
func TestJudgeReceiptLedgerOverrulesReportNegative(t *testing.T) {
	fr := &Front{Reg: miniRegistry(t)}
	ps := testPlanState()
	step := ps.Steps[0]
	// (a) 没声明产物：报告说失败就按失败论（守住“注册失败必须重干”）。
	noArt := &dispatch.FactList{HasMachineCheck: false, SuccessfulCalls: 1, SubstantiveCalls: 1, AllGreen: false}
	r := &dispatch.Receipt{DispatchID: "d1", TaskID: "t-plan", Status: "done",
		Conclusion: "干了什么：注册 CF 账号；结果是什么：注册失败；成果在哪里：无；用了哪些工具：cap-ops-python",
		FactList:   noArt}
	if v, _ := fr.judgeReceipt(context.Background(), "t-plan", ps, step, r); v != verdictRedo {
		t.Fatalf("没声明产物、报告说注册失败应判重干，实际 %s", v)
	}
	// (b) 声明产物缺失：护栏不触发。
	badArt := &dispatch.FactList{
		HasMachineCheck: true,
		Artifacts: []dispatch.ArtifactFact{
			{Path: "/tmp/plan-v2-test-out.txt", Exists: false, Problem: "不存在或不可读"},
		},
		SuccessfulCalls: 30, SubstantiveCalls: 30,
		HardFloorRed: true, HardFloorReason: "产物缺失",
		AllGreen: false,
	}
	r2 := &dispatch.Receipt{DispatchID: "d2", TaskID: "t-plan", Status: "done",
		Conclusion: "未产出，失败了", FactList: badArt}
	if v, _ := fr.judgeReceipt(context.Background(), "t-plan", ps, step, r2); v != verdictRedo {
		t.Fatalf("声明产物缺失应判重干，实际 %s", v)
	}
	// (c) 校验命令失败：机器检查是实打实的失败证据，护栏绝不压过。
	art := filepath.Join(t.TempDir(), "flow-notes.md")
	if err := os.WriteFile(art, []byte("# 清单\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	badCmd := &dispatch.FactList{
		HasMachineCheck: true,
		Artifacts: []dispatch.ArtifactFact{
			{Path: art, Exists: true, Size: 8, Fresh: true, NonEmpty: true, Valid: true},
		},
		SuccessfulCalls: 30, SubstantiveCalls: 30,
		HardFloorRed: false, AllGreen: false,
		VerifyCmd: &dispatch.VerifyCmdFact{Ran: true, Passed: false, ExitCode: 1},
	}
	r3 := &dispatch.Receipt{DispatchID: "d3", TaskID: "t-plan", Status: "done",
		Conclusion: "未产出，失败了", FactList: badCmd}
	if v, _ := fr.judgeReceipt(context.Background(), "t-plan", ps, step, r3); v != verdictRedo {
		t.Fatalf("校验命令失败应判重干（护栏不压过机器检查），实际 %s", v)
	}
}

// TestReplanStepsRetriesOnValidationError（CF-014）：改计划第一次编了个
// 不存在的工具，校验打回后把错处喂回模型重改一次，第二次改对了——
// 任务不许死，要接着往下走。
func TestReplanStepsRetriesOnValidationError(t *testing.T) {
	fr := &Front{Reg: miniRegistry(t)}
	calls := 0
	var prompts []string
	fr.CallWithTools = func(taskID, frozen, tail string, tools []llm.ToolDefinition, history []llm.Message) (llm.Message, string, int64, int64, error) {
		calls++
		prompts = append(prompts, tail)
		if calls == 1 {
			// 第一次：编了个不存在的工具 cap-ops-browser（CF-014 原样）。
			return llm.Message{Role: "assistant", Content: `{"steps":[{"step_no":1,"tool_id":"cap-ops-browser","inputs":["x"],"expected_output":"打开网页","step_type":"execute"}]}`}, "", 10, 5, nil
		}
		// 第二次：换成注册表里真实存在的工具。
		return llm.Message{Role: "assistant", Content: `{"steps":[{"step_no":1,"tool_id":"test-echo","inputs":["hi"],"expected_output":"看到 hi","step_type":"execute"}]}`}, "", 10, 5, nil
	}
	ps := &session.PlanState{SessionID: "test-session", TaskID: "t-replan", Goal: "做个东西",
		Steps:       []session.PlanStep{{StepNo: 1, ToolID: "test-echo", Status: session.StepDone, Summary: "第一步做完了"}},
		CurrentStep: 2, TotalSteps: 1}
	step := session.PlanStep{StepNo: 2, ToolID: "test-echo"}
	steps, err := fr.replanSteps(context.Background(), "t-replan", ps, step, "第 2 步卡住了")
	if err != nil {
		t.Fatalf("第二次改对了应返回新步骤，实际出错：%v", err)
	}
	if len(steps) != 1 || steps[0].ToolID != "test-echo" {
		t.Fatalf("新步骤应为 test-echo，实际 %+v", steps)
	}
	if calls != 2 {
		t.Fatalf("应调模型 2 次（第一次被打回、第二次改对），实际 %d 次", calls)
	}
	// 第二次的提示词里必须有第一次的错处（原样喂回）。
	if len(prompts) < 2 || !strings.Contains(prompts[1], "cap-ops-browser") {
		t.Fatalf("第二次提示词应含第一次的错处（cap-ops-browser），实际第二轮提示词：%q", prompts[1])
	}
}

// TestReplanStepsStopsAfterTwoBadAttempts（CF-014）：连续 2 次都编假工具，
// 2 次都改不对，才老实认失败停下；且最多只调模型 2 次。
func TestReplanStepsStopsAfterTwoBadAttempts(t *testing.T) {
	fr := &Front{Reg: miniRegistry(t)}
	calls := 0
	fr.CallWithTools = func(taskID, frozen, tail string, tools []llm.ToolDefinition, history []llm.Message) (llm.Message, string, int64, int64, error) {
		calls++
		return llm.Message{Role: "assistant", Content: `{"steps":[{"step_no":1,"tool_id":"cap-ops-browser","inputs":["x"],"expected_output":"打开网页","step_type":"execute"}]}`}, "", 10, 5, nil
	}
	ps := &session.PlanState{SessionID: "test-session", TaskID: "t-replan", Goal: "做个东西",
		Steps:       []session.PlanStep{{StepNo: 1, ToolID: "test-echo", Status: session.StepDone, Summary: "第一步做完了"}},
		CurrentStep: 2, TotalSteps: 1}
	step := session.PlanStep{StepNo: 2, ToolID: "test-echo"}
	_, err := fr.replanSteps(context.Background(), "t-replan", ps, step, "第 2 步卡住了")
	if err == nil {
		t.Fatal("连续 2 次编假工具应返回错误，实际没错")
	}
	if !strings.Contains(err.Error(), "cap-ops-browser") {
		t.Fatalf("错误应点名编造的工具，实际：%v", err)
	}
	if calls != 2 {
		t.Fatalf("最多调模型 2 次，实际 %d 次", calls)
	}
}

// TestJudgeByModelIncludesReportPointer：判卷提示词里带上这步详细报告的
// 指针（artifacts/<任务>/step-<N>-reply.md），模型拿不准时先读它再判；
// 同时带上新的四行置信度摘要（FactList.Summary() 透传）。
func TestJudgeByModelIncludesReportPointer(t *testing.T) {
	var gotTail string
	fr := &Front{Reg: miniRegistry(t)}
	fr.CallWithToolsJudge = func(taskID, frozen, tail string, tools []llm.ToolDefinition, history []llm.Message) (llm.Message, string, int64, int64, error) {
		gotTail = tail
		return llm.Message{Role: "assistant", Content: "通过\n做完了。"}, "", 10, 5, nil
	}
	ps := testPlanState()
	step := ps.Steps[0]
	fl := &dispatch.FactList{HasMachineCheck: false, SuccessfulCalls: 1, SubstantiveCalls: 1, AllGreen: false,
		Confidence: 50, ConfidenceToolPoints: 50, ConfidenceFilePoints: 0,
		ClaimedFiles: 1, VerifiedFiles: 0, MissingFiles: []string{"/tmp/plan-v2-test-out.txt"}}
	r := &dispatch.Receipt{Status: "done", Conclusion: "ok", FactList: fl}
	v, _ := fr.judgeReceipt(context.Background(), "t-plan", ps, step, r)
	if v != verdictPass {
		t.Fatalf("模型判通过时 verdict 应为 pass，实际 %s", v)
	}
	if want := "artifacts/t-plan/step-1-reply.md"; !strings.Contains(gotTail, want) {
		t.Fatalf("判卷提示词应含详细报告指针 %q，实际提示词：%s", want, gotTail)
	}
	if !strings.Contains(gotTail, "结果置信度：50%") {
		t.Fatalf("判卷提示词应含四行置信度摘要，实际提示词：%s", gotTail)
	}
}

// TestPrevStepSummariesIncludesReportPointer：下一 步派单的 prevSummaries
// 里带上上一步详细报告的指针（artifacts/<任务>/step-<N>-reply.md），
// 执行层先读它再动手。
func TestPrevStepSummariesIncludesReportPointer(t *testing.T) {
	ps := testPlanState()
	// 第 1 步已做完（带摘要与产物），第 2 步待跑。
	ps.Steps[0].Status = session.StepDone
	ps.Steps[0].Summary = "写好了问候文件"
	ps.Steps[0].Artifacts = []string{"/tmp/plan-v2-test-out.txt"}
	ps.Steps = append(ps.Steps, session.PlanStep{StepNo: 2, ToolID: "test-echo", StepType: session.StepExecute, Status: session.StepPending})
	got := prevStepSummaries(ps, 2)
	if len(got) < 2 {
		t.Fatalf("prevSummaries 应至少 2 行（摘要 + 指针），实际 %d 行：%v", len(got), got)
	}
	want := "上一步的详细报告在 artifacts/t-plan/step-1-reply.md，先读它再动手。"
	if got[len(got)-1] != want {
		t.Fatalf("prevSummaries 最后一行应为指针 %q，实际：%q", want, got[len(got)-1])
	}
	// 没有已完成步时不加指针（没有上一步可指）。
	ps2 := testPlanState()
	if got2 := prevStepSummaries(ps2, 1); len(got2) != 0 {
		t.Fatalf("没有已完成步时 prevSummaries 应为空，实际：%v", got2)
	}
}

// TestNoMoneyCapStopsTask：钱顶取消后，本单花再多也不停下——
// 记一笔远超旧上限（¥0.40）的费用（¥10），判卷仍调模型（不短路到兜底），
// 花费照常有记录。旧口径下这笔费用会触发 budgetExceeded，
// 判卷直接走 judgeFallback、任务标 FAILED。
func TestNoMoneyCapStopsTask(t *testing.T) {
	dir := t.TempDir()
	led, err := ledger.Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close(led)
	if _, err := led.RecordExpense(ledger.ExpenseRecord{
		SessionID: "test-session", TaskID: "t-plan", Model: "deepseek-flash",
		Role: "executor", CostCNYMilli: 10000,
	}); err != nil {
		t.Fatal(err)
	}
	fr := &Front{Reg: miniRegistry(t), Led: led}
	calls := 0
	fr.CallWithToolsJudge = func(taskID, frozen, tail string, tools []llm.ToolDefinition, history []llm.Message) (llm.Message, string, int64, int64, error) {
		calls++
		return llm.Message{Role: "assistant", Content: "通过\n做完了。"}, "", 10, 5, nil
	}
	ps := testPlanState()
	step := ps.Steps[0]
	fl := &dispatch.FactList{HasMachineCheck: false, SuccessfulCalls: 1, SubstantiveCalls: 1, AllGreen: false,
		Confidence: 50, ConfidenceToolPoints: 50, ConfidenceFilePoints: 0,
		ClaimedFiles: 1, VerifiedFiles: 0, MissingFiles: []string{"/tmp/plan-v2-test-out.txt"}}
	r := &dispatch.Receipt{Status: "done", Conclusion: "ok", FactList: fl}
	v, _ := fr.judgeReceipt(context.Background(), "t-plan", ps, step, r)
	if v != verdictPass {
		t.Fatalf("大额费用下模型判通过时 verdict 应为 pass，实际 %s", v)
	}
	if calls == 0 {
		t.Fatal("大额费用下判卷应调模型，实际一次没调（被钱顶短路了）")
	}
	// 花费有记录：taskTotalCost 应为 10000 毫分。
	if got := fr.taskTotalCost("t-plan"); got != 10000 {
		t.Fatalf("taskTotalCost 应为 10000 毫分，实际 %d", got)
	}
}

// TestAccountingLine：算账行报本单总花费；余额查得到就报余额，
// 查不到写“余额未知”，不影响任务成败。
// （服务器上有 DeepSeek 密钥时余额能查到；本地无密钥时写“余额未知”。）
func TestAccountingLine(t *testing.T) {
	dir := t.TempDir()
	led, err := ledger.Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close(led)
	if _, err := led.RecordExpense(ledger.ExpenseRecord{
		SessionID: "test-session", TaskID: "t-plan", Model: "deepseek-flash",
		Role: "executor", CostCNYMilli: 10000,
	}); err != nil {
		t.Fatal(err)
	}
	fr := &Front{Reg: miniRegistry(t), Led: led}
	line := fr.accountingLine("t-plan")
	if !strings.Contains(line, "本单花了 ¥10.00") {
		t.Fatalf("算账行应含“本单花了 ¥10.00”，实际：%s", line)
	}
	if !strings.Contains(line, "账户余额 ¥") && !strings.Contains(line, "账户余额未知") {
		t.Fatalf("算账行应报账户余额或写“账户余额未知”，实际：%s", line)
	}
}
