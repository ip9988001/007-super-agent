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
	"007-super-agent/internal/registry"
)

// miniRegistry 造一个最小总账：test-echo / test-true / test-false 等
// 可执行工具（与 agent_test.go 的同名 helper 一致，前置单测自用）。
func miniRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	dst := t.TempDir()
	entries := []struct{ id, ep, contract string }{
		{"test-echo", "echo", "exit 0"},
		{"test-true", "true", "exit 0"},
		{"test-false", "false", "exit 1"},
		{"cap-ops-echo", "echo", "exit 0"},
		{"cap-fetch-echo", "echo", "exit 0"},
		// cap-fetch-camoufox 与 cap-ops-python：刹车根治单测专用
		// （2026-11-26）——TestFrontNeverDispatchesCamoufoxCliForBrowser
		// 要它们在册，才能验到“在册但被打回”与“在册且放行”。
		{"cap-fetch-camoufox", "echo", "exit 0"},
		{"cap-ops-python", "echo", "exit 0"},
		{"test-rm", "rm", "exit 0"},
	}
	var ents strings.Builder
	ents.WriteString(`{"version":"v1.1-test","generated":"2026-11-18","total":8,"categories":[{"id":"fetch","name":"Fetch","description":"Fetch tools for tests."},{"id":"code","name":"Code","description":"Code tools for tests."},{"id":"seo","name":"SEO","description":"SEO tools for tests."},{"id":"design","name":"Design","description":"Design tools for tests."},{"id":"security","name":"Security","description":"Security tools for tests."},{"id":"runtime","name":"Runtime","description":"Runtime tools for tests."},{"id":"media","name":"Media","description":"Media tools for tests."},{"id":"ops","name":"Operations","description":"Operations tools for tests. Use this category for shell commands and test tools."},{"id":"memory","name":"Memory","description":"Memory tools for tests."}],"subcategories":[{"category":"ops","id":"shell","description":"Shell commands and test tools. Use echo and true/false here."}],"entries":[`)
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

// frontToolCallMsg 造一条带一次原生工具调用的 assistant 消息。
func frontToolCallMsg(id, name, args string) llm.Message {
	var tc llm.ToolCall
	tc.ID = id
	tc.Type = "function"
	tc.Function.Name = name
	tc.Function.Arguments = args
	return llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{tc}}
}

// frontTextMsg 造一条纯正文 assistant 消息（无 tool_calls = 直接回话）。
func frontTextMsg(content string) llm.Message {
	return llm.Message{Role: "assistant", Content: content}
}

// frontToolsCallRecord 是原生 stub 一次调用收到的东西（断言用）。
type frontToolsCallRecord struct {
	tools   []llm.ToolDefinition
	history []llm.Message
	tail    string
}

