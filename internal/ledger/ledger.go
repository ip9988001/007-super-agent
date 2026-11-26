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

	"007-super-agent/internal/sanitize"

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
	if _, err := db.Exec(`PRAGMA synchronous=NORMAL`); err != nil {
		db.Close()
		return nil, fmt.Errorf("ledger pragma synchronous: %w", err)
	}
	if _, err := db.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		db.Close()
		return nil, fmt.Errorf("ledger pragma busy_timeout: %w", err)
	}
	// 迁移（线 A）：旧库的 inbox_queue 没有 deadline 列，而 schema.sql 里
	// 的 idx_inbox_deadline 索引引用该列——必须在建表之前先补列，
	// 否则旧库执行建表语句会在建索引时报 no such column: deadline。
	// 表不存在（新库）时跳过，建表语句自带该列。
	{
		var hasTable int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'inbox_queue'`).Scan(&hasTable); err != nil {
			db.Close()
			return nil, fmt.Errorf("ledger migrate check inbox table: %w", err)
		}
		if hasTable > 0 {
			var n int
			if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('inbox_queue') WHERE name = 'deadline'`).Scan(&n); err != nil {
				db.Close()
				return nil, fmt.Errorf("ledger migrate check inbox deadline: %w", err)
			}
			if n == 0 {
				if _, err := db.Exec(`ALTER TABLE inbox_queue ADD COLUMN deadline TEXT NOT NULL DEFAULT ''`); err != nil {
					db.Close()
					return nil, fmt.Errorf("ledger migrate add inbox deadline: %w", err)
				}
			}
		}
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
	// 迁移（线 A P0 修复）：dispatch_events.tokens 改名 dispatch_est_tokens。
	// 旧库已有 tokens 列时改名（数据保留）；新库建表已用新列名，跳过。
	// 先查表在不在（新库空表启动时表还不存在，跳过不报错）。
	{
		var hasTable int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'dispatch_events'`).Scan(&hasTable); err != nil {
			db.Close()
			return nil, fmt.Errorf("ledger migrate check dispatch_events table: %w", err)
		}
		if hasTable > 0 {
			var hasOld, hasNew int
			if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('dispatch_events') WHERE name = 'tokens'`).Scan(&hasOld); err != nil {
				db.Close()
				return nil, fmt.Errorf("ledger migrate check dispatch_events tokens: %w", err)
			}
			if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('dispatch_events') WHERE name = 'dispatch_est_tokens'`).Scan(&hasNew); err != nil {
				db.Close()
				return nil, fmt.Errorf("ledger migrate check dispatch_events dispatch_est_tokens: %w", err)
			}
			if hasOld > 0 && hasNew == 0 {
				if _, err := db.Exec(`ALTER TABLE dispatch_events RENAME COLUMN tokens TO dispatch_est_tokens`); err != nil {
					db.Close()
					return nil, fmt.Errorf("ledger migrate rename dispatch_events tokens: %w", err)
				}
			}
		}
	}
	// 迁移（规划 v2）：expense_ledger 加 role 列（分角色记账：front_plan /
	// front_judge / executor；空串表示旧行或未分角色的调用，如分诊/监工）。
	// 放在 schemaSQL 之后，表一定存在。
	{
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('expense_ledger') WHERE name = 'role'`).Scan(&n); err != nil {
			db.Close()
			return nil, fmt.Errorf("ledger migrate check expense role: %w", err)
		}
		if n == 0 {
			if _, err := db.Exec(`ALTER TABLE expense_ledger ADD COLUMN role TEXT NOT NULL DEFAULT ''`); err != nil {
				db.Close()
				return nil, fmt.Errorf("ledger migrate add expense role: %w", err)
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

// MaxExecutionID 取 execution_ledger 当前最大 id（无行时为 0）。
// H-2：computeFactList 按“本次 runAgent 执行窗口”统计调用数——
// 调用方在 runAgent 开始前记下 MaxExecutionID，只统计 id 大于它的行，
// 不把之前步骤的账本行算进当前步（多步共用同一 taskID 时，第 2 步
// 0 调用也不能继承第 1 步的账本）。
func (l *Ledger) MaxExecutionID() (int64, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var n int64
	if err := l.db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM execution_ledger`).Scan(&n); err != nil {
		return 0, fmt.Errorf("ledger max execution id: %w", err)
	}
	return n, nil
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

// ToolStats 是某工具自 since 以来的调用统计（第 23 步试用期跟踪用）：
// 总调用数、失败数、末尾连续失败数。since 为零值时不限时间。
// 只统计 status 为 success/failure 的真实执行行（shadow 行不计）。
func (l *Ledger) ToolStats(toolID string, since time.Time) (calls, failures, consecutiveFailures int, err error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var rows *sql.Rows
	if since.IsZero() {
		rows, err = l.db.Query(`SELECT status FROM execution_ledger
			WHERE tool_id = ? AND status IN ('success','failure') ORDER BY id DESC`, toolID)
	} else {
		rows, err = l.db.Query(`SELECT status FROM execution_ledger
			WHERE tool_id = ? AND status IN ('success','failure') AND ts >= ?
			ORDER BY id DESC`, toolID, since.UTC().Format(time.RFC3339))
	}
	if err != nil {
		return 0, 0, 0, fmt.Errorf("ledger tool stats: %w", err)
	}
	defer rows.Close()
	trailing := true
	for rows.Next() {
		var status string
		if err := rows.Scan(&status); err != nil {
			return 0, 0, 0, fmt.Errorf("ledger tool stats scan: %w", err)
		}
		calls++
		if status == "failure" {
			failures++
			if trailing {
				consecutiveFailures++
			}
		} else {
			trailing = false
		}
	}
	return calls, failures, consecutiveFailures, rows.Err()
}

// CountUnsupportedFailures 统计某工具近 days 天内因“不支持该操作”类原因
// 的失败次数（第 23 步缺口检测规则 3 用）：err_head 含 unsupported /
// not supported / 不支持（大小写不敏感）即算。days<=0 时不限时间。
func (l *Ledger) CountUnsupportedFailures(toolID string, days int) (int, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	q := `SELECT COUNT(*) FROM execution_ledger
		WHERE tool_id = ? AND status = 'failure'
		AND (lower(err_head) LIKE '%unsupported%' OR lower(err_head) LIKE '%not supported%' OR err_head LIKE '%不支持%')`
	var args []any
	args = append(args, toolID)
	if days > 0 {
		since := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour).Format(time.RFC3339)
		q += ` AND ts >= ?`
		args = append(args, since)
	}
	var n int
	if err := l.db.QueryRow(q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("ledger count unsupported failures: %w", err)
	}
	return n, nil
}

// FailedInputs 取某工具最近的失败输入（第 23 步：缺口类需求回放真实历史
// 失败输入用）：返回最近 limit 条 failure 行（最新在前，含 command 与
// err_head）。limit<=0 时默认取 5 条。
func (l *Ledger) FailedInputs(toolID string, limit int) ([]Execution, error) {
	if limit <= 0 {
		limit = 5
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	rows, err := l.db.Query(`SELECT id, ts, session_id, task_id, tool_id, command,
		exit_code, stdout_tail, err_head, duration_ms, status,
		advisor_verdict, advisor_retry_count, advisor_note,
		stage_no, artifact_path
		FROM execution_ledger WHERE tool_id = ? AND status = 'failure'
		ORDER BY id DESC LIMIT ?`, toolID, limit)
	if err != nil {
		return nil, fmt.Errorf("ledger query failed inputs: %w", err)
	}
	defer rows.Close()
	var out []Execution
	for rows.Next() {
		var e Execution
		if err := rows.Scan(&e.ID, &e.TS, &e.SessionID, &e.TaskID, &e.ToolID,
			&e.Command, &e.ExitCode, &e.StdoutTail, &e.ErrHead, &e.DurationMs,
			&e.Status, &e.AdvisorVerdict, &e.AdvisorRetryCount, &e.AdvisorNote,
			&e.StageNo, &e.ArtifactPath); err != nil {
			return nil, fmt.Errorf("ledger scan failed inputs: %w", err)
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
// Role 是调用角色（规划 v2）：front_plan（前置规划）/ front_judge
// （前置判回执）/ executor（执行层）；空串表示旧行或未分角色的调用。
type ExpenseRecord struct {
	SessionID        string
	TaskID           string
	Model            string
	Role             string
	PromptTokens     int64
	CacheHitTokens   int64
	CacheMissTokens  int64
	CompletionTokens int64
	CostCNYMilli     int64
}

// Role 常量（规划 v2 分角色记账）：前置规划 / 前置判回执 / 执行层。
// ExpenseRoleRollover 是翻新摘要的独立角色（Gemini 审阅 M-5）：
// 摘要费用不计入规划 ¥0.05 熔断（planCost 只算 front_plan +
// front_judge），但仍记入总账本。
// ExpenseRoleAdvisor / ExpenseRoleFrontSummary（H-3）：监工审交付与
// 最终汇报各有专属标签，不再留空 role——空 role 的调用绕过费用监控，
// 全任务总费用算不全。
const (
	ExpenseRoleFrontPlan    = "front_plan"
	ExpenseRoleFrontJudge   = "front_judge"
	ExpenseRoleExecutor     = "executor"
	ExpenseRoleRollover     = "rollover"
	ExpenseRoleAdvisor      = "advisor"
	ExpenseRoleFrontSummary = "front_summary"
)

// RecordExpense 写一行费用账。失败返回错误。
func (l *Ledger) RecordExpense(r ExpenseRecord) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	res, err := l.db.Exec(`INSERT INTO expense_ledger
		(ts, session_id, task_id, model, role, prompt_tokens, cache_hit_tokens,
		 cache_miss_tokens, completion_tokens, cost_cny_milli)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		time.Now().UTC().Format(time.RFC3339), r.SessionID, r.TaskID, r.Model,
		r.Role, r.PromptTokens, r.CacheHitTokens, r.CacheMissTokens,
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

// TaskExpense 汇总某任务的模型调用次数与费用（供派单回执用）。
// 无记录时返回全零，不报错。只读，不写库。
func (l *Ledger) TaskExpense(taskID string) (calls, costCNYMilli int64, err error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	err = l.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(cost_cny_milli),0)
		FROM expense_ledger WHERE task_id = ?`, taskID).Scan(&calls, &costCNYMilli)
	if err != nil {
		return 0, 0, fmt.Errorf("ledger task expense: %w", err)
	}
	return calls, costCNYMilli, nil
}

// TaskCostByRoles 按角色汇总某任务的费用（规划 v2 硬熔断用）：
// 只加指定角色的 cost_cny_milli 之和（单位 0.001 元）。
// 无记录时返回 0，不报错。只读，不写库。
func (l *Ledger) TaskCostByRoles(taskID string, roles ...string) (int64, error) {
	if len(roles) == 0 {
		return 0, nil
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	placeholders := make([]string, len(roles))
	args := make([]any, 0, len(roles)+1)
	args = append(args, taskID)
	for i, r := range roles {
		placeholders[i] = "?"
		args = append(args, r)
	}
	q := `SELECT COALESCE(SUM(cost_cny_milli),0) FROM expense_ledger
		WHERE task_id = ? AND role IN (` + strings.Join(placeholders, ",") + `)`
	var cost int64
	if err := l.db.QueryRow(q, args...).Scan(&cost); err != nil {
		return 0, fmt.Errorf("ledger task cost by roles: %w", err)
	}
	return cost, nil
}

// ---- inbox_queue：网关持久化入队队列（第 14 步 B-02）----

// InboxStatus 是 inbox_queue 一行的状态。
const (
	InboxPending  = "PENDING"  // 待处理
	InboxRunning  = "RUNNING"  // 已取走未确认
	InboxDone     = "DONE"     // 已确认
	InboxFailed   = "FAILED"   // 已失败
	InboxRejected = "REJECTED" // 前置校验拒绝（线 A：派单核心指令超限等）
	InboxTimeout  = "TIMEOUT"  // 看门狗超时置位（线 A）
)

// MaxInboxRequeues 是水位扫荡对同一行的最大重排次数（P0 修复）：
// 超期 RUNNING 行最多重排 2 次；第 3 次超期直接置 FAILED 并写失败回信，
// 绝不让“重排 → 重写 deadline”无限死循环。
const MaxInboxRequeues = 2

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
	Deadline   string // RFC3339 UTC，RUNNING 行的终结期限；为空表示旧行未设
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
		source, text, status, retry_count, deadline, created_ts, updated_ts
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
		source, text, status, retry_count, deadline, created_ts, updated_ts
		FROM inbox_queue WHERE status = 'RUNNING' AND retry_count < ?
		AND updated_ts < ? ORDER BY id ASC`, MaxInboxRequeues, cutoff)
	if err != nil {
		return nil, fmt.Errorf("ledger query stale inbox: %w", err)
	}
	defer rows.Close()
	return scanInboxRows(rows)
}

// ListExhaustedStaleInbox 读出已超期且重排次数已用完的 RUNNING 行
// （retry_count >= MaxInboxRequeues）：调用方不再重排，直接置 FAILED
// 并写失败回信。
func (l *Ledger) ListExhaustedStaleInbox(olderThan time.Duration) ([]InboxRow, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	cutoff := time.Now().UTC().Add(-olderThan).Format(time.RFC3339)
	rows, err := l.db.Query(`SELECT id, ts, session_id, task_id, inbound_seq,
		source, text, status, retry_count, deadline, created_ts, updated_ts
		FROM inbox_queue WHERE status = 'RUNNING' AND retry_count >= ?
		AND updated_ts < ? ORDER BY id ASC`, MaxInboxRequeues, cutoff)
	if err != nil {
		return nil, fmt.Errorf("ledger query exhausted stale inbox: %w", err)
	}
	defer rows.Close()
	return scanInboxRows(rows)
}

func scanInboxRows(rows *sql.Rows) ([]InboxRow, error) {
	var out []InboxRow
	for rows.Next() {
		var r InboxRow
		if err := rows.Scan(&r.ID, &r.TS, &r.SessionID, &r.TaskID, &r.Seq,
			&r.Source, &r.Text, &r.Status, &r.RetryCount, &r.Deadline, &r.CreatedTS, &r.UpdatedTS); err != nil {
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

// RequeueInbox 按行 id 把一行 RUNNING 行重排回 PENDING（水位超时自动重排）：
// retry_count+1，最多重排 MaxInboxRequeues 次；调用方只对
// retry_count < MaxInboxRequeues 的行调本方法（超限的行走 FAILED）。
func (l *Ledger) RequeueInbox(id int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	res, err := l.db.Exec(`UPDATE inbox_queue
		SET status = 'PENDING', retry_count = retry_count + 1, updated_ts = ?
		WHERE id = ? AND status = 'RUNNING' AND retry_count < ?`,
		time.Now().UTC().Format(time.RFC3339), id, MaxInboxRequeues)
	if err != nil {
		return fmt.Errorf("ledger inbox requeue: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("ledger inbox requeue: id %d 不是可重排的 RUNNING 行", id)
	}
	return nil
}

// MarkInboxRejected 按行 id 把一行 RUNNING 行改成 REJECTED（前置校验拒绝，
// 如派单核心指令超限）。只在当前状态为 RUNNING 时才改。
func (l *Ledger) MarkInboxRejected(id int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	res, err := l.db.Exec(`UPDATE inbox_queue SET status = 'REJECTED', updated_ts = ?
		WHERE id = ? AND status = 'RUNNING'`,
		time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return fmt.Errorf("ledger inbox mark REJECTED: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("ledger inbox mark REJECTED: id %d 不是 RUNNING 行", id)
	}
	return nil
}

// MarkInboxTimeout 按行 id 把一行 RUNNING 行改成 TIMEOUT（看门狗超时置位）。
// 只在当前状态为 RUNNING 时才改。
func (l *Ledger) MarkInboxTimeout(id int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	res, err := l.db.Exec(`UPDATE inbox_queue SET status = 'TIMEOUT', updated_ts = ?
		WHERE id = ? AND status = 'RUNNING'`,
		time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return fmt.Errorf("ledger inbox mark TIMEOUT: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("ledger inbox mark TIMEOUT: id %d 不是 RUNNING 行", id)
	}
	return nil
}

// SetInboxDeadline 按行 id 写 deadline（RUNNING 行的终结期限）。
// 只在当前状态为 RUNNING 时才改——取走时由网关在 MarkInboxRunning
// 之后立即调；重排回 PENDING 的行 deadline 不清零，下次取走时重写。
func (l *Ledger) SetInboxDeadline(id int64, deadline time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	res, err := l.db.Exec(`UPDATE inbox_queue SET deadline = ?, updated_ts = ?
		WHERE id = ? AND status = 'RUNNING'`,
		deadline.UTC().Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return fmt.Errorf("ledger inbox set deadline: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("ledger inbox set deadline: id %d 不是 RUNNING 行", id)
	}
	return nil
}

// TouchInboxRunning 按 task_id 续期 RUNNING 行的 deadline 与 updated_ts
// （规划 v2 看门狗续期）：长计划（多步判回执）跑过 1800 秒时，前置在每步
// 派发前调一次，把 deadline 推后 1800 秒并刷新 updated_ts，免得
// sweepStale / 外部看门狗把还在干活的计划当死任务重排或置 TIMEOUT。
// 同一 task_id 有多行 RUNNING 时（seq==0 的多条投递）全部续期；
// 没有 RUNNING 行时返回错误（调用方只记日志，不阻塞任务）。
func (l *Ledger) TouchInboxRunning(taskID string, deadline time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	res, err := l.db.Exec(`UPDATE inbox_queue SET deadline = ?, updated_ts = ?
		WHERE task_id = ? AND status = 'RUNNING'`,
		deadline.UTC().Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339), taskID)
	if err != nil {
		return fmt.Errorf("ledger inbox touch running: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("ledger inbox touch running: task %s 没有 RUNNING 行", taskID)
	}
	return nil
}

// ListOverdueInbox 读出已超期未终结的 RUNNING 行（看门狗扫荡用）：
// deadline 已设且早于 now 的；或 deadline 为空（线 A 之前的旧行）
// 且 updated_ts 早于 now-legacyAge 的。按 id 升序。
func (l *Ledger) ListOverdueInbox(now time.Time, legacyAge time.Duration) ([]InboxRow, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	nowStr := now.UTC().Format(time.RFC3339)
	legacyCutoff := now.Add(-legacyAge).UTC().Format(time.RFC3339)
	rows, err := l.db.Query(`SELECT id, ts, session_id, task_id, inbound_seq,
		source, text, status, retry_count, deadline, created_ts, updated_ts
		FROM inbox_queue WHERE status = 'RUNNING' AND
		((deadline != '' AND deadline < ?) OR (deadline = '' AND updated_ts < ?))
		ORDER BY id ASC`, nowStr, legacyCutoff)
	if err != nil {
		return nil, fmt.Errorf("ledger query overdue inbox: %w", err)
	}
	defer rows.Close()
	return scanInboxRows(rows)
}

// ---- dispatch_events：派单预算事件（线 A）----

// DispatchEvent 是一次派单预算事件：软告警 / 分级裁减 / 超限拒绝 / 看门狗超时。
// 只记事件摘要，不记派单全文、不记密钥。
// session_repair / session_resume 复用这张表记会话修复与续跑事件
// （P2-a 规格 §4.3：修复后记一笔 dispatch_event）。
// DispatchEstTokens 是派单估算 token 数（EstimateDispatchTokens 保守口径），
// 字段名刻意与账本费用 token 数区分开，避免线上排障时混淆（P0 修复）。
type DispatchEvent struct {
	SessionID         string
	TaskID            string
	DispatchID        string
	Kind              string // soft_alert / trimmed / overflow_rejected / watchdog_timeout
	DispatchEstTokens int
	Detail            string // 人话摘要（截断 ≤600 字）
}

// 派单预算事件种类（与 schema.sql dispatch_events.kind 对应）。
const (
	DispatchEventSoftAlert        = "soft_alert"
	DispatchEventTrimmed          = "trimmed"
	DispatchEventOverflowRejected = "overflow_rejected"
	DispatchEventWatchdogTimeout  = "watchdog_timeout"
	// 会话事件（P2-a）：步历史修复（截残行/补闭合/丢弃）与断点续跑。
	DispatchEventSessionRepair = "session_repair"
	DispatchEventSessionResume = "session_resume"
	// DispatchEventSessionRollover 是会话翻新事件（P2-b）：水位超阈值，
	// 摘要续接到新会话，旧会话原地保留。
	DispatchEventSessionRollover = "session_rollover"
	// DispatchEventSessionArchive 是会话归档事件（P2-b）：已终结且
	// 7 天未活动的会话挪归档文件夹（不删）。
	DispatchEventSessionArchive = "session_archive"
	// 规划事件（规划 v2）：某步完成时网关发的异步回调事件（session_id、
	// step_no、回执摘要、事实清单摘要）；串行调度器凭它叫醒前置。
	DispatchEventPlanStepCallback = "plan_step_callback"
)

// RecordDispatchEvent 写一行派单预算事件。失败返回错误，不许静默吞掉。
func (l *Ledger) RecordDispatchEvent(e DispatchEvent) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	res, err := l.db.Exec(`INSERT INTO dispatch_events
		(ts, session_id, task_id, dispatch_id, kind, dispatch_est_tokens, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		time.Now().UTC().Format(time.RFC3339),
		e.SessionID, e.TaskID, e.DispatchID, e.Kind, e.DispatchEstTokens,
		truncateRunes(e.Detail, 600),
	)
	if err != nil {
		return 0, fmt.Errorf("ledger record dispatch event: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("ledger dispatch event last id: %w", err)
	}
	return id, nil
}

// ListDispatchEvents 按任务号读派单预算事件（审计与测试用，按 id 升序）。
func (l *Ledger) ListDispatchEvents(taskID string) ([]DispatchEvent, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	rows, err := l.db.Query(`SELECT session_id, task_id, dispatch_id, kind, dispatch_est_tokens, detail
		FROM dispatch_events WHERE task_id = ? ORDER BY id ASC`, taskID)
	if err != nil {
		return nil, fmt.Errorf("ledger list dispatch events: %w", err)
	}
	defer rows.Close()
	var out []DispatchEvent
	for rows.Next() {
		var e DispatchEvent
		if err := rows.Scan(&e.SessionID, &e.TaskID, &e.DispatchID, &e.Kind, &e.DispatchEstTokens, &e.Detail); err != nil {
			return nil, fmt.Errorf("ledger scan dispatch event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
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

// ---- 自进化提议表（第 20 步）----

// ProposalStatus 是 selfevolve_proposals 一行的状态。
const (
	ProposalPending  = "pending"  // 待人批（宪法/分诊提示词/代码类提议只到这一步）
	ProposalApplied  = "applied"  // 已应用（仅注册表元数据修正可自动到这一步）
	ProposalRejected = "rejected" // 应用失败（探针未通过等）
)

// Proposal 是自进化提议表里的一行。
type Proposal struct {
	ID            int64
	ProposalID    string // se-YYYY-MM-DD-NNN，全局唯一
	Date          string // 自查日期（北京时间 YYYY-MM-DD）
	Signature     string // 问题签名，如 exec_fail:cap-code-run-audit
	EvidenceCount int
	Text          string
	Status        string // pending / applied / rejected
	CreatedTS     string
}

// RecordProposal 写一条提议（初始状态一律 pending；应用与否由调用方
// 再调 UpdateProposalStatus）。proposal_id 重复时返回错误。
func (l *Ledger) RecordProposal(p Proposal) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if p.Status == "" {
		p.Status = ProposalPending
	}
	res, err := l.db.Exec(`INSERT INTO selfevolve_proposals
		(proposal_id, date, signature, evidence_count, proposal_text, status, created_ts)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		p.ProposalID, p.Date, p.Signature, p.EvidenceCount,
		truncateRunes(p.Text, 2000), p.Status, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return 0, fmt.Errorf("ledger insert selfevolve_proposals: %w", err)
	}
	return res.LastInsertId()
}

// CountProposalsByDate 数某天已有的提议数（proposal_id 编号用：se-日期-NNN）。
func (l *Ledger) CountProposalsByDate(date string) (int, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var n int
	if err := l.db.QueryRow(`SELECT COUNT(*) FROM selfevolve_proposals WHERE date = ?`, date).Scan(&n); err != nil {
		return 0, fmt.Errorf("ledger count proposals: %w", err)
	}
	return n, nil
}

// FindProposalBySignature 查某签名最近 7 天内是否已有提议（任一状态）：
// 有则返回该行（最新一条），供自查去重——同一问题一周内只提一次，
// 不重复刷屏。无则 found=false，不报错。
func (l *Ledger) FindProposalBySignature(signature string, days int) (Proposal, bool, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var p Proposal
	err := l.db.QueryRow(`SELECT id, proposal_id, date, signature,
		evidence_count, proposal_text, status, created_ts
		FROM selfevolve_proposals
		WHERE signature = ? AND created_ts >= datetime('now', ?)
		ORDER BY id DESC LIMIT 1`, signature, fmt.Sprintf("-%d days", days)).Scan(
		&p.ID, &p.ProposalID, &p.Date, &p.Signature,
		&p.EvidenceCount, &p.Text, &p.Status, &p.CreatedTS)
	if err == sql.ErrNoRows {
		return p, false, nil
	}
	if err != nil {
		return p, false, fmt.Errorf("ledger find proposal: %w", err)
	}
	return p, true, nil
}

// ListProposals 按状态读提议（最新在前，最多 limit 行；status 为空时读全部）。
func (l *Ledger) ListProposals(status string, limit int) ([]Proposal, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var rows *sql.Rows
	var err error
	if status == "" {
		rows, err = l.db.Query(`SELECT id, proposal_id, date, signature,
			evidence_count, proposal_text, status, created_ts
			FROM selfevolve_proposals ORDER BY id DESC LIMIT ?`, limit)
	} else {
		rows, err = l.db.Query(`SELECT id, proposal_id, date, signature,
			evidence_count, proposal_text, status, created_ts
			FROM selfevolve_proposals WHERE status = ? ORDER BY id DESC LIMIT ?`, status, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("ledger list proposals: %w", err)
	}
	defer rows.Close()
	var out []Proposal
	for rows.Next() {
		var p Proposal
		if err := rows.Scan(&p.ID, &p.ProposalID, &p.Date, &p.Signature,
			&p.EvidenceCount, &p.Text, &p.Status, &p.CreatedTS); err != nil {
			return nil, fmt.Errorf("ledger scan proposals: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpdateProposalStatus 按 proposal_id 改提议状态。状态不在
// pending/applied/rejected 三者之内时拒绝；行不存在时返回错误。
func (l *Ledger) UpdateProposalStatus(proposalID, status string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch status {
	case ProposalPending, ProposalApplied, ProposalRejected:
	default:
		return fmt.Errorf("ledger update proposal %s: 非法状态 %q", proposalID, status)
	}
	res, err := l.db.Exec(`UPDATE selfevolve_proposals SET status = ?
		WHERE proposal_id = ?`, status, proposalID)
	if err != nil {
		return fmt.Errorf("ledger update proposal: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("ledger update proposal: %s 不存在", proposalID)
	}
	return nil
}

// ---- 多步骤计划表（第 21 步）----

// PlanRun 是 plan_runs 里的一行：一次 PlanAndVerify 的结论。
type PlanRun struct {
	ID                 int64
	PlanID             string // pl-YYYY-MM-DD-NNN，全局唯一
	TaskID             string
	TaskText           string
	Steps              int
	PlanJSON           string // 规划器输出的步骤表原文
	Verdict            string // APPROVE / REVISE / REJECT
	ReviseRounds       int
	EstimatedCostMilli int64
	CreatedTS          string
}

// RecordPlanRun 写一行计划结论。plan_id 重复时返回错误。
func (l *Ledger) RecordPlanRun(p PlanRun) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	res, err := l.db.Exec(`INSERT INTO plan_runs
		(plan_id, task_id, task_text, steps, plan_json, verdict,
		 revise_rounds, estimated_cost_milli, created_ts)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.PlanID, p.TaskID, truncateRunes(p.TaskText, 5000), p.Steps,
		p.PlanJSON, p.Verdict, p.ReviseRounds, p.EstimatedCostMilli,
		time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return 0, fmt.Errorf("ledger insert plan_runs: %w", err)
	}
	return res.LastInsertId()
}

// CountPlanRunsByDate 数某天已有的计划数（plan_id 编号用：pl-日期-NNN）。
func (l *Ledger) CountPlanRunsByDate(date string) (int, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var n int
	// created_ts 是 UTC，plan_id 里的日期是北京时间：按 plan_id 数更准。
	// GLOB 精确匹配 pl-日期-NNN（3 位数字），重规划的 -r1 后缀行不算进来。
	if err := l.db.QueryRow(`SELECT COUNT(*) FROM plan_runs WHERE plan_id GLOB ?`,
		"pl-"+date+"-[0-9][0-9][0-9]").Scan(&n); err != nil {
		return 0, fmt.Errorf("ledger count plan runs: %w", err)
	}
	return n, nil
}

// ListPlanRuns 按任务读计划结论（最新在前，最多 limit 行）。
func (l *Ledger) ListPlanRuns(taskID string, limit int) ([]PlanRun, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	rows, err := l.db.Query(`SELECT id, plan_id, task_id, task_text, steps,
		plan_json, verdict, revise_rounds, estimated_cost_milli, created_ts
		FROM plan_runs WHERE task_id = ? ORDER BY id DESC LIMIT ?`, taskID, limit)
	if err != nil {
		return nil, fmt.Errorf("ledger list plan runs: %w", err)
	}
	defer rows.Close()
	var out []PlanRun
	for rows.Next() {
		var p PlanRun
		if err := rows.Scan(&p.ID, &p.PlanID, &p.TaskID, &p.TaskText, &p.Steps,
			&p.PlanJSON, &p.Verdict, &p.ReviseRounds, &p.EstimatedCostMilli,
			&p.CreatedTS); err != nil {
			return nil, fmt.Errorf("ledger scan plan runs: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---- 自查信号聚合（第 20 步）----

// SignalSample 是一条失败样本（供模型点评时看错误原文，最多 5 条）。
type SignalSample struct {
	ToolID  string
	Command string
	ErrHead string
}

// SignalCounts 是最近 N 天账本聚合的信号：按 tool_id 计数的四类失败 +
// 分诊错选代理信号 + 当日费用与前 7 日均值的对比。所有日期口径
// 均为北京时间（ts 是 UTC 存的，date(ts,'+8 hours') 转北京日期）。
type SignalCounts struct {
	ExecFailures              map[string]int // 执行失败（真跑失败，command 不以 probe 开头）
	ProbeFailures             map[string]int // 探针失败（command 以 probe 开头）
	AdvisorHalts              map[string]int // 监工 HALT
	AdvisorConcerns           map[string]int // 监工 CONCERN
	TriageMisfires            map[string]int // 分诊错选代理：Tier-1 挑了工具、执行随后失败（按任务去重）
	ExpenseTodayMilli         int64          // 当日费用（毫分）
	ExpensePrior7DayMeanMilli int64          // 前 7 日均费用（毫分，缺数据的天按 0 算）
	ExpenseSpike              bool           // 当日费用 > 2× 前 7 日均值（且均值 >0）
	ExpenseSpikeDays          int            // 7 天内费用突增天数（以今日前 7 日均值为基准；
	// CountFor("expense_spike") 返回该天数，突增天天数 ≥3 才够提议阈值）
	Samples []SignalSample
}

// signalKind 是问题签名的种类前缀。
const (
	SigExecFail       = "exec_fail"
	SigProbeFail      = "probe_fail"
	SigAdvisorHalt    = "advisor_halt"
	SigAdvisorConcern = "advisor_concern"
	SigTriageMisfire  = "triage_misfire"
	SigExpenseSpike   = "expense_spike"
)

// Signature 拼一条问题签名：exec_fail:cap-x；费用突增没有 tool_id，
// 就是 expense_spike 本身。
func Signature(kind, toolID string) string {
	if toolID == "" {
		return kind
	}
	return kind + ":" + toolID
}

// TotalSignals 是信号总数（各计数之和；费用突增算 1）。
func (s SignalCounts) TotalSignals() int {
	n := 0
	for _, m := range []map[string]int{s.ExecFailures, s.ProbeFailures, s.AdvisorHalts, s.AdvisorConcerns, s.TriageMisfires} {
		for _, c := range m {
			n += c
		}
	}
	if s.ExpenseSpike {
		n++
	}
	return n
}

// CountFor 按签名取 7 天内的出现次数；签名不在信号里时 ok=false。
// 费用突增签名返回 7 天内的突增天数（天级信号按天计，不按次计）。
func (s SignalCounts) CountFor(signature string) (int, bool) {
	if signature == SigExpenseSpike {
		if s.ExpenseSpikeDays > 0 {
			return s.ExpenseSpikeDays, true
		}
		return 0, false
	}
	kind, toolID, ok := strings.Cut(signature, ":")
	if !ok || toolID == "" {
		return 0, false
	}
	var m map[string]int
	switch kind {
	case SigExecFail:
		m = s.ExecFailures
	case SigProbeFail:
		m = s.ProbeFailures
	case SigAdvisorHalt:
		m = s.AdvisorHalts
	case SigAdvisorConcern:
		m = s.AdvisorConcerns
	case SigTriageMisfire:
		m = s.TriageMisfires
	default:
		return 0, false
	}
	c, ok := m[toolID]
	return c, ok
}

// AggregateSignals 聚合最近 days 天的账本信号（days 含今天；
// 北京日期口径）。只统计 tool_id 以 cap- 开头的工具行——
// 看护/备份/精炼等框架任务（watchdog-daily-check、backup-ledger、
// refine-daily）不是工具，不进信号。
func (l *Ledger) AggregateSignals(days int) (SignalCounts, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := SignalCounts{
		ExecFailures:    map[string]int{},
		ProbeFailures:   map[string]int{},
		AdvisorHalts:    map[string]int{},
		AdvisorConcerns: map[string]int{},
		TriageMisfires:  map[string]int{},
	}
	if days < 1 {
		days = 1
	}
	window := fmt.Sprintf("date(ts, '+8 hours') >= date('now', '+8 hours', '-%d days')", days-1)
	toolFilter := `tool_id LIKE 'cap-%'`

	// 执行失败 vs 探针失败：按 command 是否以 probe 开头分流。
	queries := []struct {
		m     map[string]int
		where string
	}{
		{out.ExecFailures, `status='failure' AND command NOT LIKE 'probe %'`},
		{out.ProbeFailures, `status='failure' AND command LIKE 'probe %'`},
		{out.AdvisorHalts, `advisor_verdict='HALT'`},
		{out.AdvisorConcerns, `advisor_verdict='CONCERN'`},
	}
	for _, q := range queries {
		rows, err := l.db.Query(fmt.Sprintf(`SELECT tool_id, COUNT(*) FROM execution_ledger
			WHERE %s AND %s AND %s GROUP BY tool_id`, q.where, toolFilter, window))
		if err != nil {
			return out, fmt.Errorf("ledger aggregate signals: %w", err)
		}
		for rows.Next() {
			var toolID string
			var n int
			if err := rows.Scan(&toolID, &n); err != nil {
				rows.Close()
				return out, fmt.Errorf("ledger aggregate scan: %w", err)
			}
			q.m[toolID] = n
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return out, fmt.Errorf("ledger aggregate rows: %w", err)
		}
	}

	// 分诊错选代理：Tier-1 挑了工具、同一任务的执行随后失败（按任务去重）。
	rows, err := l.db.Query(fmt.Sprintf(`SELECT t.tool_id, COUNT(DISTINCT t.task_id)
		FROM triage_metrics t JOIN execution_ledger e
		  ON e.task_id = t.task_id AND e.tool_id = t.tool_id
		WHERE t.route='TOOL' AND t.tier=1 AND e.status='failure'
		  AND t.tool_id LIKE 'cap-%%'
		  AND date(t.ts, '+8 hours') >= date('now', '+8 hours', '-%d days')
		GROUP BY t.tool_id`, days-1))
	if err != nil {
		return out, fmt.Errorf("ledger aggregate misfire: %w", err)
	}
	for rows.Next() {
		var toolID string
		var n int
		if err := rows.Scan(&toolID, &n); err != nil {
			rows.Close()
			return out, fmt.Errorf("ledger aggregate misfire scan: %w", err)
		}
		out.TriageMisfires[toolID] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("ledger aggregate misfire rows: %w", err)
	}

	// 费用：当日（北京）与前 7 日均值（缺数据的天按 0 算进 7 天）。
	if err := l.db.QueryRow(`SELECT COALESCE(SUM(cost_cny_milli), 0) FROM expense_ledger
		WHERE date(ts, '+8 hours') = date('now', '+8 hours')`).Scan(&out.ExpenseTodayMilli); err != nil {
		return out, fmt.Errorf("ledger aggregate expense today: %w", err)
	}
	var priorSum int64
	if err := l.db.QueryRow(`SELECT COALESCE(SUM(cost_cny_milli), 0) FROM expense_ledger
		WHERE date(ts, '+8 hours') >= date('now', '+8 hours', '-7 days')
		  AND date(ts, '+8 hours') < date('now', '+8 hours')`).Scan(&priorSum); err != nil {
		return out, fmt.Errorf("ledger aggregate expense prior: %w", err)
	}
	out.ExpensePrior7DayMeanMilli = priorSum / 7
	if out.ExpensePrior7DayMeanMilli > 0 && out.ExpenseTodayMilli > 2*out.ExpensePrior7DayMeanMilli {
		out.ExpenseSpike = true
	}
	// 7 天内突增天数：逐日费用与今日前 7 日均值比（同一基准），
	// 供 CountFor("expense_spike") 作阈值判断。
	dayRows, err := l.db.Query(`SELECT COALESCE(SUM(cost_cny_milli), 0)
		FROM expense_ledger
		WHERE date(ts, '+8 hours') >= date('now', '+8 hours', '-6 days')
		GROUP BY date(ts, '+8 hours')`)
	if err != nil {
		return out, fmt.Errorf("ledger aggregate expense days: %w", err)
	}
	for dayRows.Next() {
		var daySum int64
		if err := dayRows.Scan(&daySum); err != nil {
			dayRows.Close()
			return out, fmt.Errorf("ledger aggregate expense days scan: %w", err)
		}
		if out.ExpensePrior7DayMeanMilli > 0 && daySum > 2*out.ExpensePrior7DayMeanMilli {
			out.ExpenseSpikeDays++
		}
	}
	dayRows.Close()
	if err := dayRows.Err(); err != nil {
		return out, fmt.Errorf("ledger aggregate expense days rows: %w", err)
	}

	// 失败样本：最近 5 条（供模型点评看错误原文）。
	srows, err := l.db.Query(fmt.Sprintf(`SELECT tool_id, command, err_head
		FROM execution_ledger WHERE status='failure' AND %s AND %s
		ORDER BY id DESC LIMIT 5`, toolFilter, window))
	if err != nil {
		return out, fmt.Errorf("ledger aggregate samples: %w", err)
	}
	defer srows.Close()
	for srows.Next() {
		var sm SignalSample
		if err := srows.Scan(&sm.ToolID, &sm.Command, &sm.ErrHead); err != nil {
			return out, fmt.Errorf("ledger aggregate samples scan: %w", err)
		}
		out.Samples = append(out.Samples, sm)
	}
	return out, srows.Err()
}

// SummarizeExpenseByTask 按 task_id 汇总费用（自进化汇总行用）。
// 汇总行本身（model='selfevolve-summary'）不计入，避免二次自查时重复累加。
func (l *Ledger) SummarizeExpenseByTask(taskID string) (ExpenseSummary, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var s ExpenseSummary
	err := l.db.QueryRow(`SELECT COUNT(*),
		COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(cache_hit_tokens),0),
		COALESCE(SUM(cache_miss_tokens),0), COALESCE(SUM(completion_tokens),0),
		COALESCE(SUM(cost_cny_milli),0)
		FROM expense_ledger WHERE task_id = ? AND model <> 'selfevolve-summary'`,
		taskID).Scan(&s.Calls, &s.PromptTokens, &s.CacheHitTokens, &s.CacheMissTokens,
		&s.CompletionTokens, &s.CostCNYMilli)
	if err != nil {
		return s, fmt.Errorf("ledger summarize expense by task: %w", err)
	}
	return s, nil
}

// RecallRecord 是一次召回的记账行（v2.1 第七节召回账本，线 B 第一阶段）。
// Candidates 是按分排序的候选（每项 "source=score"，前 40）；
// LexicalRank / SemanticRank 是词法 / 语义两路的原始名次（source 列表），
// 调用方没有分路信息时留空；Delivered 是最终交付卡片的 source 列表。
type RecallRecord struct {
	SessionID    string
	TaskID       string
	Query        string
	Candidates   []string
	LexicalRank  []string
	SemanticRank []string
	Delivered    []string
	TokensEst    int64 // 交付卡片合计 token 估算（EstimateTokens 口径）
	LatencyMs    int64
}

// RecordRecall 写一行召回账。写入前查询、候选、名次、交付卡片全部过
// 脱敏中间件（节点 3），账本里绝不出现凭证原文；长字段按字符截断，
// 不劈开汉字。失败返回错误（调用方自行决定是否阻塞；生产热路径只记日志）。
func (l *Ledger) RecordRecall(r RecallRecord) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	join := func(items []string, n int) string {
		return sanitize.Sanitize(truncateRunes(strings.Join(items, ","), n))
	}
	res, err := l.db.Exec(`INSERT INTO recall_log
		(ts, session_id, task_id, query, candidates, lexical_rank, semantic_rank,
		 delivered, tokens_est, latency_ms, hits)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		time.Now().UTC().Format(time.RFC3339), r.SessionID, r.TaskID,
		sanitize.Sanitize(truncateRunes(r.Query, 2000)),
		join(r.Candidates, 8000), join(r.LexicalRank, 4000), join(r.SemanticRank, 4000),
		join(r.Delivered, 4000), r.TokensEst, r.LatencyMs, len(r.Candidates))
	if err != nil {
		return 0, fmt.Errorf("ledger insert recall_log: %w", err)
	}
	return res.LastInsertId()
}

// RecallLogRow 是 recall_log 的一行回读（测试与自查用）。
type RecallLogRow struct {
	ID         int64
	TaskID     string
	Query      string
	Candidates string
	Delivered  string
	TokensEst  int64
	LatencyMs  int64
	Hits       int
}

// ListRecallByTask 按 task_id 读召回账行（最新在前，最多 limit 行）。
func (l *Ledger) ListRecallByTask(taskID string, limit int) ([]RecallLogRow, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	rows, err := l.db.Query(`SELECT id, task_id, query, candidates, delivered,
		tokens_est, latency_ms, hits FROM recall_log WHERE task_id = ?
		ORDER BY id DESC LIMIT ?`, taskID, limit)
	if err != nil {
		return nil, fmt.Errorf("ledger list recall by task: %w", err)
	}
	defer rows.Close()
	var out []RecallLogRow
	for rows.Next() {
		var r RecallLogRow
		if err := rows.Scan(&r.ID, &r.TaskID, &r.Query, &r.Candidates, &r.Delivered,
			&r.TokensEst, &r.LatencyMs, &r.Hits); err != nil {
			return nil, fmt.Errorf("ledger scan recall: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PatrolRecord 是一次巡检的记账行（v2.1 §6 巡检账本，线 B 第二阶段）。
// Detail 是巡检的细节行（\n 拼接，只含路径与计数，不含文件内容）。
type PatrolRecord struct {
	SessionID         string
	TaskID            string
	Full              bool
	Scanned           int
	New               int
	Modified          int
	Renamed           int
	Deleted           int
	StaleMarked       int
	PendingMarked     int
	SkippedGenerating int
	SecretFiles       int
	SecretHits        int
	Detail            string
}

// RecordPatrol 写一行巡检账。写入前细节过脱敏中间件（节点 3），
// 账本里绝不出现凭证原文；长字段按字符截断，不劈开汉字。
// 失败返回错误（调用方自行决定是否阻塞；生产巡检只记日志）。
func (l *Ledger) RecordPatrol(r PatrolRecord) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	full := 0
	if r.Full {
		full = 1
	}
	res, err := l.db.Exec(`INSERT INTO patrol_log
		(ts, session_id, task_id, full, scanned, new_count, modified, renamed,
		 deleted, stale_marked, pending_marked, skipped_generating,
		 secret_files, secret_hits, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		time.Now().UTC().Format(time.RFC3339), r.SessionID, r.TaskID, full,
		r.Scanned, r.New, r.Modified, r.Renamed, r.Deleted,
		r.StaleMarked, r.PendingMarked, r.SkippedGenerating,
		r.SecretFiles, r.SecretHits,
		sanitize.Sanitize(truncateRunes(r.Detail, 8000)))
	if err != nil {
		return 0, fmt.Errorf("ledger insert patrol_log: %w", err)
	}
	return res.LastInsertId()
}

// PatrolLogRow 是 patrol_log 的一行回读（测试与自查用）。
type PatrolLogRow struct {
	ID                int64
	TaskID            string
	Full              bool
	Scanned           int
	New               int
	Modified          int
	Renamed           int
	Deleted           int
	StaleMarked       int
	PendingMarked     int
	SkippedGenerating int
	SecretFiles       int
	SecretHits        int
}

// ListPatrolByTask 按 task_id 读巡检账行（最新在前，最多 limit 行）。
func (l *Ledger) ListPatrolByTask(taskID string, limit int) ([]PatrolLogRow, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	rows, err := l.db.Query(`SELECT id, task_id, full, scanned, new_count,
		modified, renamed, deleted, stale_marked, pending_marked,
		skipped_generating, secret_files, secret_hits FROM patrol_log
		WHERE task_id = ? ORDER BY id DESC LIMIT ?`, taskID, limit)
	if err != nil {
		return nil, fmt.Errorf("ledger list patrol by task: %w", err)
	}
	defer rows.Close()
	var out []PatrolLogRow
	for rows.Next() {
		var r PatrolLogRow
		var full int
		if err := rows.Scan(&r.ID, &r.TaskID, &full, &r.Scanned, &r.New,
			&r.Modified, &r.Renamed, &r.Deleted, &r.StaleMarked, &r.PendingMarked,
			&r.SkippedGenerating, &r.SecretFiles, &r.SecretHits); err != nil {
			return nil, fmt.Errorf("ledger scan patrol: %w", err)
		}
		r.Full = full != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListExpenseByTask 按 task_id 读费用行（最新在前，最多 limit 行；
// 自查费用 task_id 格式测试用）。
func (l *Ledger) ListExpenseByTask(taskID string, limit int) ([]ExpenseRecord, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	rows, err := l.db.Query(`SELECT session_id, task_id, model, role, prompt_tokens,
		cache_hit_tokens, cache_miss_tokens, completion_tokens, cost_cny_milli
		FROM expense_ledger WHERE task_id = ? ORDER BY id DESC LIMIT ?`, taskID, limit)
	if err != nil {
		return nil, fmt.Errorf("ledger list expense by task: %w", err)
	}
	defer rows.Close()
	var out []ExpenseRecord
	for rows.Next() {
		var r ExpenseRecord
		if err := rows.Scan(&r.SessionID, &r.TaskID, &r.Model, &r.Role, &r.PromptTokens,
			&r.CacheHitTokens, &r.CacheMissTokens, &r.CompletionTokens,
			&r.CostCNYMilli); err != nil {
			return nil, fmt.Errorf("ledger scan expense by task: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
