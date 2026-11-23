package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"007-super-agent/internal/dispatch"
	"007-super-agent/internal/executor"
	"007-super-agent/internal/gateway"
	"007-super-agent/internal/ledger"
	"007-super-agent/internal/memory"
	"007-super-agent/internal/registry"
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
