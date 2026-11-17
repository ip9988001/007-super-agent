package gateway

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"007-super-agent/internal/config"
	"007-super-agent/internal/ledger"

	_ "modernc.org/sqlite"
)

type recorder struct {
	mu  sync.Mutex
	got []string
}

func (r *recorder) handle(e Envelope) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, e.TaskID)
}

func (r *recorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.got...)
}

func newTestGateway(t *testing.T, merge, forced time.Duration, maxPend int) (*Gateway, *recorder, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.MergeWindow = merge
	cfg.ForcedWake = forced
	cfg.MaxPending = maxPend
	l, err := ledger.Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	t.Cleanup(func() { _ = ledger.Close(l) })
	rec := &recorder{}
	gw := New(cfg, l, rec.handle)
	gw.Start()
	t.Cleanup(gw.Stop)
	return gw, rec, dir
}

func postEnvelope(t *testing.T, srv *httptest.Server, task, text string) (int, Receipt) {
	t.Helper()
	b, _ := json.Marshal(Envelope{TaskID: task, Text: text, Source: "test", TS: time.Now().UTC().Format(time.RFC3339)})
	resp, err := http.Post(srv.URL+"/wake", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	var rec Receipt
	_ = json.NewDecoder(resp.Body).Decode(&rec)
	return resp.StatusCode, rec
}

func TestValidation(t *testing.T) {
	gw, _, _ := newTestGateway(t, time.Minute, 3*time.Minute, 100)
	srv := httptest.NewServer(gw)
	defer srv.Close()

	if code, _ := postEnvelope(t, srv, "", "hi"); code != http.StatusBadRequest {
		t.Fatalf("empty task_id: got %d, want 400", code)
	}
	if code, _ := postEnvelope(t, srv, "t1", ""); code != http.StatusBadRequest {
		t.Fatalf("empty text: got %d, want 400", code)
	}
	resp, err := http.Post(srv.URL+"/wake", "application/json", bytes.NewReader([]byte("{bad json")))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad json: got %d, want 400", resp.StatusCode)
	}
	resp, err = http.Get(srv.URL + "/wake")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET: got %d, want 405", resp.StatusCode)
	}
}

