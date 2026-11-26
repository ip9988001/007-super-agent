package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"007-super-agent/internal/llm"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return New(t.TempDir(), nil)
}

func TestValidateSessionID(t *testing.T) {
	good := []string{"sess-1", "abc_XYZ-09", "a", strings.Repeat("x", 64)}
	for _, id := range good {
		if err := ValidateSessionID(id); err != nil {
			t.Errorf("ValidateSessionID(%q) 应通过: %v", id, err)
		}
	}
	bad := []string{"", "../etc", "a/b", "a b", "a.b", "a:b", strings.Repeat("x", 65), "中文"}
	for _, id := range bad {
		if err := ValidateSessionID(id); err == nil {
			t.Errorf("ValidateSessionID(%q) 应拒绝", id)
		}
	}
}

func TestDefaultSessionID(t *testing.T) {
	// 正常任务号原样通过。
	if got := DefaultSessionID("prod-smoke-001"); got != "prod-smoke-001" {
		t.Errorf("DefaultSessionID 正常号应原样返回，得到 %q", got)
	}
	// 非法字符换成 -，且结果合规；同一输入两次结果相同（可重放找回）。
	a := DefaultSessionID("task/with.dots:and spaces!")
	b := DefaultSessionID("task/with.dots:and spaces!")
	if a != b {
		t.Errorf("DefaultSessionID 不确定：%q vs %q", a, b)
	}
	if err := ValidateSessionID(a); err != nil {
		t.Errorf("DefaultSessionID 输出不合规 %q: %v", a, err)
	}
	// 超长截断后仍合规且确定。
	long := DefaultSessionID(strings.Repeat("abcdefghij", 10))
	if err := ValidateSessionID(long); err != nil {
		t.Errorf("DefaultSessionID 超长输出不合规 %q: %v", long, err)
	}
	if len(long) > 64 {
		t.Errorf("DefaultSessionID 超长输出超 64 字：%d", len(long))
	}
}

