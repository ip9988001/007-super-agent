package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"007-super-agent/internal/llm"
)

// fakeSummarizer 返回固定摘要的假模型调用（测试用）。
func fakeSummarizer(summary string) Summarizer {
	return func(ctx context.Context, taskID, prompt string) (string, error) {
		return summary, nil
	}
}

// failingSummarizer 永远失败的假模型调用（测试降级用）。
func failingSummarizer(err error) Summarizer {
	return func(ctx context.Context, taskID, prompt string) (string, error) {
		return "", err
	}
}

// mustRolloverPlan 建一个带步表的计划并存盘（测试用）。
func mustRolloverPlan(t *testing.T, s *Store, sessID, taskID string, tokens int64) *PlanState {
	t.Helper()
	ps := s.NewPlan(sessID, taskID)
	ps.Steps = append(ps.Steps, PlanStep{StepNo: 1, TaskID: taskID, ToolID: "cap-ops-python", StepType: StepExecute, Status: StepDone, Summary: "第一步：写了 hello.txt", Artifacts: []string{"/tmp/sbx/hello.txt"}})
	ps.Steps = append(ps.Steps, PlanStep{StepNo: 2, TaskID: taskID, ToolID: "cap-ops-python", StepType: StepExecute, Status: StepPending, Summary: ""})
	ps.TotalSteps = 2
	ps.CurrentStep = 2
	ps.TokensUsed = tokens
	if err := s.SavePlan(ps); err != nil {
		t.Fatal(err)
	}
	return ps
}

