package agent

import (
	"testing"

	"007-super-agent/internal/forge"
	"007-super-agent/internal/ledger"
)

func newGapTestAgent(t *testing.T) (*Agent, *forge.Store) {
	t.Helper()
	ag := newTestAgent(t, stubSeq([]string{`{"action":"answer","text":"done"}`}))
	store := &forge.Store{DataDir: t.TempDir()}
	ag.Forge = store
	return ag, store
}

func expandHistory(n int) []stepRecord {
	h := []stepRecord{}
	for i := 0; i < n; i++ {
		h = append(h, stepRecord{ToolID: "expand", Inputs: []string{"fetch", "browser"}, ExitCode: 0, Status: "success", Output: "ok"})
	}
	return h
}

func TestDetectExpandGap(t *testing.T) {
	ag, store := newGapTestAgent(t)
	reported := false
	ag.detectExpandGap("task-1", "把这份文档转成克林贡语", expandHistory(3), &reported)
	if !reported {
		t.Fatalf("连续 3 次展开应报缺口")
	}
	gaps, err := store.ListGaps("")
	if err != nil || len(gaps) != 1 {
		t.Fatalf("应写 1 条缺口，得到 %d 条 err=%v", len(gaps), err)
	}
	if gaps[0].Trigger != "no_tool" {
		t.Fatalf("触发应为 no_tool，得到 %s", gaps[0].Trigger)
	}
	// 每任务只报一次：再调不新增。
	ag.detectExpandGap("task-1", "把这份文档转成克林贡语", expandHistory(4), &reported)
	gaps, _ = store.ListGaps("")
	if len(gaps) != 1 {
		t.Fatalf("每任务只报一次，实际 %d 条", len(gaps))
	}
	// 不足 3 次不报。
	ag2, store2 := newGapTestAgent(t)
	reported2 := false
	ag2.detectExpandGap("task-2", "x", expandHistory(2), &reported2)
	if reported2 {
		t.Fatalf("连续 2 次展开不应报缺口")
	}
	gaps2, _ := store2.ListGaps("")
	if len(gaps2) != 0 {
		t.Fatalf("不足 3 次应 0 条缺口，实际 %d 条", len(gaps2))
	}
}

func TestDetectWorkaroundGap(t *testing.T) {
	ag, store := newGapTestAgent(t)
	// 3 个工具、其中 2 个失败过 → 凑合完成，报缺口。
	h := []stepRecord{
		{ToolID: "test-echo", ExitCode: 0, Status: "success"},
		{ToolID: "test-true", ExitCode: 1, Status: "failure"},
		{ToolID: "test-false", ExitCode: 1, Status: "failure"},
	}
	ag.detectWorkaroundGap("task-1", "做一件需要绕弯的事", h)
	gaps, _ := store.ListGaps("")
	if len(gaps) != 1 || gaps[0].Trigger != "workaround" {
		t.Fatalf("应报 1 条 workaround 缺口，得到 %+v", gaps)
	}
	// 只有 1 个勉强匹配 → 不报（宁可漏报）。
	ag2, store2 := newGapTestAgent(t)
	h2 := []stepRecord{
		{ToolID: "test-echo", ExitCode: 0, Status: "success"},
		{ToolID: "test-true", ExitCode: 0, Status: "success"},
		{ToolID: "test-false", ExitCode: 1, Status: "failure"},
	}
	ag2.detectWorkaroundGap("task-2", "做一件需要绕弯的事", h2)
	gaps2, _ := store2.ListGaps("")
	if len(gaps2) != 0 {
		t.Fatalf("只有 1 个勉强匹配时不应报缺口，实际 %d 条", len(gaps2))
	}
}

func TestDetectUnsupportedGap(t *testing.T) {
	ag, store := newGapTestAgent(t)
	// 账本里记 3 次 test-echo 的“不支持”失败。
	for i := 0; i < 3; i++ {
		if _, err := ag.Led.RecordExecution(ledger.ExecutionRecord{
			SessionID: "test-session", TaskID: "task-1", ToolID: "test-echo",
			Command: "test-echo x", ExitCode: 1, Status: "failure",
			ErrHead: "not supported: this operation is not supported",
		}); err != nil {
			t.Fatalf("RecordExecution: %v", err)
		}
	}
	h := []stepRecord{{ToolID: "test-echo", ExitCode: 1, Status: "failure"}}
	ag.detectUnsupportedGap("task-1", h)
	gaps, _ := store.ListGaps("")
	if len(gaps) != 1 || gaps[0].Trigger != "tool_unsupported" {
		t.Fatalf("应报 1 条 tool_unsupported 缺口，得到 %+v", gaps)
	}
	if gaps[0].Evidence["tool_id"] != "test-echo" {
		t.Fatalf("证据应含 tool_id=test-echo，得到 %v", gaps[0].Evidence)
	}
	// 同一工具 7 天内只报一次（去重）：再调不新增。
	ag.detectUnsupportedGap("task-2", h)
	gaps, _ = store.ListGaps("")
	if len(gaps) != 1 {
		t.Fatalf("同一工具 7 天内只报一次，实际 %d 条", len(gaps))
	}
	// 不足 3 次不报。
	ag2, store2 := newGapTestAgent(t)
	for i := 0; i < 2; i++ {
		if _, err := ag2.Led.RecordExecution(ledger.ExecutionRecord{
			SessionID: "test-session", TaskID: "task-9", ToolID: "test-true",
			Command: "test-true x", ExitCode: 1, Status: "failure",
			ErrHead: "unsupported",
		}); err != nil {
			t.Fatalf("RecordExecution: %v", err)
		}
	}
	ag2.detectUnsupportedGap("task-9", []stepRecord{{ToolID: "test-true", ExitCode: 1, Status: "failure"}})
	gaps2, _ := store2.ListGaps("")
	if len(gaps2) != 0 {
		t.Fatalf("不足 3 次应 0 条缺口，实际 %d 条", len(gaps2))
	}
}
