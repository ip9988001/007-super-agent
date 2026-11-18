package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"007-super-agent/internal/executor"
	"007-super-agent/internal/ledger"
	"007-super-agent/internal/registry"
)

// miniRegistry 造一个最小总账：test-echo / test-true / test-false 三个可执行工具。
func miniRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	dst := t.TempDir()
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
	// L3：每个条目一个同名 md（checkL3Correspondence 要求 1:1）。
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

// stubSeq 按顺序返回多份决定（用完后重复最后一份）。
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

func newTestAgent(t *testing.T, call ModelCallFunc) *Agent {
	t.Helper()
	rg := miniRegistry(t)
	led := openTestLedger(t)
	ex := &executor.Executor{Ledger: led}
	return &Agent{Call: call, Reg: rg, Led: led, Ex: ex, DataDir: t.TempDir(), SessionID: "test-session", MaxIter: 20}
}

func TestParseAction(t *testing.T) {
	if a, err := parseAction(`{"action":"tool","tool_id":"test-echo","inputs":["hi"]}`); err != nil || a.ToolID != "test-echo" {
		t.Fatalf("tool 解析失败: %v %+v", err, a)
	}
	if a, err := parseAction(`{"action":"answer","text":"好了"}`); err != nil || a.Text != "好了" {
		t.Fatalf("answer 解析失败: %v %+v", err, a)
	}
	// 模型常在 JSON 外包解释文字：取最外层对象。
	if a, err := parseAction(`先调工具：{"action":"tool","tool_id":"test-true","inputs":[]} 就这样`); err != nil || a.ToolID != "test-true" {
		t.Fatalf("包解释文字的 JSON 应能解析: %v %+v", err, a)
	}
	if _, err := parseAction(`{"action":"tool"}`); err == nil {
		t.Fatalf("缺 tool_id 应报错")
	}
	if _, err := parseAction(`不是 JSON`); err == nil {
		t.Fatalf("非法 JSON 应报错")
	}
	if _, err := parseAction(`{"action":"wait"}`); err == nil {
		t.Fatalf("未知 action 应报错")
	}
}

func TestRunToolThenAnswer(t *testing.T) {
	ag := newTestAgent(t, stubSeq([]string{
		`{"action":"tool","tool_id":"test-echo","inputs":["hello"]}`,
		`{"action":"answer","text":"echo 说了 hello"}`,
	}))
	res, err := ag.Run(context.Background(), "t1", "让 echo 说 hello", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "done" || res.CompletedSteps != 1 {
		t.Fatalf("应 done 且 1 步成功，实为 status=%s steps=%d", res.Status, res.CompletedSteps)
	}
	if !strings.Contains(res.ReplyDraft, "hello") {
		t.Fatalf("回复应含 hello，实为 %q", res.ReplyDraft)
	}
	if b, err := os.ReadFile(filepath.Join(ag.DataDir, "artifacts", "t1", "step-1.txt")); err != nil || !strings.Contains(string(b), "hello") {
		t.Fatalf("step-1.txt 应含 hello：%v %q", err, string(b))
	}
}

func TestRunResolveErrorThenRecover(t *testing.T) {
	ag := newTestAgent(t, stubSeq([]string{
		`{"action":"tool","tool_id":"test-nope","inputs":[]}`,
		`{"action":"tool","tool_id":"test-echo","inputs":["ok"]}`,
		`{"action":"answer","text":"换工具后成功"}`,
	}))
	res, err := ag.Run(context.Background(), "t2", "先调个不存在的工具再调 echo", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "done" || res.CompletedSteps != 1 {
		t.Fatalf("应 done 且 1 步成功（坏工具不计），实为 status=%s steps=%d", res.Status, res.CompletedSteps)
	}
}

func TestRunMaxIterations(t *testing.T) {
	ag := newTestAgent(t, stubSeq([]string{
		`{"action":"tool","tool_id":"test-echo","inputs":["x"]}`,
	}))
	ag.MaxIter = 2
	res, err := ag.Run(context.Background(), "t3", "一直调工具不交活", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "max_iterations" || res.CompletedSteps != 2 {
		t.Fatalf("应 max_iterations 且 2 步，实为 status=%s steps=%d", res.Status, res.CompletedSteps)
	}
}

func TestRunNoOutputCountsAsFailure(t *testing.T) {
	ag := newTestAgent(t, stubSeq([]string{
		`{"action":"tool","tool_id":"test-true","inputs":[]}`,
		`{"action":"answer","text":"true 没输出"}`,
	}))
	res, err := ag.Run(context.Background(), "t4", "调 true（exit 0 但无输出）", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "done" || res.CompletedSteps != 0 {
		t.Fatalf("无输出的成功不应计入完成步数，实为 status=%s steps=%d", res.Status, res.CompletedSteps)
	}
}
