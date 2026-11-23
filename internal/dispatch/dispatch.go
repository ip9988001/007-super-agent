// Package dispatch 是前置会话与执行会话之间的内部通道（第 1 项）。
//
// 派单（Dispatch）是前置写给执行的唯一信物：用户原话、目标、验收标准、
// 工具编号、步骤骨架、记忆碎片、指针、约束、预估步数与耗时，
// 总量 ≤1000 token（F-08）。执行只按派单干活，不重分诊、不重查记忆。
//
// 通道分两层：
//   - 落盘：每份派单写 data/dispatch/<id>.json，每份回执写
//     data/dispatch/<id>.receipt.json；进程重启后，未回执的派单
//     由 Worker 启动时重入队（不丢单）。
//   - 内存：Send 把派单推进缓冲 channel，Worker  goroutine 取出执行；
//     执行完把回执（Receipt）写回文件，并经 ReplyCh 交还给等它的
//     前置（同步流）；没人等时只落盘，前置下次唤醒再汇报（F-10）。
package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// MaxDispatchTokens 是派单的 token 上限（F-08：≤1000 token）。
const MaxDispatchTokens = 1000

// Dispatch 是前置写给执行的派单（F-09 内容）。
type Dispatch struct {
	ID           string   `json:"id"`            // 派单编号（任务号 + 时间戳，全局唯一）
	TaskID       string   `json:"task_id"`       // 归属任务
	Route        string   `json:"route"`         // TOOL / AGENT / META
	UserText     string   `json:"user_text"`     // 用户原话
	Goal         string   `json:"goal"`          // 目标
	Criteria     string   `json:"criteria"`      // 验收标准
	ToolIDs      []string `json:"tool_ids"`      // 工具编号（已分诊，执行不重分诊）
	PlanSkeleton []string `json:"plan_skeleton"` // 步骤骨架（工具编号 + 顺序 + 指针）
	Memory       []string `json:"memory"`        // 记忆碎片（前置已查到的前 3 条，只贴碎片）
	Pointers     []string `json:"pointers"`      // 指针（路径/链接/记忆 ID，不贴全文）
	Constraints  []string `json:"constraints"`   // 约束
	EstSteps     int      `json:"est_steps"`     // 预估步数
	EstSeconds   int      `json:"est_seconds"`   // 预估耗时（秒）
	ShardNo      int      `json:"shard_no"`      // 分片号（第 15 项：0 表示不分片；>0 表示这是第几片）
	ResumeFrom   string   `json:"resume_from"`   // 续跑起点（上一片的断点指针；第一片为空）
	CreatedAt    string   `json:"created_at"`    // RFC3339 UTC

	// ReplyCh 是执行完交回执的通道（进程内，不落盘）。为 nil 表示没人等，
	// 执行完只写回执文件。缓冲 1，执行方发送不阻塞。
	ReplyCh chan *Receipt `json:"-"`
}

// Receipt 是执行交回的短回执（F-10：成败只看回执）。
type Receipt struct {
	DispatchID    string   `json:"dispatch_id"`
	TaskID        string   `json:"task_id"`
	Status        string   `json:"status"`         // done / stalled / failed / max_iterations
	Conclusion    string   `json:"conclusion"`     // 结论（人话短句，不贴日志）
	Artifacts     []string `json:"artifacts"`      // 产物指针（路径）
	CostCNYMilli  int64    `json:"cost_cny_milli"` // 本单费用，单位 0.001 元
	Steps         int      `json:"steps"`          // 已跑步数
	NextStep      string   `json:"next_step"`      // 下一步建议（可空）
	AdvisorNote   string   `json:"advisor_note"`   // 监工 CONCERN（有则填，无则空）
	ShardNo       int      `json:"shard_no"`       // 本片号（第 15 项：0 表示不分片）
	ResumePointer string   `json:"resume_pointer"` // 续跑指针（第 15 项：下一片从这里接续；本片做完则为空）
	CreatedAt     string   `json:"created_at"`     // RFC3339 UTC
}

// ExecuteFunc 是执行派单的回调（由 main.go 接线：TOOL 走 runTool，
// AGENT/META 走 runAgent）。返回回执；出错时返回 error，Worker 会把
// error 包成 failed 回执落盘。
type ExecuteFunc func(ctx context.Context, d *Dispatch) (*Receipt, error)

// Channel 是派单通道：落盘 + 内存缓冲。
type Channel struct {
	dataDir string
	queue   chan *Dispatch
	mu      sync.Mutex
	seq     int
}

// New 建一个派单通道（缓冲 16）。dataDir 是数据目录（data/dispatch 落盘处）。
func New(dataDir string) *Channel {
	return &Channel{dataDir: dataDir, queue: make(chan *Dispatch, 16)}
}

// Queue 返回 worker 取派单的通道（只读）。
func (c *Channel) Queue() <-chan *Dispatch { return c.queue }

