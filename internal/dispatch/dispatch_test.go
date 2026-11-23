package dispatch

import (
	"context"
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
