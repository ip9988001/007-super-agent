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
	// 迁移：第 14 步 B-06 给 execution_ledger 加 stage_no / artifact_path
	// （Stage 边界落盘）。旧库建表时没有这两列，用 ALTER TABLE 补；
	// 列名是代码里的常量，不是外部输入。
	for _, col := range []struct{ name, typ string }{
		{"stage_no", "INTEGER NOT NULL DEFAULT 0"},
		{"artifact_path", "TEXT NOT NULL DEFAULT ''"},
	} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('execution_ledger') WHERE name = ?`, col.name).Scan(&n); err != nil {
			db.Close()
			return nil, fmt.Errorf("ledger migrate check %s: %w", col.name, err)
		}
		if n == 0 {
			if _, err := db.Exec(`ALTER TABLE execution_ledger ADD COLUMN ` + col.name + ` ` + col.typ); err != nil {
				db.Close()
				return nil, fmt.Errorf("ledger migrate add %s: %w", col.name, err)
			}
		}
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
	StageNo           int    // 第 14 步 B-06：长任务已完成 Stage 号，0 表示非分片记录
	ArtifactPath      string // 第 14 步 B-06：该 Stage 的落盘产物路径
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
		 duration_ms, status, advisor_verdict, advisor_retry_count, advisor_note,
		 stage_no, artifact_path)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		time.Now().UTC().Format(time.RFC3339), r.SessionID, r.TaskID, r.ToolID,
		r.Command, r.ExitCode, lastLines(r.StdoutTail, 20), truncateRunes(r.ErrHead, 600),
		r.DurationMs, r.Status, r.AdvisorVerdict, r.AdvisorRetryCount,
		truncateRunes(r.AdvisorNote, 600), r.StageNo, r.ArtifactPath)
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
	StageNo           int
	ArtifactPath      string
}

// ListExecutions 按任务查执行回执（最新在前，最多 limit 行）。
func (l *Ledger) ListExecutions(taskID string, limit int) ([]Execution, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	rows, err := l.db.Query(`SELECT id, ts, session_id, task_id, tool_id, command,
		exit_code, stdout_tail, err_head, duration_ms, status,
		advisor_verdict, advisor_retry_count, advisor_note,
		stage_no, artifact_path
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
			&e.Status, &e.AdvisorVerdict, &e.AdvisorRetryCount, &e.AdvisorNote,
			&e.StageNo, &e.ArtifactPath); err != nil {
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

// ---- inbox_queue：网关持久化入队队列（第 14 步 B-02）----

// InboxStatus 是 inbox_queue 一行的状态。
const (
	InboxPending = "PENDING" // 待处理
	InboxRunning = "RUNNING" // 已取走未确认
	InboxDone    = "DONE"    // 已确认
	InboxFailed  = "FAILED"  // 已失败
)

// InboxRecord 是一次入队的信。
type InboxRecord struct {
	SessionID string
	TaskID    string
	Seq       int64 // 发信方给的入站序号；0 表示发信方没给序号，不做去重
	Source    string
	Text      string
}

// InboxRow 是从 inbox_queue 读回的一行。
type InboxRow struct {
	ID         int64
	TS         string
	SessionID  string
	TaskID     string
	Seq        int64
	Source     string
	Text       string
	Status     string
	RetryCount int
	CreatedTS  string
	UpdatedTS  string
}

// EnqueueInbox 先落盘再回执：同一事务里写 PENDING 行，返回行 id。
// seq>0 且同 (task_id, seq) 已有 PENDING/RUNNING 行时视为重复投递，
// 返回 inserted=false，不写新行（调用方直接回合并回执）。
// seq==0 时每条都是独立投递，恒插入。失败返回错误，调用方不得发回执。
func (l *Ledger) EnqueueInbox(r InboxRecord) (id int64, inserted bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := l.db.Begin()
	if err != nil {
		return 0, false, fmt.Errorf("ledger inbox begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if r.Seq > 0 {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM inbox_queue
			WHERE task_id = ? AND inbound_seq = ? AND status IN ('PENDING','RUNNING')`,
			r.TaskID, r.Seq).Scan(&n); err != nil {
			return 0, false, fmt.Errorf("ledger inbox dedup check: %w", err)
		}
		if n > 0 {
			return 0, false, nil
		}
	}
	res, err := tx.Exec(`INSERT INTO inbox_queue
		(ts, session_id, task_id, inbound_seq, source, text, status, retry_count, created_ts, updated_ts)
		VALUES (?, ?, ?, ?, ?, ?, 'PENDING', 0, ?, ?)`,
		now, r.SessionID, r.TaskID, r.Seq, r.Source, r.Text, now, now)
	if err != nil {
		return 0, false, fmt.Errorf("ledger inbox insert: %w", err)
	}
	id, err = res.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("ledger inbox last id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("ledger inbox commit: %w", err)
	}
	return id, true, nil
}

// ListInboxByStatus 按状态读 inbox_queue 行（启动重放用，按 id 升序）。
func (l *Ledger) ListInboxByStatus(status string) ([]InboxRow, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	rows, err := l.db.Query(`SELECT id, ts, session_id, task_id, inbound_seq,
		source, text, status, retry_count, created_ts, updated_ts
		FROM inbox_queue WHERE status = ? ORDER BY id ASC`, status)
	if err != nil {
		return nil, fmt.Errorf("ledger query inbox_queue: %w", err)
	}
	defer rows.Close()
	return scanInboxRows(rows)
}

// ListStaleInbox 读出超阈值未确认的 RUNNING 行（水位重排用）：
// updated_ts 早于 now-threshold 且 retry_count==0 的才算（只重排 1 次）。
func (l *Ledger) ListStaleInbox(olderThan time.Duration) ([]InboxRow, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	cutoff := time.Now().UTC().Add(-olderThan).Format(time.RFC3339)
	rows, err := l.db.Query(`SELECT id, ts, session_id, task_id, inbound_seq,
		source, text, status, retry_count, created_ts, updated_ts
		FROM inbox_queue WHERE status = 'RUNNING' AND retry_count = 0
		AND updated_ts < ? ORDER BY id ASC`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("ledger query stale inbox: %w", err)
	}
	defer rows.Close()
	return scanInboxRows(rows)
}

func scanInboxRows(rows *sql.Rows) ([]InboxRow, error) {
	var out []InboxRow
	for rows.Next() {
		var r InboxRow
		if err := rows.Scan(&r.ID, &r.TS, &r.SessionID, &r.TaskID, &r.Seq,
			&r.Source, &r.Text, &r.Status, &r.RetryCount, &r.CreatedTS, &r.UpdatedTS); err != nil {
			return nil, fmt.Errorf("ledger scan inbox_queue: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkInboxRunning 按行 id 把一行 PENDING 行改成 RUNNING（取走待确认）。
// 只在当前状态为 PENDING 时才改。按行 id 改，不按 (task_id, seq) 改——
// seq==0 的多条投递共享 task_id。
func (l *Ledger) MarkInboxRunning(id int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	res, err := l.db.Exec(`UPDATE inbox_queue SET status = 'RUNNING', updated_ts = ?
		WHERE id = ? AND status = 'PENDING'`,
		time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return fmt.Errorf("ledger inbox mark RUNNING: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("ledger inbox mark RUNNING: id %d 不是 PENDING 行", id)
	}
	return nil
}

// MarkInboxDone 按行 id 把一行 RUNNING 行改成 DONE（已确认）。
// 只在当前状态为 RUNNING 时才改——被水位重排回 PENDING 的行不会被
// 原始执行误标为 DONE（重排后的副本仍会走自己的 RUNNING→DONE）。
func (l *Ledger) MarkInboxDone(id int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	res, err := l.db.Exec(`UPDATE inbox_queue SET status = 'DONE', updated_ts = ?
		WHERE id = ? AND status = 'RUNNING'`,
		time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return fmt.Errorf("ledger inbox mark DONE: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("ledger inbox mark DONE: id %d 不是 RUNNING 行", id)
	}
	return nil
}

// MarkInboxFailed 按行 id 把一行 RUNNING 行改成 FAILED（执行时崩溃）。
func (l *Ledger) MarkInboxFailed(id int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	res, err := l.db.Exec(`UPDATE inbox_queue SET status = 'FAILED', updated_ts = ?
		WHERE id = ? AND status = 'RUNNING'`,
		time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return fmt.Errorf("ledger inbox mark FAILED: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("ledger inbox mark FAILED: id %d 不是 RUNNING 行", id)
	}
	return nil
}

// RequeueInbox 按行 id 把一行 RUNNING 行重排回 PENDING（水位超时自动重排 1 次）：
// retry_count+1，调用方只对 retry_count==0 的行调本方法。
func (l *Ledger) RequeueInbox(id int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	res, err := l.db.Exec(`UPDATE inbox_queue
		SET status = 'PENDING', retry_count = retry_count + 1, updated_ts = ?
		WHERE id = ? AND status = 'RUNNING' AND retry_count = 0`,
		time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return fmt.Errorf("ledger inbox requeue: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("ledger inbox requeue: id %d 不是可重排的 RUNNING 行", id)
	}
	return nil
}

// ---- Stage 边界落盘（第 14 步 B-06）----

// StageRecord 是长任务一个已完成 Stage 的落盘记录。
type StageRecord struct {
	SessionID    string
	TaskID       string
	ToolID       string
	StageNo      int    // 从 1 开始编号
	ArtifactPath string // 该 Stage 的落盘产物路径（为空表示无产物）
	Command      string // 该 Stage 跑过的命令摘要（为空时自动填 stage <n>）
}

// RecordStage 把一个已完成 Stage 写进 execution_ledger（stage_no /
// artifact_path 两列）。失败返回错误，不许静默吞掉。
func (l *Ledger) RecordStage(r StageRecord) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cmd := r.Command
	if cmd == "" {
		cmd = fmt.Sprintf("stage %d", r.StageNo)
	}
	res, err := l.db.Exec(`INSERT INTO execution_ledger
		(ts, session_id, task_id, tool_id, command, exit_code, stdout_tail, err_head,
		 duration_ms, status, advisor_verdict, advisor_retry_count, advisor_note,
		 stage_no, artifact_path)
		VALUES (?, ?, ?, ?, ?, 0, '', '', 0, 'success', 'NONE', 0, '', ?, ?)`,
		time.Now().UTC().Format(time.RFC3339), r.SessionID, r.TaskID, r.ToolID,
		cmd, r.StageNo, r.ArtifactPath)
	if err != nil {
		return 0, fmt.Errorf("ledger insert stage: %w", err)
	}
	return res.LastInsertId()
}

// LastStage 查某任务某工具最后一个已完成 Stage（重跑时从下一 Stage 接续）。
// 无记录时 found=false，不报错。
func (l *Ledger) LastStage(taskID, toolID string) (stageNo int, artifactPath string, found bool, err error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	err = l.db.QueryRow(`SELECT stage_no, artifact_path FROM execution_ledger
		WHERE task_id = ? AND tool_id = ? AND stage_no > 0
		ORDER BY stage_no DESC LIMIT 1`, taskID, toolID).Scan(&stageNo, &artifactPath)
	if err == sql.ErrNoRows {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, fmt.Errorf("ledger query last stage: %w", err)
	}
	return stageNo, artifactPath, true, nil
}
