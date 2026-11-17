package main

import (
	"fmt"
	"os"

	"007-super-agent/internal/ledger"
	"007-super-agent/internal/watchdog"
)

func main() {
	dir := os.Getenv("DATA_DIR")
	if dir == "" {
		fmt.Println("watchdog-verify: 需要 DATA_DIR")
		os.Exit(1)
	}
	led, err := ledger.Open(dir + "/ledger.db")
	if err != nil {
		fmt.Println("open ledger:", err)
		os.Exit(1)
	}
	defer ledger.Close(led)
	w := watchdog.New(led, dir+"/ledger.db", watchdog.DefaultProbe)
	res, err := w.CheckRegistry(dir)
	fmt.Println("check:", res.Checked, "ok:", res.OK, "broken:", len(res.Broken), "err:", err)
	sv, err := w.DailySavings()
	fmt.Println("savings: triage:", sv.TriageCount, "cost_milli:", sv.CostCNYMilli, "net_saved:", sv.NetSavedTokens, "warn:", sv.Warn, "err:", err)
	alert, reason := watchdog.CheckQueue(watchdog.QueueWatermark{Seen: 5, Acked: 3, OldestUnackedS: 3600})
	fmt.Println("queue alert:", alert, reason)
}