func TestPlanRoundTripAndStatus(t *testing.T) {
	s := newTestStore(t)
	ps := s.NewPlan("sess-1", "task-1")
	if ps.Status != PlanRunning {
		t.Fatalf("新计划应为 RUNNING，得到 %s", ps.Status)
	}
	if err := s.SavePlan(ps); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	got, err := s.LoadPlan("sess-1")
	if err != nil || got == nil {
		t.Fatalf("LoadPlan: %v, got=%v", err, got)
	}
	if got.PlanID != ps.PlanID || got.SessionID != "sess-1" || got.TaskID != "task-1" {
		t.Errorf("往返后字段变了：%+v", got)
	}
	// 状态流转：RUNNING → DONE。
	got.Status = PlanDone
	if err := s.SavePlan(got); err != nil {
		t.Fatalf("SavePlan DONE: %v", err)
	}
	got2, _ := s.LoadPlan("sess-1")
	if got2.Status != PlanDone {
		t.Errorf("状态流转失败，得到 %s", got2.Status)
	}
	// 不存在的会话返回 nil, nil。
	none, err := s.LoadPlan("no-such-session")
	if err != nil || none != nil {
		t.Errorf("不存在的会话应返回 nil,nil，得到 %v, %v", none, err)
	}
	// 损坏的 plan_state 返回错误（不静默丢弃）。
	p, _ := s.PlanPath("sess-1")
	if err := os.WriteFile(p, []byte("{不是 json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadPlan("sess-1"); err == nil {
		t.Errorf("损坏的 plan_state 应返回错误")
	}
}

func TestOpenSessionTerminalAntiOverwrite(t *testing.T) {
	s := newTestStore(t)
	// 建一个已终结的计划（含一个已完成步）。
	ps := s.NewPlan("sess-1", "task-1")
	ps.Steps = append(ps.Steps, PlanStep{StepNo: 1, TaskID: "task-1", ToolID: "cap-ops-python", StepType: StepExecute, Status: StepDone, Summary: "写了 hello", Artifacts: []string{"/tmp/x/hello.txt"}})
	ps.TotalSteps = 1
	ps.CurrentStep = 2
	ps.Status = PlanDone
	if err := s.SavePlan(ps); err != nil {
		t.Fatal(err)
	}
	// 新任务同 session：不得覆写旧计划，应新开 plan_id，只继承摘要。
	np, isNew, err := s.OpenSession("sess-1", "task-2")
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if !isNew {
		t.Errorf("终结计划后新任务应新开计划")
	}
	if np.PlanID == ps.PlanID {
		t.Errorf("新计划 plan_id 不得与旧计划相同")
	}
	if np.PrevPlanID != ps.PlanID {
		t.Errorf("新计划应记 PrevPlanID=%s，得到 %q", ps.PlanID, np.PrevPlanID)
	}
	if len(np.PrevSummaries) != 1 || !strings.Contains(np.PrevSummaries[0], "hello.txt") {
		t.Errorf("新计划应继承旧计划已完成步摘要，得到 %v", np.PrevSummaries)
	}
	if len(np.Steps) != 0 {
		t.Errorf("新计划步表应为空，得到 %d 步", len(np.Steps))
	}
	// 未终结的计划：同一会话再打开应返回原计划（不新开）。
	np2, isNew2, err := s.OpenSession("sess-1", "task-3")
	if err != nil {
		t.Fatalf("OpenSession 2: %v", err)
	}
	if isNew2 || np2.PlanID != np.PlanID {
		t.Errorf("未终结计划应返回原计划，isNew=%v plan=%s", isNew2, np2.PlanID)
	}
}

func TestOpenSessionCorruptRebuild(t *testing.T) {
	s := newTestStore(t)
	// 先写两个步历史文件，再把 plan_state 写坏。
	if err := s.AppendMessages("sess-1", 1, "task-1", []llm.Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMessages("sess-1", 2, "task-1", []llm.Message{{Role: "user", Content: "hi2"}}); err != nil {
		t.Fatal(err)
	}
	p, _ := s.PlanPath("sess-1")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{坏"), 0o600); err != nil {
		t.Fatal(err)
	}
	np, isNew, err := s.OpenSession("sess-1", "task-2")
	if err != nil {
		t.Fatalf("OpenSession 损坏重建: %v", err)
	}
	if !isNew {
		t.Errorf("损坏后应重建新计划")
	}
	if np.CurrentStep != 3 {
		t.Errorf("重建后 CurrentStep 应为 3（2 个历史文件 +1），得到 %d", np.CurrentStep)
	}
}

func TestAllocateStepDedupAndComplete(t *testing.T) {
	s := newTestStore(t)
	// 第一次分配：步 1。
	n1, err := s.AllocateStep("sess-1", "task-1", PlanStep{ToolID: "cap-ops-python", StepType: StepExecute, ExpectedArtifacts: []string{"/tmp/out.txt"}})
	if err != nil || n1 != 1 {
		t.Fatalf("AllocateStep 1: n=%d err=%v", n1, err)
	}
	// 同任务、步还在 RUNNING：复用步 1（崩溃重放），Attempts+1。
	n2, err := s.AllocateStep("sess-1", "task-1", PlanStep{ToolID: "cap-ops-python", StepType: StepExecute})
	if err != nil || n2 != 1 {
		t.Fatalf("AllocateStep 复用：n=%d err=%v", n2, err)
	}
	ps, _ := s.LoadPlan("sess-1")
	if ps.Steps[0].Attempts != 2 {
		t.Errorf("复用后 Attempts 应为 2，得到 %d", ps.Steps[0].Attempts)
	}
	// 完成步 1。
	if err := s.CompleteStep("sess-1", 1, StepDone, "写好了", []string{"/tmp/out.txt"}, ""); err != nil {
		t.Fatalf("CompleteStep: %v", err)
	}
	ps, _ = s.LoadPlan("sess-1")
	if ps.Steps[0].Status != StepDone || len(ps.Steps[0].Artifacts) != 1 {
		t.Errorf("完成步状态不对：%+v", ps.Steps[0])
	}
	if ps.CurrentStep != 2 {
		t.Errorf("完成后 CurrentStep 应为 2，得到 %d", ps.CurrentStep)
	}
	// 同任务再派单：上一步已 DONE，应开新步 2。
	n3, err := s.AllocateStep("sess-1", "task-1", PlanStep{ToolID: "cap-fetch-curl-jq", StepType: StepResearch})
	if err != nil || n3 != 2 {
		t.Fatalf("AllocateStep 2: n=%d err=%v", n3, err)
	}
	// 失败步记进 Failures。
	if err := s.CompleteStep("sess-1", 2, StepFailed, "", nil, "agent_error"); err != nil {
		t.Fatalf("CompleteStep 失败步: %v", err)
	}
	ps, _ = s.LoadPlan("sess-1")
	if len(ps.Failures) != 1 || ps.Failures[0] != "agent_error" {
		t.Errorf("失败原因应记进 Failures，得到 %v", ps.Failures)
	}
	// 不存在的步更新应报错。
	if err := s.CompleteStep("sess-1", 99, StepDone, "", nil, ""); err == nil {
		t.Errorf("更新不存在的步应报错")
	}
	// step_type 非法应报错。
	if _, err := s.AllocateStep("sess-1", "task-1", PlanStep{ToolID: "x", StepType: "bogus"}); err == nil {
		t.Errorf("非法 step_type 应报错")
	}
}

func TestPlanSummary(t *testing.T) {
	s := newTestStore(t)
	if sum, _ := s.PlanSummary("no-such"); sum != "" {
		t.Errorf("无计划时摘要应为空，得到 %q", sum)
	}
	_, err := s.AllocateStep("sess-1", "task-1", PlanStep{ToolID: "cap-ops-python", StepType: StepExecute, ExpectedArtifacts: []string{"/tmp/a.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	if sum, _ := s.PlanSummary("sess-1"); sum != "" {
		t.Errorf("步未完成时摘要应为空，得到 %q", sum)
	}
	if err := s.CompleteStep("sess-1", 1, StepDone, "写好了", []string{"/tmp/a.txt"}, ""); err != nil {
		t.Fatal(err)
	}
	sum, err := s.PlanSummary("sess-1")
	if err != nil || !strings.Contains(sum, "/tmp/a.txt") {
		t.Errorf("摘要应含产物路径，得到 %q err=%v", sum, err)
	}
}

func TestHistoryAppendLoadRoundTrip(t *testing.T) {
	s := newTestStore(t)
	msgs := []llm.Message{
		{Role: "user", Content: "写 hello 到 /tmp/out.txt"},
		{Role: "assistant", Content: "", ToolCalls: []llm.ToolCall{{ID: "call-1", Type: "function", Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: "cap-ops-python", Arguments: `{"inputs":["--out","/tmp/out.txt"]}`}}}},
		{Role: "tool", ToolCallID: "call-1", Content: "ok"},
	}
	if err := s.AppendMessages("sess-1", 1, "task-1", msgs); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	got, rep, err := s.LoadMessages("sess-1", 1, "task-1")
	if err != nil || rep != nil {
		t.Fatalf("LoadMessages: err=%v rep=%v", err, rep)
	}
	if len(got) != 3 {
		t.Fatalf("应读回 3 条，得到 %d", len(got))
	}
	if got[1].ToolCalls[0].Function.Name != "cap-ops-python" {
		t.Errorf("tool_calls 未原样读回：%+v", got[1])
	}
	// 不存在的文件返回 nil。
	got2, _, err := s.LoadMessages("sess-1", 99, "task-1")
	if err != nil || got2 != nil {
		t.Errorf("不存在的步历史应返回 nil，得到 %v, %v", got2, err)
	}
	// front.jsonl（stepNo<=0）同样可用。
	if err := s.AppendMessages("sess-1", 0, "task-1", []llm.Message{{Role: "user", Content: "front"}}); err != nil {
		t.Fatalf("AppendMessages front: %v", err)
	}
	got3, _, err := s.LoadMessages("sess-1", 0, "task-1")
	if err != nil || len(got3) != 1 || got3[0].Content != "front" {
		t.Errorf("front.jsonl 往返失败：%v %v", got3, err)
	}
}

func TestHistoryTailRepair(t *testing.T) {
	s := newTestStore(t)
	msgs := []llm.Message{
		{Role: "user", Content: "第一条"},
		{Role: "assistant", Content: "第二条"},
		{Role: "tool", ToolCallID: "c1", Content: "第三条"},
	}
	if err := s.AppendMessages("sess-1", 1, "task-1", msgs); err != nil {
		t.Fatal(err)
	}
	// 手工在末尾加半行（模拟 kill -9 写半截）。
	p, _ := s.StepPath("sess-1", 1)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"role":"assistant","content":"半截`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	got, rep, err := s.LoadMessages("sess-1", 1, "task-1")
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	if rep == nil || rep.TruncatedLines != 1 {
		t.Fatalf("应报告截断 1 行，得到 %+v", rep)
	}
	if len(got) != 3 {
		t.Fatalf("前面 3 条应全保留，得到 %d 条", len(got))
	}
	// 文件本身也被截干净了：再读一次不再报修复。
	got2, rep2, err := s.LoadMessages("sess-1", 1, "task-1")
	if err != nil || rep2 != nil || len(got2) != 3 {
		t.Fatalf("二次读回应干净：len=%d rep=%v err=%v", len(got2), rep2, err)
	}
	// 中间坏行：跳过并计数，后面的好行保留。
	raw, _ := os.ReadFile(p)
	lines := strings.Split(string(raw), "\n")
	// 在第 1 行后插入一行坏 JSON。
	bad := append([]string{lines[0], "{坏行"}, lines[1:]...)
	if err := os.WriteFile(p, []byte(strings.Join(bad, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	got3, rep3, err := s.LoadMessages("sess-1", 1, "task-1")
	if err != nil {
		t.Fatalf("LoadMessages 中间坏行: %v", err)
	}
	if rep3 == nil || rep3.TruncatedLines != 1 {
		t.Fatalf("中间坏行应报告截断 1 行，得到 %+v", rep3)
	}
	if len(got3) != 3 {
		t.Fatalf("中间坏行跳过后应剩 3 条，得到 %d", len(got3))
	}
}

func TestHistoryToolCallsClosure(t *testing.T) {
	s := newTestStore(t)
	// 末尾 assistant(tool_calls) 没有 tool 回应（崩溃卡在中间）。
	msgs := []llm.Message{
		{Role: "user", Content: "任务"},
		{Role: "assistant", Content: "", ToolCalls: []llm.ToolCall{
			{ID: "call-1", Type: "function", Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{Name: "cap-ops-python", Arguments: `{"inputs":[]}`}},
			{ID: "call-2", Type: "function", Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{Name: "cap-read", Arguments: `{"inputs":[]}`}},
		}},
		{Role: "tool", ToolCallID: "call-1", Content: "ok"}, // call-2 缺回应
	}
	if err := s.AppendMessages("sess-1", 1, "task-1", msgs); err != nil {
		t.Fatal(err)
	}
	if missing := UnclosedToolCallIDs(msgs); len(missing) != 1 || missing[0] != "call-2" {
		t.Fatalf("UnclosedToolCallIDs 应报 call-2，得到 %v", missing)
	}
	got, rep, err := s.LoadMessages("sess-1", 1, "task-1")
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	if rep == nil || rep.ClosedToolCalls != 1 {
		t.Fatalf("应补闭合 1 个调用，得到 %+v", rep)
	}
	if len(got) != 4 || got[3].Role != "tool" || got[3].ToolCallID != "call-2" || got[3].Content != InterruptedToolNote {
		t.Fatalf("补的 tool 消息不对：%+v", got)
	}
	if missing := UnclosedToolCallIDs(got); len(missing) != 0 {
		t.Fatalf("修复后应无未闭合调用，得到 %v", missing)
	}
	// 补的消息也写进了文件：再读一次不再修复。
	got2, rep2, err := s.LoadMessages("sess-1", 1, "task-1")
	if err != nil || rep2 != nil || len(got2) != 4 {
		t.Fatalf("二次读回应干净：len=%d rep=%v err=%v", len(got2), rep2, err)
	}
	// 闭合完整的历史不修复。
	s2 := newTestStore(t)
	full := []llm.Message{
		{Role: "user", Content: "u"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "c1", Type: "function"}}},
		{Role: "tool", ToolCallID: "c1", Content: "ok"},
	}
	if err := s2.AppendMessages("s", 1, "t", full); err != nil {
		t.Fatal(err)
	}
	if _, rep, err := s2.LoadMessages("s", 1, "t"); err != nil || rep != nil {
		t.Fatalf("闭合完整的历史不应修复：rep=%v err=%v", rep, err)
	}
}

func TestHistoryMidUnclosedDrops(t *testing.T) {
	s := newTestStore(t)
	// 中途 assistant 未闭合、后面还有别的 assistant：截到该条之前。
	msgs := []llm.Message{
		{Role: "user", Content: "u1"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "c1", Type: "function"}}},
		{Role: "tool", ToolCallID: "c1", Content: "ok"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "c2", Type: "function"}}}, // 未闭合
		{Role: "assistant", Content: "后面的"},
	}
	// 直接写文件（绕过 AppendMessages 的正常成对写入）。
	p, _ := s.StepPath("sess-1", 1)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for _, m := range msgs {
		b, _ := json.Marshal(m)
		sb.Write(b)
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(p, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	got, rep, err := s.LoadMessages("sess-1", 1, "task-1")
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	if rep == nil || rep.DroppedMessages != 2 {
		t.Fatalf("中途未闭合应丢弃 2 条，得到 %+v", rep)
	}
	if len(got) != 3 {
		t.Fatalf("应截到未闭合 assistant 之前剩 3 条，得到 %d", len(got))
	}
	if missing := UnclosedToolCallIDs(got); len(missing) != 0 {
		t.Fatalf("截后应无未闭合调用，得到 %v", missing)
	}
}

func TestBreakpointResume(t *testing.T) {
	s := newTestStore(t)
	// 模拟一次“kill 后恢复”：计划 + 步历史已落盘，重新打开应原样读回。
	n, err := s.AllocateStep("sess-1", "task-1", PlanStep{ToolID: "cap-ops-python", StepType: StepExecute, ExpectedArtifacts: []string{"/tmp/out.txt"}})
	if err != nil || n != 1 {
		t.Fatalf("AllocateStep: n=%d err=%v", n, err)
	}
	msgs := []llm.Message{
		{Role: "user", Content: "写 hello"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "c1", Type: "function"}}},
		{Role: "tool", ToolCallID: "c1", Content: "ok"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "c2", Type: "function"}}},
		{Role: "tool", ToolCallID: "c2", Content: "ok2"},
	}
	if err := s.AppendMessages("sess-1", 1, "task-1", msgs); err != nil {
		t.Fatal(err)
	}
	// “重启”：同一数据目录重新打开（模拟进程重启后同一 Store 语义）。
	ps, isNew, err := s.OpenSession("sess-1", "task-1")
	if err != nil || isNew {
		t.Fatalf("OpenSession 恢复：isNew=%v err=%v", isNew, err)
	}
	if ps.CurrentStep != 1 {
		t.Fatalf("恢复后 CurrentStep 应为 1，得到 %d", ps.CurrentStep)
	}
	got, rep, err := s.LoadMessages("sess-1", 1, "task-1")
	if err != nil || rep != nil || len(got) != 5 {
		t.Fatalf("恢复后应读回 5 条历史无修复：len=%d rep=%v err=%v", len(got), rep, err)
	}
	// 再追加一轮，证明是接着写不是覆盖。
	if err := s.AppendMessages("sess-1", 1, "task-1", []llm.Message{{Role: "assistant", Content: "继续"}}); err != nil {
		t.Fatal(err)
	}
	got2, _, err := s.LoadMessages("sess-1", 1, "task-1")
	if err != nil || len(got2) != 6 {
		t.Fatalf("追加后应为 6 条，得到 %d err=%v", len(got2), err)
	}
}

