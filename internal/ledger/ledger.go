// Package ledger 是 007 超级 Agent 的可观测账本：ledger.db 的唯一写入口。
//
// 设计依据（《超级Agent-全新设计方案》v1.5）：
//   - 五张表全部由框架代码自动落盘，不靠模型自觉记账；每日 09:00 实测核算省 token 率。
//   - 第 6 轮 Go 避坑规范：驱动必须用纯 Go 无 CGO 的 modernc.org/sqlite；
//     连接池开 PRAGMA journal_mode=WAL、PRAGMA busy_timeout=5000；
//     全部写事务用单个 sync.Mutex 串行化，防 SQLITE_BUSY。
//   - 第 6 轮坑 2：所有截断先转 []rune 按字符切片，防中文从字节中间劈开乱码。
//   - 第 7 轮：execution_ledger 新增 advisor_verdict / advisor_retry_count /
//     advisor_note 三字段；监工消耗记入 expense_ledger。
//
// 铁律：写入失败必须返回错误，不许静默吞掉；不写任何密钥、Token。
package ledger

import (
	"database/sql"
	_ "embed"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// Ledger 是账本的唯一写入口。写事务由 mu 串行化，读用 RLock。
type Ledger struct {
	db *sql.DB
	mu sync.RWMutex
}

// Open 打开（不存在则创建）ledger.db，建五张表。path 的父目录不存在时自动创建。
func Open(path string) (*Ledger, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("ledger open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1) // 单连接 + 串行写，彻底杜绝 SQLITE_BUSY
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		db.Close()
		return nil, fmt.Errorf("ledger pragma WAL: %w", err)
	}
	if _, err := db.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		db.Close()
		return nil, fmt.Errorf("ledger pragma busy_timeout: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("ledger create schema: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ledger ping: %w", err)
	}
	return &Ledger{db: db}, nil
}

// Close 关闭账本。
func Close(l *Ledger) error { return l.db.Close() }

// truncateRunes 按字符截断到 n 个字（第 6 轮坑 2：先转 []rune，防中文乱码）。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// lastLines 取文本末尾 n 行。先去掉结尾的空行，否则 "a\nb\n" 这类
// 带结尾换行的输出会被多算一行空行，丢掉真正的倒数第 n 行。
func lastLines(s string, n int) string {
	s = strings.TrimSuffix(s, "\n")
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// TriageRecord 是一次分诊的记账行。
type TriageRecord struct {
	SessionID string
	TaskID    string
	Route     string // DIRECT / TOOL / META
	Tier      int    // 0=规则直出，1=模型分诊
	ToolID    string
	TokensIn  int64
	TokensOut int64
	LatencyMs int64
}

// RecordTriage 写一行分诊账。失败返回错误。
func (l *Ledger) RecordTriage(r TriageRecord) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	res, err := l.db.Exec(`INSERT INTO triage_metrics
		(ts, session_id, task_id, route, tier, tool_id, tokens_in, tokens_out, latency_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		time.Now().UTC().Format(time.RFC3339), r.SessionID, r.TaskID,
		r.Route, r.Tier, r.ToolID, r.TokensIn, r.TokensOut, r.LatencyMs)
	if err != nil {
		return 0, fmt.Errorf("ledger insert triage_metrics: %w", err)
	}
	return res.LastInsertId()
}

// ExecutionRecord 是一次工具执行的回执。
type ExecutionRecord struct {
	SessionID         string
	TaskID            string
	ToolID            string
	Command           string
	ExitCode          int
	StdoutTail        string // 入库时自动只留末尾 20 行
	ErrHead           string // 入库时自动按字符截断到 600 字
	DurationMs        int64
	Status            string // 为空时按 ExitCode 推导
	AdvisorVerdict    string // NONE / NOTE / CONCERN / HALT，为空时记 NONE
	AdvisorRetryCount int    // 0..2
	AdvisorNote       string // 入库时自动按字符截断到 600 字
}

// RecordExecution 写一行执行回执。失败返回错误。
func (l *Ledger) RecordExecution(r ExecutionRecord) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if r.Status == "" {
		if r.ExitCode == 0 {
			r.Status = "success"
		} else {
			r.Status = "failure"
		}
	}
	if r.AdvisorVerdict == "" {
		r.AdvisorVerdict = "NONE"
	}
	res, err := l.db.Exec(`INSERT INTO execution_ledger
		(ts, session_id, task_id, tool_id, command, exit_code, stdout_tail, err_head,
		 duration_ms, status, advisor_verdict, advisor_retry_count, advisor_note)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		time.Now().UTC().Format(time.RFC3339), r.SessionID, r.TaskID, r.ToolID,
		r.Command, r.ExitCode, lastLines(r.StdoutTail, 20), truncateRunes(r.ErrHead, 600),
		r.DurationMs, r.Status, r.AdvisorVerdict, r.AdvisorRetryCount,
		truncateRunes(r.AdvisorNote, 600))
	if err != nil {
		return 0, fmt.Errorf("ledger insert execution_ledger: %w", err)
	}
	return res.LastInsertId()
}

// Execution 是从 execution_ledger 读回的一行。
type Execution struct {
	ID                int64
	TS                string
	SessionID         string
	TaskID            string
	ToolID            string
	Command           string
	ExitCode          int
	StdoutTail        string
	ErrHead           string
	DurationMs        int64
	Status            string
	AdvisorVerdict    string
	AdvisorRetryCount int
	AdvisorNote       string
}

// ListExecutions 按任务查执行回执（最新在前，最多 limit 行）。
func (l *Ledger) ListExecutions(taskID string, limit int) ([]Execution, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	rows, err := l.db.Query(`SELECT id, ts, session_id, task_id, tool_id, command,
		exit_code, stdout_tail, err_head, duration_ms, status,
		advisor_verdict, advisor_retry_count, advisor_note
		FROM execution_ledger WHERE task_id = ? ORDER BY id DESC LIMIT ?`, taskID, limit)
	if err != nil {
		return nil, fmt.Errorf("ledger query execution_ledger: %w", err)
	}
	defer rows.Close()
	var out []Execution
	for rows.Next() {
		var e Execution
		if err := rows.Scan(&e.ID, &e.TS, &e.SessionID, &e.TaskID, &e.ToolID,
			&e.Command, &e.ExitCode, &e.StdoutTail, &e.ErrHead, &e.DurationMs,
			&e.Status, &e.AdvisorVerdict, &e.AdvisorRetryCount, &e.AdvisorNote); err != nil {
			return nil, fmt.Errorf("ledger scan execution_ledger: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// WatermarkRecord 是一行队列水位（queue_watermarks，供网关写）。
type WatermarkRecord struct {
	Box               string
	SeenSeq           int64
	AckedSeq          int64
	OldestUnackedAgeS int64
	UnackedAlert      bool
}

// RecordWatermark 写一行队列水位。失败返回错误。
func (l *Ledger) RecordWatermark(r WatermarkRecord) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	alert := 0
	if r.UnackedAlert {
		alert = 1
	}
	res, err := l.db.Exec(`INSERT INTO queue_watermarks
		(ts, box, seen_seq, acked_seq, oldest_unacked_age_s, unacked_alert)
		VALUES (?, ?, ?, ?, ?, ?)`,
		time.Now().UTC().Format(time.RFC3339), r.Box, r.SeenSeq, r.AckedSeq,
		r.OldestUnackedAgeS, alert)
	if err != nil {
		return 0, fmt.Errorf("ledger insert queue_watermarks: %w", err)
	}
	return res.LastInsertId()
}

// ExpenseRecord 是一次模型调用的费用行。CostCNYMilli 单位是 0.001 元。
type ExpenseRecord struct {
	SessionID        string
	TaskID           string
	Model            string
	PromptTokens     int64
	CacheHitTokens   int64
	CacheMissTokens  int64
	CompletionTokens int64
	CostCNYMilli     int64
}

// RecordExpense 写一行费用账。失败返回错误。
func (l *Ledger) RecordExpense(r ExpenseRecord) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	res, err := l.db.Exec(`INSERT INTO expense_ledger
		(ts, session_id, task_id, model, prompt_tokens, cache_hit_tokens,
		 cache_miss_tokens, completion_tokens, cost_cny_milli)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		time.Now().UTC().Format(time.RFC3339), r.SessionID, r.TaskID, r.Model,
		r.PromptTokens, r.CacheHitTokens, r.CacheMissTokens,
		r.CompletionTokens, r.CostCNYMilli)
	if err != nil {
		return 0, fmt.Errorf("ledger insert expense_ledger: %w", err)
	}
	return res.LastInsertId()
}

// ExpenseSummary 是某会话的费用汇总（供每日 09:00 实测核算）。
type ExpenseSummary struct {
	PromptTokens     int64
	CacheHitTokens   int64
	CacheMissTokens  int64
	CompletionTokens int64
	CostCNYMilli     int64
	Calls            int64
}

// SummarizeExpense 汇总某会话的费用。无记录时返回全零，不报错。
func (l *Ledger) SummarizeExpense(sessionID string) (ExpenseSummary, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var s ExpenseSummary
	err := l.db.QueryRow(`SELECT COUNT(*),
		COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(cache_hit_tokens),0),
		COALESCE(SUM(cache_miss_tokens),0), COALESCE(SUM(completion_tokens),0),
		COALESCE(SUM(cost_cny_milli),0)
		FROM expense_ledger WHERE session_id = ?`, sessionID).Scan(
		&s.Calls, &s.PromptTokens, &s.CacheHitTokens, &s.CacheMissTokens,
		&s.CompletionTokens, &s.CostCNYMilli)
	if err != nil {
		return s, fmt.Errorf("ledger summarize expense_ledger: %w", err)
	}
	return s, nil
}