func TestRecordTokensPeakNotSum(t *testing.T) {
	s := newTestStore(t)
	ps := s.NewPlan("sess-1", "task-1")
	if err := s.SavePlan(ps); err != nil {
		t.Fatal(err)
	}
	// 三次调用：100、300、200。峰值应是 300，不是累计 600。
	for _, n := range []int64{100, 300, 200} {
		if err := s.RecordTokens("sess-1", n); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.LoadPlan("sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.TokensUsed != 300 {
		t.Errorf("水位应是峰值 300（不是累计 600），得到 %d", got.TokensUsed)
	}
	// 无计划时不报错、不建计划。
	if err := s.RecordTokens("no-such", 100); err != nil {
		t.Errorf("无计划时 RecordTokens 应静默跳过，得到 %v", err)
	}
	// 0 与负数不记。
	if err := s.RecordTokens("sess-1", 0); err != nil {
		t.Fatal(err)
	}
	got, _ = s.LoadPlan("sess-1")
	if got.TokensUsed != 300 {
		t.Errorf("0 token 不应改水位，得到 %d", got.TokensUsed)
	}
}

func TestMaybeRolloverUnderThreshold(t *testing.T) {
	s := newTestStore(t)
	mustRolloverPlan(t, s, "sess-1", "task-1", 100)
	cfg := RolloverConfig{ThresholdTokens: 1000, SummaryTimeout: time.Second, ArchiveAfter: 7 * 24 * time.Hour}
	newID, rolled, err := s.MaybeRollover(context.Background(), "sess-1", "task-1", "做个测试", cfg, fakeSummarizer("摘要"))
	if err != nil {
		t.Fatal(err)
	}
	if rolled || newID != "" {
		t.Errorf("水位 100 未超阈值 1000，不应翻新，得到 rolled=%v newID=%q", rolled, newID)
	}
	// 计划原样未动。
	got, _ := s.LoadPlan("sess-1")
	if got.Status != PlanRunning || got.TokensUsed != 100 {
		t.Errorf("未翻新时计划不应变，得到 status=%s tokens=%d", got.Status, got.TokensUsed)
	}
}

func TestMaybeRolloverHappyPath(t *testing.T) {
	s := newTestStore(t)
	old := mustRolloverPlan(t, s, "sess-1", "task-1", 5000)
	summary := strings.Repeat("关键细节：路径 /tmp/sbx/hello.txt，端口 6263，阈值 200000。", 40) // 约 1200 字
	cfg := RolloverConfig{ThresholdTokens: 1000, SummaryTimeout: time.Second, ArchiveAfter: 7 * 24 * time.Hour}
	newID, rolled, err := s.MaybeRollover(context.Background(), "sess-1", "task-1", "做个测试", cfg, fakeSummarizer(summary))
	if err != nil {
		t.Fatal(err)
	}
	if !rolled || newID == "" {
		t.Fatalf("水位 5000 超阈值 1000，应翻新，得到 rolled=%v newID=%q", rolled, newID)
	}
	if err := ValidateSessionID(newID); err != nil {
		t.Errorf("新会话号不合规 %q: %v", newID, err)
	}
	if newID == "sess-1" {
		t.Errorf("新会话号不得与旧会话相同")
	}
	// 新计划：carryover 含前缀 + 摘要 + 旧路径，token 清零，步表继承。
	nps, err := s.LoadPlan(newID)
	if err != nil || nps == nil {
		t.Fatalf("新计划应可读：%v", err)
	}
	if !strings.HasPrefix(nps.Carryover, "【上一段会话的接续摘要】") {
		t.Errorf("carryover 应以前缀开头，得到 %.80q", nps.Carryover)
	}
	if !strings.Contains(nps.Carryover, "关键细节：路径 /tmp/sbx/hello.txt") {
		t.Errorf("carryover 应含摘要正文，得到 %.200q", nps.Carryover)
	}
	oldPath, _ := s.PlanPath("sess-1")
	if !strings.Contains(nps.Carryover, "【上一段会话完整原文】"+oldPath) {
		t.Errorf("carryover 末尾应附旧会话 plan_state.json 绝对路径 %s，得到 %.200q", oldPath, nps.Carryover)
	}
	if nps.TokensUsed != 0 {
		t.Errorf("新会话 token 应清零，得到 %d", nps.TokensUsed)
	}
	if len(nps.Steps) != 2 || nps.Steps[0].Status != StepDone || nps.Steps[1].Status != StepPending {
		t.Errorf("新计划应继承步表（2 步，一步 DONE 一步 PENDING），得到 %+v", nps.Steps)
	}
	if nps.CurrentStep != 2 || nps.TotalSteps != 2 {
		t.Errorf("新计划应继承进度，得到 current=%d total=%d", nps.CurrentStep, nps.TotalSteps)
	}
	if nps.RolloverCount != 1 {
		t.Errorf("翻新次数应为 1，得到 %d", nps.RolloverCount)
	}
	if len(nps.Prev) != 1 || nps.Prev[0].SessionID != "sess-1" || nps.Prev[0].PlanID != old.PlanID {
		t.Errorf("prev 应记旧会话，得到 %+v", nps.Prev)
	}
	if nps.Prev[0].PlanPath != oldPath {
		t.Errorf("prev 应记旧会话 plan_state.json 路径，得到 %q", nps.Prev[0].PlanPath)
	}
	// 旧计划：标 ROLLED_OVER，文件原地保留（读原始文件，LoadPlan 会
	// 沿 RolledTo 链追到新会话，这里要看的是旧计划本身的字面状态）。
	ops, err := s.loadPlanRaw("sess-1")
	if err != nil || ops == nil {
		t.Fatalf("旧计划应可读：%v", err)
	}
	if ops.Status != PlanRolledOver {
		t.Errorf("旧计划应标 ROLLED_OVER，得到 %s", ops.Status)
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Errorf("旧会话 plan_state.json 应原地保留：%v", err)
	}
	// 翻新过的旧会话再翻：终态，不翻。
	newID2, rolled2, err := s.MaybeRollover(context.Background(), "sess-1", "task-1", "做个测试", cfg, fakeSummarizer(summary))
	if err != nil || rolled2 || newID2 != "" {
		t.Errorf("已翻新的旧会话不应再翻，得到 rolled=%v newID=%q err=%v", rolled2, newID2, err)
	}
}

func TestMaybeRolloverSummaryFailureFallback(t *testing.T) {
	s := newTestStore(t)
	mustRolloverPlan(t, s, "sess-1", "task-1", 5000)
	// 写 8 轮历史到第 1 步：降级应抽最近 6 轮（第 3–8 轮）。
	var msgs []llm.Message
	for i := 1; i <= 8; i++ {
		msgs = append(msgs, llm.Message{Role: "user", Content: fmt.Sprintf("第 %d 轮用户话", i)})
		msgs = append(msgs, llm.Message{Role: "assistant", Content: fmt.Sprintf("第 %d 轮模型答", i)})
	}
	if err := s.AppendMessages("sess-1", 1, "task-1", msgs); err != nil {
		t.Fatal(err)
	}
	cfg := RolloverConfig{ThresholdTokens: 1000, SummaryTimeout: time.Second, ArchiveAfter: 7 * 24 * time.Hour}
	newID, rolled, err := s.MaybeRollover(context.Background(), "sess-1", "task-1", "做个测试", cfg, failingSummarizer(errors.New("模型挂了")))
	if err != nil || !rolled || newID == "" {
		t.Fatalf("摘要失败也应翻新（走降级），得到 rolled=%v newID=%q err=%v", rolled, newID, err)
	}
	nps, _ := s.LoadPlan(newID)
	if !strings.Contains(nps.Carryover, "最近 6 轮原文") {
		t.Errorf("降级 carryover 应标明是最近 6 轮原文，得到 %.300q", nps.Carryover)
	}
	// 最近 6 轮在里面（第 3–8 轮：按 assistant 计数倒推 6 轮，
	// 第 8 轮的 2 条 + 第 7 轮的 2 条 + 第 6 轮的 2 条都在内）。
	for _, want := range []string{"第 8 轮用户话", "第 8 轮模型答", "第 6 轮用户话"} {
		if !strings.Contains(nps.Carryover, want) {
			t.Errorf("降级 carryover 应含 %q，得到 %.400q", want, nps.Carryover)
		}
	}
	// 第 1 轮（最早的）不应在里面。
	if strings.Contains(nps.Carryover, "第 1 轮用户话") {
		t.Errorf("降级只抽最近 6 轮，第 1 轮不应在里面，得到 %.400q", nps.Carryover)
	}
}

func TestMaybeRolloverCarryoverHardCap(t *testing.T) {
	s := newTestStore(t)
	mustRolloverPlan(t, s, "sess-1", "task-1", 5000)
	// 7000 字摘要：应硬截到 6000 字。
	long := strings.Repeat("字", 7000)
	cfg := RolloverConfig{ThresholdTokens: 1000, SummaryTimeout: time.Second, ArchiveAfter: 7 * 24 * time.Hour}
	newID, rolled, err := s.MaybeRollover(context.Background(), "sess-1", "task-1", "做个测试", cfg, fakeSummarizer(long))
	if err != nil || !rolled {
		t.Fatalf("应翻新，得到 rolled=%v err=%v", rolled, err)
	}
	nps, _ := s.LoadPlan(newID)
	// 摘要正文夹在前缀与旧路径标记之间，数它。
	start := strings.Index(nps.Carryover, "\n")
	end := strings.Index(nps.Carryover, "\n【上一段会话完整原文】")
	if start < 0 || end < 0 || end <= start {
		t.Fatalf("carryover 格式不对，得到 %.200q", nps.Carryover)
	}
	body := nps.Carryover[start+1 : end]
	if n := utf8.RuneCountInString(body); n > CarryoverMaxRunes {
		t.Errorf("摘要正文应硬截到 6000 字，得到 %d 字", n)
	}
	if n := utf8.RuneCountInString(body); n < CarryoverMaxRunes-10 {
		t.Errorf("7000 字摘要截到 6000 字应接近上限，得到 %d 字", n)
	}
}

func TestMaybeRolloverNilSummarizerFallback(t *testing.T) {
	s := newTestStore(t)
	mustRolloverPlan(t, s, "sess-1", "task-1", 5000)
	cfg := RolloverConfig{ThresholdTokens: 1000, SummaryTimeout: time.Second, ArchiveAfter: 7 * 24 * time.Hour}
	newID, rolled, err := s.MaybeRollover(context.Background(), "sess-1", "task-1", "做个测试", cfg, nil)
	if err != nil || !rolled || newID == "" {
		t.Fatalf("无模型时也应翻新（走降级），得到 rolled=%v newID=%q err=%v", rolled, newID, err)
	}
	nps, _ := s.LoadPlan(newID)
	if !strings.Contains(nps.Carryover, "最近 6 轮原文") && !strings.Contains(nps.Carryover, "退回步摘要") {
		t.Errorf("无模型降级 carryover 应有退回标记，得到 %.200q", nps.Carryover)
	}
}

// writePlanRaw 直接写 plan_state.json（测试用）：SavePlan 会刷新
// UpdatedTS，归档测试要旧时间戳，只能直接写文件。
func writePlanRaw(t *testing.T, s *Store, ps *PlanState) {
	t.Helper()
	p, err := s.PlanPath(ps.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(ps, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveIdleSessions(t *testing.T) {
	s := newTestStore(t)
	oldTS := time.Now().Add(-8 * 24 * time.Hour).UTC().Format(time.RFC3339)
	// (a) 旧的已终结会话：应归档。
	pa := s.NewPlan("sess-old-done", "task-a")
	pa.Status = PlanDone
	pa.UpdatedTS = oldTS
	writePlanRaw(t, s, pa)
	// (b) 新的已终结会话：不归档。
	pb := s.NewPlan("sess-new-done", "task-b")
	pb.Status = PlanDone
	if err := s.SavePlan(pb); err != nil {
		t.Fatal(err)
	}
	// (c) 旧的未终结会话：绝不归档（还在跑）。
	pc := s.NewPlan("sess-old-running", "task-c")
	pc.Status = PlanRunning
	pc.UpdatedTS = oldTS
	writePlanRaw(t, s, pc)
	// (d) 旧的已翻新会话：应归档（ROLLED_OVER 是终态）。
	pd := s.NewPlan("sess-old-rolled", "task-d")
	pd.Status = PlanRolledOver
	pd.UpdatedTS = oldTS
	writePlanRaw(t, s, pd)

	archived, err := s.ArchiveIdleSessions(7 * 24 * time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(archived) != 2 {
		t.Fatalf("应归档 2 个（old-done、old-rolled），得到 %v", archived)
	}
	for _, want := range []string{"sess-old-done", "sess-old-rolled"} {
		found := false
		for _, got := range archived {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("应归档 %s，得到 %v", want, archived)
		}
	}
	// 归档目标：文件仍在、不删。
	// dataDir 是 Store 的临时目录：从 PlanPath 反推。
	p, _ := s.PlanPath("sess-new-done")
	dataDir := filepath.Dir(filepath.Dir(filepath.Dir(p)))
	for _, id := range []string{"sess-old-done", "sess-old-rolled"} {
		dst := filepath.Join(dataDir, "sessions-archive", id, "plan_state.json")
		if _, err := os.Stat(dst); err != nil {
			t.Errorf("归档后 %s 的 plan_state.json 应在，%v", id, err)
		}
		if _, err := os.Stat(filepath.Join(dataDir, "sessions", id)); !os.IsNotExist(err) {
			t.Errorf("归档后 sessions/%s 应已移走", id)
		}
	}
	// (b)(c) 还在原地。
	for _, id := range []string{"sess-new-done", "sess-old-running"} {
		if _, err := os.Stat(filepath.Join(dataDir, "sessions", id, "plan_state.json")); err != nil {
			t.Errorf("%s 应留在原地：%v", id, err)
		}
	}
}

func TestPlanSummaryIncludesCarryover(t *testing.T) {
	s := newTestStore(t)
	ps := s.NewPlan("sess-1", "task-1")
	ps.Carryover = "【上一段会话的接续摘要】\n上段做了 X，证据 /tmp/x。\n【上一段会话完整原文】/tmp/old/plan_state.json 需要时可直接读回"
	if err := s.SavePlan(ps); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AllocateStep("sess-1", "task-1", PlanStep{ToolID: "cap-ops-python", StepType: StepExecute}); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteStep("sess-1", 1, StepDone, "写好了", []string{"/tmp/a.txt"}, ""); err != nil {
		t.Fatal(err)
	}
	sum, err := s.PlanSummary("sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sum, "【上一段会话的接续摘要】") || !strings.Contains(sum, "上段做了 X") {
		t.Errorf("PlanSummary 应含 carryover，得到 %.300q", sum)
	}
	if !strings.Contains(sum, "/tmp/a.txt") {
		t.Errorf("PlanSummary 仍应含本会话已完成步产物，得到 %.300q", sum)
	}
	// carryover 在最前面（新会话开头先看到它）。
	if !strings.HasPrefix(sum, "【上一段会话的接续摘要】") {
		t.Errorf("carryover 应在摘要最前面，得到 %.120q", sum)
	}
}

func TestDefaultRolloverConfigEnvOverride(t *testing.T) {
	t.Setenv(RolloverThresholdEnv, "50")
	if got := DefaultRolloverConfig().ThresholdTokens; got != 50 {
		t.Errorf("环境变量 50 应覆盖阈值，得到 %d", got)
	}
	t.Setenv(RolloverThresholdEnv, "not-a-number")
	if got := DefaultRolloverConfig().ThresholdTokens; got != DefaultRolloverThreshold {
		t.Errorf("非法环境变量应用默认 200000，得到 %d", got)
	}
	t.Setenv(RolloverThresholdEnv, "0")
	if got := DefaultRolloverConfig().ThresholdTokens; got != DefaultRolloverThreshold {
		t.Errorf("0 应回落默认 200000，得到 %d", got)
	}
	// 未设：默认 200000。
	os.Unsetenv(RolloverThresholdEnv)
	if got := DefaultRolloverConfig().ThresholdTokens; got != DefaultRolloverThreshold {
		t.Errorf("默认阈值应为 200000，得到 %d", got)
	}
}

func TestSummaryPromptContainsNumberThreeWording(t *testing.T) {
	// 摘要提示词须含三号原话（800–5000 字、5 段、禁项）。
	p := buildSummaryPrompt("素材")
	for _, want := range []string{
		"不得少于 800 字，不得多于 5000 字",
		"信息不足时宁可只写 800 字也不许凑字数",
		"① 当前目标及为什么",
		"② 做到哪步 + 证据",
		"③ 关键决定与技术细节",
		"④ 被推翻的结论与踩过的坑",
		"⑤ 下一步待办",
		"禁止：寒暄、过程叙述、表格、复述已知",
		"素材",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("摘要提示词应含 %q", want)
		}
	}
}

// H-6 回归：翻新后按原 session_id 崩溃续跑——OpenSession/LoadPlan 沿
// RolledTo 链追到新会话，不丢进度；新计划保留原 CreatedTS。
func TestRolloverCrashResumeByOldSessionID(t *testing.T) {
	s := newTestStore(t)
	old := mustRolloverPlan(t, s, "sess-1", "task-1", 5000)
	oldCreated := old.CreatedTS
	summary := strings.Repeat("关键细节：路径 /tmp/sbx/hello.txt，端口 6263，阈值 200000。", 40)
	cfg := RolloverConfig{ThresholdTokens: 1000, SummaryTimeout: time.Second, ArchiveAfter: 7 * 24 * time.Hour}
	newID, rolled, err := s.MaybeRollover(context.Background(), "sess-1", "task-1", "做个测试", cfg, fakeSummarizer(summary))
	if err != nil {
		t.Fatal(err)
	}
	if !rolled || newID == "" {
		t.Fatalf("应翻新，得到 rolled=%v newID=%q", rolled, newID)
	}
	// 旧计划写了前进指针。
	oldAfter, err := s.loadPlanRaw("sess-1")
	if err != nil || oldAfter == nil {
		t.Fatalf("旧计划应可读：%v", err)
	}
	if oldAfter.Status != PlanRolledOver {
		t.Fatalf("旧计划状态应为 ROLLED_OVER，得到 %s", oldAfter.Status)
	}
	if oldAfter.RolledTo != newID {
		t.Fatalf("旧计划 RolledTo 应为新会话 %q，得到 %q", newID, oldAfter.RolledTo)
	}
	// 崩溃重启拿原号恢复：LoadPlan 沿链追到新会话（第 1 步 DONE、第 2 步待做）。
	resumed, err := s.LoadPlan("sess-1")
	if err != nil || resumed == nil {
		t.Fatalf("按原号 LoadPlan 应找回新计划：%v", err)
	}
	if resumed.SessionID != newID {
		t.Fatalf("追踪后会话号应为新会话 %q，得到 %q", newID, resumed.SessionID)
	}
	if resumed.CurrentStep != 2 {
		t.Fatalf("续跑当前步应为 2，得到 %d", resumed.CurrentStep)
	}
	if len(resumed.Steps) != 2 || resumed.Steps[0].Status != StepDone {
		t.Fatalf("第 1 步 DONE 的进度不应丢，得到 %+v", resumed.Steps)
	}
	// 新计划保留原 CreatedTS（产物新鲜度基线不重置）。
	if resumed.CreatedTS != oldCreated {
		t.Fatalf("新计划 CreatedTS 应保留原值 %q，得到 %q", oldCreated, resumed.CreatedTS)
	}
	// OpenSession 按原号也应找回新会话（不是新建、不是旧会话）。
	ps, isNew, err := s.OpenSession("sess-1", "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if isNew {
		t.Fatal("OpenSession 按原号应续跑新会话，不应新建计划")
	}
	if ps.SessionID != newID {
		t.Fatalf("OpenSession 追踪后会话号应为 %q，得到 %q", newID, ps.SessionID)
	}
	// 翻新链防环：手动把新计划的 RolledTo 指回旧号，再 LoadPlan 不应死循环。
	nps, err := s.loadPlanRaw(newID)
	if err != nil || nps == nil {
		t.Fatal(err)
	}
	nps.RolledTo = "sess-1"
	if err := s.SavePlan(nps); err != nil {
		t.Fatal(err)
	}
	looped, err := s.LoadPlan("sess-1")
	if err != nil {
		t.Fatalf("环链 LoadPlan 不应报错：%v", err)
	}
	if looped == nil {
		t.Fatal("环链 LoadPlan 应返回一个可读计划")
	}
}

// TestMaybeRolloverCopiesStepHistories（M-1）：翻新时把旧会话的步历史
// 文件（steps/step-*.jsonl）拷到新会话——翻新后执行层按新会话号读历史，
// 拷过来才能读回、resumed 仍为 true。
func TestMaybeRolloverCopiesStepHistories(t *testing.T) {
	s := newTestStore(t)
	mustRolloverPlan(t, s, "sess-1", "task-1", 5000)
	// 预置第 1 步历史（2 条消息）。
	seed := []llm.Message{
		{Role: "user", Content: "写问候文件"},
		{Role: "assistant", Content: "做完了"},
	}
	if err := s.AppendMessages("sess-1", 1, "task-1", seed); err != nil {
		t.Fatal(err)
	}
	cfg := RolloverConfig{ThresholdTokens: 1000, SummaryTimeout: time.Second, ArchiveAfter: 7 * 24 * time.Hour}
	newID, rolled, err := s.MaybeRollover(context.Background(), "sess-1", "task-1", "做个测试", cfg, fakeSummarizer("摘要正文"))
	if err != nil || !rolled || newID == "" {
		t.Fatalf("应翻新，得到 rolled=%v newID=%q err=%v", rolled, newID, err)
	}
	// 新会话里应能读回第 1 步的 2 条历史。
	restored, _, err := s.LoadMessages(newID, 1, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 2 {
		t.Fatalf("新会话应读回 2 条步历史，实际 %d 条", len(restored))
	}
	if restored[0].Role != "user" || !strings.Contains(restored[0].Content, "写问候文件") {
		t.Fatalf("新会话步历史第 1 条不对：%+v", restored[0])
	}
	// 旧会话的历史原地保留。
	old, _, err := s.LoadMessages("sess-1", 1, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(old) != 2 {
		t.Fatalf("旧会话步历史应原地保留 2 条，实际 %d 条", len(old))
	}
}