// stubFrontToolsSeq 按顺序返回多条 assistant 消息（用完后重复最后一条），
// 并记录每次调用收到的 tools/history/tail。frs 与 msgs 等长时指定
// 每轮的 finish_reason（可空）。
func stubFrontToolsSeq(t *testing.T, msgs []llm.Message, frs []string, rec *[]frontToolsCallRecord) ModelCallToolsFunc {
	t.Helper()
	i := 0
	return func(taskID, frozen, tail string, tools []llm.ToolDefinition, history []llm.Message) (llm.Message, string, int64, int64, error) {
		cpTools := append([]llm.ToolDefinition(nil), tools...)
		cpHist := append([]llm.Message(nil), history...)
		*rec = append(*rec, frontToolsCallRecord{tools: cpTools, history: cpHist, tail: tail})
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

// newNativeTestFront 建一个原生路径的前置单测对象：接 miniRegistry 与
// 假执行器（收到派单立刻回 done 回执），CallWithTools 用 stub。
// 假回执带一份全绿事实清单（模拟网关已验货：产物在、校验通过、
// 调用 1 次），供规划循环 v2 的快路径走；事实清单缺失时前置判回执
// 会走模型，单测里不希望这样。
func newNativeTestFront(t *testing.T, call ModelCallToolsFunc) *Front {
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
			SubstantiveCalls: 1, SuccessfulCalls: 1,
		}
		for _, p := range d.ExpectedArtifacts {
			fl.Artifacts = append(fl.Artifacts, dispatch.ArtifactFact{
				Path: p, Exists: true, Size: 100, Fresh: true, NonEmpty: true, Valid: true,
			})
		}
		if len(d.VerifyCmd) > 0 {
			fl.VerifyCmd = &dispatch.VerifyCmdFact{Cmd: d.VerifyCmd, Ran: true, ExitCode: 0, Passed: true}
		}
		fl.AllGreen = fl.HasMachineCheck
		// 置信度（2026-11-26）：桩回执模拟“全核对有”的成功步——调了工具，
		// 声明产物全在且通过：有声明产物时工具 50 + 成果 50 = 100%；
		// 无声明产物时工具占 100%。快路径要求 Confidence==100，
		// 这里必须写全，否则 plan_task 的路会多调判卷模型。
		if len(d.ExpectedArtifacts) > 0 {
			fl.Confidence = 100
			fl.ConfidenceToolPoints = 50
			fl.ConfidenceFilePoints = 50
			fl.ClaimedFiles = len(d.ExpectedArtifacts)
			fl.VerifiedFiles = len(d.ExpectedArtifacts)
		} else {
			fl.Confidence = 100
			fl.ConfidenceToolPoints = 100
			fl.ConfidenceFilePoints = 0
			fl.ClaimedFiles = 0
			fl.VerifiedFiles = 0
		}
		fl.ToolCallBreakdown = "test-echo×1"
		return &dispatch.Receipt{
			DispatchID: d.ID, TaskID: d.TaskID, Status: "done",
			Conclusion: "ok", CreatedAt: time.Now().UTC().Format(time.RFC3339),
			FactList: fl,
		}, nil
	})
	return &Front{CallWithTools: call, Reg: rg, Ch: ch, DataDir: dataDir, SessionID: "test-session", Led: led}
}

// TestNativeFrontDeclaresSixTools：原生路径首轮只声明 6 个前置工具
// （recall/registry/read/bash/dispatch/plan_task），不塞注册表的约 90 个
// 工具；模型不调工具只回正文时直接回话（DIRECT），reply.md 落盘。
func TestNativeFrontDeclaresSixTools(t *testing.T) {
	var rec []frontToolsCallRecord
	fr := newNativeTestFront(t, stubFrontToolsSeq(t, []llm.Message{
		frontTextMsg("你好，这是一个纯讨论。"),
	}, nil, &rec))
	res, err := fr.Run(context.Background(), "t-front-1", "你好")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "done" || res.Route != "DIRECT" {
		t.Fatalf("Status=%s Route=%s, want done/DIRECT", res.Status, res.Route)
	}
	if len(rec) != 1 {
		t.Fatalf("模型应被调 1 轮，实际 %d 轮", len(rec))
	}
	want := map[string]bool{
		"recall": true, "registry": true, "read": true,
		"bash": true, "dispatch": true, "plan_task": true,
	}
	if len(rec[0].tools) != len(want) {
		t.Fatalf("声明的工具应为 6 个，实际 %d 个：%v", len(rec[0].tools), rec[0].tools)
	}
	for _, td := range rec[0].tools {
		if !want[td.Function.Name] {
			t.Fatalf("声明里不应有 %q（只应有 6 个前置工具）", td.Function.Name)
		}
		if td.Function.Name == "test-echo" || td.Function.Name == "cap-ops-echo" {
			t.Fatalf("声明里不应塞注册表工具 %q", td.Function.Name)
		}
	}
	if b, err := os.ReadFile(filepath.Join(fr.DataDir, "artifacts", "t-front-1", "reply.md")); err != nil || !strings.Contains(string(b), "你好") {
		t.Fatalf("reply.md 应是正文答案: %v %q", err, string(b))
	}
}

