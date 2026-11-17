// Package gateway 是 007 超级 Agent 的入口网关。
//
// 设计依据：
//   - 审计实测（no2-brain.md §1.6、no3-comms-failure-swarm.md §1.2）：
//     旧 lx 回执文件名 <毫秒时间戳>-<随机3字节>.json；派单即返回铁律——
//     30 秒内估时、派完立刻返回等唤醒，不轮询、不重派；串行唤醒同 task_id
//     60 秒合并、首封 180 秒强制唤醒防饿死。
//   - 设计方案 v1.5 第 4 轮：三水位进 ledger.db queue_watermarks，
//     未确认告警 = seen>acked 且最久未确认 >1800 秒。
//   - 第 14 步 B-01：孤信只等 3 秒防抖，不再干等 60 秒合并窗口；
//     同 task_id 有追加信时仍等 60 秒合并窗口（防抖期重启的重放直接放行）。
//   - 第 14 步 B-02：收信先落盘（ledger.db inbox_queue）再回执，
//     进程崩溃也不丢任务；启动时重放 PENDING/RUNNING 行；
//     (task_id, inbound_seq) 唯一索引防重放（seq>0 时）。
//   - 第 14 步 B-08：双车道——Fast（并发 2）+ Heavy（并发 1）。
//     任务先进 Fast 车道跑；跑超 30 秒（与 executor.InlineThreshold 同值）
//     释放 Fast 槽位转 Heavy 车道记账，短任务不再被长活堵住。
//     车道只做并发记账，不暂停任务本身。
//   - 本仓库没有共享的 ContextBuilder 消息体：llm.BuildMessages 每次
//     新建消息切片、无共享可写状态；真正的共享可写状态是 memory.Store
//     （已有 mu 串行化）、ledger（已有 mu）与本包的 pending/firstSeen
//     （g.mu）——并发写锁就落在这三处，不另造锁。
package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"007-super-agent/internal/config"
	"007-super-agent/internal/executor"
	"007-super-agent/internal/ledger"
)

// staleThreshold 是未确认重排的阈值（第 14 步 B-06）：
// RUNNING 行超此时长未确认且 retry_count==0，自动重排 1 次。
// 与 watchdog.CheckQueue 的 1800 秒告警口径同值。
const staleThreshold = 1800 * time.Second

// sweepInterval 是水位扫荡周期：重排超期 RUNNING 行 + 补捞内存里没有的
// PENDING 行（崩溃重放的边角）。
const sweepInterval = 60 * time.Second

// Envelope 是入站任务信封（WakeEnvelope 的最小字段集）。
type Envelope struct {
	TaskID string `json:"task_id"`
	Text   string `json:"text"`          // 用户原话
	Source string `json:"source"`        // 来源，如 muse-link
	TS     string `json:"ts"`            // RFC3339 UTC
	Seq    int64  `json:"seq,omitempty"` // 发信方给的入站序号；0 表示没给，不做去重
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
	merged    int   // 合并进来的同 task_id 信数（不含首封）
	mergeOpen bool  // 首封后又来了同 task_id 追加信，正在合并窗口内
	seq       int64 // 入站序号（0 表示没给）
	rowID     int64 // inbox_queue 行 id（0 表示没落盘，如 led 为 nil）
	running   bool  // 是否已把 inbox 行标过 RUNNING（放回重取时不再重复标）
}

// Gateway 是双车道入口网关：HTTP 收信 → 落盘/合并/排队 → 双车道处理。
type Gateway struct {
	cfg     *config.Config
	led     *ledger.Ledger
	handler func(Envelope)

	mu      sync.Mutex
	pending map[string]*pendingItem
	order   []string // 待处理 task_id 的 FIFO 顺序
	seen    int64
	acked   int64

	fastSem  chan struct{} // Fast 车道信号量，容量 2
	heavySem chan struct{} // Heavy 车道信号量，容量 1
	inflight sync.WaitGroup

	// laneMigration 是 Fast→Heavy 的迁移阈值（默认 executor.InlineThreshold
	// 30 秒）。SetLaneMigration 只供测试调小阈值，生产不动它。
	laneMigration time.Duration

	sessionID string

	stop chan struct{}
	done chan struct{}
}