func TestMergeAndSerial(t *testing.T) {
	gw, rec, dir := newTestGateway(t, 2*time.Second, 5*time.Second, 100)
	srv := httptest.NewServer(gw)
	defer srv.Close()

	if code, r := postEnvelope(t, srv, "t1", "first"); code != 200 || r.Merged {
		t.Fatalf("first t1: code=%d merged=%v", code, r.Merged)
	}
	if code, r := postEnvelope(t, srv, "t1", "second"); code != 200 || !r.Merged {
		t.Fatalf("second t1: code=%d merged=%v, want merged", code, r.Merged)
	}
	if code, r := postEnvelope(t, srv, "t2", "other"); code != 200 || r.Merged {
		t.Fatalf("t2: code=%d merged=%v", code, r.Merged)
	}

	// 合并窗口 2 秒后 worker 串行处理：t1 只处理 1 次，t2 单独 1 次。
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got := rec.list(); len(got) == 2 {
			if got[0] != "t1" || got[1] != "t2" {
				t.Fatalf("processed order = %v, want [t1 t2]", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for processing, got %v", rec.list())
		}
		time.Sleep(50 * time.Millisecond)
	}
	// 回执落盘 2 个文件。
	ents, err := os.ReadDir(filepath.Join(dir, "receipts"))
	if err != nil || len(ents) != 2 {
		t.Fatalf("receipt files = %d, err = %v", len(ents), err)
	}
	w := gw.Watermark()
	if w.Seen != 3 || w.Acked != 2 || w.UnackedAlert {
		t.Fatalf("watermark = %+v, want seen=3 acked=2 alert=false", w)
	}
}

func TestQueueFull(t *testing.T) {
	// 窗口设 30 分钟，处理来不及发生，队列上限 1：第二条直接 503。
	gw, _, _ := newTestGateway(t, 30*time.Minute, 60*time.Minute, 1)
	srv := httptest.NewServer(gw)
	defer srv.Close()
	if code, _ := postEnvelope(t, srv, "a", "x"); code != 200 {
		t.Fatalf("first: got %d, want 200", code)
	}
	if code, _ := postEnvelope(t, srv, "b", "y"); code != http.StatusServiceUnavailable {
		t.Fatalf("second: got %d, want 503", code)
	}
}

func TestReceiptFileNameFormat(t *testing.T) {
	gw, _, dir := newTestGateway(t, time.Second, 2*time.Second, 100)
	srv := httptest.NewServer(gw)
	defer srv.Close()
	if code, _ := postEnvelope(t, srv, "t9", "x"); code != 200 {
		t.Fatalf("post: got %d", code)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		ents, _ := os.ReadDir(filepath.Join(dir, "receipts"))
		if len(ents) == 1 {
			name := ents[0].Name()
			// <毫秒时间戳>-<随机3字节hex>.json
			if len(name) != len("1700000000000-abcdef.json") {
				t.Fatalf("receipt name = %q, want <millis>-<6hex>.json", name)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for receipt file")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// postEnvelopeSeq 发带 seq 的信（第 14 步 B-02 去重用）。
func postEnvelopeSeq(t *testing.T, srv *httptest.Server, task, text string, seq int64) (int, Receipt) {
	t.Helper()
	b, _ := json.Marshal(Envelope{TaskID: task, Text: text, Source: "test", Seq: seq, TS: time.Now().UTC().Format(time.RFC3339)})
	resp, err := http.Post(srv.URL+"/wake", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	var rec Receipt
	_ = json.NewDecoder(resp.Body).Decode(&rec)
	return resp.StatusCode, rec
}

// TestDebounceSingle 是 B-01 的回归：孤信只等防抖（默认 3 秒），
// 不再干等 60 秒合并窗口——5 秒内必须处理完。
func TestDebounceSingle(t *testing.T) {
	gw, rec, _ := newTestGateway(t, time.Minute, 3*time.Minute, 100)
	srv := httptest.NewServer(gw)
	defer srv.Close()
	start := time.Now()
	if code, _ := postEnvelope(t, srv, "solo", "hi"); code != 200 {
		t.Fatalf("post: got %d", code)
	}
	deadline := start.Add(5 * time.Second)
	for {
		if got := rec.list(); len(got) == 1 && got[0] == "solo" {
			if d := time.Since(start); d > 5*time.Second {
				t.Fatalf("solo processed after %v, want < 5s", d)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for solo, got %v", rec.list())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSequentialThree 是 fastSem 泄漏的回归测试（2026-11-17 实测）：
// processLane 的 Fast 完成路径曾漏放 fastSem，两个槽位被前两条任务
// 永久占满后，第三条起 tryDispatch 永远走 putBack，队列彻底堵死。
// 三条顺序任务必须全部完成，且 inbox 里三行都是 DONE。
func TestSequentialThree(t *testing.T) {
	gw, rec, _ := newTestGateway(t, 100*time.Millisecond, time.Minute, 100)
	srv := httptest.NewServer(gw)
	defer srv.Close()
	for _, task := range []string{"seq1", "seq2", "seq3"} {
		if code, _ := postEnvelope(t, srv, task, "hello "+task); code != 200 {
			t.Fatalf("post %s: code=%d", task, code)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got := rec.list(); len(got) == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout: only %v completed, fast lane likely leaked", rec.list())
		}
		time.Sleep(20 * time.Millisecond)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		rows, err := gw.led.ListInboxByStatus(ledger.InboxDone)
		if err != nil {
			t.Fatalf("list done: %v", err)
		}
		if len(rows) == 3 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for 3 DONE rows, got %+v", rows)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestInboxDedup 是 B-02 的去重：同 (task_id, seq) 重复投递只处理一次。
// 注意：第二次投递在内存里合并（mergeOpen），所以合并窗口只设 2 秒，
// 合并后 2 秒即处理，不用等满 60 秒。
func TestInboxDedup(t *testing.T) {
	gw, rec, _ := newTestGateway(t, 2*time.Second, 5*time.Second, 100)
	srv := httptest.NewServer(gw)
	defer srv.Close()
	if code, r := postEnvelopeSeq(t, srv, "dup", "first", 7); code != 200 || r.Merged {
		t.Fatalf("first: code=%d merged=%v", code, r.Merged)
	}
	if code, r := postEnvelopeSeq(t, srv, "dup", "first again", 7); code != 200 || !r.Merged {
		t.Fatalf("second: code=%d merged=%v, want merged", code, r.Merged)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got := rec.list(); len(got) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout, got %v", rec.list())
		}
		time.Sleep(20 * time.Millisecond)
	}
	// inbox_queue 里同 (task_id, seq) 只有一行；收尾（DONE 标记）在
	// handler 返回后才跑，等它落袋再断言（防竞态）。
	deadline = time.Now().Add(5 * time.Second)
	var rows []ledger.InboxRow
	for {
		var err error
		rows, err = gw.led.ListInboxByStatus(ledger.InboxDone)
		if err != nil {
			t.Fatalf("list done: %v", err)
		}
		if len(rows) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for DONE row, got %+v", rows)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if rows[0].TaskID != "dup" || rows[0].Seq != 7 {
		t.Fatalf("done rows = %+v, want exactly one (dup, seq 7)", rows)
	}
}

// TestInboxReplay 是 B-02 的崩溃重放：处理前停掉进程，
// 重启后任务仍被处理且只处理一次。
func TestInboxReplay(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.MergeWindow = time.Minute
	cfg.ForcedWake = 3 * time.Minute
	cfg.Debounce = 30 * time.Second // 防抖 30 秒：停掉时还没取走
	cfg.MaxPending = 100
	l, err := ledger.Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	defer ledger.Close(l)

	rec1 := &recorder{}
	gw1 := New(cfg, l, rec1.handle)
	gw1.Start()
	srv := httptest.NewServer(gw1)
	if code, _ := postEnvelope(t, srv, "crash", "before kill"); code != 200 {
		t.Fatalf("post: got %d", code)
	}
	srv.Close()
	gw1.Stop() // 处理前停掉：行还是 PENDING
	if got := rec1.list(); len(got) != 0 {
		t.Fatalf("gw1 processed %v before stop, want none", got)
	}

	// 新进程：重放后立即处理（跳过剩余防抖），只处理一次。
	rec2 := &recorder{}
	gw2 := New(cfg, l, rec2.handle)
	n, err := gw2.ReplayInbox()
	if err != nil {
		t.Fatalf("ReplayInbox: %v", err)
	}
	if n != 1 {
		t.Fatalf("replayed = %d, want 1", n)
	}
	gw2.Start()
	defer gw2.Stop()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got := rec2.list(); len(got) == 1 && got[0] == "crash" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for replay, got %v", rec2.list())
		}
		time.Sleep(20 * time.Millisecond)
	}
	// 收尾（DONE 标记）在 handler 返回后才跑，等它落袋再断言（防竞态）。
	deadline = time.Now().Add(5 * time.Second)
	var rows []ledger.InboxRow
	for {
		var err error
		rows, err = l.ListInboxByStatus(ledger.InboxDone)
		if err != nil {
			t.Fatalf("list done: %v", err)
		}
		if len(rows) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for DONE row, got %+v", rows)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestDualLane 是 B-08 的核心：长活堵住时短任务照样跑，不被堵住。
func TestDualLane(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.MergeWindow = time.Minute
	cfg.ForcedWake = 3 * time.Minute
	cfg.Debounce = 100 * time.Millisecond
	cfg.MaxPending = 100
	l, err := ledger.Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	defer ledger.Close(l)

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var mu sync.Mutex
	var order []string
	handler := func(e Envelope) {
		if e.TaskID == "long" {
			select {
			case started <- struct{}{}:
			default:
			}
			<-release // 长活一直堵着，直到测试放行
			return
		}
		mu.Lock()
		order = append(order, e.TaskID)
		mu.Unlock()
	}
	gw := New(cfg, l, handler)
	gw.Start()
	defer gw.Stop()
	srv := httptest.NewServer(gw)
	defer srv.Close()

	if code, _ := postEnvelope(t, srv, "long", "sleep"); code != 200 {
		t.Fatalf("post long: got %d", code)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("long handler never started")
	}
	if code, _ := postEnvelope(t, srv, "short", "quick"); code != 200 {
		t.Fatalf("post short: got %d", code)
	}
	// 短任务必须在长活放行前就完成。
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		done := len(order) == 1 && order[0] == "short"
		mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("short not done while long blocked, order=%v", order)
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(release)
}

// TestDualLaneMigration 是 B-08 的迁移：Fast 车道满了之后，
// 长活跑超迁移阈值就释放 Fast 槽位，后来的短任务不再干等。
func TestDualLaneMigration(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.MergeWindow = time.Minute
	cfg.ForcedWake = 3 * time.Minute
	cfg.Debounce = 50 * time.Millisecond
	cfg.MaxPending = 100
	l, err := ledger.Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	defer ledger.Close(l)

	started := make(chan string, 10)
	release := make(chan struct{})
	var mu sync.Mutex
	var done []string
	handler := func(e Envelope) {
		if e.TaskID == "short3" {
			mu.Lock()
			done = append(done, e.TaskID)
			mu.Unlock()
			return
		}
		started <- e.TaskID
		<-release
	}
	gw := New(cfg, l, handler)
	gw.SetLaneMigration(200 * time.Millisecond) // 测试用小阈值
	gw.Start()
	defer gw.Stop()
	srv := httptest.NewServer(gw)
	defer srv.Close()

	for _, task := range []string{"long1", "long2"} {
		if code, _ := postEnvelope(t, srv, task, "sleep"); code != 200 {
			t.Fatalf("post %s: got %d", task, code)
		}
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("long handlers never started")
		}
	}
	// Fast 车道已满（2/2），第三个短任务进来。
	if code, _ := postEnvelope(t, srv, "short3", "quick"); code != 200 {
		t.Fatalf("post short3: got %d", code)
	}
	// 迁移阈值 200ms 后 Fast 槽位释放，short3 必须在长活放行前完成。
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		ok := len(done) == 1 && done[0] == "short3"
		mu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("short3 not done after migration, done=%v", done)
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(release)
}

// TestSweepStale 是 B-06 的重排：超 1800 秒未确认的 RUNNING 行自动重排 1 次，
// 且只重排 1 次（第二次扫荡不再动它）。
func TestSweepStale(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.MergeWindow = time.Minute
	cfg.ForcedWake = 3 * time.Minute
	cfg.Debounce = 50 * time.Millisecond
	cfg.MaxPending = 100
	l, err := ledger.Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	defer ledger.Close(l)

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	handler := func(e Envelope) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			select {
			case started <- struct{}{}:
			default:
			}
			<-release // 第一次执行一直堵着，模拟超期未确认
		}
	}
	gw := New(cfg, l, handler)
	gw.Start()
	defer gw.Stop()
	srv := httptest.NewServer(gw)
	defer srv.Close()

	if code, _ := postEnvelope(t, srv, "stale1", "wedged"); code != 200 {
		t.Fatalf("post: got %d", code)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never started")
	}
	// 把 RUNNING 行的 updated_ts 拨到 3 小时前，模拟超期。
	running, err := l.ListInboxByStatus(ledger.InboxRunning)
	if err != nil || len(running) != 1 {
		t.Fatalf("running rows = %+v, err = %v, want exactly one", running, err)
	}
	if err := backdateInboxRow(t, filepath.Join(dir, "ledger.db"), running[0].ID, 3*time.Hour); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	gw.sweepStale()
	// 重排后行回到 PENDING 且 retry_count=1，副本立即被取走执行（第二次调用）。
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := calls
		mu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("requeued copy never ran, calls=%d", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
	pending, err := l.ListInboxByStatus(ledger.InboxPending)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending rows after requeue = %+v, err = %v, want none", pending, err)
	}
	// 收尾（DONE 标记）在 handler 返回后才跑，等它落袋再断言（防竞态）。
	deadline = time.Now().Add(5 * time.Second)
	var done []ledger.InboxRow
	for {
		var err error
		done, err = l.ListInboxByStatus(ledger.InboxDone)
		if err != nil {
			t.Fatalf("list done: %v", err)
		}
		if len(done) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for DONE row, got %+v", done)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// 第二次扫荡：retry_count 已是 1，不再重排。
	gw.sweepStale()
	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 2 {
		t.Fatalf("second sweep requeued again, calls=%d, want 2", n)
	}
	// 记账行：watchdog-requeue 恰好 1 行。
	rows, err := l.ListExecutions("stale1", 10)
	if err != nil {
		t.Fatalf("ListExecutions: %v", err)
	}
	requeues := 0
	for _, r := range rows {
		if r.ToolID == "watchdog-requeue" {
			requeues++
		}
	}
	if requeues != 1 {
		t.Fatalf("requeue ledger rows = %d, want 1 (rows=%+v)", requeues, rows)
	}
	close(release)
}

// backdateInboxRow 用独立连接把某行的 updated_ts 拨早 d（测试用）。
func backdateInboxRow(t *testing.T, dbPath string, id int64, d time.Duration) error {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	old := time.Now().UTC().Add(-d).Format(time.RFC3339)
	_, err = db.Exec(`UPDATE inbox_queue SET updated_ts = ? WHERE id = ?`, old, id)
	return err
}