// TestNativeFrontRegistryThenDispatch：模型先查工具单 L1→L2，再用查到
// 的真实编号派单；派单成功，回执状态即本单状态；历史里有 registry 的
// tool 结果，派单带的工具编号是真实的。
func TestNativeFrontRegistryThenDispatch(t *testing.T) {
	var rec []frontToolsCallRecord
	fr := newNativeTestFront(t, stubFrontToolsSeq(t, []llm.Message{
		frontToolCallMsg("c1", "registry", `{"level":"L1"}`),
		frontToolCallMsg("c2", "registry", `{"level":"L2","category":"ops"}`),
		frontToolCallMsg("c3", "dispatch", `{"route":"AGENT","goal":"回显 hi","criteria":"看到 hi","tool_ids":["test-echo"],"task_type":"execute","expected_artifacts":["/tmp/t-front-2.txt"]}`),
	}, nil, &rec))
	res, err := fr.Run(context.Background(), "t-front-2", "回显 hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "done" {
		t.Fatalf("Status=%s, want done", res.Status)
	}
	if len(res.ToolIDs) != 1 || res.ToolIDs[0] != "test-echo" {
		t.Fatalf("ToolIDs=%v, want [test-echo]", res.ToolIDs)
	}
	if len(rec) != 3 {
		t.Fatalf("模型应被调 3 轮，实际 %d 轮", len(rec))
	}
	// 第三轮的历史里应有 registry L1/L2 的 tool 结果（含 test-echo）。
	hist := rec[2].history
	found := false
	for _, m := range hist {
		if m.Role == "tool" && strings.Contains(m.Content, "test-echo") {
			found = true
		}
	}
	if !found {
		t.Fatalf("第三轮历史里应有含 test-echo 的 registry tool 结果: %+v", hist)
	}
}

// TestNativeFrontBadToolIDRetries：模型先用编造的工具编号派单（cap-bash，
// 不在册），代码校验打回成工具结果；模型换真实编号重发，派单成功。
// 幻觉编号绝不能进执行。
func TestNativeFrontBadToolIDRetries(t *testing.T) {
	var rec []frontToolsCallRecord
	fr := newNativeTestFront(t, stubFrontToolsSeq(t, []llm.Message{
		frontToolCallMsg("c1", "dispatch", `{"route":"AGENT","goal":"回显 hi","criteria":"看到 hi","tool_ids":["cap-bash"],"task_type":"execute"}`),
		frontToolCallMsg("c2", "dispatch", `{"route":"AGENT","goal":"回显 hi","criteria":"看到 hi","tool_ids":["test-echo"],"task_type":"execute"}`),
	}, nil, &rec))
	res, err := fr.Run(context.Background(), "t-front-3", "回显 hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "done" {
		t.Fatalf("Status=%s, want done", res.Status)
	}
	if len(res.ToolIDs) != 1 || res.ToolIDs[0] != "test-echo" {
		t.Fatalf("ToolIDs=%v, want [test-echo]", res.ToolIDs)
	}
	if len(rec) != 2 {
		t.Fatalf("模型应被调 2 轮，实际 %d 轮", len(rec))
	}
	// 第二轮的历史里应有第一轮 dispatch 校验失败的 tool 结果。
	hist := rec[1].history
	found := false
	for _, m := range hist {
		if m.Role == "tool" && strings.Contains(m.Content, "cap-bash") && strings.Contains(m.Content, "不可用") {
			found = true
		}
	}
	if !found {
		t.Fatalf("第二轮历史里应有 cap-bash 校验失败的 tool 结果: %+v", hist)
	}
}

// TestNativeFrontEmptyToolIDRetries：派单 tool_ids 里有空串，校验打回；
// 模型换真实编号重发，派单成功。
func TestNativeFrontEmptyToolIDRetries(t *testing.T) {
	var rec []frontToolsCallRecord
	fr := newNativeTestFront(t, stubFrontToolsSeq(t, []llm.Message{
		frontToolCallMsg("c1", "dispatch", `{"route":"AGENT","goal":"回显 hi","criteria":"看到 hi","tool_ids":[""],"task_type":"execute"}`),
		frontToolCallMsg("c2", "dispatch", `{"route":"AGENT","goal":"回显 hi","criteria":"看到 hi","tool_ids":["test-true"],"task_type":"execute"}`),
	}, nil, &rec))
	res, err := fr.Run(context.Background(), "t-front-4", "回显 hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "done" || len(res.ToolIDs) != 1 || res.ToolIDs[0] != "test-true" {
		t.Fatalf("Status=%s ToolIDs=%v, want done/[test-true]", res.Status, res.ToolIDs)
	}
	if len(rec) != 2 {
		t.Fatalf("模型应被调 2 轮，实际 %d 轮", len(rec))
	}
}