func TestConcurrentSessionWrites(t *testing.T) {
	s := newTestStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			taskID := fmt.Sprintf("task-%d", i)
			if _, err := s.AllocateStep("sess-1", taskID, PlanStep{ToolID: "cap-ops-python", StepType: StepExecute}); err != nil {
				t.Errorf("AllocateStep %d: %v", i, err)
				return
			}
			if err := s.AppendMessages("sess-1", 1, taskID, []llm.Message{{Role: "user", Content: taskID}}); err != nil {
				t.Errorf("AppendMessages %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	ps, err := s.LoadPlan("sess-1")
	if err != nil || ps == nil {
		t.Fatalf("LoadPlan: %v", err)
	}
	if len(ps.Steps) != 8 {
		t.Fatalf("8 个并发分配应产生 8 步，得到 %d 步", len(ps.Steps))
	}
	seen := map[int]bool{}
	for _, st := range ps.Steps {
		if seen[st.StepNo] {
			t.Fatalf("步号 %d 重复", st.StepNo)
		}
		seen[st.StepNo] = true
	}
	got, _, err := s.LoadMessages("sess-1", 1, "task-0")
	if err != nil || len(got) != 8 {
		t.Fatalf("并发追加后应读回 8 条，得到 %d err=%v", len(got), err)
	}
	// plan_state.json 本身仍是合法 JSON（原子改名没写坏）。
	p, _ := s.PlanPath("sess-1")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var probe PlanState
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatalf("并发写后 plan_state 应仍是合法 JSON: %v", err)
	}
}

// TestReopenStepAndCancelPlan（规划 v2）：重开一步与叫停计划。
func TestReopenStepAndCancelPlan(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.AllocateStep("sess-r", "task-r", PlanStep{ToolID: "cap-ops-python", StepType: StepExecute}); err != nil {
		t.Fatalf("AllocateStep: %v", err)
	}
	if err := s.CompleteStep("sess-r", 1, StepDone, "写好了", []string{"/tmp/out.txt"}, ""); err != nil {
		t.Fatalf("CompleteStep: %v", err)
	}
	ps, _ := s.LoadPlan("sess-r")
	if ps.CurrentStep != 2 {
		t.Fatalf("完成后 CurrentStep 应为 2，得到 %d", ps.CurrentStep)
	}
	// 重开步 1：状态回 RUNNING、Attempts+1、CurrentStep 回 1。
	if err := s.ReopenStep("sess-r", 1, "测试重干原因"); err != nil {
		t.Fatalf("ReopenStep: %v", err)
	}
	ps, _ = s.LoadPlan("sess-r")
	if ps.Steps[0].Status != StepRunning {
		t.Fatalf("重开后步状态应为 RUNNING，得到 %s", ps.Steps[0].Status)
	}
	if ps.Steps[0].Attempts != 2 {
		t.Fatalf("重开后 Attempts 应为 2，得到 %d", ps.Steps[0].Attempts)
	}
	// M-2：重干原因写进该步的 LastRedoReason（下次派单带给执行层）。
	if ps.Steps[0].LastRedoReason != "测试重干原因" {
		t.Fatalf("重开后 LastRedoReason 应为“测试重干原因”，得到 %q", ps.Steps[0].LastRedoReason)
	}
	if ps.CurrentStep != 1 {
		t.Fatalf("重开后 CurrentStep 应为 1，得到 %d", ps.CurrentStep)
	}
	// 不存在的步重开应报错。
	if err := s.ReopenStep("sess-r", 99, ""); err == nil {
		t.Fatalf("重开不存在的步应报错")
	}
	// 叫停：Status 变 CANCELLED（终态），再叫停应报错。
	if err := s.CancelPlan("sess-r", "用户叫停"); err != nil {
		t.Fatalf("CancelPlan: %v", err)
	}
	ps, _ = s.LoadPlan("sess-r")
	if ps.Status != PlanCancelled || !ps.Status.IsTerminal() {
		t.Fatalf("叫停后 Status 应为 CANCELLED 终态，得到 %s", ps.Status)
	}
	if len(ps.Failures) != 1 || ps.Failures[0] != "用户叫停" {
		t.Fatalf("叫停原因应记进 Failures，得到 %v", ps.Failures)
	}
	if err := s.CancelPlan("sess-r", "再停"); err == nil {
		t.Fatalf("终态计划再叫停应报错")
	}
}

// TestSetPhase（规划 v2）：计划阶段流转 RUNNING → WAITING_STEP → RESUMABLE，
// 终态计划改阶段应报错。
func TestSetPhase(t *testing.T) {
	s := newTestStore(t)
	ps, isNew, err := s.OpenSession("sess-p", "task-p")
	if err != nil || !isNew {
		t.Fatalf("OpenSession: isNew=%v err=%v", isNew, err)
	}
	if err := s.SetPhase("sess-p", PhaseWaitingStep); err != nil {
		t.Fatalf("SetPhase WAITING_STEP: %v", err)
	}
	ps, _ = s.LoadPlan("sess-p")
	if ps.Phase != PhaseWaitingStep {
		t.Fatalf("Phase 应为 WAITING_STEP，得到 %s", ps.Phase)
	}
	if err := s.SetPhase("sess-p", PhaseResumable); err != nil {
		t.Fatalf("SetPhase RESUMABLE: %v", err)
	}
	ps, _ = s.LoadPlan("sess-p")
	if ps.Phase != PhaseResumable {
		t.Fatalf("Phase 应为 RESUMABLE，得到 %s", ps.Phase)
	}
	// 终态后改阶段应报错。
	if err := s.CancelPlan("sess-p", "停"); err != nil {
		t.Fatalf("CancelPlan: %v", err)
	}
	if err := s.SetPhase("sess-p", PhaseRunning); err == nil {
		t.Fatalf("终态计划改阶段应报错")
	}
	_ = ps
}

// TestRecordStepStartOnlyFirst（方向 b）：首次派发前记账本起点与开始时间；
// 已记过时再调不覆盖（重干不更新）。
func TestRecordStepStartOnlyFirst(t *testing.T) {
	s := newTestStore(t)
	ps := s.NewPlan("sess-rs", "task-rs")
	ps.Steps = append(ps.Steps, PlanStep{StepNo: 1, TaskID: "task-rs", ToolID: "cap-ops-python", StepType: StepExecute, Status: StepRunning})
	ps.TotalSteps = 1
	if err := s.SavePlan(ps); err != nil {
		t.Fatal(err)
	}
	start1 := time.Now().Add(-time.Hour)
	if err := s.RecordStepStart("sess-rs", 1, 100, start1); err != nil {
		t.Fatalf("RecordStepStart: %v", err)
	}
	got, _ := s.LoadPlan("sess-rs")
	if got.Steps[0].FirstExecIDBefore != 100 {
		t.Fatalf("FirstExecIDBefore 应为 100，得到 %d", got.Steps[0].FirstExecIDBefore)
	}
	if !got.Steps[0].FirstStartTime.Equal(start1) {
		t.Fatalf("FirstStartTime 应为 %v，得到 %v", start1, got.Steps[0].FirstStartTime)
	}
	// 再记一次（更大的值）不应覆盖。
	if err := s.RecordStepStart("sess-rs", 1, 200, time.Now()); err != nil {
		t.Fatalf("RecordStepStart 第二次: %v", err)
	}
	got, _ = s.LoadPlan("sess-rs")
	if got.Steps[0].FirstExecIDBefore != 100 {
		t.Fatalf("已记过时再记不应覆盖，FirstExecIDBefore 应仍为 100，得到 %d", got.Steps[0].FirstExecIDBefore)
	}
	if !got.Steps[0].FirstStartTime.Equal(start1) {
		t.Fatalf("已记过时再记不应覆盖 FirstStartTime，得到 %v", got.Steps[0].FirstStartTime)
	}
	// 不存在的步应报错。
	if err := s.RecordStepStart("sess-rs", 99, 100, time.Now()); err == nil {
		t.Fatal("不存在的步记起点应报错")
	}
}

// TestReopenStepSecondAttemptArchivesHistory（低危防锚定兜底）：
// 第 2 次重开（Attempts==2）时把旧的 step-<n>.jsonl 归档改名，
// 让这次重干在干净上下文中重新开始；第 1 次重开不归档。
func TestReopenStepSecondAttemptArchivesHistory(t *testing.T) {
	s := newTestStore(t)
	ps := s.NewPlan("sess-arch", "task-arch")
	ps.Steps = append(ps.Steps, PlanStep{StepNo: 1, TaskID: "task-arch", ToolID: "cap-ops-python", StepType: StepExecute, Status: StepRunning})
	ps.TotalSteps = 1
	if err := s.SavePlan(ps); err != nil {
		t.Fatal(err)
	}
	// 预置本步历史（2 条消息）。
	seed := []llm.Message{
		{Role: "user", Content: "写问候文件"},
		{Role: "assistant", Content: "做完了"},
	}
	if err := s.AppendMessages("sess-arch", 1, "task-arch", seed); err != nil {
		t.Fatal(err)
	}
	stepPath, err := s.StepPath("sess-arch", 1)
	if err != nil {
		t.Fatal(err)
	}
	archived := strings.TrimSuffix(stepPath, ".jsonl") + ".attempt-1.jsonl"
	// 第 1 次重开（Attempts 0→1）：不归档。
	if err := s.ReopenStep("sess-arch", 1, "第一次打回"); err != nil {
		t.Fatalf("ReopenStep #1: %v", err)
	}
	if _, err := os.Stat(stepPath); err != nil {
		t.Fatalf("第 1 次重开不应归档，历史文件应还在：%v", err)
	}
	if _, err := os.Stat(archived); err == nil {
		t.Fatal("第 1 次重开不应产生归档文件")
	}
	// 第 2 次重开（Attempts 1→2）：归档。
	if err := s.ReopenStep("sess-arch", 1, "第二次打回"); err != nil {
		t.Fatalf("ReopenStep #2: %v", err)
	}
	if _, err := os.Stat(stepPath); err == nil {
		t.Fatal("第 2 次重开应把旧历史归档，原文件应不在")
	}
	if _, err := os.Stat(archived); err != nil {
		t.Fatalf("第 2 次重开应产生归档文件 %s：%v", archived, err)
	}
	// 归档后本步历史应为空（干净上下文）。
	restored, _, err := s.LoadMessages("sess-arch", 1, "task-arch")
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 0 {
		t.Fatalf("归档后本步历史应为空（干净上下文），实际 %d 条", len(restored))
	}
}

// TestHistoryToolOutputSlimmed（CF 诊断 M-1）：落盘 step-<n>.jsonl 时，
// 超 3KB 的 role:"tool" 历史输出只保留前 1500 字 + 末尾 500 字 +
// 原输出落盘路径提示（当前轮执行内仍用完整输出，仅历史归档瘦身）。
func TestHistoryToolOutputSlimmed(t *testing.T) {
	s := newTestStore(t)
	// 造一个 5000 字的 tool 输出：开头与末尾各放一个唯一标记。
	head := "HEAD-MARKER-" + strings.Repeat("a", 100)
	tail := "TAIL-MARKER-" + strings.Repeat("z", 100)
	big := head + strings.Repeat("x", 5000-len(head)-len(tail)) + tail
	if len([]rune(big)) <= 3072 {
		t.Fatalf("测试用的 tool 输出应超 3KB，实际 %d 字", len([]rune(big)))
	}
	msgs := []llm.Message{{Role: "tool", ToolCallID: "c1", Content: big}}
	if err := s.AppendMessages("sess-1", 1, "task-1", msgs); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	got, _, err := s.LoadMessages("sess-1", 1, "task-1")
	if err != nil || len(got) != 1 {
		t.Fatalf("LoadMessages: err=%v n=%d", err, len(got))
	}
	c := got[0].Content
	if len([]rune(c)) > 2600 {
		t.Fatalf("瘦身后 tool 输出应 ≤2600 字左右，实际 %d 字", len([]rune(c)))
	}
	if !strings.Contains(c, "HEAD-MARKER-") {
		t.Fatalf("瘦身后应保留开头标记，实际内容：%.100s", c)
	}
	if !strings.Contains(c, "TAIL-MARKER-") {
		t.Fatalf("瘦身后应保留末尾标记，实际内容：%.100s", c)
	}
	if !strings.Contains(c, "历史归档截断") {
		t.Fatalf("瘦身后应有截断标记，实际内容：%.200s", c)
	}
	if !strings.Contains(c, "step-1.txt") {
		t.Fatalf("截断标记应含原输出落盘路径提示（step-1.txt），实际内容：%.200s", c)
	}
	// 小输出不瘦身：原样读回。
	small := "ok"
	if err := s.AppendMessages("sess-1", 2, "task-1", []llm.Message{{Role: "tool", ToolCallID: "c2", Content: small}}); err != nil {
		t.Fatalf("AppendMessages small: %v", err)
	}
	got2, _, err := s.LoadMessages("sess-1", 2, "task-1")
	if err != nil || len(got2) != 1 || got2[0].Content != small {
		t.Fatalf("小输出应原样读回，得到 %v, %v", got2, err)
	}
}
