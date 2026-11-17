package gateway

import (
	"bytes"
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