// TestNativeFrontTruncationVoidsRound：原生路径截断整轮作废——
// 截断轮的 registry 工具调用不执行、不进历史；下一轮模型重发后正常收口。
func TestNativeFrontTruncationVoidsRound(t *testing.T) {
	var rec []frontToolsCallRecord
	fr := newNativeTestFront(t, stubFrontToolsSeq(t, []llm.Message{
		frontToolCallMsg("c1", "registry", `{"level":"L1"}`),
		frontTextMsg("截断后重发，直接回话。"),
	}, []string{"length", ""}, &rec))
	res, err := fr.Run(context.Background(), "t-front-5", "你好")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "done" || res.Route != "DIRECT" {
		t.Fatalf("Status=%s Route=%s, want done/DIRECT", res.Status, res.Route)
	}
	if len(rec) != 2 {
		t.Fatalf("模型应被调 2 轮，实际 %d 轮", len(rec))
	}
	// 第二轮的尾巴里应有截断提醒；历史里不应有第一轮 registry 的结果
	// （截断轮的 assistant 消息作废、不进历史）。
	if !strings.Contains(rec[1].tail, "截断") {
		t.Fatalf("第二轮尾巴应有截断提醒: %q", rec[1].tail)
	}
	for _, m := range rec[1].history {
		if m.Role == "tool" {
			t.Fatalf("截断轮的工具调用不应进历史: %+v", m)
		}
	}
	// 首轮 user 消息（任务正文）仍在历史里，未因截断丢失。
	if len(rec[1].history) == 0 || rec[1].history[0].Role != "user" {
		t.Fatalf("历史第一条应是首轮 user 消息: %+v", rec[1].history)
	}
}

// TestNativeFrontContentFilterBlocked：内容过滤/空回复整轮作废，
// 模型换个说法重发后正常收口，不写空 reply.md。
func TestNativeFrontContentFilterBlocked(t *testing.T) {
	var rec []frontToolsCallRecord
	fr := newNativeTestFront(t, stubFrontToolsSeq(t, []llm.Message{
		frontTextMsg(""),
		frontTextMsg("换个说法，直接回话。"),
	}, []string{"content_filter", ""}, &rec))
	res, err := fr.Run(context.Background(), "t-front-6", "你好")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "done" || res.Route != "DIRECT" {
		t.Fatalf("Status=%s Route=%s, want done/DIRECT", res.Status, res.Route)
	}
	if b, err := os.ReadFile(filepath.Join(fr.DataDir, "artifacts", "t-front-6", "reply.md")); err != nil || !strings.Contains(string(b), "换个说法") {
		t.Fatalf("reply.md 应是第二轮的正文: %v %q", err, string(b))
	}
}

// TestValidatePlanInputs：plan_task 的 inputs 校验只拦 shell 语法与
// ./、../，普通参数（正则里的 |()、绝对路径）不误杀。
func TestValidatePlanInputs(t *testing.T) {
	if err := validatePlanInputs("test-echo", []string{"test-echo", "hi"}); err == nil {
		t.Fatalf("inputs 首个参数重复写工具名应报错")
	}
	if err := validatePlanInputs("test-echo", []string{"./rel.txt"}); err == nil {
		t.Fatalf("inputs 含 ./ 相对路径应报错")
	}
	if err := validatePlanInputs("test-echo", []string{"../up.txt"}); err == nil {
		t.Fatalf("inputs 含 ../ 相对路径应报错")
	}
	if err := validatePlanInputs("test-echo", []string{"a", "|", "b"}); err == nil {
		t.Fatalf("inputs 含独立的 | 应报错")
	}
	if err := validatePlanInputs("test-echo", []string{"a", "&&", "b"}); err == nil {
		t.Fatalf("inputs 含独立的 && 应报错")
	}
	if err := validatePlanInputs("test-echo", []string{"$HOME/x"}); err == nil {
		t.Fatalf("inputs 含 $ 变量应报错")
	}
	// 正则里的 |() 是合法参数，不误杀（B-04）。
	if err := validatePlanInputs("test-echo", []string{`(?<=src=")[^"]+\.js`, "hi"}); err != nil {
		t.Fatalf("正则参数不应被误杀: %v", err)
	}
	if err := validatePlanInputs("test-echo", []string{"--out", "/tmp/x.txt"}); err != nil {
		t.Fatalf("绝对路径参数不应被误杀: %v", err)
	}
	if err := validatePlanInputs("test-echo", nil); err != nil {
		t.Fatalf("空 inputs 应通过: %v", err)
	}
}

