// Command gateway-watchdog 是网关侧外部看门狗（线 A）：一次性扫荡，
// 供 systemd 定时器每 5 分钟跑一次。直接读 ledger.db：把超 deadline
// 未终结的 RUNNING 行置为 TIMEOUT 并补发失败回信；产物已落袋的
// 超期行标 DONE。跑完即退出，不常驻。
//
// 用法：gateway-watchdog --data-dir /root/super-agent-007/data
// dataDir 为空时用 DATA_DIR 环境变量，再没有就用当前目录。
// 注意：只读账本的扫荡，与网关进程并发跑是安全的（同一行只被置位一次，
// 靠“只改 RUNNING 行”的行级守卫）。
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"007-super-agent/internal/gateway"
	"007-super-agent/internal/ledger"
)

func main() {
	dataDir := flag.String("data-dir", "", "数据目录（ledger.db 与 artifacts 所在目录）")
	flag.Parse()
	dir := *dataDir
	if dir == "" {
		dir = os.Getenv("DATA_DIR")
	}
	if dir == "" {
		dir = "."
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		log.Fatalf("gateway-watchdog: 数据目录解析失败: %v", err)
	}
	led, err := ledger.Open(filepath.Join(abs, "ledger.db"))
	if err != nil {
		log.Fatalf("gateway-watchdog: 打开账本失败: %v", err)
	}
	defer ledger.Close(led)
	timedOut, recovered, err := gateway.WatchdogSweep(led, abs, time.Now())
	if err != nil {
		log.Fatalf("gateway-watchdog: 扫荡失败: %v", err)
	}
	fmt.Printf("gateway-watchdog: timeout=%d recovered=%d\n", timedOut, recovered)
}