// New 建网关。handler 由双车道 worker 并发调用，调用方须保证 handler
// 自身并发安全（memory.Store、ledger、executor 均已串行化）。
func New(cfg *config.Config, led *ledger.Ledger, handler func(Envelope)) *Gateway {
	return &Gateway{
		cfg: cfg, led: led, handler: handler,
		pending:  map[string]*pendingItem{},
		fastSem:  make(chan struct{}, 2),
		heavySem: make(chan struct{}, 1),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// SetSession 给 inbox_queue 的记账行打上会话标识（main.go 在 Start 前调）。
func (g *Gateway) SetSession(sessionID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sessionID = sessionID
}

// debounce 返回首封防抖时长（配置缺失时默认 3 秒）。
func (g *Gateway) debounce() time.Duration {
	if g.cfg.Debounce > 0 {
		return g.cfg.Debounce
	}
	return 3 * time.Second
}

// SetLaneMigration 调 Fast→Heavy 的迁移阈值（只供测试用，生产不动它）。
func (g *Gateway) SetLaneMigration(d time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.laneMigration = d
}

// laneMigrationOrDefault 返回迁移阈值（未设置时用 executor.InlineThreshold）。
func (g *Gateway) laneMigrationOrDefault() time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.laneMigration > 0 {
		return g.laneMigration
	}
	return executor.InlineThreshold
}

// Start 启动 worker 循环。
func (g *Gateway) Start() { go g.loop() }

// Stop 停 worker，等当前处理完（含在途的双车道任务）。
func (g *Gateway) Stop() {
	close(g.stop)
	<-g.done
	g.inflight.Wait()
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
	merged, full, err := g.enqueue(env)
	if err != nil {
		// 落盘失败：不发回执，调用方重试（回执先于落盘是 B-02 的原罪）。
		log.Printf("gateway enqueue task=%s: 落盘失败: %v", env.TaskID, err)
		http.Error(w, "queue persist failed", http.StatusInternalServerError)
		return
	}
	if full {
		g.recordWatermark(true)
		http.Error(w, "queue full", http.StatusServiceUnavailable)
		return
	}
	rec := Receipt{Received: time.Now().UTC().Format(time.RFC3339), TaskID: env.TaskID, TS: env.TS, Merged: merged}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rec)
}

// enqueue 入队：先看内存合并，再落盘，最后进内存队列。
// 合并的追加信不单独落盘（随首封行重放；崩溃会丢追加信的文本，
// 与旧合并语义一致——合并本来就只留首封原话）。
// 返回 merged、队列是否已满、落盘错误。
func (g *Gateway) enqueue(env Envelope) (merged, full bool, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.seen++
	// 1. 内存合并：同 task_id 在合并窗口内则合并，不再落盘。
	if it, ok := g.pending[env.TaskID]; ok && time.Since(it.firstSeen) < g.cfg.MergeWindow {
		it.merged++
		it.mergeOpen = true
		return true, false, nil
	}
	// 2. 容量检查放落盘之前：满了直接 503，不写脏行。
	if len(g.order) >= g.cfg.MaxPending {
		return false, true, nil
	}
	// 3. 先落盘再回执（B-02）：seq>0 的重复投递返回 merged，不建新行。
	var rowID int64
	if g.led != nil {
		id, inserted, derr := g.led.EnqueueInbox(ledger.InboxRecord{
			SessionID: g.sessionID, TaskID: env.TaskID, Seq: env.Seq,
			Source: env.Source, Text: env.Text,
		})
		if derr != nil {
			return false, false, derr
		}
		if !inserted {
			return true, false, nil
		}
		rowID = id
	}
	// 4. 进内存队列。
	g.pending[env.TaskID] = &pendingItem{env: env, firstSeen: time.Now(), seq: env.Seq, rowID: rowID}
	g.order = append(g.order, env.TaskID)
	return false, false, nil
}

// loop 是 worker 循环：每 50ms 扫一次取一条派给车道；
// 每 60 秒扫荡一次水位（重排超期 RUNNING 行 + 补捞漏网的 PENDING 行）。
func (g *Gateway) loop() {
	defer close(g.done)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	sweep := time.NewTicker(sweepInterval)
	defer sweep.Stop()
	for {
		select {
		case <-g.stop:
			return
		case <-tick.C:
			if it := g.pick(); it != nil {
				g.tryDispatch(it)
			}
		case <-sweep.C:
			g.sweepStale()
		}
	}
}