// TestValidatePlan：plan_task 的计划校验——工具编号白名单、inputs、
// 计划校验：目标、步骤数、工具编号白名单、inputs、step_type。
// 验收（expected_artifacts / verify_cmd）全可选（对齐二号三号），
// 不校验；非法时返回 ErrDispatchValidation（可重试）。
func TestValidatePlan(t *testing.T) {
	rg := miniRegistry(t)
	fr := &Front{Reg: rg}
	ok := PlanArgs{
		Goal: "回显 hi",
		Steps: []PlanStep{
			{StepNo: 1, ToolID: "test-echo", Inputs: []string{"hi"}, ExpectedOutput: "看到 hi", VerifyCmd: []string{"true"}},
		},
	}
	if err := fr.validatePlan(ok); err != nil {
		t.Fatalf("合法计划应通过: %v", err)
	}
	badTool := PlanArgs{
		Goal: "回显 hi",
		Steps: []PlanStep{
			{StepNo: 1, ToolID: "python3", Inputs: []string{"hi"}, ExpectedOutput: "看到 hi", VerifyCmd: []string{"true"}},
		},
	}
	if err := fr.validatePlan(badTool); err == nil {
		t.Fatalf("编造的工具编号应报错")
	} else if !strings.Contains(err.Error(), "python3") {
		t.Fatalf("报错应点名 python3: %v", err)
	}
	// 对齐二号三号（2026-11-25）：验收全可选——某步既无
	// expected_artifacts 又无 verify_cmd 也放行，
	// 网关有账本兜底与硬底线把关。
	noVerify := PlanArgs{
		Goal: "回显 hi",
		Steps: []PlanStep{
			{StepNo: 1, ToolID: "test-echo", Inputs: []string{"hi"}, ExpectedOutput: "看到 hi"},
		},
	}
	if err := fr.validatePlan(noVerify); err != nil {
		t.Fatalf("每步缺验收也应通过（验收可选）: %v", err)
	}
	badInputs := PlanArgs{
		Goal: "回显 hi",
		Steps: []PlanStep{
			{StepNo: 1, ToolID: "test-echo", Inputs: []string{"./x"}, ExpectedOutput: "看到 hi", VerifyCmd: []string{"true"}},
		},
	}
	if err := fr.validatePlan(badInputs); err == nil {
		t.Fatalf("inputs 含相对路径应报错")
	}
	empty := PlanArgs{Goal: "回显 hi"}
	if err := fr.validatePlan(empty); err == nil {
		t.Fatalf("steps 为空应报错")
	}
}

// TestStepToAction：计划里的一步转单步 AGENT 派单——工具编号只有
// 这一步的、步骤骨架只有这一行、验收与步类型都取自该步、步号进 PlanStepNo。
func TestStepToAction(t *testing.T) {
	p := PlanArgs{
		Goal: "两步回显", Criteria: "两次都看到",
		Steps: []PlanStep{
			{StepNo: 1, ToolID: "test-echo", Inputs: []string{"hi"}, ExpectedOutput: "看到 hi", ExpectedArtifacts: []string{"/tmp/a.txt"}},
			{StepNo: 2, ToolID: "test-echo", Inputs: []string{"bye"}, ExpectedOutput: "看到 bye", ExpectedArtifacts: []string{"/tmp/b.txt"}},
			{StepNo: 3, ToolID: "test-true", ExpectedOutput: "通过", VerifyCmd: []string{"true"}},
		},
	}
	act := stepToAction(p, p.Steps[0], []string{"碎片"}, []string{"前面已做完的步"})
	if act.Route != "AGENT" || act.Goal == "" || !strings.Contains(act.Goal, "第 1 步") {
		t.Fatalf("Route/Goal 不对: %+v", act)
	}
	if len(act.ToolIDs) != 1 || act.ToolIDs[0] != "test-echo" {
		t.Fatalf("ToolIDs 应只有 [test-echo]，实际 %v", act.ToolIDs)
	}
	if len(act.Plan) != 1 || !strings.Contains(act.Plan[0], "test-echo") || !strings.Contains(act.Plan[0], "/tmp/a.txt") {
		t.Fatalf("Plan 骨架应只有这一步且含验收: %v", act.Plan)
	}
	if len(act.ExpectedArtifacts) != 1 || act.ExpectedArtifacts[0] != "/tmp/a.txt" {
		t.Fatalf("ExpectedArtifacts 应只有这一步的产物，实际 %v", act.ExpectedArtifacts)
	}
	if act.PlanStepNo != 1 {
		t.Fatalf("PlanStepNo=%d, want 1", act.PlanStepNo)
	}
	if len(act.Memory) != 2 || act.Memory[1] != "前面已做完的步" {
		t.Fatalf("Memory 应带记忆碎片 + 前面步的摘要，实际 %v", act.Memory)
	}
	// 第三步（verify_cmd 步）转出来应带 VerifyCmd、步类型 execute。
	act3 := stepToAction(p, p.Steps[2], nil, nil)
	if len(act3.VerifyCmd) != 1 || act3.VerifyCmd[0] != "true" {
		t.Fatalf("VerifyCmd 不对: %v", act3.VerifyCmd)
	}
	if act3.TaskType != "execute" || act3.PlanStepNo != 3 {
		t.Fatalf("TaskType/PlanStepNo 不对: %+v", act3)
	}
}

