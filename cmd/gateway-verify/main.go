package main

import (
	"007-super-agent/internal/config"
	"007-super-agent/internal/gateway"
	"007-super-agent/internal/ledger"
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"time"
)

func main() {
	dir, _ := os.MkdirTemp("", "gwverify")
	cfg := config.Defaults()
	cfg.DataDir, cfg.MergeWindow, cfg.ForcedWake = dir, 2*time.Second, 5*time.Second
	l, err := ledger.Open(dir + "/ledger.db")
	if err != nil {
		panic(err)
	}
	defer ledger.Close(l)
	var done []string
	gw := gateway.New(cfg, l, func(e gateway.Envelope) { done = append(done, e.TaskID) })
	gw.Start()
	s := httptest.NewServer(gw)
	for _, task := range []string{"t1", "t1", "t2"} {
		b := []byte(`{"task_id":"` + task + `","text":"hi","source":"verify","ts":"` + time.Now().UTC().Format(time.RFC3339) + `"}`)
		r, err := http.Post(s.URL+"/wake", "application/json", bytes.NewReader(b))
		if err != nil {
			panic(err)
		}
		fmt.Printf("POST %s -> %d\n", task, r.StatusCode)
		r.Body.Close()
	}
	time.Sleep(4 * time.Second)
	ents, _ := os.ReadDir(dir + "/receipts")
	fmt.Printf("processed=%v receipts=%d watermark=%+v\n", done, len(ents), gw.Watermark())
}