// dispatchFilePath 是某派单的落盘路径。
func (c *Channel) dispatchFilePath(id string) string {
	return filepath.Join(c.dataDir, "dispatch", id+".json")
}

// receiptFilePath 是某派单回执的落盘路径。
func (c *Channel) receiptFilePath(id string) string {
	return filepath.Join(c.dataDir, "dispatch", id+".receipt.json")
}

// NewID 生成一个派单编号：任务号 + 纳秒时间戳，全局唯一。
func (c *Channel) NewID(taskID string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	return fmt.Sprintf("%s-%d-%d", taskID, time.Now().UnixNano(), c.seq)
}

// Send 落盘并入队。ReplyCh 为 nil 表示没人等回执（只落盘）。
// 入队时若缓冲满则阻塞——调用方是前置会话，派单即返回前必须确保
// 派单已进队；缓冲 16 且 worker 常驻，正常不阻塞。
func (c *Channel) Send(ctx context.Context, d *Dispatch) error {
	if strings.TrimSpace(d.ID) == "" {
		return fmt.Errorf("dispatch: 派单缺 id")
	}
	if strings.TrimSpace(d.TaskID) == "" {
		return fmt.Errorf("dispatch: 派单缺 task_id")
	}
	dir := filepath.Join(c.dataDir, "dispatch")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("dispatch: 建 dispatch 目录失败: %w", err)
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("dispatch: 序列化派单失败: %w", err)
	}
	if err := os.WriteFile(c.dispatchFilePath(d.ID), b, 0o600); err != nil {
		return fmt.Errorf("dispatch: 写派单文件失败: %w", err)
	}
	select {
	case c.queue <- d:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("dispatch: 入队被取消: %w", ctx.Err())
	}
}

// WriteReceipt 落盘一份回执，并（ReplyCh 非空时）把回执交还给等它的前置。
// 回执文件与派单文件同目录，文件名 <id>.receipt.json。
func (c *Channel) WriteReceipt(d *Dispatch, r *Receipt) error {
	if r == nil {
		return fmt.Errorf("dispatch: 回执为空")
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("dispatch: 序列化回执失败: %w", err)
	}
	if err := os.WriteFile(c.receiptFilePath(d.ID), b, 0o600); err != nil {
		return fmt.Errorf("dispatch: 写回执文件失败: %w", err)
	}
	if d.ReplyCh != nil {
		select {
		case d.ReplyCh <- r:
		default: // 没人等或已取走，不阻塞
		}
	}
	return nil
}

// PendingDispatches 列出落盘了但还没有回执的派单（供 Worker 启动时重入队）。
func (c *Channel) PendingDispatches() ([]*Dispatch, error) {
	dir := filepath.Join(c.dataDir, "dispatch")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("dispatch: 读 dispatch 目录失败: %w", err)
	}
	var out []*Dispatch
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".receipt.json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		if _, err := os.Stat(c.receiptFilePath(id)); err == nil {
			continue // 已有回执，不重入队
		}
		b, err := os.ReadFile(c.dispatchFilePath(id))
		if err != nil {
			log.Printf("dispatch: 读待重入队派单 %s 失败: %v", id, err)
			continue
		}
		var d Dispatch
		if err := json.Unmarshal(b, &d); err != nil {
			log.Printf("dispatch: 解析待重入队派单 %s 失败: %v", id, err)
			continue
		}
		out = append(out, &d)
	}
	return out, nil
}

// Worker 是执行端的工作循环：从通道取派单，调 ExecuteFunc 执行，
// 把回执落盘并交还。启动时先把落盘未回执的派单重入队（不丢单）。
// ctx 取消时退出（正在执行的派单由 ExecuteFunc 自行处理取消）。
func Worker(ctx context.Context, ch *Channel, exec ExecuteFunc) {
	pending, err := ch.PendingDispatches()
	if err != nil {
		log.Printf("dispatch worker: 列待重入队派单失败: %v", err)
	} else if len(pending) > 0 {
		log.Printf("dispatch worker: 重启重入队 %d 份未回执派单", len(pending))
		for _, d := range pending {
			select {
			case ch.queue <- d:
			case <-ctx.Done():
				return
			}
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case d := <-ch.queue:
			r, err := exec(ctx, d)
			if err != nil {
				log.Printf("dispatch worker: 执行派单 %s 失败: %v", d.ID, err)
				r = &Receipt{
					DispatchID: d.ID, TaskID: d.TaskID, Status: "failed",
					Conclusion: "执行失败：" + err.Error(),
					CreatedAt:  time.Now().UTC().Format(time.RFC3339),
				}
			}
			if r != nil {
				if werr := ch.WriteReceipt(d, r); werr != nil {
					log.Printf("dispatch worker: 写回执 %s 失败: %v", d.ID, werr)
				}
			}
		}
	}
}
