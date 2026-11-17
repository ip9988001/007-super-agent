// Package gateway 是 007 超级 Agent 的入口网关。
//
// 设计依据：
//   - 审计实测（no2-brain.md §1.6、no3-comms-failure-swarm.md §1.2）：
//     旧 lx 回执文件名 <毫秒时间戳>-<随机3字节>.json；派单即返回铁律——
//     30 秒内估时、派完立刻返回等唤醒，不轮询、不重派；串行唤醒同 task_id
//     60 秒合并、首封 180 秒强制唤醒防饿死。
//   - 设计方案 v1.5 第 4 轮：三水位进 ledger.db queue_watermarks，
//     未确认告警 = seen>acked 且最久未确认 >1800 秒。
//   - 新 Agent 是单机：网关收信后立即回执（received + task_id + 时间戳），
//     执行由内部 worker 串行跑（MAX_PARALLEL=1），回执落盘到数据目录。
package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"007-super-agent/internal/config"
	"007-super-agent/internal/ledger"
)

// Envelope 是入站任务信封（WakeEnvelope 的最小字段集）。
type Envelope struct {
	TaskID string `json:"task_id"`
	Text   string `json:"text"`   // 用户原话
	Source string `json:"source"` // 来源，如 muse-link
	TS     string `json:"ts"`     // RFC3339 UTC
}

// Receipt 是立即返回的回执（不等待执行结果）。
type Receipt struct {
	Received string `json:"received"` // RFC3339 UTC
	TaskID   string `json:"task_id"`
	TS       string `json:"ts"`
	Merged   bool   `json:"merged,omitempty"`
}

// Watermark 是队列三水位快照（seen/acked/最久未确认）。
type Watermark struct {
	Seen           int64
	Acked          int64
	OldestUnackedS int64
	UnackedAlert   bool
}

type pendingItem struct {
	env       Envelope
	firstSeen time.Time
	merged    int // 合并进来的同 task_id 信数（不含首封）
}

// Gateway 是串行入口网关：HTTP 收信 → 合并/排队 → worker 串行处理。
type Gateway struct {
	cfg     *config.Config
	led     *ledger.Ledger
	handler func(Envelope)

	mu      sync.Mutex
	pending map[string]*pendingItem
	order   []string // 待处理 task_id 的 FIFO 顺序
	seen    int64
	acked   int64

	stop chan struct{}
	done chan struct{}
}

// New 建网关。handler 由 worker 串行调用，同一时间只跑一个。
func New(cfg *config.Config, led *ledger.Ledger, handler func(Envelope)) *Gateway {
	return &Gateway{
		cfg: cfg, led: led, handler: handler,
		pending: map[string]*pendingItem{},
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
}

// Start 启动 worker 循环。
func (g *Gateway) Start() { go g.loop() }

// Stop 停 worker，等当前处理完。
func (g *Gateway) Stop() {
	close(g.stop)
	<-g.done
}

// ServeHTTP 只接 POST /wake，其余 404/405。
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/wake" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	var env Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if env.TaskID == "" || env.Text == "" {
		http.Error(w, "task_id and text are required", http.StatusBadRequest)
		return
	}
	if env.TS == "" {
		env.TS = time.Now().UTC().Format(time.RFC3339)
	}
	merged, full := g.enqueue(env)
	if full {
		g.recordWatermark(true)
		http.Error(w, "queue full", http.StatusServiceUnavailable)
		return
	}
	rec := Receipt{Received: time.Now().UTC().Format(time.RFC3339), TaskID: env.TaskID, TS: env.TS, Merged: merged}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rec)
}