// TestNativeFrontPlanTaskBadThenGood：plan_task 先给编造的工具编号，
// 校验打回成工具结果；模型换真实编号重发，计划派单成功。
// 规划 v2：模型共调 3 轮——第 1 轮 plan_task（编造工具被打回），
// 第 2 轮 plan_task（真实工具，规划循环做完），第 3 轮是最终汇报
// 的模型调用（桩重复最后一条 plan_task 工具调用，走模板汇报兜底）。
func TestNativeFrontPlanTaskBadThenGood(t *testing.T) {
	var rec []frontToolsCallRecord
	fr := newNativeTestFront(t, stubFrontToolsSeq(t, []llm.Message{
		frontToolCallMsg("c1", "plan_task", `{"goal":"回显 hi","steps":[{"step_no":1,"tool_id":"cap-shell-exec","inputs":["hi"],"expected_output":"看到 hi","verify_cmd":["true"]}]}`),
		frontToolCallMsg("c2", "plan_task", `{"goal":"回显 hi","steps":[{"step_no":1,"tool_id":"test-echo","inputs":["hi"],"expected_output":"看到 hi","expected_artifacts":["/tmp/t-front-7.txt"]}]}`),
	}, nil, &rec))
	res, err := fr.Run(context.Background(), "t-front-7", "回显 hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "done" {
		t.Fatalf("Status=%s, want done", res.Status)
	}
	if len(res.ToolIDs) != 1 || res.ToolIDs[0] != "test-echo" {
		t.Fatalf("ToolIDs=%v, want [test-echo]", res.ToolIDs)
	}
	if len(rec) != 3 {
		t.Fatalf("模型应被调 3 轮（2 轮规划 + 1 轮最终汇报），实际 %d 轮", len(rec))
	}
	hist := rec[1].history
	found := false
	for _, m := range hist {
		if m.Role == "tool" && strings.Contains(m.Content, "cap-shell-exec") {
			found = true
		}
	}
	if !found {
		t.Fatalf("第二轮历史里应有 cap-shell-exec 校验失败的 tool 结果: %+v", hist)
	}
}

// TestNativeFrontReadBashCap：read/bash 自查合计超 3 次后，第 4 次
// 回填“自查已满”而不是真执行。
func TestNativeFrontReadBashCap(t *testing.T) {
	var rec []frontToolsCallRecord
	fr := newNativeTestFront(t, stubFrontToolsSeq(t, []llm.Message{
		frontToolCallMsg("c1", "bash", `{"command":"pwd"}`),
		frontToolCallMsg("c2", "bash", `{"command":"pwd"}`),
		frontToolCallMsg("c3", "bash", `{"command":"pwd"}`),
		frontToolCallMsg("c4", "bash", `{"command":"pwd"}`),
		frontTextMsg("查够了，直接回话。"),
	}, nil, &rec))
	res, err := fr.Run(context.Background(), "t-front-8", "看看目录")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "done" || res.Route != "DIRECT" {
		t.Fatalf("Status=%s Route=%s, want done/DIRECT", res.Status, res.Route)
	}
	// 第 5 轮的历史里应有“自查已满 3 次”的 tool 结果。
	hist := rec[4].history
	found := false
	for _, m := range hist {
		if m.Role == "tool" && strings.Contains(m.Content, "自查已满 3 次") {
			found = true
		}
	}
	if !found {
		t.Fatalf("第 5 轮历史里应有自查已满的 tool 结果: %+v", hist)
	}
}

