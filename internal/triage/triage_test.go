package triage

import (
	"database/sql"
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
	return func(prompt string) (string, int64, int64, error) {
		return reply, 100, 20, nil
	}
}

func TestTier0PrefixHit(t *testing.T) {
	rg := loadReg(t)
	cases := []struct {
		text   string
		toolID string
	}{
		{"/audit 审一下这段代码", "cap-code-semgrep"},
		{"/rea 逆向这个二进制", "cap-code-rea"},
		{"/audit审一下", "cap-code-semgrep"}, // 前缀后直接跟中文也命中
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
	p := BuildTriagePrompt("帮我查一下今天天气怎么样")
	for _, want := range []string{"DIRECT|TOOL|META", `{"route"`, "帮我查一下今天天气怎么样"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if strings.Contains(p, "cap-") {
		t.Errorf("plain prompt must not leak tool ids")
	}
	pn := BuildTriagePromptNarrowed("/seo 优化", []string{"cap-seo-llms-txt", "cap-seo-claude-seo"})
	for _, want := range []string{"cap-seo-llms-txt", "cap-seo-claude-seo", "只许从中挑"} {
		if !strings.Contains(pn, want) {
			t.Errorf("narrowed prompt missing %q", want)
		}
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
