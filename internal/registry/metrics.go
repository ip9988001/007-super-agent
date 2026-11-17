// Package registry 的 index_metrics 记账：超限读索引时留痕。
//
// 说明：ledger 包目前只提供 triage/execution/watermark/expense 四张表的
// 写方法，没有 index_metrics 的写入口。本文件只在 registry 包内提供
// 一个最小适配器（LedgerFileRecorder），直接对 ledger.db 文件写
// index_metrics 一张表——连接同样走 modernc.org/sqlite + WAL +
// busy_timeout=5000 + 单连接串行写；表结构与 ledger/schema.sql 的
// index_metrics 逐字段一致。若将来 ledger 包补了写方法，应改为调用它，
// 删掉本适配器。
package registry

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// IndexMetric 是一次索引读取的记账行（字段与 ledger.db index_metrics 对应）。
type IndexMetric struct {
	SessionID         string
	TaskID            string
	Level             string // L1 / L2 / L3
	ToolID            string
	Hit               bool
	TokensInjected    int
	TokensSavedVsFull int
}

// MetricsRecorder 是 index_metrics 的写入口（超限记账用）。
type MetricsRecorder interface {
	RecordIndexMetric(m IndexMetric) error
}

const indexMetricsDDL = `CREATE TABLE IF NOT EXISTS index_metrics (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    ts                    TEXT    NOT NULL,
    session_id            TEXT    NOT NULL,
    task_id               TEXT    NOT NULL,
    level                 TEXT    NOT NULL,
    tool_id               TEXT    NOT NULL DEFAULT '',
    hit                   INTEGER NOT NULL DEFAULT 0,
    tokens_injected       INTEGER NOT NULL DEFAULT 0,
    tokens_saved_vs_full  INTEGER NOT NULL DEFAULT 0
);`

// LedgerFileRecorder 是 MetricsRecorder 的文件实现：直连 ledger.db，
// 只写 index_metrics。调用方用完要 Close。
type LedgerFileRecorder struct {
	db *sql.DB
	mu sync.Mutex
}

// NewLedgerFileRecorder 打开（不存在则创建）ledger.db 并确保
// index_metrics 表存在。path 的父目录不存在时自动创建由调用方保证——
// 这里不自动建目录，路径错了就明确报错。
func NewLedgerFileRecorder(dbPath string) (*LedgerFileRecorder, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("registry metrics open %s: %w", dbPath, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		db.Close()
		return nil, fmt.Errorf("registry metrics pragma WAL: %w", err)
	}
	if _, err := db.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		db.Close()
		return nil, fmt.Errorf("registry metrics pragma busy_timeout: %w", err)
	}
	if _, err := db.Exec(indexMetricsDDL); err != nil {
		db.Close()
		return nil, fmt.Errorf("registry metrics create table: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("registry metrics ping: %w", err)
	}
	return &LedgerFileRecorder{db: db}, nil
}

// RecordIndexMetric 写一行 index_metrics。hit 为 true 表示命中。
func (r *LedgerFileRecorder) RecordIndexMetric(m IndexMetric) error {
	hit := 0
	if m.Hit {
		hit = 1
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.db.Exec(
		`INSERT INTO index_metrics
		 (ts, session_id, task_id, level, tool_id, hit,
		  tokens_injected, tokens_saved_vs_full)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		time.Now().UTC().Format(time.RFC3339),
		m.SessionID, m.TaskID, m.Level, m.ToolID, hit,
		m.TokensInjected, m.TokensSavedVsFull,
	)
	if err != nil {
		return fmt.Errorf("registry metrics insert: %w", err)
	}
	return nil
}

// Close 关闭到底层数据库的连接。
func (r *LedgerFileRecorder) Close() error { return r.db.Close() }

// EstimateTokens 按第 1 步审计口径估算 token 数：1 汉字≈0.6 token，
// 1 非汉字字符≈0.25 token。仅为估算，非实测分词。
func EstimateTokens(s string) int {
	total := 0.0
	for _, r := range s {
		if (r >= 0x4E00 && r <= 0x9FFF) || // CJK 统一汉字
			(r >= 0x3400 && r <= 0x4DBF) || // CJK 扩展 A
			(r >= 0xF900 && r <= 0xFAFF) { // CJK 兼容汉字
			total += 0.6
		} else {
			total += 0.25
		}
	}
	return int(total + 0.5)
}