// TestTextPathStillWorks：文本 JSON 路径未动——模型回 JSON reply 动作，
// 直接回话；CallWithTools 为空时走旧路径。
func TestTextPathStillWorks(t *testing.T) {
	rg := miniRegistry(t)
	led := openTestLedger(t)
	dataDir := t.TempDir()
	ch := dispatch.New(dataDir)
	fr := &Front{
		Call: func(taskID, frozen, tail string) (string, int64, int64, error) {
			return `{"action":"reply","text":"文本路径还在。"}`, 100, 20, nil
		},
		Reg: rg, Ch: ch, DataDir: dataDir, SessionID: "test-session", Led: led,
	}
	res, err := fr.Run(context.Background(), "t-front-9", "你好")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "done" || res.Route != "DIRECT" {
		t.Fatalf("Status=%s Route=%s, want done/DIRECT", res.Status, res.Route)
	}
	if b, err := os.ReadFile(filepath.Join(dataDir, "artifacts", "t-front-9", "reply.md")); err != nil || !strings.Contains(string(b), "文本路径还在") {
		t.Fatalf("reply.md 应是正文答案: %v %q", err, string(b))
	}
}

// TestNativeFrontFallbackDispatch：5 轮都不收口（只查工具单不派单），
// 触发 F-12 兜底：按手头信息派 AGENT 单（工具未定则空着），回执 done。
func TestNativeFrontFallbackDispatch(t *testing.T) {
	var rec []frontToolsCallRecord
	fr := newNativeTestFront(t, stubFrontToolsSeq(t, []llm.Message{
		frontToolCallMsg("c1", "registry", `{"level":"L1"}`),
	}, nil, &rec))
	res, err := fr.Run(context.Background(), "t-front-10", "做点事")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "done" {
		t.Fatalf("Status=%s, want done", res.Status)
	}
	if len(rec) != MaxFrontRounds {
		t.Fatalf("模型应被调 %d 轮，实际 %d 轮", MaxFrontRounds, len(rec))
	}
}

// ---- 第二轮修复（2026-11-26，Gemini 刹车审阅 M2/M5c） ----

// TestNativeFrontCamoufoxRejectedThenPythonAccepted（M2/M5c 场景 A）：
// 第 1 轮派 cap-fetch-camoufox 被代码层打回（ErrDispatchValidation），
// 模型换 cap-ops-python 重派后放行，整单 done；模型只被调 2 轮。
func TestNativeFrontCamoufoxRejectedThenPythonAccepted(t *testing.T) {
	var rec []frontToolsCallRecord
	fr := newNativeTestFront(t, stubFrontToolsSeq(t, []llm.Message{
		frontToolCallMsg("c1", "dispatch", `{"route":"AGENT","goal":"打开网页注册","criteria":"页面打开","tool_ids":["cap-fetch-camoufox"],"task_type":"execute"}`),
		frontToolCallMsg("c2", "dispatch", `{"route":"AGENT","goal":"打开网页注册","criteria":"页面打开","tool_ids":["cap-ops-python"],"task_type":"execute"}`),
	}, nil, &rec))
	res, err := fr.Run(context.Background(), "t-front-m2a", "打开网页注册")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "done" {
		t.Fatalf("Status=%s, want done", res.Status)
	}
	if len(res.ToolIDs) != 1 || res.ToolIDs[0] != "cap-ops-python" {
		t.Fatalf("ToolIDs=%v, want [cap-ops-python]", res.ToolIDs)
	}
	if len(rec) != 2 {
		t.Fatalf("模型应被调 2 轮，实际 %d 轮", len(rec))
	}
	// 第二轮的历史里应有第一轮打回的 tool 结果，点名 cap-fetch-camoufox
	// 且指明换 cap-ops-python。
	hist := rec[1].history
	found := false
	for _, m := range hist {
		if m.Role == "tool" && strings.Contains(m.Content, "cap-fetch-camoufox") && strings.Contains(m.Content, "cap-ops-python") {
			found = true
		}
	}
	if !found {
		t.Fatalf("第二轮历史里应有 cap-fetch-camoufox 打回的 tool 结果: %+v", hist)
	}
}