// pick 取一条待处理（调用方须持有锁的场景外，本函数自己加锁）。
// 优先级：等超 ForcedWake 的最老一条（防饿死插队）→
// 正在合并且等超 MergeWindow 的最老一条 →
// 无合并追加、等超防抖（默认 3 秒）的最老一条；都不满足返回 nil。
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
		if it := g.pending[id]; it != nil && it.mergeOpen && now.Sub(it.firstSeen) >= g.cfg.MergeWindow {
			return g.take(id)
		}
	}
	debounce := g.debounce()
	for _, id := range g.order {
		if it := g.pending[id]; it != nil && !it.mergeOpen && now.Sub(it.firstSeen) >= debounce {
			return g.take(id)
		}
	}
	return nil
}

// take 把一条从待处理里摘出来（调用方须持有锁），并把它的 inbox 行标 RUNNING。
func (g *Gateway) take(id string) *pendingItem {
	it := g.pending[id]
	delete(g.pending, id)
	for i, oid := range g.order {
		if oid == id {
			g.order = append(g.order[:i], g.order[i+1:]...)
			break
		}
	}
	if it != nil && g.led != nil && it.rowID > 0 && !it.running {
		if err := g.led.MarkInboxRunning(it.rowID); err != nil {
			log.Printf("gateway take task=%s: 标 RUNNING 失败: %v", it.env.TaskID, err)
		}
		it.running = true
	}
	return it
}

// putBack 把一条放回队首（Fast 车道满了派不出去时用，下轮再试）。
func (g *Gateway) putBack(it *pendingItem) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.pending[it.env.TaskID]; ok {
		// 已有同 task 项（取走后又有新信入队）：丢弃旧条。
		// 旧条的 inbox 行已被 take() 标过 RUNNING（不是 PENDING），
		// 不处理会幽灵 RUNNING 30 分钟直到水位扫荡才重排；这里直接
		// 标 FAILED（任务内容由新的同 task 信继续承载），不留幽灵行。
		if g.led != nil && it.rowID > 0 {
			_ = g.led.MarkInboxFailed(it.rowID)
		}
		return
	}
	g.pending[it.env.TaskID] = it
	g.order = append([]string{it.env.TaskID}, g.order...)
}

// tryDispatch 派一条给 Fast 车道：有空位立刻起协程跑，
// 没空位放回队首等下轮（不阻塞 worker 循环）。
func (g *Gateway) tryDispatch(it *pendingItem) {
	select {
	case g.fastSem <- struct{}{}:
		g.inflight.Add(1)
		go g.processLane(it)
	default:
		g.putBack(it)
	}
}

// processLane 在车道里跑一条：先在 Fast 车道跑 handler；
// 超过 executor.InlineThreshold（30 秒）仍未结束时，释放 Fast 槽位、
// 转 Heavy 车道记账——短任务不再被长活堵住。车道只做并发记账，
// 不暂停任务本身；handler 崩溃只记，不杀 worker。
func (g *Gateway) processLane(it *pendingItem) {
	defer g.inflight.Done()
	done := make(chan struct{})
	var panicked bool
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				panicked = true
				log.Printf("gateway handler panic task=%s: %v", it.env.TaskID, r)
			}
		}()
		g.handler(it.env)
	}()
	timer := time.NewTimer(g.laneMigrationOrDefault())
	defer timer.Stop()
	select {
	case <-done:
		// Fast 路径完成：释放 Fast 槽位。漏放是致命 bug：两条任务各占
		// 一个槽位后永不释放，从第三条起 tryDispatch 永远走 putBack，
		// 循环里反复取回又放回，队列彻底堵死（2026-11-17 实测）。
		<-g.fastSem
		g.finish(it, panicked)
		return
	case <-timer.C:
		// 长任务：释放 Fast 槽位，转 Heavy 车道记账。
		<-g.fastSem
		select {
		case g.heavySem <- struct{}{}:
			<-done
			<-g.heavySem
		case <-done:
		}
		g.finish(it, panicked)
		return
	}
}

// finish 收尾：写回执文件 → 记 acked 与水位 → inbox 行标 DONE/FAILED。
func (g *Gateway) finish(it *pendingItem, panicked bool) {
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
	if g.led != nil && it.rowID > 0 {
		var err error
		if panicked {
			err = g.led.MarkInboxFailed(it.rowID)
		} else {
			err = g.led.MarkInboxDone(it.rowID)
		}
		if err != nil {
			// “不是 RUNNING 行”是预期竞态：水位重排的副本已先确认，
			// 原始执行晚到一步，不记为错误。
			if strings.Contains(err.Error(), "不是 RUNNING 行") {
				log.Printf("gateway finish task=%s: inbox 行已由重排副本确认，忽略", it.env.TaskID)
			} else {
				log.Printf("gateway finish task=%s: 标 DONE/FAILED 失败: %v", it.env.TaskID, err)
			}
		}
	}
}