// enqueue 入队：同 task_id 在合并窗口内则合并，否则新排一条。
// 返回 merged 与队列是否已满。
func (g *Gateway) enqueue(env Envelope) (merged, full bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.seen++
	if it, ok := g.pending[env.TaskID]; ok && time.Since(it.firstSeen) < g.cfg.MergeWindow {
		it.merged++
		return true, false
	}
	if len(g.order) >= g.cfg.MaxPending {
		return false, true
	}
	g.pending[env.TaskID] = &pendingItem{env: env, firstSeen: time.Now()}
	g.order = append(g.order, env.TaskID)
	return false, false
}

// loop 是 worker：每 50ms 扫一次，按规则取一条串行处理。
func (g *Gateway) loop() {
	defer close(g.done)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-g.stop:
			return
		case <-tick.C:
			if it := g.pick(); it != nil {
				g.process(it)
			}
		}
	}
}

// pick 取一条待处理：先取等超 ForcedWake 的最老一条（防饿死插队），
// 再取等超 MergeWindow 的最老一条；都不满足返回 nil。
func (g *Gateway) pick() *pendingItem {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	for _, id := range g.order {
		if it := g.pending[id]; it != nil && now.Sub(it.firstSeen) >= g.cfg.ForcedWake {
			return g.take(id)
		}
	}
	for _, id := range g.order {
		if it := g.pending[id]; it != nil && now.Sub(it.firstSeen) >= g.cfg.MergeWindow {
			return g.take(id)
		}
	}
	return nil
}

// take 把一条从待处理里摘出来（调用方须持有锁）。
func (g *Gateway) take(id string) *pendingItem {
	it := g.pending[id]
	delete(g.pending, id)
	for i, oid := range g.order {
		if oid == id {
			g.order = append(g.order[:i], g.order[i+1:]...)
			break
		}
	}
	return it
}

// process 串行跑一条：调 handler → 写回执文件 → 记 acked 与水位。
func (g *Gateway) process(it *pendingItem) {
	func() {
		defer func() { _ = recover() }() // handler  panic 不杀 worker
		g.handler(it.env)
	}()
	now := time.Now().UTC()
	rec := map[string]any{
		"received": now.Format(time.RFC3339),
		"task_id":  it.env.TaskID,
		"ts":       it.env.TS,
		"merged":   it.merged,
	}
	dir := filepath.Join(g.cfg.ResolveDataDir(), "receipts")
	_ = os.MkdirAll(dir, 0700)
	var rb [3]byte
	_, _ = rand.Read(rb[:])
	name := fmt.Sprintf("%d-%s.json", now.UnixMilli(), hex.EncodeToString(rb[:]))
	if b, err := json.Marshal(rec); err == nil {
		_ = os.WriteFile(filepath.Join(dir, name), b, 0600)
	}
	g.mu.Lock()
	g.acked++
	g.mu.Unlock()
	g.recordWatermark(false)
}

// Watermark 返回当前三水位（seen/acked/最久未确认秒数/是否告警）。
func (g *Gateway) Watermark() Watermark {
	g.mu.Lock()
	defer g.mu.Unlock()
	w := Watermark{Seen: g.seen, Acked: g.acked}
	var oldest time.Duration
	for _, it := range g.pending {
		if age := time.Since(it.firstSeen); age > oldest {
			oldest = age
		}
	}
	w.OldestUnackedS = int64(oldest / time.Second)
	// 第 4 轮口径：seen>acked 且最久未确认 >1800 秒才告警。
	w.UnackedAlert = w.Seen > w.Acked && w.OldestUnackedS > 1800
	return w
}

// recordWatermark 把当前水位写进 ledger.db queue_watermarks（失败只记，不阻拦干活）。
func (g *Gateway) recordWatermark(forceAlert bool) {
	if g.led == nil {
		return
	}
	w := g.Watermark()
	if forceAlert {
		w.UnackedAlert = true
	}
	_, _ = g.led.RecordWatermark(ledger.WatermarkRecord{
		Box: "gateway", SeenSeq: w.Seen, AckedSeq: w.Acked,
		OldestUnackedAgeS: w.OldestUnackedS, UnackedAlert: w.UnackedAlert,
	})
}
