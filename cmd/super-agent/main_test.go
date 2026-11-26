package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"007-super-agent/internal/agent"
	"007-super-agent/internal/dispatch"
	"007-super-agent/internal/executor"
	"007-super-agent/internal/gateway"
	"007-super-agent/internal/ledger"
	"007-super-agent/internal/memory"
	"007-super-agent/internal/registry"
	"007-super-agent/internal/session"
	"007-super-agent/internal/watchdog"
)

// testDaemon 搭一个影子模式的整机（不接模型、不监听端口），只测 handle。
func testDaemon(t *testing.T) (*daemon, *memory.Store, string) {
	t.Helper()
	dir := t.TempDir()
	reg, err := registry.Load("../../step1")
	if err != nil {
		t.Fatalf("registry load step1: %v", err)
	}
	led, err := ledger.Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatalf("ledger open: %v", err)
	}
	t.Cleanup(func() { ledger.Close(led) })
	mem, err := memory.Open(filepath.Join(dir, "memory"))
	if err != nil {
		t.Fatalf("memory open: %v", err)
	}
	ex := &executor.Executor{Ledger: led}
	wd := watchdog.New(led, filepath.Join(dir, "ledger.db"), nil)
	d := &daemon{led: led, reg: reg, mem: mem, ex: ex, wd: wd,
		session: "test-session", shadow: true}
	return d, mem, dir
}

// 逐条采集：分诊失败的信与 Tier-0 工具信，处理完都要落一行 L1 日志。
func TestHandleWritesL1(t *testing.T) {
	d, mem, _ := testDaemon(t)
	d.handle(gateway.Envelope{TaskID: "t1", Text: "你好，你是谁"})
	d.handle(gateway.Envelope{TaskID: "t2", Text: "/rea 逆向这个二进制"})
	today := time.Now().Format("2006-01-02")
	text, err := mem.ReadL1(today)
	if err != nil {
		t.Fatalf("ReadL1: %v", err)
	}
	for _, want := range []string{"task t1 route=ERROR tool= status=triage_error", "task t2 route=TOOL tool=cap-code-rea status=shadow"} {
		if !strings.Contains(text, want) {
			t.Errorf("L1 日志里没有 %q，实际内容：\n%s", want, text)
		}
	}
}

// L1 写失败只记日志，不阻塞回执：删掉记忆目录后再处理也不 panic。
func TestHandleL1FailureDoesNotBlock(t *testing.T) {
	d, mem, dir := testDaemon(t)
	if err := os.RemoveAll(filepath.Join(dir, "memory")); err != nil {
		t.Fatal(err)
	}
	_ = mem
	d.handle(gateway.Envelope{TaskID: "t3", Text: "你好"})
}

// TestHandleWritesFailureReply 是线 A 的兜底回信（场景 6 第四档）：
// 前置失败（分诊报错）时 handle 返回 FAILED 终态，且产物目录里补写一封
// 失败回信；已有真实回信时不覆盖。
func TestHandleWritesFailureReply(t *testing.T) {
	d, _, dir := testDaemon(t)
	d.dataDir = dir // 测试整机默认没设数据目录，不设则不写回信
	outcome := d.handle(gateway.Envelope{TaskID: "t-fail", Text: "你好，你是谁"})
	if outcome.State != gateway.OutcomeFailed {
		t.Fatalf("分诊失败应返回 FAILED 终态，得到 %+v", outcome)
	}
	b, err := os.ReadFile(filepath.Join(dir, "artifacts", "t-fail", "reply.md"))
	if err != nil {
		t.Fatalf("兜底失败回信未写：%v", err)
	}
	if !strings.Contains(string(b), "任务未能完成") {
		t.Fatalf("失败回信应含“任务未能完成”，得到 %q", string(b))
	}
	// 已有真实回信时不覆盖。
	real := []byte("真实回信：做完了")
	if err := os.WriteFile(filepath.Join(dir, "artifacts", "t-fail", "reply.md"), real, 0o600); err != nil {
		t.Fatal(err)
	}
	d.handle(gateway.Envelope{TaskID: "t-fail", Text: "你好，你是谁"})
	b, err = os.ReadFile(filepath.Join(dir, "artifacts", "t-fail", "reply.md"))
	if err != nil || string(b) != "真实回信：做完了" {
		t.Fatalf("已有真实回信不应被覆盖，得到 %q, err=%v", string(b), err)
	}
}

// TestArtifactExistsTrimsSpace 是 P0 修复的 DONE 验收：
// 只含空白字符的 reply.md 不算产物；兜底失败回信应替换掉它。
func TestArtifactExistsTrimsSpace(t *testing.T) {
	d, _, dir := testDaemon(t)
	d.dataDir = dir
	artDir := filepath.Join(dir, "artifacts", "t-blank")
	if err := os.MkdirAll(artDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artDir, "reply.md"), []byte("\n   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if d.artifactExists("t-blank") {
		t.Fatal("只含空白字符的 reply.md 不应算产物")
	}
	d.ensureFailureReply("t-blank", "测试失败原因")
	b, err := os.ReadFile(filepath.Join(artDir, "reply.md"))
	if err != nil {
		t.Fatalf("兜底回信未写：%v", err)
	}
	if !strings.Contains(string(b), "任务未能完成") {
		t.Fatalf("空白产物应被失败回信替换，得到 %q", string(b))
	}
	// 真实回信不再被覆盖。
	if err := os.WriteFile(filepath.Join(artDir, "reply.md"), []byte("真实回信"), 0o600); err != nil {
		t.Fatal(err)
	}
	d.ensureFailureReply("t-blank", "测试失败原因")
	b, err = os.ReadFile(filepath.Join(artDir, "reply.md"))
	if err != nil || string(b) != "真实回信" {
		t.Fatalf("已有真实回信不应被覆盖，得到 %q, err=%v", string(b), err)
	}
}

// fastPathOK 回归测试：TOOL 空参裸调的根因修法（方案 D）。
// 带参工具与未声明 fast_path 的工具必须走 runAgent；只有显式 fast_path 的
// 单工具才走 runTool；多工具强制走 runAgent。
func TestFastPathOK(t *testing.T) {
	leanCtx := &registry.Entry{ToolID: "cap-ops-lean-ctx", FastPath: false}
	fastTool := &registry.Entry{ToolID: "cap-ops-fast", FastPath: true}
	cases := []struct {
		name string
		disp *dispatch.Dispatch
		e    *registry.Entry
		want bool
	}{
		{"带参工具不走快捷路径", &dispatch.Dispatch{Route: "TOOL", ToolIDs: []string{"cap-ops-lean-ctx"}}, leanCtx, false},
		{"未声明 fast_path 安全默认走 runAgent", &dispatch.Dispatch{Route: "TOOL", ToolIDs: []string{"cap-ops-new"}}, &registry.Entry{ToolID: "cap-ops-new"}, false},
		{"零参工具单工具走快捷路径", &dispatch.Dispatch{Route: "TOOL", ToolIDs: []string{"cap-ops-fast"}}, fastTool, true},
		{"零参工具多工具强制走 runAgent", &dispatch.Dispatch{Route: "TOOL", ToolIDs: []string{"cap-ops-fast", "cap-ops-lean-ctx"}}, fastTool, false},
		{"AGENT 路由不走快捷路径", &dispatch.Dispatch{Route: "AGENT", ToolIDs: []string{"cap-ops-fast"}}, fastTool, false},
	}
	for _, c := range cases {
		if got := fastPathOK(c.disp, c.e); got != c.want {
			t.Errorf("%s: fastPathOK=%v, want %v", c.name, got, c.want)
		}
	}
}

// P0-2 网关验货（2026-11-26 一票否决已去掉）：执行任务声明了产物但文件
// 不存在 → 网关照常放行（done），不再因硬底线标 failed、覆写 reply.md；
// 事实清单里照实记硬底线红（产物缺失），由前置看事实自己判失败。
func TestVerifyDeliveryExecuteArtifactMissing(t *testing.T) {
	d, _, dir := testDaemon(t)
	d.dataDir = dir
	taskID := "t-v-missing"
	artDir := filepath.Join(dir, "artifacts", taskID)
	if err := os.MkdirAll(artDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artDir, "reply.md"), []byte("草稿：我做完了"), 0o600); err != nil {
		t.Fatal(err)
	}
	disp := &dispatch.Dispatch{ID: "d1", TaskID: taskID, TaskType: "execute",
		ExpectedArtifacts: []string{filepath.Join(dir, "no-such-file.txt")}}
	res := &agent.RunResult{ReplyDraft: "草稿：我做完了", Status: "stopped"}
	if got := d.verifyDelivery(disp, taskID, res, time.Now(), 0); got != "done" {
		t.Fatalf("声明的产物不存在，网关不再否决、应 done（由前置判），得到 %s", got)
	}
	if disp.FactList == nil || !disp.FactList.HardFloorRed {
		t.Fatalf("产物缺失时事实清单应照实记硬底线红，得到 %+v", disp.FactList)
	}
	b, err := os.ReadFile(filepath.Join(artDir, "reply.md"))
	if err != nil || strings.Contains(string(b), "网关验货未通过") {
		t.Fatalf("网关放行时不应覆写 reply.md，得到 %q, err=%v", string(b), err)
	}
}