// TestNativeFrontCamoufoxAlwaysRejectedStopsAtCap（M2/M5c 场景 B）：
// 模型一直派 cap-fetch-camoufox（stub 重复最后一条），连续校验失败
// 超 2 次（第 3 次）就停下写失败回信，不再重问模型——模型只被调 3 轮，
// 绝不是跑满 7 轮再走 F-12 兜底派单；状态是 failed，不是 done。
func TestNativeFrontCamoufoxAlwaysRejectedStopsAtCap(t *testing.T) {
	var rec []frontToolsCallRecord
	fr := newNativeTestFront(t, stubFrontToolsSeq(t, []llm.Message{
		frontToolCallMsg("c1", "dispatch", `{"route":"AGENT","goal":"打开网页注册","criteria":"页面打开","tool_ids":["cap-fetch-camoufox"],"task_type":"execute"}`),
	}, nil, &rec))
	res, err := fr.Run(context.Background(), "t-front-m2b", "打开网页注册")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "failed" {
		t.Fatalf("Status=%s, want failed（连续校验失败超限应停下）", res.Status)
	}
	if len(rec) != 3 {
		t.Fatalf("模型应被调 3 轮（第 3 次打回后停下），实际 %d 轮", len(rec))
	}
	if !strings.Contains(res.ReplyDraft, "连续 3 次校验失败") {
		t.Fatalf("失败回信应写明“连续 3 次校验失败”，实际：%q", res.ReplyDraft)
	}
}

// TestTryAttachSearchHintIdempotent：重复调用不重复贴 hint。
func TestTryAttachSearchHintIdempotent(t *testing.T) {
	d := &dispatch.Dispatch{PlanSkeleton: []string{"做 CF 注册"}}
	if !tryAttachSearchHint(d) {
		t.Fatalf("第一次应贴上 hint")
	}
	if !tryAttachSearchHint(d) {
		t.Fatalf("第二次也应返回 true（已在）")
	}
	if strings.Count(strings.Join(d.PlanSkeleton, "\n"), memorySearchHint) != 1 {
		t.Fatalf("hint 只应出现 1 次：%v", d.PlanSkeleton)
	}
}

// TestTryAttachSearchHintWithFullCards：卡片齐全（不限于 0 张）时也贴 hint，
// 不再只在 0 张卡片时贴；且 hint 不再有“不重查”压制。
func TestTryAttachSearchHintWithFullCards(t *testing.T) {
	d := &dispatch.Dispatch{
		Memory:       []string{"【W-0001】CF 注册流程：用 Camoufox 过 Cloudflare 注册验证。", "【W-0002】另一条记忆。"},
		PlanSkeleton: []string{"做 CF 注册"},
	}
	if !tryAttachSearchHint(d) {
		t.Fatalf("卡片齐全时也应贴上 hint")
	}
	if !strings.Contains(strings.Join(d.PlanSkeleton, "\n"), memorySearchHint) {
		t.Fatalf("PlanSkeleton 应含 hint：%v", d.PlanSkeleton)
	}
	if strings.Contains(memorySearchHint, "不重查") {
		t.Fatalf("hint 不应再有“不重查”压制：%q", memorySearchHint)
	}
}

// TestStepToActionCarriesMustReadMemories：规划里的 must_read_memories
// 应带进派单 Action。
func TestStepToActionCarriesMustReadMemories(t *testing.T) {
	p := PlanArgs{
		Goal: "注册 CF", Criteria: "拿到密钥",
		MustReadMemories: []string{"W-0001"},
		Steps: []PlanStep{
			{StepNo: 1, ToolID: "test-echo", Inputs: []string{"hi"}, ExpectedOutput: "看到 hi"},
		},
	}
	act := stepToAction(p, p.Steps[0], nil, nil)
	if len(act.MustReadMemories) != 1 || act.MustReadMemories[0] != "W-0001" {
		t.Fatalf("Action 应带 must_read_memories [W-0001]，实际 %v", act.MustReadMemories)
	}
}