// enqueueReplay 把一行 inbox 行重新推进内存队列（启动重放 / 水位重排用）。
// firstSeen 设到防抖之前，跳过剩余防抖时间直接放行（B-01×B-02）。
// 同 task 已有内存项时只记 merged，不重复建项（那行留在 PENDING，
// 60 秒扫荡会补捞）。
func (g *Gateway) enqueueReplay(r ledger.InboxRow) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if it, ok := g.pending[r.TaskID]; ok {
		it.merged++
		return
	}
	env := Envelope{TaskID: r.TaskID, Text: r.Text, Source: r.Source, TS: r.TS, Seq: r.Seq}
	g.pending[r.TaskID] = &pendingItem{
		env: env, firstSeen: time.Now().Add(-g.debounce()),
		seq: r.Seq, rowID: r.ID,
	}
	g.order = append(g.order, r.TaskID)
	g.seen++
}

// ReplayInbox 启动重放：把 ledger.db 里的 PENDING 行与 RUNNING 行
// （崩溃前取走未确认的）重新推进内存队列。RUNNING 行先重排回 PENDING
// （retry_count+1）。须在 Start 之前调。返回重放的条数。
func (g *Gateway) ReplayInbox() (int, error) {
	if g.led == nil {
		return 0, nil
	}
	pending, err := g.led.ListInboxByStatus(ledger.InboxPending)
	if err != nil {
		return 0, fmt.Errorf("gateway replay list PENDING: %w", err)
	}
	running, err := g.led.ListInboxByStatus(ledger.InboxRunning)
	if err != nil {
		return 0, fmt.Errorf("gateway replay list RUNNING: %w", err)
	}
	n := 0
	for _, r := range running {
		if err := g.led.RequeueInbox(r.ID); err != nil {
			log.Printf("gateway replay task=%s: 重排 RUNNING 行失败: %v", r.TaskID, err)
			continue
		}
		r.Status = ledger.InboxPending
		r.RetryCount++
		g.enqueueReplay(r)
		n++
	}
	for _, r := range pending {
		g.enqueueReplay(r)
		n++
	}
	return n, nil
}

// sweepStale 每 60 秒扫荡水位（第 14 步 B-06）：
// (a) 超 1800 秒未确认且 retry_count==0 的 RUNNING 行自动重排 1 次并记账；
// (b) 补捞内存里没有的 PENDING 行（崩溃重放的边角、合并丢行后的自愈）。
func (g *Gateway) sweepStale() {
	if g.led == nil {
		return
	}
	stale, err := g.led.ListStaleInbox(staleThreshold)
	if err != nil {
		log.Printf("gateway sweep stale: %v", err)
	} else {
		for _, r := range stale {
			if err := g.led.RequeueInbox(r.ID); err != nil {
				log.Printf("gateway sweep task=%s: 重排失败: %v", r.TaskID, err)
				continue
			}
			r.Status = ledger.InboxPending
			r.RetryCount++
			g.enqueueReplay(r)
			g.recordRequeue(r)
		}
	}
	pending, err := g.led.ListInboxByStatus(ledger.InboxPending)
	if err != nil {
		log.Printf("gateway sweep pending: %v", err)
		return
	}
	for _, r := range pending {
		g.mu.Lock()
		_, inMem := g.pending[r.TaskID]
		g.mu.Unlock()
		if !inMem {
			g.enqueueReplay(r)
		}
	}
}

// recordRequeue 把一次水位自动重排写进 execution_ledger。
func (g *Gateway) recordRequeue(r ledger.InboxRow) {
	if g.led == nil {
		return
	}
	_, err := g.led.RecordExecution(ledger.ExecutionRecord{
		SessionID: g.sessionID,
		TaskID:    r.TaskID,
		ToolID:    "watchdog-requeue",
		Command:   "requeue stale inbox",
		ExitCode:  0,
		Status:    "success",
		ErrHead: fmt.Sprintf("task=%s seq=%d 1800 秒未确认，自动重排 1 次（retry_count 0→1）",
			r.TaskID, r.Seq),
	})
	if err != nil {
		log.Printf("gateway requeue record task=%s: %v", r.TaskID, err)
	}
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
