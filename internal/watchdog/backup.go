// 账本备份：第 14 步 B-07，ledger.db 是唯一事实源，必须有离线备份。
//
// 每天北京时间 03:30 跑一次 SQLite 在线热备（VACUUM INTO，不锁 WAL，
// 备份期间正常读写不受影响），落到 <数据目录>/backups/ledger-YYYYMMDD.db，
// 只留最近 7 天，备份结果写进 execution_ledger（ToolID backup-ledger）。
// 只备私密部署的账本——备份文件含全部记账数据，绝不进公开仓库。

package watchdog

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"007-super-agent/internal/ledger"

	_ "modernc.org/sqlite"
)

// BackupRetentionDays 是备份保留天数。
const BackupRetentionDays = 7

// BackupWindowStartMin 是每日备份窗口（北京时间 03:30–03:45）。
const (
	BackupWindowHour      = 3
	BackupWindowStartMin  = 30
	BackupWindowEndMin    = 45
	BackupCheckInterval   = 60 * time.Second
)

// backupFileName 是某天的备份文件名。
func backupFileName(date string) string { return "ledger-" + date + ".db" }

// backupMarkerFile 是“当天已跑过备份”的标记文件（放数据目录，内容为日期）。
func backupMarkerFile(dataDir string) string {
	return filepath.Join(dataDir, "backup_last")
}

// BackupLedger 做一次账本热备：VACUUM INTO 到 backups/ledger-YYYYMMDD.db，
// 删 7 天前的文件，结果写进 execution_ledger。返回备份文件路径。
// 同一天重复跑会覆盖当天的备份文件（VACUUM INTO 要求目标不存在，
// 先删旧文件再备）。dbPath 为空时返回错误。
func (w *Watchdog) BackupLedger(dataDir string) (string, error) {
	if strings.TrimSpace(w.dbPath) == "" {
		return "", fmt.Errorf("watchdog backup: 未指定 ledger.db 路径")
	}
	date := beijingNow().Format("2006-01-02")
	dir := filepath.Join(dataDir, "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("watchdog backup mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, backupFileName(date))
	if err := w.vacuumInto(path); err != nil {
		w.recordBackup(date, path, 1, err.Error())
		return "", err
	}
	pruned := w.pruneBackups(dir, date)
	w.recordBackup(date, path, 0, fmt.Sprintf("pruned=%d", pruned))
	log.Printf("backup ledger: %s (pruned %d old)", path, pruned)
	return path, nil
}

// vacuumInto 用单独连接跑 VACUUM INTO（在线热备，不锁 WAL）。
// 目标文件已存在时先删（VACUUM INTO 要求目标不存在）。
func (w *Watchdog) vacuumInto(path string) error {
	if _, err := os.Stat(path); err == nil {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("watchdog backup remove old %s: %w", path, err)
		}
	}
	db, err := sql.Open("sqlite", "file:"+w.dbPath)
	if err != nil {
		return fmt.Errorf("watchdog backup open ledger.db: %w", err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		return fmt.Errorf("watchdog backup pragma: %w", err)
	}
	// VACUUM INTO 不支持绑定参数；路径由本地配置拼出，单引号转义后拼进 SQL。
	quoted := "'" + strings.ReplaceAll(path, "'", "''") + "'"
	if _, err := db.Exec(`VACUUM INTO ` + quoted); err != nil {
		return fmt.Errorf("watchdog backup VACUUM INTO %s: %w", path, err)
	}
	return nil
}

// pruneBackups 删 7 天前的备份文件，返回删掉的数量。
func (w *Watchdog) pruneBackups(dir, today string) int {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	cutoff := ""
	if t, err := time.Parse("2006-01-02", today); err == nil {
		cutoff = t.AddDate(0, 0, -BackupRetentionDays).Format("2006-01-02")
	}
	pruned := 0
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "ledger-") || !strings.HasSuffix(name, ".db") {
			continue
		}
		date := strings.TrimSuffix(strings.TrimPrefix(name, "ledger-"), ".db")
		if cutoff != "" && date < cutoff {
			if err := os.Remove(filepath.Join(dir, name)); err == nil {
				pruned++
			}
		}
	}
	return pruned
}

// recordBackup 把一次备份结果写进 execution_ledger（失败只记日志，
// 不掩盖备份本身的错误）。led 为 nil 时跳过记账。
func (w *Watchdog) recordBackup(date, path string, exitCode int, note string) {
	if w.led == nil {
		return
	}
	status := "success"
	if exitCode != 0 {
		status = "failure"
	}
	size := int64(-1)
	if st, err := os.Stat(path); err == nil {
		size = st.Size()
	}
	_, err := w.led.RecordExecution(ledger.ExecutionRecord{
		SessionID: "watchdog",
		TaskID:    "backup-" + date,
		ToolID:    "backup-ledger",
		Command:   "backup ledger.db",
		ExitCode:  exitCode,
		Status:    status,
		StdoutTail: fmt.Sprintf("path=%s size=%d", path, size),
		ErrHead:    note,
	})
	if err != nil {
		log.Printf("watchdog backup record failed: %v", err)
	}
}

// shouldBackup 判断现在是否该跑备份：北京时间 03:30–03:45，
// 且标记文件里的日期不是今天（当天未跑过）。
func shouldBackup(now time.Time, marker string) bool {
	if now.Hour() != BackupWindowHour || now.Minute() < BackupWindowStartMin || now.Minute() >= BackupWindowEndMin {
		return false
	}
	return marker != now.Format("2006-01-02")
}

// BackupLoop 常驻轻量循环：每 60 秒检查一次，北京时间 03:30–03:45 且
// 当天未跑过（标记文件）才跑一次 BackupLedger。跑完才写当天标记；
// ctx 取消时退出。备份失败只记日志，不报错、不阻塞主程序。
func (w *Watchdog) BackupLoop(ctx context.Context, dataDir string) {
	tick := time.NewTicker(BackupCheckInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		now := beijingNow()
		marker := ""
		if b, err := os.ReadFile(backupMarkerFile(dataDir)); err == nil {
			marker = strings.TrimSpace(string(b))
		}
		if !shouldBackup(now, marker) {
			continue
		}
		if _, err := w.BackupLedger(dataDir); err != nil {
			log.Printf("backup loop: %v", err)
			continue
		}
		if err := os.WriteFile(backupMarkerFile(dataDir), []byte(now.Format("2006-01-02")+"\n"), 0o600); err != nil {
			log.Printf("backup loop: 写标记文件失败: %v（下个 60 秒会重跑一次）", err)
		}
	}
}

// backupDatesSorted 供测试：列出备份目录里的备份日期（升序）。
func backupDatesSorted(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, "ledger-") && strings.HasSuffix(name, ".db") {
			out = append(out, strings.TrimSuffix(strings.TrimPrefix(name, "ledger-"), ".db"))
		}
	}
	sort.Strings(out)
	return out
}
