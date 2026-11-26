// Package watchdog 的简介巡检循环（线 B 第二阶段第 4 棒，v2.1 §6）：
// 增量巡检每小时 :17 与 :47 各一次（错开整点/半点的全量巡检 02:00、
// 备份 03:30、精炼 04:00、自进化 04:30），全量 SHA-256 校准每天
// 北京时间 02:00–02:15 一次。增量与全量共用 Watchdog.patrolMu
// 互斥锁：全量运行时增量直接跳过。
// 巡检写主库与账本，失败只记日志，不阻塞主程序；影子模式不启动
// （由 main.go 控制，与 RefineLoop/BackupLoop 同规矩）。
package watchdog

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"007-super-agent/internal/ledger"
	"007-super-agent/internal/memory"
)

// shouldPatrolIncremental 判断现在是否该跑增量巡检：分钟为 :17 或 :47，
// 且上次增量不在同一触发槽位（同一槽位内 60 秒 tick 只跑一次）。
// 整点/半点全是别人的窗口（全量巡检 02:00、备份 03:30、精炼 04:00、
// 自进化 04:30），增量错峰到 :17/:47，避免与文件替换窗口撞车。
// 上次运行时间缺失时，等到下一个 :17/:47 槽位再跑（启动即跑会撞上
// 任意整点/半点窗口，故不等槽位不跑）。
func shouldPatrolIncremental(now time.Time, last time.Time, hasLast bool) bool {
	if now.Minute() != 17 && now.Minute() != 47 {
		return false
	}
	if !hasLast {
		return true
	}
	return !(last.Year() == now.Year() && last.YearDay() == now.YearDay() &&
		last.Hour() == now.Hour() && last.Minute() == now.Minute())
}

// patrolLastFile 是“上次增量巡检时间”的时间戳文件（RFC3339，数据目录）。
func patrolLastFile(dataDir string) string {
	return filepath.Join(dataDir, "patrol_last")
}

// patrolFullMarkerFile 是“当天已跑过全量巡检”的标记文件（内容为日期）。
func patrolFullMarkerFile(dataDir string) string {
	return filepath.Join(dataDir, "patrol_full_last")
}

// readPatrolMarker 读一个标记文件（不存在或读失败返回 ""）。
func readPatrolMarker(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// shouldPatrolFull 判断现在是否该跑全量巡检：北京时间 02:00–02:15，
// 且标记文件里的日期不是今天（当天未跑过）。
func shouldPatrolFull(now time.Time, marker string) bool {
	if now.Hour() != 2 || now.Minute() >= 15 {
		return false
	}
	return marker != now.Format("2006-01-02")
}

// PatrolOnce 跑一次巡检并写账本（循环与 patrol-run CLI 共用）。
// 主库文件不存在时报错（不建空库，与简介主库接线口径一致——
// 空库会让两阶段召回误以为简介已建完）。
// led 为 nil 时只跑巡检、不记账。taskID 写入 patrol_log 供追溯
// （循环用 "patrol"，CLI 用 "patrol-run"）。
func PatrolOnce(memRoot, storePath string, full bool, led *ledger.Ledger, sessionID, taskID string) (memory.PatrolResult, error) {
	var res memory.PatrolResult
	if _, err := os.Stat(storePath); err != nil {
		return res, fmt.Errorf("简介主库不存在 %s：%w", storePath, err)
	}
	st, err := memory.OpenSummaryStore(storePath)
	if err != nil {
		return res, fmt.Errorf("巡检开主库失败: %w", err)
	}
	res, err = memory.PatrolSummaryStore(memRoot, st, memory.PatrolOptions{Full: full})
	_ = st.Close()
	if err != nil {
		return res, err
	}
	if led != nil {
		if _, rerr := led.RecordPatrol(ledger.PatrolRecord{
			SessionID: sessionID, TaskID: taskID, Full: full,
			Scanned: res.Scanned, New: res.New, Modified: res.Modified,
			Renamed: res.Renamed, Deleted: res.Deleted,
			StaleMarked: res.StaleMarked, PendingMarked: res.PendingMarked,
			SkippedGenerating: res.SkippedGenerating,
			SecretFiles:       res.SecretFiles, SecretHits: res.SecretHits,
			Detail: strings.Join(res.Details, "\n"),
		}); rerr != nil {
			log.Printf("patrol: patrol_log 记账失败（不阻塞）: %v", rerr)
		}
	}
	return res, nil
}

// PatrolLoop 常驻轻量循环：每 60 秒检查一次。
// 增量巡检在每小时 :17 与 :47 触发（上次运行时间存 data/patrol_last，
// 标记缺失时等到下一个槽位再跑）；全量校准每天北京时间 02:00–02:15
// 一次（日期存 data/patrol_full_last），全量跑完也刷新增量时间戳
// （全量已覆盖增量，不再紧接着补跑一次）。增量与全量共用
// w.patrolMu：全量运行时增量直接跳过。ctx 取消时退出。
func (w *Watchdog) PatrolLoop(ctx context.Context, memRoot, storePath, dataDir, sessionID string) {
	tick := time.NewTicker(60 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		now := beijingNow()
		if shouldPatrolFull(now, readPatrolMarker(patrolFullMarkerFile(dataDir))) {
			w.patrolMu.Lock()
			res, err := PatrolOnce(memRoot, storePath, true, w.led, sessionID, "patrol")
			w.patrolMu.Unlock()
			if err != nil {
				log.Printf("patrol loop 全量: %v", err)
				continue
			}
			log.Printf("patrol loop 全量完成：%s", res.Summary())
			if err := os.WriteFile(patrolFullMarkerFile(dataDir), []byte(now.Format("2006-01-02")+"\n"), 0o600); err != nil {
				log.Printf("patrol loop: 写全量标记失败: %v（下个 60 秒会重跑一次）", err)
			}
			if err := os.WriteFile(patrolLastFile(dataDir), []byte(now.Format(time.RFC3339)+"\n"), 0o600); err != nil {
				log.Printf("patrol loop: 写增量时间戳失败: %v", err)
			}
			continue
		}
		lastTime, perr := time.Parse(time.RFC3339, readPatrolMarker(patrolLastFile(dataDir)))
		if !shouldPatrolIncremental(now, lastTime, perr == nil) {
			continue
		}
		if !w.patrolMu.TryLock() {
			log.Printf("patrol loop 增量跳过：全量巡检正在运行")
			continue
		}
		res, err := PatrolOnce(memRoot, storePath, false, w.led, sessionID, "patrol")
		w.patrolMu.Unlock()
		if err != nil {
			log.Printf("patrol loop 增量: %v", err)
			continue
		}
		log.Printf("patrol loop 增量完成：%s", res.Summary())
		if err := os.WriteFile(patrolLastFile(dataDir), []byte(now.Format(time.RFC3339)+"\n"), 0o600); err != nil {
			log.Printf("patrol loop: 写增量时间戳失败: %v", err)
		}
	}
}
