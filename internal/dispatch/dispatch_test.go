package dispatch

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestSendAndReceipt 派单落盘 + 回写入队 + 回执落盘与交还。
func TestSendAndReceipt(t *testing.T) {
	dir := t.TempDir()
	ch := New(dir)
	ctx := context.Background()
	d := &Dispatch{
		ID: ch.NewID("task-1"), TaskID: "task-1", Route: "AGENT",
		UserText: "做一件小事", Goal: "完成", Criteria: "完成即验收",
		EstSteps: 2, EstSeconds: 60,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		ReplyCh:   make(chan *Receipt, 1),
	}
	if err := ch.Send(ctx, d); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// 队列里应能取出同一份派单。
	select {
	case got := <-ch.Queue():
		if got.ID != d.ID || got.TaskID != "task-1" {
			t.Fatalf("取出的派单不对：%+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("队列里取不到派单")
	}
	// 回执落盘 + 交还。
	r := &Receipt{
		DispatchID: d.ID, TaskID: "task-1", Status: "done",
		Conclusion: "做完了", CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := ch.WriteReceipt(d, r); err != nil {
		t.Fatalf("WriteReceipt: %v", err)
	}
	select {
	case got := <-d.ReplyCh:
		if got.Status != "done" || got.Conclusion != "做完了" {
			t.Fatalf("交还的回执不对：%+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("ReplyCh 里取不到回执")
	}
	// 已有回执的派单不再算待办。
	pending, err := ch.PendingDispatches()
	if err != nil {
		t.Fatalf("PendingDispatches: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("已有回执还列为待办：%d 份", len(pending))
	}
}

// TestPendingDispatches 未回执的派单列为待办（重启重入队用）。
func TestPendingDispatches(t *testing.T) {
	dir := t.TempDir()
	ch := New(dir)
	ctx := context.Background()
	d := &Dispatch{
		ID: ch.NewID("task-2"), TaskID: "task-2", Route: "TOOL",
		UserText: "跑一个工具", Goal: "跑完", EstSteps: 1, EstSeconds: 30,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := ch.Send(ctx, d); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// 取出队列里的（模拟 worker 已取走，但还没回执）。
	<-ch.Queue()
	pending, err := ch.PendingDispatches()
	if err != nil {
		t.Fatalf("PendingDispatches: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != d.ID {
		t.Fatalf("待办应为 1 份（%s），得到 %d 份", d.ID, len(pending))
	}
}

// TestWorkerRunsDispatch Worker 取派单、调执行回调、写回执。
func TestWorkerRunsDispatch(t *testing.T) {
	dir := t.TempDir()
	ch := New(dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	executed := make(chan string, 1)
	go Worker(ctx, ch, func(_ context.Context, d *Dispatch) (*Receipt, error) {
		executed <- d.ID
		return &Receipt{
			DispatchID: d.ID, TaskID: d.TaskID, Status: "done",
			Conclusion: "执行完了", CreatedAt: time.Now().UTC().Format(time.RFC3339),
		}, nil
	})
	d := &Dispatch{
		ID: ch.NewID("task-3"), TaskID: "task-3", Route: "AGENT",
		UserText: "做事", Goal: "做完", EstSteps: 1, EstSeconds: 30,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		ReplyCh:   make(chan *Receipt, 1),
	}
	if err := ch.Send(ctx, d); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case id := <-executed:
		if id != d.ID {
			t.Fatalf("执行了错误的派单：%s", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Worker 未执行派单")
	}
	select {
	case r := <-d.ReplyCh:
		if r.Status != "done" {
			t.Fatalf("回执状态不对：%s", r.Status)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("未收到回执")
	}
}

// TestFactListSummaryConfidenceFormat：FactList.Summary() 是六行摘要
// （2026-11-26 用户定，两次改版）：置信度 / 工具调用 / 回执内成果文件 /
// 成果文件核对结果（只报各类个数，不报单个文件名）/ 任务开始时间
// （北京时间）/ 注意句。没声明成果文件时第 4 行省略（共五行）。
func TestFactListSummaryConfidenceFormat(t *testing.T) {
	taskStart := time.Date(2026, 11, 26, 7, 57, 32, 0, time.UTC) // 北京时间 15:57:32
	fl := &FactList{
		Confidence: 90, ConfidenceToolPoints: 50, ConfidenceFilePoints: 40,
		SuccessfulCalls: 41, ToolCallBreakdown: "cap-ops-python×41",
		ClaimedFiles: 10, VerifiedFiles: 8,
		MissingFiles:  []string{"/tmp/m1.txt", "/tmp/m2.txt"},
		TaskStartTime: taskStart, MissingEarlyTask: 2,
	}
	want := "结果置信度：90%（工具 50% + 成果 40%）\n" +
		"工具调用：41 次（cap-ops-python×41）\n" +
		"回执内成果文件：10 个\n" +
		"成果文件核对结果（回执 10/实际 8/缺失 2）：缺失的 2 个都是这单里早一步做的，所以判为 成果文件缺失！\n" +
		"任务开始时间：2026-11-26 15:57:32\n" +
		"注意命令：以上事实信息仅供参考。如果置信度低于 100%，必须先读这步的详细报告并核对成果文件，确认后再判！"
	if got := fl.Summary(); got != want {
		t.Fatalf("Summary 六行格式不对：\n实际：\n%s\n期望：\n%s", got, want)
	}
	// 没声明成果文件：第 4 行省略（共五行）。
	fl2 := &FactList{
		Confidence: 100, ConfidenceToolPoints: 100, ConfidenceFilePoints: 0,
		SuccessfulCalls: 3, ToolCallBreakdown: "test-echo×3",
		ClaimedFiles: 0, VerifiedFiles: 0, TaskStartTime: taskStart,
	}
	want2 := "结果置信度：100%（工具 100% + 成果 0%）\n" +
		"工具调用：3 次（test-echo×3）\n" +
		"回执内成果文件：0 个（回执没声明成果文件）\n" +
		"任务开始时间：2026-11-26 15:57:32\n" +
		"注意命令：以上事实信息仅供参考。如果置信度低于 100%，必须先读这步的详细报告并核对成果文件，确认后再判！"
	if got := fl2.Summary(); got != want2 {
		t.Fatalf("没声明文件时 Summary 五行格式不对：\n实际：\n%s\n期望：\n%s", got, want2)
	}
	// FactList 为 nil 时不崩。
	if got := (*FactList)(nil).Summary(); got != "事实清单缺失（执行层未算）" {
		t.Fatalf("nil FactList 的 Summary 应为缺失提示，实际：%q", got)
	}
}

// TestFactListSummaryReminderLine：Summary() 第 6 行是提醒前置核实的
// 注意句，原样是用户定的那句（2026-11-26）：“注意命令：……必须……”。
func TestFactListSummaryReminderLine(t *testing.T) {
	fl := &FactList{
		Confidence: 90, ConfidenceToolPoints: 50, ConfidenceFilePoints: 40,
		SuccessfulCalls: 41, ToolCallBreakdown: "cap-ops-python×41",
		ClaimedFiles: 10, VerifiedFiles: 8,
		MissingFiles:     []string{"/tmp/m1.txt", "/tmp/m2.txt"},
		TaskStartTime:    time.Date(2026, 11, 26, 7, 57, 32, 0, time.UTC),
		MissingEarlyTask: 2,
	}
	want := "注意命令：以上事实信息仅供参考。如果置信度低于 100%，必须先读这步的详细报告并核对成果文件，确认后再判！"
	lines := strings.Split(fl.Summary(), "\n")
	if len(lines) != 6 {
		t.Fatalf("Summary 应为 6 行，实际 %d 行：\n%s", len(lines), fl.Summary())
	}
	if lines[5] != want {
		t.Fatalf("第 6 行提醒不对：\n实际：%q\n期望：%q", lines[5], want)
	}
}

// TestFactListSummaryVerificationReasonCases：第 4 行“成果文件核对结果”
// 的各种写法（2026-11-26 用户定）：只报各类个数，不报单个文件名；
// 全过 / 缺失全属一类 / 缺失分属多类 / 没声明文件时省略该行。
func TestFactListSummaryVerificationReasonCases(t *testing.T) {
	taskStart := time.Date(2026, 11, 26, 7, 57, 32, 0, time.UTC) // 北京时间 15:57:32
	cases := []struct {
		name                           string
		claimed, verified              int
		missing                        []string
		early, old, nonexistent, empty int
		want                           string // 第 4 行全文；为空表示该行应省略
	}{
		{"全过", 2, 2, nil, 0, 0, 0, 0,
			"成果文件核对结果（回执 2/实际 2/缺失 0）：有文件存在，最后修改/操作时间在本轮派单之后，所以判为 有 文件！"},
		{"缺失全是早一步做的", 2, 0, []string{"/tmp/a", "/tmp/b"}, 2, 0, 0, 0,
			"成果文件核对结果（回执 2/实际 0/缺失 2）：缺失的 2 个都是这单里早一步做的，所以判为 成果文件缺失！"},
		{"缺失全是以前的旧文件", 2, 0, []string{"/tmp/a", "/tmp/b"}, 0, 2, 0, 0,
			"成果文件核对结果（回执 2/实际 0/缺失 2）：缺失的 2 个都是以前的旧文件，所以判为 成果文件缺失！"},
		{"缺失全是不存在", 2, 0, []string{"/tmp/a", "/tmp/b"}, 0, 0, 2, 0,
			"成果文件核对结果（回执 2/实际 0/缺失 2）：缺失的 2 个都不存在，所以判为 成果文件缺失！"},
		{"缺失全是空的", 2, 0, []string{"/tmp/a", "/tmp/b"}, 0, 0, 0, 2,
			"成果文件核对结果（回执 2/实际 0/缺失 2）：缺失的 2 个都是空的，所以判为 成果文件缺失！"},
		{"缺失混合旧文件加不存在", 4, 2, []string{"/tmp/a", "/tmp/b"}, 0, 1, 1, 0,
			"成果文件核对结果（回执 4/实际 2/缺失 2）：缺失的 2 个里，1 个是以前的旧文件，1 个不存在，所以判为 成果文件缺失！"},
		{"缺失混合三类", 5, 2, []string{"/tmp/a", "/tmp/b", "/tmp/c"}, 1, 1, 0, 1,
			"成果文件核对结果（回执 5/实际 2/缺失 3）：缺失的 3 个里，1 个是这单里早一步做的，1 个是以前的旧文件，1 个是空的，所以判为 成果文件缺失！"},
		{"没声明文件省略该行", 0, 0, nil, 0, 0, 0, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fl := &FactList{
				Confidence: 90, ConfidenceToolPoints: 50, ConfidenceFilePoints: 40,
				SuccessfulCalls: 5, ToolCallBreakdown: "cap-ops-python×5",
				ClaimedFiles: tc.claimed, VerifiedFiles: tc.verified,
				MissingFiles: tc.missing, TaskStartTime: taskStart,
				MissingEarlyTask: tc.early, MissingOld: tc.old,
				MissingNonexistent: tc.nonexistent, MissingEmpty: tc.empty,
			}
			sum := fl.Summary()
			lines := strings.Split(sum, "\n")
			if tc.want == "" {
				if strings.Contains(sum, "成果文件核对结果") {
					t.Fatalf("没声明文件时不应有核对结果行，实际：\n%s", sum)
				}
				if len(lines) != 5 {
					t.Fatalf("没声明文件时 Summary 应为 5 行，实际 %d 行：\n%s", len(lines), sum)
				}
				return
			}
			if len(lines) != 6 {
				t.Fatalf("Summary 应为 6 行，实际 %d 行：\n%s", len(lines), sum)
			}
			if lines[3] != tc.want {
				t.Fatalf("第 4 行不对：\n实际：%q\n期望：%q", lines[3], tc.want)
			}
			// 第 4 行绝不出现单个文件名。
			for _, p := range tc.missing {
				if strings.Contains(lines[3], p) {
					t.Fatalf("第 4 行不应出现文件名 %s，实际：%q", p, lines[3])
				}
			}
			// 第 5 行任务开始时间（北京时间）。
			if lines[4] != "任务开始时间：2026-11-26 15:57:32" {
				t.Fatalf("第 5 行任务开始时间不对：%q", lines[4])
			}
		})
	}
	// 任务开始时间取不到时写“未知”，不编一个假时间。
	fl := &FactList{
		Confidence: 100, ConfidenceToolPoints: 50, ConfidenceFilePoints: 50,
		SuccessfulCalls: 1, ToolCallBreakdown: "cap-ops-python×1",
		ClaimedFiles: 1, VerifiedFiles: 1,
	}
	lines := strings.Split(fl.Summary(), "\n")
	if lines[4] != "任务开始时间：未知" {
		t.Fatalf("任务开始时间未知时第 5 行应写“未知”，实际：%q", lines[4])
	}
}

// TestDispatchMustReadMemoriesRoundTrip：must_read_memories 字段经 JSON
// 落盘/回读不丢（前置→执行端的通道靠它）。
func TestDispatchMustReadMemoriesRoundTrip(t *testing.T) {
	d := &Dispatch{TaskID: "t-1", Goal: "注册 CF", MustReadMemories: []string{"W-0001"}}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back Dispatch
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(back.MustReadMemories) != 1 || back.MustReadMemories[0] != "W-0001" {
		t.Fatalf("must_read_memories 应为 [W-0001]，实际 %v", back.MustReadMemories)
	}
}
