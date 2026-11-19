package triage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"007-super-agent/internal/ledger"
	"007-super-agent/internal/registry"

	_ "modernc.org/sqlite"
)

// fixture 把 step1 的 registry.json + index/ 原样拷进临时数据目录。
func fixture(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	if err := copyDir(filepath.Join("..", "..", "step1", "index"), filepath.Join(dst, "index")); err != nil {
		t.Fatalf("copy index fixture: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dst, "registry"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(filepath.Join("..", "..", "step1", "registry", "registry.json"),
		filepath.Join(dst, "registry", "registry.json")); err != nil {
		t.Fatalf("copy registry fixture: %v", err)
	}
	return dst
}

func copyDir(src, dst string) error {
	ents, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, e := range ents {
		s, d := filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := copyDir(s, d); err != nil {
				return err
			}
		} else if err := copyFile(s, d); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func loadReg(t *testing.T) *registry.Registry {
	t.Helper()
	rg, err := registry.Load(fixture(t))
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	return rg
}

func openLedger(t *testing.T) *ledger.Ledger {
	t.Helper()
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	t.Cleanup(func() { _ = ledger.Close(led) })
	return led
}

// stubModel 返回固定回复的 Tier-1 桩。
func stubModel(reply string) ModelCallFunc {
	return func(taskID, frozen, tail string) (string, int64, int64, error) {
		return reply, 100, 20, nil
	}
}

func TestTier0PrefixHit(t *testing.T) {
	rg := loadReg(t)
	cases := []struct {
		text   string
		toolID string
	}{
		{"/audit 审一下这段代码", "cap-code-run-audit"},
		{"/rea 逆向这个二进制", "cap-code-rea"},
		{"/audit审一下", "cap-code-run-audit"}, // 前缀后直接跟中文也命中
	}
	for _, c := range cases {
		r := Tier0(rg, Input{Text: c.text})
		if !r.Matched || r.ToolID != c.toolID {
			t.Errorf("Tier0(%q) = matched=%v tool=%q, want tool %q", c.text, r.Matched, r.ToolID, c.toolID)
		}
	}
	// "/auditX" 后跟 ASCII 字母不算命中。
	if r := Tier0(rg, Input{Text: "/auditX 东西"}); r.Matched {
		t.Errorf("Tier0(/auditX) should not match, got %+v", r)
	}
	// "/seo" 多候选：命中但不定工具，只收窄。
	r := Tier0(rg, Input{Text: "/seo 优化一下"})
	if !r.Matched || r.ToolID != "" || len(r.Candidates) != 6 {
		t.Errorf("Tier0(/seo) = %+v, want matched with 6 candidates, no tool", r)
	}
	// 纯闲聊无前缀：未命中。
	if r := Tier0(rg, Input{Text: "今天天气怎么样"}); r.Matched {
		t.Errorf("Tier0(plain text) should not match, got %+v", r)
	}
	// 定时任务预注册工具直出；预注册工具不存在时落到 Tier-1（不硬定）。
	if r := Tier0(rg, Input{Scheduled: true, ScheduledToolID: "cap-code-semgrep"}); !r.Matched || r.ToolID != "cap-code-semgrep" || !r.Scheduled {
		t.Errorf("Tier0(scheduled) = %+v, want direct cap-code-semgrep", r)
	}
	if r := Tier0(rg, Input{Scheduled: true, ScheduledToolID: "no-such-tool"}); r.Matched {
		t.Errorf("Tier0(scheduled unknown tool) should not match, got %+v", r)
	}
}

func TestBuildTriagePrompt(t *testing.T) {
	rg := loadReg(t)
	p := BuildTriagePrompt(rg, "帮我查一下今天天气怎么样")
	for _, want := range []string{"DIRECT|TOOL|META|AGENT", `{"route"`, "帮我查一下今天天气怎么样"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	// 第 13 步：提示词必须带可用工具清单（模型只能从清单里挑，
	// 清单外的一律非法），不再是光秃秃的提示词头。
	for _, want := range []string{"可用工具清单", "只许从中挑", "cap-code-semgrep", "cap-fetch-curl-jq"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	// available:false 的不进清单。
	if strings.Contains(p, "cap-memory-playbooks") {
		t.Errorf("prompt must not list unavailable tool cap-memory-playbooks")
	}
	// 知识条目（doc/sop/tool/mcp/data）不进清单：只能读、不能跑，
	// 模型没机会把它当步骤派去执行（2026-11-18 ping0 测试教训）。
	for _, notWant := range []string{"cap-memory-recall-memory", "cap-seo-design-md", "cap-seo-llms-txt"} {
		if strings.Contains(p, notWant) {
			t.Errorf("prompt must not list non-executable tool %s", notWant)
		}
	}
	pn := BuildTriagePromptNarrowed(rg, "/seo 优化", []string{"cap-seo-llms-txt", "cap-seo-claude-seo"})
	for _, want := range []string{"cap-seo-claude-seo", "只许从中挑"} {
		if !strings.Contains(pn, want) {
			t.Errorf("narrowed prompt missing %q", want)
		}
	}
	// 收窄候选里的知识条目（sop）同样被滤掉。
	if strings.Contains(pn, "cap-seo-llms-txt") {
		t.Errorf("narrowed prompt must not list non-executable tool cap-seo-llms-txt")
	}
	// 收窄清单里不许出现候选集之外的工具。
	if strings.Contains(pn, "cap-code-semgrep") {
		t.Errorf("narrowed prompt must not list tools outside candidates")
	}
}

// TestFrozenListDeterministic：冻结清单跨调用内容固定（顺序、条数一致），
// 否则缓存前缀命中不了；且清单与总账可用条目一一对应。
func TestFrozenListDeterministic(t *testing.T) {
	rg := loadReg(t)
	a := BuildTriageFrozen(rg, nil)
	b := BuildTriageFrozen(rg, nil)
	if a != b {
		t.Fatalf("frozen prompt not deterministic")
	}
	lines := 0
	for _, ln := range strings.Split(a, "\n") {
		if strings.HasPrefix(ln, "- ") {
			lines++
		}
	}
	if lines != 47 { // 84 条总账：6 条 available:false 不进清单
		// （bili/tvly/lean-ctx/llmtrim 4 条 cli + link-selftest 1 条
		// service + playbooks 1 条 doc）；32 条知识条目
		// （doc/sop/tool/mcp/data）不可执行、也不进清单；
		// 清单只剩 39 cli + 8 service = 47 条可执行工具
		// （清单行含 cap-/ext-/skill- 三种前缀，不能只数 "- cap-"）
		t.Errorf("frozen tool list lines = %d, want 47", lines)
	}
	// 冻结前缀 + 尾部分开发送：天气问题走 DIRECT（清单里没有天气工具，
	// 模型不许编 tool_id；这里用桩模拟模型回 DIRECT，验证链路本身）。
	led := openLedger(t)
	d, err := Triage(rg, led, Input{Text: "帮我查一下今天天气怎么样", TaskID: "w1", SessionID: "s1"},
		stubModel(`{"route":"DIRECT","tool_id":"","reason":"无天气工具，直接答"}`))
	if err != nil || d.Route != RouteDirect {
		t.Fatalf("Triage weather = %+v, %v, want DIRECT", d, err)
	}
}

func TestParseTriageReply(t *testing.T) {
	rg := loadReg(t)
	valid := []struct {
		reply string
		route Route
		tool  string
	}{
		{`{"route":"DIRECT","tool_id":"","reason":"常识问答"}`, RouteDirect, ""},
		{`{"route":"META","tool_id":"","reason":"改自身配置"}`, RouteMeta, ""},
		{`{"route":"TOOL","tool_id":"cap-code-semgrep","reason":"要审计"}`, RouteTool, "cap-code-semgrep"},
		{`{"route":"AGENT","tool_id":"","reason":"多步任务走主循环"}`, RouteAgent, ""},
		{"前面多一句废话\n" + `{"route":"DIRECT","tool_id":"","reason":"x"}` + "\n后面也有一句", RouteDirect, ""},
	}
	for _, c := range valid {
		d, err := ParseTriageReply(c.reply, rg)
		if err != nil || d.Route != c.route || d.ToolID != c.tool {
			t.Errorf("ParseTriageReply(%q) = %+v, %v, want route=%s tool=%s", c.reply, d, err, c.route, c.tool)
		}
	}
	invalid := []string{
		`{"route":"MAYBE","tool_id":"","reason":"x"}`,                    // 非法 route
		`{"route":"TOOL","tool_id":"","reason":"x"}`,                     // TOOL 无 tool_id
		`{"route":"TOOL","tool_id":"no-such-tool","reason":"x"}`,         // tool_id 不存在
		`{"route":"TOOL","tool_id":"cap-memory-playbooks","reason":"x"}`, // available:false
		`{"route":"DIRECT","tool_id":"no-such-tool","reason":"x"}`,       // DIRECT 带不存在的 tool_id
		`{"route":"AGENT","tool_id":"no-such-tool","reason":"x"}`,         // AGENT 带不存在的 tool_id
		`没有 JSON`,          // 无 JSON
		`{"route":"TOOL",`, // 残缺 JSON
	}
	for _, reply := range invalid {
		if _, err := ParseTriageReply(reply, rg); err == nil {
			t.Errorf("ParseTriageReply(%q) should fail", reply)
		}
	}
}

func TestResolveTool(t *testing.T) {
	rg := loadReg(t)
	e, l3, err := ResolveTool(rg, "cap-code-semgrep")
	if err != nil || e == nil || e.ToolID != "cap-code-semgrep" || l3 == "" {
		t.Fatalf("ResolveTool(cap-code-semgrep) = %v, %q, %v", e, l3, err)
	}
	if !strings.Contains(l3, "semgrep") {
		t.Errorf("L3 text does not mention semgrep")
	}
	for _, id := range []string{"", "no-such-tool"} {
		if _, _, err := ResolveTool(rg, id); !errors.Is(err, ErrToolNotFound) {
			t.Errorf("ResolveTool(%q) err = %v, want ErrToolNotFound", id, err)
		}
	}
	if _, _, err := ResolveTool(rg, "cap-memory-playbooks"); !errors.Is(err, ErrToolNotAllowed) {
		t.Errorf("ResolveTool(cap-memory-playbooks) err = %v, want ErrToolNotAllowed", err)
	}
}

func TestDirectReadsNoTables(t *testing.T) {
	// Load 后把 index/ 整个删掉：DIRECT 分诊不读任何表，必须照常成功；
	// TOOL 分诊本身只做内存校验也能成功，但随后 ResolveTool 必须报错
	// （证明 Triage 自己没读表、没取 L3）。
	dir := fixture(t)
	rg, err := registry.Load(dir)
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "index")); err != nil {
		t.Fatal(err)
	}
	led := openLedger(t)
	d, err := Triage(rg, led, Input{Text: "帮我查一下今天天气怎么样"}, stubModel(`{"route":"DIRECT","tool_id":"","reason":"常识问答"}`))
	if err != nil || d.Route != RouteDirect || d.Tier != 1 || d.ToolID != "" {
		t.Fatalf("Triage DIRECT after index removed = %+v, %v", d, err)
	}
	d2, err := Triage(rg, led, Input{Text: "审一下"}, stubModel(`{"route":"TOOL","tool_id":"cap-code-semgrep","reason":"审计"}`))
	if err != nil || d2.Route != RouteTool {
		t.Fatalf("Triage TOOL after index removed = %+v, %v", d2, err)
	}
	if _, _, err := ResolveTool(rg, "cap-code-semgrep"); err == nil {
		t.Errorf("ResolveTool should fail after index removed")
	}
}

func TestTriageMetricRecorded(t *testing.T) {
	rg := loadReg(t)
	dir := t.TempDir()
	led, err := ledger.Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close(led)
	if _, err := Triage(rg, led, Input{Text: "/audit 审一下", TaskID: "t1", SessionID: "s1"}, nil); err != nil {
		t.Fatalf("Triage Tier-0: %v", err)
	}
	if _, err := Triage(rg, led, Input{Text: "你好", TaskID: "t2", SessionID: "s1"}, stubModel(`{"route":"DIRECT","tool_id":"","reason":"打招呼"}`)); err != nil {
		t.Fatalf("Triage Tier-1: %v", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM triage_metrics`).Scan(&n); err != nil {
		t.Fatalf("count triage_metrics: %v", err)
	}
	if n != 2 {
		t.Errorf("triage_metrics rows = %d, want 2", n)
	}
	var route string
	var tier int
	if err := db.QueryRow(`SELECT route, tier FROM triage_metrics WHERE task_id='t1'`).Scan(&route, &tier); err != nil {
		t.Fatalf("query t1: %v", err)
	}
	if route != "TOOL" || tier != 0 {
		t.Errorf("t1 row = route %s tier %d, want TOOL/0", route, tier)
	}
}

func TestTriageNoModel(t *testing.T) {
	rg := loadReg(t)
	if _, err := Triage(rg, nil, Input{Text: "你好"}, nil); !errors.Is(err, ErrNoModel) {
		t.Errorf("Triage without model = %v, want ErrNoModel", err)
	}
}

// fixtureWithEntryPoint 把 fixture 里某条工具的 entry_point 改掉
// （verified 保持 false，用于确定性地测探针通过/失败两条路）。
func fixtureWithEntryPoint(t *testing.T, toolID, entryPoint string) string {
	t.Helper()
	dir := fixture(t)
	path := filepath.Join(dir, "registry", "registry.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read registry.json: %v", err)
	}
	var f struct {
		Version   string           `json:"version"`
		Generated string           `json:"generated"`
		Total     int              `json:"total"`
		Entries   []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parse registry.json: %v", err)
	}
	found := false
	for _, e := range f.Entries {
		if e["tool_id"] == toolID {
			e["entry_point"] = entryPoint
			found = true
		}
	}
	if !found {
		t.Fatalf("tool %s not in fixture", toolID)
	}
	out, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal registry.json: %v", err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("write registry.json: %v", err)
	}
	return dir
}

// TestResolveToolProbeFailed 是 B-03 的失败路：verified=false 且
// entry_point 不存在的工具，ResolveTool 必须报 ErrProbeFailed，
// 调用方据此记账，不执行。
func TestResolveToolProbeFailed(t *testing.T) {
	dir := fixtureWithEntryPoint(t, "cap-ops-tesseract", "no-such-cmd-xyz-123")
	rg, err := registry.Load(dir)
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	if _, _, err := ResolveTool(rg, "cap-ops-tesseract"); !errors.Is(err, ErrProbeFailed) {
		t.Fatalf("ResolveTool err = %v, want ErrProbeFailed", err)
	}
}

// TestResolveToolProbePasses 是 B-03 的通过路：verified=false 但
// entry_point 真实存在的工具，探针通过后照常定位，不禁用。
func TestResolveToolProbePasses(t *testing.T) {
	dir := fixtureWithEntryPoint(t, "cap-ops-tesseract", "echo tesseract")
	rg, err := registry.Load(dir)
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	e, l3, err := ResolveTool(rg, "cap-ops-tesseract")
	if err != nil || e == nil || l3 == "" {
		t.Fatalf("ResolveTool = %v, %q, %v, want success after probe", e, l3, err)
	}
	if rg.NeedsProbe("cap-ops-tesseract") {
		t.Fatal("probe passed but NeedsProbe still true")
	}
}

// writeMiniRegistry（与 registry 包的同名 helper 相同口径）：
// 建一个最小总账数据目录，满足 Load 的 1:1 校验。
// 用来测清单过滤，不依赖 step1 真实总账的条目在测试机上装没装。
type miniRegEntry struct {
	id    string
	ep    string
	ct    string
	avail bool
}

func writeMiniRegistry(t *testing.T, ents []miniRegEntry) string {
	t.Helper()
	dst := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dst, "registry"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dst, "index", "L3"), 0o755); err != nil {
		t.Fatal(err)
	}
	type mini struct {
		ToolID         string `json:"tool_id"`
		Category       string `json:"category"`
		Subcategory    string `json:"subcategory"`
		CallType       string `json:"call_type"`
		EntryPoint     string `json:"entry_point"`
		OutputContract string `json:"output_contract"`
		SourceFile     string `json:"source_file"`
		OneLiner       string `json:"one_liner"`
		Available      bool   `json:"available"`
	}
	ms := make([]mini, 0, len(ents))
	for _, e := range ents {
		ms = append(ms, mini{
			ToolID: e.id, Category: "ops", Subcategory: "test",
			CallType: e.ct, EntryPoint: e.ep,
			OutputContract: "text", SourceFile: "test.md",
			OneLiner: "test tool " + e.id, Available: e.avail,
		})
		if err := os.WriteFile(filepath.Join(dst, "index", "L3", e.id+".md"), []byte("# "+e.id+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := json.Marshal(map[string]any{
		"version": "test", "total": len(ms), "entries": ms,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "registry", "registry.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return dst
}

// 选项2：探针失败过的条目不进规划器清单；没探过的照常列。
func TestCompactToolListExcludesProbeFailed(t *testing.T) {
	dir := writeMiniRegistry(t, []miniRegEntry{
		{id: "cap-test-good", ep: "echo", ct: "cli", avail: true},
		{id: "cap-test-bad", ep: "no-such-cmd-xyz-123", ct: "cli", avail: true},
		{id: "cap-test-unprobed", ep: "echo", ct: "cli", avail: true},
	})
	rg, err := registry.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// 探之前：三条都在。
	list := CompactToolList(rg, nil)
	for _, id := range []string{"cap-test-good", "cap-test-bad", "cap-test-unprobed"} {
		if !strings.Contains(list, id) {
			t.Fatalf("list before probe missing %q:\n%s", id, list)
		}
	}
	// 探坏一条：清单里不再有它，没探过的还在。
	if err := rg.Probe("cap-test-bad"); err == nil {
		t.Fatal("Probe(bad): want error, got nil")
	}
	list = CompactToolList(rg, nil)
	if strings.Contains(list, "cap-test-bad") {
		t.Fatalf("list after probe still contains failed entry:\n%s", list)
	}
	for _, id := range []string{"cap-test-good", "cap-test-unprobed"} {
		if !strings.Contains(list, id) {
			t.Fatalf("list after probe missing %q:\n%s", id, list)
		}
	}
	// 候选集路径同样过滤；候选全坏时回落到全量过滤后清单（不含坏条）。
	list = CompactToolList(rg, []string{"cap-test-bad", "cap-test-good"})
	if strings.Contains(list, "cap-test-bad") || !strings.Contains(list, "cap-test-good") {
		t.Fatalf("candidate list wrong:\n%s", list)
	}
	list = CompactToolList(rg, []string{"cap-test-bad"})
	if strings.Contains(list, "cap-test-bad") || !strings.Contains(list, "cap-test-good") {
		t.Fatalf("candidate fallback wrong:\n%s", list)
	}
}
