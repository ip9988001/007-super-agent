// 每日 09:00 省 token 核算定时（第 6 项）。
//
// R-31 要求“每天 09:00 出省 token 核算”，但旧实现只在守护进程启动时
// 调一次 DailySavings()、只记一行日志、什么都不落盘——是一条假规则。
// 本文件把它变成真定时：常驻轻量循环（60 秒一 tick，照抄 RefineLoop
// 的模式），北京时间 09:00–09:15 且当天未跑过才跑一次，核算前一天
// （UTC）的完整账，落盘到 data/daily-savings/<日期>.json，净省为负
// 时记一条告警日志。纯算术，不调模型，成本约等于零。
package watchdog

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// savingsMarkerFile 是“当天已跑过 09:00 核算”的标记文件（放数据目录，内容为日期）。
func savingsMarkerFile(dataDir string) string {
	return filepath.Join(dataDir, "savings_last")
}

// shouldRunSavings 判断现在是否该跑 09:00 核算：北京时间 09:00–09:15，
// 且标记文件里的日期不是今天（当天未跑过）。
func shouldRunSavings(now time.Time, marker string) bool {
	if now.Hour() != 9 || now.Minute() >= 15 {
		return false
	}
	return marker != now.Format("2006-01-02")
}

// savingsFilePath 是某日核算结果的落盘路径。
func savingsFilePath(dataDir, date string) string {
	return filepath.Join(dataDir, "daily-savings", date+".json")
}

// SavingsLoop 常驻轻量循环：每 60 秒检查一次，北京时间 09:00–09:15 且
// 当天未跑过（标记文件）才跑一次。核算前一天（UTC）的完整账——09:00
// 北京时是 UTC 01:00，前一天 UTC 已完整；落盘后才写当天标记。
// 净省为负时记一条告警日志（不发通知，看门狗只判断）。ctx 取消时退出。
func (w *Watchdog) SavingsLoop(ctx context.Context, dataDir string) {
	tick := time.NewTicker(60 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		now := beijingNow()
		marker := ""
		if b, err := os.ReadFile(savingsMarkerFile(dataDir)); err == nil {
			marker = string(b)
			if len(marker) > 10 {
				marker = marker[:10]
			}
		}
		if !shouldRunSavings(now, marker) {
			continue
		}
		// 核算前一天（UTC）的完整账。09:00 北京时 = UTC 01:00，
		// 前一天 UTC 与前一天北京是同一天，口径一致。
		date := now.AddDate(0, 0, -1).Format("2006-01-02")
		sv, err := w.DailySavingsFor(date)
		if err != nil {
			log.Printf("savings loop: 核算 %s 失败: %v（下个 60 秒会重跑一次）", date, err)
			continue
		}
		if err := w.persistSavings(dataDir, date, sv); err != nil {
			log.Printf("savings loop: 落盘 %s 失败: %v（下个 60 秒会重跑一次）", date, err)
			continue
		}
		if sv.Warn {
			log.Printf("savings loop: 告警 %s：%s", date, sv.WarnReason)
		}
		log.Printf("savings loop: %s 核算完成：分诊 %d 次（Tier-0 %.0f%%），索引注入 %d、省下 %d、净省 %d token，当日调用 %d 次、费用 %.3f 元",
			date, sv.TriageCount, sv.Tier0Ratio*100, sv.IndexInjected, sv.IndexSaved,
			sv.NetSavedTokens, sv.ExpenseCalls, float64(sv.CostCNYMilli)/1000)
		if err := os.WriteFile(savingsMarkerFile(dataDir), []byte(now.Format("2006-01-02")+"\n"), 0o600); err != nil {
			log.Printf("savings loop: 写标记文件失败: %v（下个 60 秒会重跑一次）", err)
		}
	}
}

// persistSavings 把一次核算结果写进 data/daily-savings/<日期>.json。
func (w *Watchdog) persistSavings(dataDir, date string, sv Savings) error {
	dir := filepath.Join(dataDir, "daily-savings")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("watchdog: 建 daily-savings 目录失败: %w", err)
	}
	b, err := json.MarshalIndent(sv, "", "  ")
	if err != nil {
		return fmt.Errorf("watchdog: 序列化核算结果失败: %w", err)
	}
	if err := os.WriteFile(savingsFilePath(dataDir, date), b, 0o600); err != nil {
		return fmt.Errorf("watchdog: 写核算文件失败: %w", err)
	}
	return nil
}