// P0-2 网关验货：执行任务产物真实存在且新鲜 + 账本有执行方调用 → done。
func TestVerifyDeliveryExecuteArtifactOK(t *testing.T) {
	d, _, dir := testDaemon(t)
	d.dataDir = dir
	taskID := "t-v-ok"
	start := time.Now()
	artPath := filepath.Join(dir, "out.txt")
	if err := os.WriteFile(artPath, []byte("real output"), 0o600); err != nil {
		t.Fatal(err)
	}
	artDir := filepath.Join(dir, "artifacts", taskID)
	if err := os.MkdirAll(artDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artDir, "reply.md"), []byte("草稿：做完了"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 底线（Gemini 审阅高危）：产物再真，执行方一次工具没调也不能算完成。
	if _, err := d.led.RecordExecution(ledger.ExecutionRecord{
		SessionID: "test-session", TaskID: taskID, ToolID: "test-echo",
		Command: "echo hi", ExitCode: 0, Status: "success"}); err != nil {
		t.Fatal(err)
	}
	disp := &dispatch.Dispatch{ID: "d2", TaskID: taskID, TaskType: "execute",
		ExpectedArtifacts: []string{artPath}}
	res := &agent.RunResult{ReplyDraft: "草稿：做完了", Status: "stopped"}
	if got := d.verifyDelivery(disp, taskID, res, start, 0); got != "done" {
		t.Fatalf("产物真实存在应 done，得到 %s", got)
	}
}

// Gemini 审阅高危（2026-11-26 一票否决已去掉）：声明了产物但执行方一次
// 工具都没调 → 网关照常放行（done），不再因硬底线标 failed；事实清单里
// 照实记 0 次调用、硬底线红，由前置看事实自己判失败。不能靠现成产物
// 直接拿 DONE 的责任现在在前置判卷，不在网关否决。
func TestVerifyDeliveryExecuteZeroCallsWithArtifacts(t *testing.T) {
	d, _, dir := testDaemon(t)
	d.dataDir = dir
	taskID := "t-v-zero"
	start := time.Now()
	artPath := filepath.Join(dir, "out-zero.txt")
	if err := os.WriteFile(artPath, []byte("real output"), 0o600); err != nil {
		t.Fatal(err)
	}
	artDir := filepath.Join(dir, "artifacts", taskID)
	if err := os.MkdirAll(artDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artDir, "reply.md"), []byte("草稿：做完了"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 账本里 0 行：模型一次工具没调。
	disp := &dispatch.Dispatch{ID: "d2z", TaskID: taskID, TaskType: "execute",
		ExpectedArtifacts: []string{artPath}}
	res := &agent.RunResult{ReplyDraft: "草稿：做完了", Status: "stopped"}
	if got := d.verifyDelivery(disp, taskID, res, start, 0); got != "done" {
		t.Fatalf("0 次调用 + 产物存在，网关不再否决、应 done（由前置判），得到 %s", got)
	}
	if disp.FactList == nil || !disp.FactList.HardFloorRed {
		t.Fatalf("0 次调用时事实清单应照实记硬底线红，得到 %+v", disp.FactList)
	}
	if disp.FactList.SuccessfulCalls != 0 {
		t.Fatalf("0 次调用时成功调用应为 0，得到 %d", disp.FactList.SuccessfulCalls)
	}
}

// P0-2 网关验货：未声明产物时走账本兜底——有非只读成功调用 → done。
func TestVerifyDeliveryExecuteLedgerFallback(t *testing.T) {
	d, _, dir := testDaemon(t)
	d.dataDir = dir
	taskID := "t-v-led"
	if _, err := d.led.RecordExecution(ledger.ExecutionRecord{
		SessionID: "test-session", TaskID: taskID, ToolID: "test-echo",
		Command: "echo hi", ExitCode: 0, Status: "success"}); err != nil {
		t.Fatal(err)
	}
	disp := &dispatch.Dispatch{ID: "d3", TaskID: taskID, TaskType: "execute"}
	res := &agent.RunResult{ReplyDraft: "草稿", Status: "stopped"}
	if got := d.verifyDelivery(disp, taskID, res, time.Now(), 0); got != "done" {
		t.Fatalf("账本有实质执行应 done，得到 %s", got)
	}
}

// P0-2 网关验货：账本里只有只读调用 → failed（004/007 的死法：只读不算动手）。
func TestVerifyDeliveryExecuteLedgerOnlyReads(t *testing.T) {
	d, _, dir := testDaemon(t)
	d.dataDir = dir
	taskID := "t-v-reads"
	for _, tool := range []string{"cap-ops-lean-ctx", "search_memory"} {
		if _, err := d.led.RecordExecution(ledger.ExecutionRecord{
			SessionID: "test-session", TaskID: taskID, ToolID: tool,
			Command: tool, ExitCode: 0, Status: "success"}); err != nil {
			t.Fatal(err)
		}
	}
	disp := &dispatch.Dispatch{ID: "d4", TaskID: taskID, TaskType: "execute"}
	res := &agent.RunResult{ReplyDraft: "草稿：我查完了", Status: "stopped"}
	if got := d.verifyDelivery(disp, taskID, res, time.Now(), 0); got != "failed" {
		t.Fatalf("账本只有只读调用应 failed，得到 %s", got)
	}
}

// TestVerifyDeliveryWhitelistedReadsStillBlocked（M5b）：白名单下网关拦截
// 纯只读假 DONE 的完整链路——执行器只调 3 次白名单只读工具
// （read_memory×2 + cap-fetch-tvly，刹车口径下全都不算只读）就报 DONE，
// 网关仍须按名字口径判“0 次实质调用”并拦截：结论 failed，且事实清单
// SuccessfulCalls==3、SubstantiveCalls==0、HardFloorRed==true、AllGreen==false。
// （白名单只管刹车，不管网关——这条链一断，假 DONE 就能混过去。
// 2026-11-26 一票否决已去掉：这里的拦截靠账本兜底（未声明验收且 0 次
// 实质调用），不再靠硬底线否决；HardFloorRed 只当记录供前置看。）
func TestVerifyDeliveryWhitelistedReadsStillBlocked(t *testing.T) {
	d, _, dir := testDaemon(t)
	d.dataDir = dir
	taskID := "t-v-wlreads"
	for _, tool := range []string{"read_memory", "read_memory", "cap-fetch-tvly"} {
		if _, err := d.led.RecordExecution(ledger.ExecutionRecord{
			SessionID: "test-session", TaskID: taskID, ToolID: tool,
			Command: tool, ExitCode: 0, Status: "success"}); err != nil {
			t.Fatal(err)
		}
	}
	disp := &dispatch.Dispatch{ID: "d4b", TaskID: taskID, TaskType: "execute"}
	res := &agent.RunResult{ReplyDraft: "草稿：我查完了", Status: "stopped"}
	if got := d.verifyDelivery(disp, taskID, res, time.Now(), 0); got != "failed" {
		t.Fatalf("3 次白名单只读调用应 failed，得到 %s", got)
	}
	if disp.FactList == nil {
		t.Fatalf("verifyDelivery 应在 disp 上挂事实清单，实际为空")
	}
	fl := disp.FactList
	if fl.SuccessfulCalls != 3 {
		t.Fatalf("SuccessfulCalls=%d, want 3", fl.SuccessfulCalls)
	}
	if fl.SubstantiveCalls != 0 {
		t.Fatalf("SubstantiveCalls=%d, want 0（3 次全是只读）", fl.SubstantiveCalls)
	}
	if !fl.HardFloorRed {
		t.Fatalf("HardFloorRed 应为 true（0 次实质调用）")
	}
	if fl.AllGreen {
		t.Fatalf("AllGreen 应为 false")
	}
}

// P0-3 调研底线（2026-11-26 一票否决已去掉）：0 次成功调用就交报告 →
// 网关照常放行（done），不再因硬底线标 failed；事实清单里照实记 0 次
// 调用、硬底线红、置信度 0%，由前置看事实自己判失败。
func TestVerifyDeliveryResearchNoCalls(t *testing.T) {
	d, _, dir := testDaemon(t)
	d.dataDir = dir
	taskID := "t-v-r0"
	artDir := filepath.Join(dir, "artifacts", taskID)
	if err := os.MkdirAll(artDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artDir, "reply.md"), []byte("报告：凭空写的结论"), 0o600); err != nil {
		t.Fatal(err)
	}
	disp := &dispatch.Dispatch{ID: "d5", TaskID: taskID, TaskType: "research"}
	res := &agent.RunResult{ReplyDraft: "报告：凭空写的结论", Status: "stopped"}
	if got := d.verifyDelivery(disp, taskID, res, time.Now(), 0); got != "done" {
		t.Fatalf("调研 0 次调用，网关不再否决、应 done（由前置判），得到 %s", got)
	}
	if disp.FactList == nil || !disp.FactList.HardFloorRed {
		t.Fatalf("调研 0 次调用时事实清单应照实记硬底线红，得到 %+v", disp.FactList)
	}
}

// P0-3 调研底线：有 1 次成功调用 + reply.md → done。
func TestVerifyDeliveryResearchOK(t *testing.T) {
	d, _, dir := testDaemon(t)
	d.dataDir = dir
	taskID := "t-v-r1"
	artDir := filepath.Join(dir, "artifacts", taskID)
	if err := os.MkdirAll(artDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artDir, "reply.md"), []byte("报告：查证过的结论"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.led.RecordExecution(ledger.ExecutionRecord{
		SessionID: "test-session", TaskID: taskID, ToolID: "cap-ops-lean-ctx",
		Command: "lean-ctx", ExitCode: 0, Status: "success"}); err != nil {
		t.Fatal(err)
	}
	disp := &dispatch.Dispatch{ID: "d6", TaskID: taskID, TaskType: "research"}
	res := &agent.RunResult{ReplyDraft: "报告：查证过的结论", Status: "stopped"}
	if got := d.verifyDelivery(disp, taskID, res, time.Now(), 0); got != "done" {
		t.Fatalf("调研有查证应 done，得到 %s", got)
	}
}

// P0-2 verify_cmd：exit 0 过，exit 非 0 不过。
func TestVerifyCmdPassFail(t *testing.T) {
	d, _, dir := testDaemon(t)
	d.dataDir = dir
	taskID := "t-v-cmd"
	if ok, _ := d.runVerifyCmd(&dispatch.Dispatch{TaskID: taskID, VerifyCmd: []string{"true"}}, taskID); !ok {
		t.Fatal("verify_cmd [true] 应通过")
	}
	ok, reason := d.runVerifyCmd(&dispatch.Dispatch{TaskID: taskID, VerifyCmd: []string{"false"}}, taskID)
	if ok {
		t.Fatal("verify_cmd [false] 应不通过")
	}
	if !strings.Contains(reason, "exit=1") {
		t.Fatalf("失败原因应含 exit=1，得到 %q", reason)
	}
}

// P0-2/Q7 产物核对：.json 合法性、目录、相对路径、.. 逃逸、不存在。
func TestVerifyArtifacts(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()
	good := filepath.Join(dir, "good.json")
	if err := os.WriteFile(good, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	badJSON := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(badJSON, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if bad := verifyArtifacts([]string{good}, start); len(bad) != 0 {
		t.Fatalf("合法 JSON 产物应通过，得到 %v", bad)
	}
	if bad := verifyArtifacts([]string{badJSON}, start); len(bad) == 0 {
		t.Fatal("非法 JSON 产物应不通过")
	}
	if bad := verifyArtifacts([]string{dir}, start); len(bad) == 0 {
		t.Fatal("目录不应算产物")
	}
	if bad := verifyArtifacts([]string{"relative/path.txt"}, start); len(bad) == 0 {
		t.Fatal("相对路径不应算产物")
	}
	if bad := verifyArtifacts([]string{dir + "/../escape.txt"}, start); len(bad) == 0 {
		t.Fatal("含 .. 的路径不应算产物")
	}
	if bad := verifyArtifacts([]string{filepath.Join(dir, "missing.txt")}, start); len(bad) == 0 {
		t.Fatal("不存在的文件不应算产物")
	}
	// Gemini 审阅中危：全空白文件（只有空格/换行）不应算产物。
	blank := filepath.Join(dir, "blank.txt")
	if err := os.WriteFile(blank, []byte("   \n\n  \t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if bad := verifyArtifacts([]string{blank}, start); len(bad) == 0 {
		t.Fatal("全空白文件不应算产物")
	}
}

// 非 stopped 状态原样透传，不验货。
func TestVerifyDeliveryPassThrough(t *testing.T) {
	d, _, dir := testDaemon(t)
	d.dataDir = dir
	disp := &dispatch.Dispatch{ID: "d7", TaskID: "t-v-pt", TaskType: "execute"}
	res := &agent.RunResult{ReplyDraft: "汇总", Status: "max_iterations"}
	if got := d.verifyDelivery(disp, "t-v-pt", res, time.Now(), 0); got != "max_iterations" {
		t.Fatalf("max_iterations 应原样透传，得到 %s", got)
	}
}

// 对齐二号三号回归（2026-11-25）：verify_cmd 不再一票否决——
// 没跑起来（Ran=false，二进制不存在）或跑起来 exit 非 0，
// 都只是事实清单里的一条参考证据（Passed=false），
// 不标硬底线红；但也不算“全绿”（快路径不直接放行，
// 交前置模型看失败记录再判；模型仍可判通过）。
// H-1 原意保留在新形式里：没跑起来的校验命令绝不能被算成通过。
func TestComputeFactListVerifyCmdNotStartedIsRed(t *testing.T) {
	d, _, dir := testDaemon(t)
	d.dataDir = dir
	taskID := "t-h1-vc"
	start := time.Now()
	// 账本里记一次成功调用，免得 0 调用先红灯、掩盖本案的断言。
	if _, err := d.led.RecordExecution(ledger.ExecutionRecord{
		SessionID: "test-session", TaskID: taskID, ToolID: "test-echo",
		Command: "echo hi", ExitCode: 0, Status: "success"}); err != nil {
		t.Fatal(err)
	}
	disp := &dispatch.Dispatch{ID: "dh1", TaskID: taskID, TaskType: "execute",
		VerifyCmd: []string{"no-such-binary-xyz-007"}}
	fl := d.computeFactList(disp, taskID, start, 0, false)
	if fl.VerifyCmd == nil {
		t.Fatal("verify_cmd 声明了，事实清单里应有 VerifyCmd")
	}
	// 执行器对不存在的二进制返回 ExitCode=-1（不是 Go 错误），
	// 所以 Ran 可能为 true；关键是 Passed=false 必须如实记下，
	// 不能被算成通过。
	if fl.VerifyCmd.Passed {
		t.Fatalf("没跑起来/跑不起来的校验命令不应算通过，得到 %+v", fl.VerifyCmd)
	}
	if fl.VerifyCmd.ExitCode == 0 {
		t.Fatalf("不存在的二进制 exit 应非 0，得到 %+v", fl.VerifyCmd)
	}
	// verify_cmd 失败不标硬底线红（不再一票否决）。
	if fl.HardFloorRed {
		t.Fatalf("verify_cmd 失败不应标硬底线红，得到 reason=%q", fl.HardFloorReason)
	}
	// 但也不算全绿：快路径不直接放行，交前置模型判。
	if fl.AllGreen {
		t.Fatal("verify_cmd 失败时 AllGreen 应为 false（不直接放行，交模型判）")
	}
	// 对照：verify_cmd 通过 + 有成功调用 → 全绿，快路径照常。
	dispOK := &dispatch.Dispatch{ID: "dh1-ok", TaskID: taskID, TaskType: "execute",
		VerifyCmd: []string{"true"}}
	flOK := d.computeFactList(dispOK, taskID, start, 0, false)
	if !flOK.AllGreen {
		t.Fatalf("verify_cmd 通过且有成功调用时应全绿，得到 %+v", flOK)
	}
}

// H-2 回归：第 2 步 0 调用不继承第 1 步的账本——按本次 runAgent 窗口
// （execIDBefore 之后的 id）统计；窗口外的行不算。
func TestComputeFactListSecondStepZeroCallsNotInherited(t *testing.T) {
	d, _, dir := testDaemon(t)
	d.dataDir = dir
	taskID := "t-h2-win"
	// 第 1 步：一次成功调用（账本里留一行）。
	if _, err := d.led.RecordExecution(ledger.ExecutionRecord{
		SessionID: "test-session", TaskID: taskID, ToolID: "test-echo",
		Command: "echo hi", ExitCode: 0, Status: "success"}); err != nil {
		t.Fatal(err)
	}
	// 第 2 步开始前记下账本最大行号（runAgent 做的事）。
	before, err := d.led.MaxExecutionID()
	if err != nil {
		t.Fatal(err)
	}
	if before == 0 {
		t.Fatal("第 1 步的账本行应已存在，MaxExecutionID 不应为 0")
	}
	// 第 2 步 0 调用：按窗口统计应为 0 次成功调用 → 红灯。
	disp := &dispatch.Dispatch{ID: "dh2", TaskID: taskID, TaskType: "execute"}
	fl := d.computeFactList(disp, taskID, time.Now(), before, false)
	if fl.SuccessfulCalls != 0 {
		t.Fatalf("第 2 步 0 调用，窗口内成功调用应为 0，得到 %d", fl.SuccessfulCalls)
	}
	if !fl.HardFloorRed {
		t.Fatal("第 2 步 0 调用应红灯（不能继承第 1 步账本）")
	}
	// 正向对照：第 2 步里调一次，窗口内应数到 1 次。
	if _, err := d.led.RecordExecution(ledger.ExecutionRecord{
		SessionID: "test-session", TaskID: taskID, ToolID: "test-echo",
		Command: "echo hi2", ExitCode: 0, Status: "success"}); err != nil {
		t.Fatal(err)
	}
	fl2 := d.computeFactList(disp, taskID, time.Now(), before, false)
	if fl2.SuccessfulCalls != 1 {
		t.Fatalf("第 2 步调 1 次后，窗口内成功调用应为 1，得到 %d", fl2.SuccessfulCalls)
	}
	if fl2.HardFloorRed {
		t.Fatalf("第 2 步有 1 次调用不应红灯，得到 %q", fl2.HardFloorReason)
	}
	// 旧口径对照：boundary=0 时全单统计应数到 2 次（第 1 步 + 第 2 步）。
	flAll := d.computeFactList(disp, taskID, time.Now(), 0, false)
	if flAll.SuccessfulCalls != 2 {
		t.Fatalf("旧口径（boundary=0）应数到 2 次，得到 %d", flAll.SuccessfulCalls)
	}
}

// TestComputeFactListPythonCountsSubstantive：网关账本口径里，
// cap-ops-python 成功调用算实质执行（见 gatewaySubstantiveCall）。
// 对齐 E2E 任务 A（2026-11-25）：执行层靠 python 复制文件，
// 名字口径算成 0 次实质调用，网关直接判失败、前置重干烧光预算。
// 对照：纯读工具（read_memory）成功调用不算实质，0 实质 + 无机器检查
// 的 execute 步仍红灯。
func TestComputeFactListPythonCountsSubstantive(t *testing.T) {
	d, _, dir := testDaemon(t)
	d.dataDir = dir
	taskID := "t-py-sub"
	start := time.Now()
	// 账本里记一次 cap-ops-python 成功调用（模拟写文件那步）。
	if _, err := d.led.RecordExecution(ledger.ExecutionRecord{
		SessionID: "test-session", TaskID: taskID, ToolID: "cap-ops-python",
		Command: "write file", ExitCode: 0, Status: "success"}); err != nil {
		t.Fatal(err)
	}
	// 无产物声明、无 verify_cmd 的 execute 步：python 调用应算实质，
	// 不红灯；网关账本兜底应通过。
	disp := &dispatch.Dispatch{ID: "dpy", TaskID: taskID, TaskType: "execute"}
	fl := d.computeFactList(disp, taskID, start, 0, false)
	if fl.SuccessfulCalls != 1 || fl.SubstantiveCalls != 1 {
		t.Fatalf("cap-ops-python 成功调用应算 1 次实质，得到成功=%d 实质=%d", fl.SuccessfulCalls, fl.SubstantiveCalls)
	}
	if fl.HardFloorRed {
		t.Fatalf("python 干活的步不应红灯，得到 %q", fl.HardFloorReason)
	}
	if ok, reason := d.verifyExecute(disp, taskID, fl); !ok {
		t.Fatalf("无声明验收 + python 实质调用，网关兜底应通过，得到 %q", reason)
	}
	// 对照：只调纯读工具 read_memory → 0 实质 → 红灯。
	taskID2 := "t-py-read"
	if _, err := d.led.RecordExecution(ledger.ExecutionRecord{
		SessionID: "test-session", TaskID: taskID2, ToolID: "read_memory",
		Command: "read", ExitCode: 0, Status: "success"}); err != nil {
		t.Fatal(err)
	}
	disp2 := &dispatch.Dispatch{ID: "dpy2", TaskID: taskID2, TaskType: "execute"}
	fl2 := d.computeFactList(disp2, taskID2, start, 0, false)
	if fl2.SubstantiveCalls != 0 {
		t.Fatalf("read_memory 成功调用不应算实质，得到 %d", fl2.SubstantiveCalls)
	}
	if !fl2.HardFloorRed {
		t.Fatal("纯读工具 + 无机器检查的 execute 步应红灯")
	}
}

// TestComputeFactListVerifyCmdFailedZeroSubstantiveIsRed（H-2 回归）：
// execute 步无 ExpectedArtifacts、声明了 verify_cmd（失败）、
// 账本里只有 1 次 read_memory（0 实质调用）→ 必须硬底线红。
// 以前 HasMachineCheck 把 verify_cmd 算在内，0 次实质调用的检查
// 被跳过，写了 verify_cmd 反而比不写更松。
func TestComputeFactListVerifyCmdFailedZeroSubstantiveIsRed(t *testing.T) {
	d, _, dir := testDaemon(t)
	d.dataDir = dir
	taskID := "t-h2-vc-fail"
	start := time.Now()
	// 账本里只记一次 read_memory 成功调用（0 实质调用）。
	if _, err := d.led.RecordExecution(ledger.ExecutionRecord{
		SessionID: "test-session", TaskID: taskID, ToolID: "read_memory",
		Command: "read", ExitCode: 0, Status: "success"}); err != nil {
		t.Fatal(err)
	}
	disp := &dispatch.Dispatch{ID: "dh2v", TaskID: taskID, TaskType: "execute",
		VerifyCmd: []string{"false"}}
	fl := d.computeFactList(disp, taskID, start, 0, false)
	if fl.SubstantiveCalls != 0 {
		t.Fatalf("只有 read_memory 调用，实质调用应为 0，得到 %d", fl.SubstantiveCalls)
	}
	if fl.VerifyCmd == nil || fl.VerifyCmd.Passed {
		t.Fatalf("verify_cmd 声明且失败，事实清单里应记 Passed=false，得到 %+v", fl.VerifyCmd)
	}
	if !fl.HardFloorRed {
		t.Fatal("声明了失败的 verify_cmd、无产物、0 实质调用的 execute 步必须硬底线红")
	}
	// 一票否决已去掉（2026-11-26 用户定）：硬底线红时网关照常放行，
	// 由前置看事实清单（verify_cmd 失败、0 实质调用）自己判失败。
	if ok, reason := d.verifyExecute(disp, taskID, fl); !ok {
		t.Fatalf("硬底线红时网关不再否决、应放行（由前置判），得到 %q", reason)
	}
	// 对照：verify_cmd 真实通过时，0 实质调用可豁免（不红灯）。
	dispOK := &dispatch.Dispatch{ID: "dh2v-ok", TaskID: taskID, TaskType: "execute",
		VerifyCmd: []string{"true"}}
	flOK := d.computeFactList(dispOK, taskID, start, 0, false)
	if flOK.HardFloorRed {
		t.Fatalf("verify_cmd 真实通过时 0 实质调用应豁免，得到 reason=%q", flOK.HardFloorReason)
	}
}

// TestComputeFactListResumedZeroNewCallsNotRed（方向 b 严谨版回归）：
// 本步 FirstExecIDBefore 是步开始前账本最大行号，账本里本步之前尝试
// 有 3 次成功调用；重试时 execIDBefore 已到最新、resumed=true、
// 新窗口 0 调用 → 硬底线不红、快路径不走、PriorStepCalls==3；
// 网关验货（research 与 execute）都应通过。
func TestComputeFactListResumedZeroNewCallsNotRed(t *testing.T) {
	d, _, dir := testDaemon(t)
	d.dataDir = dir
	taskID := "t-resume-0new"
	start := time.Now()
	// 步开始前记下账本最大行号（前置首次派发前做的事）。
	before, err := d.led.MaxExecutionID()
	if err != nil {
		t.Fatal(err)
	}
	// 本步首轮：3 次 cap-ops-python 成功调用（实质）。
	for i := 0; i < 3; i++ {
		if _, err := d.led.RecordExecution(ledger.ExecutionRecord{
			SessionID: "test-session", TaskID: taskID, ToolID: "cap-ops-python",
			Command: "write file", ExitCode: 0, Status: "success"}); err != nil {
			t.Fatal(err)
		}
	}
	execIDBefore, err := d.led.MaxExecutionID()
	if err != nil {
		t.Fatal(err)
	}
	if execIDBefore-before != 3 {
		t.Fatalf("首轮应记 3 行账本，实际 %d 行", execIDBefore-before)
	}
	disp := &dispatch.Dispatch{ID: "dr0", TaskID: taskID, TaskType: "execute",
		FirstExecIDBefore: before, FirstStartTime: start}
	fl := d.computeFactList(disp, taskID, start, execIDBefore, true)
	if fl.PriorStepCalls != 3 {
		t.Fatalf("本步历史成功调用应为 3，得到 %d", fl.PriorStepCalls)
	}
	if fl.PriorStepSubstantiveCalls != 3 {
		t.Fatalf("本步历史实质调用应为 3，得到 %d", fl.PriorStepSubstantiveCalls)
	}
	if fl.SuccessfulCalls != 0 {
		t.Fatalf("新窗口 0 调用，SuccessfulCalls 应为 0，得到 %d", fl.SuccessfulCalls)
	}
	if fl.HardFloorRed {
		t.Fatalf("续跑 + 本步历史有调用，硬底线不应红，得到 %q", fl.HardFloorReason)
	}
	if fl.AllGreen {
		t.Fatal("本轮 0 新调用，AllGreen 必须为 false（绝不走快车道免判）")
	}
	if !fl.Resumed {
		t.Fatal("resumed=true 时 FactList.Resumed 应为 true")
	}
	// 网关验货：execute 步（无产物无 verify_cmd，有历史实质调用）应通过。
	if ok, reason := d.verifyExecute(disp, taskID, fl); !ok {
		t.Fatalf("续跑 execute 步网关应通过，得到 %q", reason)
	}
	// 网关验货：research 步（回信文件存在）应通过。
	artDir := filepath.Join(dir, "artifacts", taskID)
	if err := os.MkdirAll(artDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artDir, "reply.md"), []byte("报告：续跑确认"), 0o600); err != nil {
		t.Fatal(err)
	}
	dispR := &dispatch.Dispatch{ID: "dr0r", TaskID: taskID, TaskType: "research",
		FirstExecIDBefore: before, FirstStartTime: start}
	flR := d.computeFactList(dispR, taskID, start, execIDBefore, true)
	if ok, reason := d.verifyResearch(taskID, dispR, flR); !ok {
		t.Fatalf("续跑 research 步网关应通过，得到 %q", reason)
	}
}

// TestComputeFactListStep2CannotInheritStep1（H-2 不退化）：
// 第 1 步产生 3 行账本；第 2 步 FirstExecIDBefore 已到最新、
// execIDBefore 相同、resumed=false、0 新调用 → PriorStepCalls==0、
// HardFloorRed==true（第 2 步绝不能继承第 1 步）。
func TestComputeFactListStep2CannotInheritStep1(t *testing.T) {
	d, _, dir := testDaemon(t)
	d.dataDir = dir
	taskID := "t-step2-noinherit"
	start := time.Now()
	// 第 1 步：3 次成功调用。
	for i := 0; i < 3; i++ {
		if _, err := d.led.RecordExecution(ledger.ExecutionRecord{
			SessionID: "test-session", TaskID: taskID, ToolID: "cap-ops-python",
			Command: "write file", ExitCode: 0, Status: "success"}); err != nil {
			t.Fatal(err)
		}
	}
	before2, err := d.led.MaxExecutionID()
	if err != nil {
		t.Fatal(err)
	}
	// 第 2 步：首次派发，FirstExecIDBefore=before2，0 新调用，未续跑。
	disp := &dispatch.Dispatch{ID: "ds2", TaskID: taskID, TaskType: "execute",
		FirstExecIDBefore: before2, FirstStartTime: start}
	fl := d.computeFactList(disp, taskID, start, before2, false)
	if fl.PriorStepCalls != 0 {
		t.Fatalf("第 2 步 PriorStepCalls 应为 0（不能继承第 1 步），得到 %d", fl.PriorStepCalls)
	}
	if fl.SuccessfulCalls != 0 {
		t.Fatalf("第 2 步 0 调用，SuccessfulCalls 应为 0，得到 %d", fl.SuccessfulCalls)
	}
	if !fl.HardFloorRed {
		t.Fatal("第 2 步 0 调用应硬底线红（不能继承第 1 步账本）")
	}
}

// TestComputeFactListResumedWithZeroPriorStillRed：本步续跑，但历史上
// 也只有失败调用或 0 次调用 → 硬底线仍红（豁免只给“历史真干过活”的步）。
func TestComputeFactListResumedWithZeroPriorStillRed(t *testing.T) {
	d, _, dir := testDaemon(t)
	d.dataDir = dir
	taskID := "t-resume-zeroprior"
	start := time.Now()
	before, err := d.led.MaxExecutionID()
	if err != nil {
		t.Fatal(err)
	}
	// 本步首轮：只有 1 次失败调用（exit 1），不算成功。
	if _, err := d.led.RecordExecution(ledger.ExecutionRecord{
		SessionID: "test-session", TaskID: taskID, ToolID: "test-false",
		Command: "false", ExitCode: 1, Status: "failed"}); err != nil {
		t.Fatal(err)
	}
	execIDBefore, err := d.led.MaxExecutionID()
	if err != nil {
		t.Fatal(err)
	}
	disp := &dispatch.Dispatch{ID: "drz", TaskID: taskID, TaskType: "execute",
		FirstExecIDBefore: before, FirstStartTime: start}
	fl := d.computeFactList(disp, taskID, start, execIDBefore, true)
	if fl.PriorStepCalls != 0 {
		t.Fatalf("历史上只有失败调用，PriorStepCalls 应为 0，得到 %d", fl.PriorStepCalls)
	}
	if !fl.HardFloorRed {
		t.Fatal("续跑但本步历史 0 次成功调用，硬底线应红")
	}
}

// TestComputeFactListLegacyZeroFirstExecIDNoLeak（旧计划表兼容）：
// FirstExecIDBefore==0、resumed==true、新窗口 0 调用，即使全单历史
// 有旧账本行，也不得误计入 PriorStepCalls，应安全标红。
func TestComputeFactListLegacyZeroFirstExecIDNoLeak(t *testing.T) {
	d, _, dir := testDaemon(t)
	d.dataDir = dir
	taskID := "t-legacy-nofirst"
	start := time.Now()
	// 全单历史：3 次成功调用（旧任务/旧步留下的）。
	for i := 0; i < 3; i++ {
		if _, err := d.led.RecordExecution(ledger.ExecutionRecord{
			SessionID: "test-session", TaskID: taskID, ToolID: "cap-ops-python",
			Command: "write file", ExitCode: 0, Status: "success"}); err != nil {
			t.Fatal(err)
		}
	}
	execIDBefore, err := d.led.MaxExecutionID()
	if err != nil {
		t.Fatal(err)
	}
	// 老计划表：FirstExecIDBefore==0（缺字段），续跑、0 新调用。
	disp := &dispatch.Dispatch{ID: "dlg", TaskID: taskID, TaskType: "execute",
		FirstExecIDBefore: 0}
	fl := d.computeFactList(disp, taskID, start, execIDBefore, true)
	if fl.PriorStepCalls != 0 {
		t.Fatalf("FirstExecIDBefore==0 时不得追溯历史，PriorStepCalls 应为 0，得到 %d", fl.PriorStepCalls)
	}
	if !fl.HardFloorRed {
		t.Fatal("老计划表 + 0 新调用应安全标红（不追溯全单历史）")
	}
}

// TestVerifyDeliveryStalledAttachesFactListButNeverDone（CF 诊断中优）：
// stalled/max_iterations 回执也携带事实清单（供前置判回执参考），
// 但状态保持原样非 done，绝不触发验货改标 done。
func TestVerifyDeliveryStalledAttachesFactListButNeverDone(t *testing.T) {
	d, _, dir := testDaemon(t)
	d.dataDir = dir
	taskID := "t-v-stalled"
	// 账本里记一次成功调用，让事实清单有内容。
	if _, err := d.led.RecordExecution(ledger.ExecutionRecord{
		SessionID: "test-session", TaskID: taskID, ToolID: "test-echo",
		Command: "echo hi", ExitCode: 0, Status: "success"}); err != nil {
		t.Fatal(err)
	}
	disp := &dispatch.Dispatch{ID: "d-st", TaskID: taskID, TaskType: "execute"}
	res := &agent.RunResult{ReplyDraft: "停滞", Status: "stalled"}
	if got := d.verifyDelivery(disp, taskID, res, time.Now(), 0); got != "stalled" {
		t.Fatalf("stalled 应原样返回，得到 %s", got)
	}
	if disp.FactList == nil {
		t.Fatal("stalled 回执也应携带事实清单")
	}
	if disp.FactList.SuccessfulCalls < 1 {
		t.Fatalf("事实清单应记下 1 次成功调用，实际 %d", disp.FactList.SuccessfulCalls)
	}
	// max_iterations 同理。
	disp2 := &dispatch.Dispatch{ID: "d-mi", TaskID: taskID, TaskType: "execute"}
	res2 := &agent.RunResult{ReplyDraft: "超轮", Status: "max_iterations"}
	if got := d.verifyDelivery(disp2, taskID, res2, time.Now(), 0); got != "max_iterations" {
		t.Fatalf("max_iterations 应原样返回，得到 %s", got)
	}
	if disp2.FactList == nil {
		t.Fatal("max_iterations 回执也应携带事实清单")
	}
}

// TestDaemonNoAdvisorWired（监工下线回归）：守护进程的执行器不接
// 任何监工——ex.Advisor 为 nil，交活不再走 ReviewDelivery。
// （main.go 已不再 import internal/advisor；沙箱 E2E 另验：
// 跑任务时监工调用次数为 0、费用为 0。）
func TestDaemonNoAdvisorWired(t *testing.T) {
	d, _, _ := testDaemon(t)
	if d.ex.Advisor != nil {
		t.Fatal("守护进程的执行器不应接监工（Advisor 应为 nil）")
	}
	if _, err := d.ex.ReviewDelivery(executor.Task{TaskID: "t-noadv", SessionID: "s", ToolID: "agent", Text: "x", Draft: "y"}); !errors.Is(err, executor.ErrNoAdvisor) {
		t.Fatalf("无监工时 ReviewDelivery 应返回 ErrNoAdvisor，实际 err=%v", err)
	}
}

// 置信度单测辅助：在指定目录写回执正文（reply.md 或 step-N-reply.md）。
func writeConfidenceReply(t *testing.T, dir, taskID string, stepNo int, body string) {
	t.Helper()
	artDir := filepath.Join(dir, "artifacts", taskID)
	if err := os.MkdirAll(artDir, 0o700); err != nil {
		t.Fatal(err)
	}
	name := "reply.md"
	if stepNo > 0 {
		name = fmt.Sprintf("step-%d-reply.md", stepNo)
	}
	if err := os.WriteFile(filepath.Join(artDir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeClaimedFile 在指定目录写一个非空文件，返回绝对路径。
func writeClaimedFile(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("内容"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestComputeFactListConfidence100：调了工具 + 回执声明的 1 个文件全核对有
// → 置信度 100%（工具 50% + 成果 50%）。
func TestComputeFactListConfidence100(t *testing.T) {
	d, _, dir := testDaemon(t)
	d.dataDir = dir
	taskID := "t-conf-100"
	start := time.Now()
	claimDir := t.TempDir()
	f1 := writeClaimedFile(t, claimDir, "out.txt")
	if _, err := d.led.RecordExecution(ledger.ExecutionRecord{
		SessionID: "test-session", TaskID: taskID, ToolID: "cap-ops-python",
		Command: "write file", ExitCode: 0, Status: "success"}); err != nil {
		t.Fatal(err)
	}
	writeConfidenceReply(t, dir, taskID, 0,
		"干了什么：写了文件\n结果是什么：写好了\n成果在哪里："+f1+"\n用了哪些工具：cap-ops-python\n")
	disp := &dispatch.Dispatch{ID: "dc100", TaskID: taskID, TaskType: "execute"}
	fl := d.computeFactList(disp, taskID, start, 0, false)
	if fl.Confidence != 100 {
		t.Fatalf("置信度应为 100%%，实际 %d（工具 %d%% + 成果 %d%%），claimed=%d verified=%d missing=%v",
			fl.Confidence, fl.ConfidenceToolPoints, fl.ConfidenceFilePoints, fl.ClaimedFiles, fl.VerifiedFiles, fl.MissingFiles)
	}
	if fl.ConfidenceToolPoints != 50 || fl.ConfidenceFilePoints != 50 {
		t.Fatalf("工具应 50%%、成果应 50%%，实际工具 %d%% 成果 %d%%", fl.ConfidenceToolPoints, fl.ConfidenceFilePoints)
	}
	if fl.ClaimedFiles != 1 || fl.VerifiedFiles != 1 || len(fl.MissingFiles) != 0 {
		t.Fatalf("claimed 应 1、verified 应 1、missing 应 0，实际 %d/%d/%v", fl.ClaimedFiles, fl.VerifiedFiles, fl.MissingFiles)
	}
	if fl.ToolCallBreakdown != "cap-ops-python×1" {
		t.Fatalf("工具分项应为 cap-ops-python×1，实际 %q", fl.ToolCallBreakdown)
	}
	// Summary 四行格式。
	sum := fl.Summary()
	if !strings.Contains(sum, "结果置信度：100%（工具 50% + 成果 50%）") {
		t.Fatalf("Summary 第 1 行应为置信度 100%%，实际：\n%s", sum)
	}
	if !strings.Contains(sum, "工具调用：1 次（cap-ops-python×1）") {
		t.Fatalf("Summary 第 2 行应为工具调用 1 次，实际：\n%s", sum)
	}
	if !strings.Contains(sum, "回执内成果文件：1 个") {
		t.Fatalf("Summary 第 3 行应为成果文件 1 个，实际：\n%s", sum)
	}
	if !strings.Contains(sum, "成果文件核对结果（回执 1/实际 1/缺失 0）：有文件存在，最后修改/操作时间在本轮派单之后，所以判为 有 文件！") {
		t.Fatalf("Summary 第 4 行应为核对结果全过，实际：\n%s", sum)
	}
}

// TestComputeFactListConfidence90：调了工具 + 回执声明 10 个文件、核对有 8 个
// → 置信度 90%（工具 50% + 成果 40%），缺失 2 个列出。
func TestComputeFactListConfidence90(t *testing.T) {
	d, _, dir := testDaemon(t)
	d.dataDir = dir
	taskID := "t-conf-90"
	start := time.Now()
	claimDir := t.TempDir()
	var claimed []string
	for i := 0; i < 8; i++ {
		claimed = append(claimed, writeClaimedFile(t, claimDir, fmt.Sprintf("ok-%d.txt", i)))
	}
	missing1 := filepath.Join(claimDir, "missing-1.txt")
	missing2 := filepath.Join(claimDir, "missing-2.txt")
	claimed = append(claimed, missing1, missing2)
	if _, err := d.led.RecordExecution(ledger.ExecutionRecord{
		SessionID: "test-session", TaskID: taskID, ToolID: "cap-ops-python",
		Command: "write files", ExitCode: 0, Status: "success"}); err != nil {
		t.Fatal(err)
	}
	body := "干了什么：写了 10 个文件\n结果是什么：写好了 8 个\n成果在哪里：\n" +
		strings.Join(claimed, "\n") + "\n用了哪些工具：cap-ops-python\n"
	writeConfidenceReply(t, dir, taskID, 0, body)
	disp := &dispatch.Dispatch{ID: "dc90", TaskID: taskID, TaskType: "execute"}
	fl := d.computeFactList(disp, taskID, start, 0, false)
	if fl.Confidence != 90 {
		t.Fatalf("置信度应为 90%%，实际 %d，claimed=%d verified=%d missing=%v",
			fl.Confidence, fl.ClaimedFiles, fl.VerifiedFiles, fl.MissingFiles)
	}
	if fl.ClaimedFiles != 10 || fl.VerifiedFiles != 8 || len(fl.MissingFiles) != 2 {
		t.Fatalf("claimed 应 10、verified 应 8、missing 应 2，实际 %d/%d/%v", fl.ClaimedFiles, fl.VerifiedFiles, fl.MissingFiles)
	}
	for _, m := range []string{missing1, missing2} {
		found := false
		for _, got := range fl.MissingFiles {
			if got == m {
				found = true
			}
		}
		if !found {
			t.Fatalf("缺失列表应含 %s，实际 %v", m, fl.MissingFiles)
		}
	}
}

// TestComputeFactListConfidenceZeroNoCalls：0 次调用 + 回执声明的文件都在盘上
// 但 mtime 在接单时间之前（上一步留下的旧文件）→ 时间关全算缺失，
// 置信度 0%（工具 0% + 成果 0%）。这是“没调工具但文件都在”的口子被堵死的回归。
func TestComputeFactListConfidenceZeroNoCalls(t *testing.T) {
	d, _, dir := testDaemon(t)
	d.dataDir = dir
	taskID := "t-conf-0"
	claimDir := t.TempDir()
	f1 := writeClaimedFile(t, claimDir, "old-1.txt")
	f2 := writeClaimedFile(t, claimDir, "old-2.txt")
	// 把两个文件的 mtime 拨回 1 小时前（接单时间之前），模拟旧文件。
	old := time.Now().Add(-time.Hour)
	for _, f := range []string{f1, f2} {
		if err := os.Chtimes(f, old, old); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	// 账本里 0 次调用。
	writeConfidenceReply(t, dir, taskID, 0,
		"干了什么：没干活\n结果是什么：没做完\n成果在哪里："+f1+"、"+f2+"\n用了哪些工具：无\n")
	disp := &dispatch.Dispatch{ID: "dc0", TaskID: taskID, TaskType: "execute"}
	fl := d.computeFactList(disp, taskID, start, 0, false)
	if fl.Confidence != 0 {
		t.Fatalf("0 次调用且文件全是旧的，置信度应为 0%%，实际 %d，claimed=%d verified=%d missing=%v",
			fl.Confidence, fl.ClaimedFiles, fl.VerifiedFiles, fl.MissingFiles)
	}
	if fl.ClaimedFiles != 2 || fl.VerifiedFiles != 0 || len(fl.MissingFiles) != 2 {
		t.Fatalf("claimed 应 2、verified 应 0、missing 应 2，实际 %d/%d/%v", fl.ClaimedFiles, fl.VerifiedFiles, fl.MissingFiles)
	}
}

// TestComputeFactListConfidenceNoClaimedFiles：回执没声明成果文件
// （“成果在哪里：无”）且调了工具 → 工具那点占 100%，置信度 100%。
func TestComputeFactListConfidenceNoClaimedFiles(t *testing.T) {
	d, _, dir := testDaemon(t)
	d.dataDir = dir
	taskID := "t-conf-nofile"
	start := time.Now()
	if _, err := d.led.RecordExecution(ledger.ExecutionRecord{
		SessionID: "test-session", TaskID: taskID, ToolID: "cap-ops-python",
		Command: "read files", ExitCode: 0, Status: "success"}); err != nil {
		t.Fatal(err)
	}
	writeConfidenceReply(t, dir, taskID, 0,
		"干了什么：调研\n结果是什么：查清楚了\n成果在哪里：无\n用了哪些工具：cap-ops-python\n")
	disp := &dispatch.Dispatch{ID: "dcnf", TaskID: taskID, TaskType: "research"}
	fl := d.computeFactList(disp, taskID, start, 0, false)
	if fl.Confidence != 100 {
		t.Fatalf("没声明文件且调了工具，置信度应为 100%%，实际 %d", fl.Confidence)
	}
	if fl.ConfidenceToolPoints != 100 || fl.ConfidenceFilePoints != 0 {
		t.Fatalf("工具应 100%%、成果应 0%%，实际工具 %d%% 成果 %d%%", fl.ConfidenceToolPoints, fl.ConfidenceFilePoints)
	}
	if fl.ClaimedFiles != 0 {
		t.Fatalf("claimed 应为 0，实际 %d", fl.ClaimedFiles)
	}
	// Summary：没声明文件时第 3 行写“0 个（回执没声明成果文件）”，
	// 第 4 行核对结果省略（共五行）。
	sum := fl.Summary()
	if !strings.Contains(sum, "回执内成果文件：0 个（回执没声明成果文件）") {
		t.Fatalf("Summary 第 3 行应写没声明成果文件，实际：\n%s", sum)
	}
	if strings.Contains(sum, "成果文件核对结果") {
		t.Fatalf("没声明文件时不应有核对结果行，实际：\n%s", sum)
	}
}

// TestComputeFactListConfidenceTimeGate：2 个声明文件，1 个是本步新建的
// （mtime 在接单后），1 个是旧文件（mtime 在接单前）→ 核对有 1 个、
// 缺失 1 个，置信度 75%（工具 50% + 成果 25%）。
func TestComputeFactListConfidenceTimeGate(t *testing.T) {
	d, _, dir := testDaemon(t)
	d.dataDir = dir
	taskID := "t-conf-time"
	start := time.Now()
	claimDir := t.TempDir()
	fNew := writeClaimedFile(t, claimDir, "new.txt")
	fOld := writeClaimedFile(t, claimDir, "old.txt")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(fOld, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := d.led.RecordExecution(ledger.ExecutionRecord{
		SessionID: "test-session", TaskID: taskID, ToolID: "cap-ops-python",
		Command: "write file", ExitCode: 0, Status: "success"}); err != nil {
		t.Fatal(err)
	}
	writeConfidenceReply(t, dir, taskID, 0,
		"干了什么：写了文件\n结果是什么：写好了\n成果在哪里："+fNew+"、"+fOld+"\n用了哪些工具：cap-ops-python\n")
	disp := &dispatch.Dispatch{ID: "dctime", TaskID: taskID, TaskType: "execute"}
	fl := d.computeFactList(disp, taskID, start, 0, false)
	if fl.Confidence != 75 {
		t.Fatalf("置信度应为 75%%，实际 %d，claimed=%d verified=%d missing=%v",
			fl.Confidence, fl.ClaimedFiles, fl.VerifiedFiles, fl.MissingFiles)
	}
	if fl.VerifiedFiles != 1 || len(fl.MissingFiles) != 1 || fl.MissingFiles[0] != fOld {
		t.Fatalf("verified 应 1、missing 应 [%s]，实际 verified=%d missing=%v", fOld, fl.VerifiedFiles, fl.MissingFiles)
	}
}

// TestComputeFactListConfidenceBaselineIsDispatchTime：计划创建时间比接单
// 时间早 3 分钟（CF-015 实测：计划 09:47:22 建，第 2 步 09:50:19 接单）：
// 计划创建时间与接单时间之间创建的文件，时间关必须不通过；接单时间
// 之后创建的文件，时间关必须通过。回归：以前 factBaseline 取 plan
// created_ts，上一步在 3 分钟窗口里写的文件也能通过这一步的时间关，太松。
func TestComputeFactListConfidenceBaselineIsDispatchTime(t *testing.T) {
	d, _, dir := testDaemon(t)
	d.dataDir = dir
	// 会话存盘 + 计划（创建时间 = 接单时间 - 3 分钟）。
	d.sess = session.New(dir, d.led)
	taskID := "t-conf-baseline"
	planCreated := time.Now().Add(-5 * time.Minute)
	start := planCreated.Add(3 * time.Minute)
	if err := d.sess.SavePlan(&session.PlanState{
		PlanID: "plan-base", SessionID: "sess-base", TaskID: taskID,
		Status:    session.PlanRunning,
		CreatedTS: planCreated.Format(time.RFC3339),
		UpdatedTS: planCreated.Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	claimDir := t.TempDir()
	fBetween := writeClaimedFile(t, claimDir, "between.txt")
	fAfter := writeClaimedFile(t, claimDir, "after.txt")
	// between：在计划创建时间与接单时间之间；after：在接单时间之后。
	if err := os.Chtimes(fBetween, planCreated.Add(time.Minute), planCreated.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(fAfter, start.Add(time.Minute), start.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.led.RecordExecution(ledger.ExecutionRecord{
		SessionID: "test-session", TaskID: taskID, ToolID: "cap-ops-python",
		Command: "write files", ExitCode: 0, Status: "success"}); err != nil {
		t.Fatal(err)
	}
	writeConfidenceReply(t, dir, taskID, 0,
		"干了什么：写了文件\n结果是什么：写好了\n成果在哪里："+fBetween+"、"+fAfter+"\n用了哪些工具：cap-ops-python\n")
	disp := &dispatch.Dispatch{ID: "dcbase", TaskID: taskID, TaskType: "execute", SessionID: "sess-base"}
	// 基线必须是接单时间，不是计划创建时间。
	baseline := d.factBaseline(disp, start, false)
	if !baseline.Equal(start) {
		t.Fatalf("基线应为接单时间 %v，实际 %v（计划创建时间 %v）", start, baseline, planCreated)
	}
	fl := d.computeFactList(disp, taskID, baseline, 0, false)
	if fl.Confidence != 75 {
		t.Fatalf("置信度应为 75%%，实际 %d，claimed=%d verified=%d missing=%v",
			fl.Confidence, fl.ClaimedFiles, fl.VerifiedFiles, fl.MissingFiles)
	}
	if fl.VerifiedFiles != 1 || len(fl.MissingFiles) != 1 || fl.MissingFiles[0] != fBetween {
		t.Fatalf("verified 应 1、missing 应 [%s]，实际 verified=%d missing=%v", fBetween, fl.VerifiedFiles, fl.MissingFiles)
	}
}

// TestComputeFactListConfidencePlanStep：计划步读 step-<N>-reply.md
// （不是顶层 reply.md）解析成果文件，置信度照常算。
func TestComputeFactListConfidencePlanStep(t *testing.T) {
	d, _, dir := testDaemon(t)
	d.dataDir = dir
	taskID := "t-conf-plan"
	start := time.Now()
	claimDir := t.TempDir()
	f1 := writeClaimedFile(t, claimDir, "out.txt")
	if _, err := d.led.RecordExecution(ledger.ExecutionRecord{
		SessionID: "test-session", TaskID: taskID, ToolID: "cap-ops-python",
		Command: "write file", ExitCode: 0, Status: "success"}); err != nil {
		t.Fatal(err)
	}
	writeConfidenceReply(t, dir, taskID, 1,
		"干了什么：写了文件\n结果是什么：写好了\n成果在哪里："+f1+"\n用了哪些工具：cap-ops-python\n")
	disp := &dispatch.Dispatch{ID: "dcplan", TaskID: taskID, TaskType: "execute", PlanStep: true, StepNo: 1}
	fl := d.computeFactList(disp, taskID, start, 0, false)
	if fl.Confidence != 100 || fl.ClaimedFiles != 1 || fl.VerifiedFiles != 1 {
		t.Fatalf("计划步置信度应为 100%%（claimed 1 verified 1），实际 %d%% claimed=%d verified=%d missing=%v",
			fl.Confidence, fl.ClaimedFiles, fl.VerifiedFiles, fl.MissingFiles)
	}
}

// TestBuildReceiptExcludesPerCallDumps：回执产物列表里不列每次调用的
// 输出底稿（step-<N>.txt），只列 step-<N>-reply.md、reply.md 与真文件；
// 底稿文件本身留在盘上备查。
func TestBuildReceiptExcludesPerCallDumps(t *testing.T) {
	d, _, dir := testDaemon(t)
	d.dataDir = dir
	taskID := "t-b2"
	artDir := filepath.Join(dir, "artifacts", taskID)
	if err := os.MkdirAll(artDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"step-1.txt", "step-2.txt", "step-1-reply.md", "reply.md", "real-output.md"} {
		if err := os.WriteFile(filepath.Join(artDir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	disp := &dispatch.Dispatch{ID: "db2", TaskID: taskID, TaskType: "execute"}
	r := d.buildReceipt(disp, "done")
	for _, p := range r.Artifacts {
		base := filepath.Base(p)
		if base == "step-1.txt" || base == "step-2.txt" {
			t.Fatalf("回执产物不应含输出底稿 %s，实际：%v", base, r.Artifacts)
		}
	}
	for _, want := range []string{"step-1-reply.md", "reply.md", "real-output.md"} {
		found := false
		for _, p := range r.Artifacts {
			if filepath.Base(p) == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("回执产物应含 %s，实际：%v", want, r.Artifacts)
		}
	}
	// 底稿文件本身还在盘上（只是回执里不列）。
	for _, name := range []string{"step-1.txt", "step-2.txt"} {
		if _, err := os.Stat(filepath.Join(artDir, name)); err != nil {
			t.Fatalf("底稿 %s 应留在盘上备查，实际不存在：%v", name, err)
		}
	}
}

// TestParseClaimedReceiptFiles：解析回执“成果在哪里”栏列出的绝对路径。
func TestParseClaimedReceiptFiles(t *testing.T) {
	// 四栏格式，路径在“成果在哪里”栏里。
	got := parseClaimedReceiptFiles("干了什么：写了文件\n结果是什么：写好了\n成果在哪里：/tmp/a.txt、/tmp/b.md\n用了哪些工具：cap-ops-python\n")
	if len(got) != 2 || got[0] != "/tmp/a.txt" || got[1] != "/tmp/b.md" {
		t.Fatalf("应解析出 [/tmp/a.txt /tmp/b.md]，实际 %v", got)
	}
	// 没有“成果在哪里”栏 → 空。
	if got := parseClaimedReceiptFiles("干了什么：调研\n结果是什么：查清楚了\n"); len(got) != 0 {
		t.Fatalf("没有成果栏时应为空，实际 %v", got)
	}
	// “成果在哪里：无” → 空。
	if got := parseClaimedReceiptFiles("成果在哪里：无\n"); len(got) != 0 {
		t.Fatalf("成果栏写“无”时应为空，实际 %v", got)
	}
	// URL 里的路径不算文件路径。
	got = parseClaimedReceiptFiles("成果在哪里：参考 https://example.com/path/to/page，文件在 /tmp/c.txt\n")
	if len(got) != 1 || got[0] != "/tmp/c.txt" {
		t.Fatalf("URL 不应被当成文件路径，应只解析出 [/tmp/c.txt]，实际 %v", got)
	}
	// 重复路径去重。
	got = parseClaimedReceiptFiles("成果在哪里：/tmp/d.txt、/tmp/d.txt\n")
	if len(got) != 1 || got[0] != "/tmp/d.txt" {
		t.Fatalf("重复路径应去重为 [/tmp/d.txt]，实际 %v", got)
	}
	// “成果在哪里”栏之后的“用了哪些工具”栏里的路径不算。
	got = parseClaimedReceiptFiles("成果在哪里：/tmp/e.txt\n用了哪些工具：cap-ops-python（脚本 /tmp/f.py）\n")
	if len(got) != 1 || got[0] != "/tmp/e.txt" {
		t.Fatalf("只应解析成果栏里的路径，应为 [/tmp/e.txt]，实际 %v", got)
	}
}

// TestDispatchMemorySummaryHeader：执行端摘要标题不再有“不要编造”压制，
// 只写“相关记忆摘要（前置已查）”；空碎片返回空串。
func TestDispatchMemorySummaryHeader(t *testing.T) {
	d := &dispatch.Dispatch{Memory: []string{"【W-0001】CF 注册流程：用 Camoufox 过 Cloudflare 注册验证。"}}
	s := dispatchMemorySummary(d)
	if strings.Contains(s, "不要编造") {
		t.Fatalf("执行端摘要标题不应含“不要编造”压制：%q", s)
	}
	if !strings.Contains(s, "相关记忆摘要（前置已查）：") {
		t.Fatalf("应含新标题“相关记忆摘要（前置已查）：”：%q", s)
	}
	if !strings.Contains(s, "【W-0001】") {
		t.Fatalf("应含记忆碎片：%q", s)
	}
	if got := dispatchMemorySummary(&dispatch.Dispatch{}); got != "" {
		t.Fatalf("空碎片应返回空串，实际 %q", got)
	}
}

// writeDispatchFile 在 dir/dispatch/ 写一份派单文件（任务开始时间
// 来源测试用）：文件名 <taskID>-<纳秒>-<序号>.json，内容只留
// task_id 与 created_at（earliestDispatchTime 只读这两字段）。
func writeDispatchFile(t *testing.T, dir, taskID string, createdAt time.Time, seq int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "dispatch"), 0o755); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("%s-%d-%d.json", taskID, createdAt.UnixNano(), seq)
	body := fmt.Sprintf(`{"id":%q,"task_id":%q,"created_at":%q}`,
		taskID+"-x", taskID, createdAt.UTC().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(dir, "dispatch", name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestTaskStartTimeFromDispatchFile：任务开始时间取该任务最早一份
// 派单文件的 CreatedAt；同前缀的别的任务、回执文件不算；取不到为零值。
func TestTaskStartTimeFromDispatchFile(t *testing.T) {
	d, _, dir := testDaemon(t)
	d.dataDir = dir
	taskID := "t-start-1"
	// 第二步派单（晚）先写、第一步派单（早）后写——取最早的。
	writeDispatchFile(t, dir, taskID, time.Date(2026, 11, 26, 8, 5, 25, 0, time.UTC), 2)
	writeDispatchFile(t, dir, taskID, time.Date(2026, 11, 26, 7, 57, 32, 0, time.UTC), 1)
	// 同前缀的别的任务（t-start-10）与回执文件不应被算进来。
	writeDispatchFile(t, dir, taskID+"0", time.Date(2026, 11, 26, 6, 0, 0, 0, time.UTC), 1)
	receipt := filepath.Join(dir, "dispatch", taskID+"-1795679852601362113-1.receipt.json")
	if err := os.WriteFile(receipt, []byte(`{"task_id":`+`"`+taskID+`"`+`,"created_at":"2026-11-26T05:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := d.taskStartTime(taskID)
	want := time.Date(2026, 11, 26, 7, 57, 32, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("任务开始时间应为 %v（北京时间 15:57:32），实际 %v", want, got)
	}
	// 没有派单文件的任务取不到，返回零值。
	if got := d.taskStartTime("t-start-none"); !got.IsZero() {
		t.Fatalf("无派单文件的任务开始时间应为零值，实际 %v", got)
	}
}

// TestClassifyMissingFile：缺失原因四类（2026-11-26 用户定）。
func TestClassifyMissingFile(t *testing.T) {
	claimDir := t.TempDir()
	now := time.Now()
	taskStart := now.Add(-time.Hour)
	// 早一步做的：存在、非空，mtime 在任务开始之后（30 分钟前）。
	fEarly := writeClaimedFile(t, claimDir, "early.txt")
	if err := os.Chtimes(fEarly, now.Add(-30*time.Minute), now.Add(-30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// 以前的旧文件：存在、非空，mtime 在任务开始之前（2 小时前）。
	fOld := writeClaimedFile(t, claimDir, "old.txt")
	if err := os.Chtimes(fOld, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// 是空的：存在但内容为空。
	fEmpty := filepath.Join(claimDir, "empty.txt")
	if err := os.WriteFile(fEmpty, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	// 只有空白的也算空。
	fBlank := filepath.Join(claimDir, "blank.txt")
	if err := os.WriteFile(fBlank, []byte("  \n\t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 目录冒充文件也算不存在。
	fDir := filepath.Join(claimDir, "somedir")
	if err := os.Mkdir(fDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		path string
		task time.Time
		want int
	}{
		{"早一步做的", fEarly, taskStart, dispatch.MissingReasonEarlyTask},
		{"以前的旧文件", fOld, taskStart, dispatch.MissingReasonOld},
		{"不存在", filepath.Join(claimDir, "nope.txt"), taskStart, dispatch.MissingReasonNonexistent},
		{"是空的", fEmpty, taskStart, dispatch.MissingReasonEmpty},
		{"只有空白算空", fBlank, taskStart, dispatch.MissingReasonEmpty},
		{"目录算不存在", fDir, taskStart, dispatch.MissingReasonNonexistent},
		// 任务开始时间取不到时按 fail-closed 判旧文件。
		{"任务开始未知判旧文件", fEarly, time.Time{}, dispatch.MissingReasonOld},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyMissingFile(tc.path, tc.task); got != tc.want {
				t.Fatalf("分类应为 %d，实际 %d", tc.want, got)
			}
		})
	}
}

// TestComputeFactListMissingReasonBreakdown：端到端——回执声明 4 个文件，
// 1 个这单里早一步做的、1 个以前的旧文件、1 个不存在、1 个是空的
// → 四类计数各 1，Summary 第 4 行聚合写清，置信度算法本身不动
// （工具 50% + 成果 0% = 50%）。
func TestComputeFactListMissingReasonBreakdown(t *testing.T) {
	d, _, dir := testDaemon(t)
	d.dataDir = dir
	taskID := "t-conf-reason"
	now := time.Now()
	taskStart := now.Add(-time.Hour).Truncate(time.Second) // CreatedAt 是秒级 RFC3339，截到秒再比
	// 任务开始时间：第一步派单文件的 CreatedAt（北京时间 = UTC+8）。
	writeDispatchFile(t, dir, taskID, taskStart, 1)
	start := now // 本轮派单时间（接单时间）
	claimDir := t.TempDir()
	fEarly := writeClaimedFile(t, claimDir, "early.txt")
	if err := os.Chtimes(fEarly, now.Add(-30*time.Minute), now.Add(-30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	fOld := writeClaimedFile(t, claimDir, "old.txt")
	if err := os.Chtimes(fOld, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	fMissing := filepath.Join(claimDir, "nope.txt")
	fEmpty := filepath.Join(claimDir, "empty.txt")
	if err := os.WriteFile(fEmpty, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.led.RecordExecution(ledger.ExecutionRecord{
		SessionID: "test-session", TaskID: taskID, ToolID: "cap-ops-python",
		Command: "write files", ExitCode: 0, Status: "success"}); err != nil {
		t.Fatal(err)
	}
	body := "干了什么：写了 4 个文件\n结果是什么：只写好 0 个\n成果在哪里：\n" +
		strings.Join([]string{fEarly, fOld, fMissing, fEmpty}, "\n") + "\n用了哪些工具：cap-ops-python\n"
	writeConfidenceReply(t, dir, taskID, 0, body)
	disp := &dispatch.Dispatch{ID: "dcreason", TaskID: taskID, TaskType: "execute"}
	fl := d.computeFactList(disp, taskID, start, 0, false)
	if fl.ClaimedFiles != 4 || fl.VerifiedFiles != 0 || len(fl.MissingFiles) != 4 {
		t.Fatalf("claimed 应 4、verified 应 0、missing 应 4，实际 %d/%d/%v", fl.ClaimedFiles, fl.VerifiedFiles, fl.MissingFiles)
	}
	if fl.MissingEarlyTask != 1 || fl.MissingOld != 1 || fl.MissingNonexistent != 1 || fl.MissingEmpty != 1 {
		t.Fatalf("四类计数应各 1，实际早一步做=%d 旧文件=%d 不存在=%d 空=%d",
			fl.MissingEarlyTask, fl.MissingOld, fl.MissingNonexistent, fl.MissingEmpty)
	}
	// 置信度算法不动：工具 50% + 成果 0% = 50%。
	if fl.Confidence != 50 || fl.ConfidenceToolPoints != 50 || fl.ConfidenceFilePoints != 0 {
		t.Fatalf("置信度应为 50%%（工具 50%% + 成果 0%%），实际 %d%%（工具 %d%% + 成果 %d%%）",
			fl.Confidence, fl.ConfidenceToolPoints, fl.ConfidenceFilePoints)
	}
	// 任务开始时间取自派单文件。
	if !fl.TaskStartTime.Equal(taskStart.UTC()) {
		t.Fatalf("任务开始时间应为 %v，实际 %v", taskStart.UTC(), fl.TaskStartTime)
	}
	sum := fl.Summary()
	wantLine := "成果文件核对结果（回执 4/实际 0/缺失 4）：缺失的 4 个里，1 个是这单里早一步做的，1 个是以前的旧文件，1 个不存在，1 个是空的，所以判为 成果文件缺失！"
	if !strings.Contains(sum, wantLine) {
		t.Fatalf("Summary 第 4 行应为聚合原因，实际：\n%s\n期望含：\n%s", sum, wantLine)
	}
	wantStart := "任务开始时间：" + taskStart.UTC().Add(8*time.Hour).Format("2006-01-02 15:04:05")
	if !strings.Contains(sum, wantStart) {
		t.Fatalf("Summary 第 5 行应为任务开始时间 %q，实际：\n%s", wantStart, sum)
	}
	if !strings.Contains(sum, "注意命令：以上事实信息仅供参考。如果置信度低于 100%，必须先读这步的详细报告并核对成果文件，确认后再判！") {
		t.Fatalf("Summary 第 6 行应为新提醒句，实际：\n%s", sum)
	}
	// 第 4 行绝不出现单个文件名。
	for _, p := range []string{fEarly, fOld, fMissing, fEmpty} {
		if strings.Contains(sum, p) {
			t.Fatalf("Summary 不应出现文件名 %s，实际：\n%s", p, sum)
		}
	}
}
