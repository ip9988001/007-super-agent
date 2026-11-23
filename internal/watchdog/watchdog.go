// Package watchdog 是 007 超级 Agent 的看护：每日体检 + 每日省 token 核算 +
// 队列水位看门狗。
//
// 设计依据（《超级Agent-全新设计方案》v1.5，第 4 轮看护规格）：
//   - 每日 04:30 体检总账真身与可跑性；每日 09:00 按账本出省 token 核算；
//     未确认告警口径 seen>acked 且最久未确认 >1800 秒。
//   - 体检只读总账与索引，坏条目记进 execution_ledger，不删、不改总账。
//   - 核算的净省 = index_metrics 当日省下的 token − 当日注入的 token；
//     净省为负只返回警告值，由调用方决定是否告警。
//   - 看门狗只判断、不直接发通知。
//
// 铁律：写入失败必须返回错误，不许静默吞掉；不写任何密钥、Token。
package watchdog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"007-super-agent/internal/ledger"
	"007-super-agent/internal/registry"

	_ "modernc.org/sqlite"
)

// UnackedAlertThresholdS 是未确认告警的最久未确认秒数阈值（第 4 轮口径）。
const UnackedAlertThresholdS = 1800

// ProbeFunc 探查一条工具的真身：entry_point 是否存在、能不能跑。
// 默认探针只做存在性检查，不真跑命令；调用方可以注入更严格的探针
// （例如跑 --version）。
type ProbeFunc func(e registry.Entry) error

// DefaultProbe 是默认探针：按 call_type 分流（cli 查命令/路径、service
// 查脚本文件、doc/sop/tool/mcp/data 不探针）。只做存在性检查，
// 除 "python3 -c ..." 的只读 import 外不执行命令。
// 实现委托给 registry.ProbeEntry（探针口径唯一实现，防两处漂移）。
func DefaultProbe(e registry.Entry) error {
	return registry.ProbeEntry(e)
}

// Watchdog 是看护：体检写账本用 led，核算只读 dbPath 指定的 ledger.db。
// led 为 nil 时体检只出报告、不记账；dbPath 为空时核算报错。
type Watchdog struct {
	led    *ledger.Ledger
	dbPath string
	probe  ProbeFunc
	// backupHook 是精炼成功后调的记忆备份钩子（第 15 步）；为 nil 时不跑。
	// 由 main.go 接线；钩子失败只记日志，不影响精炼标记与主循环。
	backupHook func(ctx context.Context, date string) error
	// semanticHook 是精炼成功后调的语义索引增量重建钩子（第 22 步）；
	// 为 nil 时不跑。由 main.go 接线；跑在后台 goroutine 里，不阻塞
	// 精炼循环；失败只记日志。重建只嵌新增/改动过的文档，日常只有
	// 几篇，几秒完成。
	semanticHook func(ctx context.Context) error
}

// SetBackupHook 接入精炼成功后的记忆备份钩子（第 15 步）。
func (w *Watchdog) SetBackupHook(fn func(ctx context.Context, date string) error) {
	w.backupHook = fn
}

// SetSemanticHook 接入精炼成功后的语义索引增量重建钩子（第 22 步）。
func (w *Watchdog) SetSemanticHook(fn func(ctx context.Context) error) {
	w.semanticHook = fn
}

// afterRefineSemantic 在精炼成功后后台跑语义索引增量重建；钩子失败
// 只记日志，不报错、不阻塞。跑在后台 goroutine 里，精炼循环不等它。
func (w *Watchdog) afterRefineSemantic(ctx context.Context) {
	if w.semanticHook == nil {
		return
	}
	go func() {
		if err := w.semanticHook(ctx); err != nil {
			log.Printf("refine loop: 语义索引增量重建失败（只记日志，不影响精炼）: %v", err)
		}
	}()
}

// afterRefine 在精炼成功后调备份钩子；钩子失败只记日志，不报错、不阻塞。
func (w *Watchdog) afterRefine(ctx context.Context, date string) {
	if w.backupHook == nil {
		return
	}
	if err := w.backupHook(ctx, date); err != nil {
		log.Printf("refine loop: 记忆备份失败（只记日志，不影响精炼）: %v", err)
	}
}

// New 建一个看护。probe 为 nil 时用 DefaultProbe。
func New(led *ledger.Ledger, dbPath string, probe ProbeFunc) *Watchdog {
	if probe == nil {
		probe = DefaultProbe
	}
	return &Watchdog{led: led, dbPath: dbPath, probe: probe}
}

// BrokenEntry 是一条体检不合格的工具。
type BrokenEntry struct {
	ToolID string
	Reason string
}

// CheckResult 是一次体检的结果。
type CheckResult struct {
	Checked int // 实际核对的条目数（available:true）
	OK      int // 合格条目数
	Skipped int // available:false，按设计跳过（不可调用，不算坏）
	Broken  []BrokenEntry
}

// sopMarkers 是 L3 固定四节里 SOP 的三要素。
var sopMarkers = []string{"输入", "步骤", "验收"}

// CheckRegistry 体检总账：逐条核对真身（entry_point 存在、探针通过、
// L3 的 SOP 三要素齐全）。坏条目只写一行汇总回执进 execution_ledger
// （checked/ok/broken 数 + 前 5 个坏条目名），明细只记日志——
// 逐条写 failure 会淹没账本（一次启动曾写 66 行）。不删总账、不改总账。
// 记账失败会汇总进返回的错误。
func (w *Watchdog) CheckRegistry(dataDir string) (CheckResult, error) {
	var res CheckResult
	rg, err := registry.Load(dataDir)
	if err != nil {
		return res, fmt.Errorf("watchdog: load registry: %w", err)
	}
	for _, cat := range registry.Categories() {
		for _, e := range rg.EntriesByCategory(cat) {
			if !e.Available {
				res.Skipped++
				continue
			}
			res.Checked++
			var reason string
			if err := w.probe(*e); err != nil {
				reason = fmt.Sprintf("探针失败：%v", err)
			} else if l3, err := rg.L3(e.ToolID); err != nil {
				reason = fmt.Sprintf("读 L3 失败：%v", err)
			} else {
				for _, m := range sopMarkers {
					if !strings.Contains(l3, m) {
						reason = fmt.Sprintf("L3 缺 SOP 要素 %q", m)
						break
					}
				}
			}
			if reason == "" {
				res.OK++
				continue
			}
			res.Broken = append(res.Broken, BrokenEntry{ToolID: e.ToolID, Reason: reason})
			// 明细只记日志，不进账本。
			log.Printf("watchdog check broken: %s: %s", e.ToolID, reason)
		}
	}
	if w.led != nil {
		if err := w.recordCheckSummary(res); err != nil {
			return res, fmt.Errorf("watchdog: 体检汇总记账失败：%w", err)
		}
	}
	return res, nil
}

// recordCheckSummary 只写一行体检汇总回执：checked/ok/broken 数 +
// 前 5 个坏条目名。有坏条目时状态 failure，否则 success。
func (w *Watchdog) recordCheckSummary(res CheckResult) error {
	names := make([]string, 0, 5)
	for i, b := range res.Broken {
		if i >= 5 {
			break
		}
		names = append(names, b.ToolID)
	}
	broken := "none"
	if len(names) > 0 {
		broken = strings.Join(names, ", ")
		if len(res.Broken) > 5 {
			broken += fmt.Sprintf(" 等 %d 条", len(res.Broken))
		}
	}
	status := "success"
	exitCode := 0
	if len(res.Broken) > 0 {
		status = "failure"
		exitCode = 1
	}
	_, err := w.led.RecordExecution(ledger.ExecutionRecord{
		SessionID: "watchdog",
		TaskID:    "watchdog-daily-check",
		ToolID:    "watchdog-daily-check",
		Command:   "watchdog check summary",
		ExitCode:  exitCode,
		Status:    status,
		ErrHead: fmt.Sprintf("checked=%d ok=%d broken=%d skipped=%d; broken: %s",
			res.Checked, res.OK, len(res.Broken), res.Skipped, broken),
	})
	return err
}

// Savings 是一次每日省 token 核算的结果。
type Savings struct {
	Date                string  // 核算日期（UTC）
	TriageCount         int64   // 当日分诊次数
	Tier0Count          int64   // 当日 Tier-0 规则直出次数
	Tier0Ratio          float64 // Tier-0 占比
	TriageTokensIn      int64   // 当日分诊注入 token
	TriageTokensOut     int64   // 当日分诊产出 token
	IndexInjected       int64   // 当日索引注入 token
	IndexSaved          int64   // 当日索引省下的 token（相对全量读）
	TokensInjectedTotal int64   // 当日注入 token 总量（分诊 + 索引）
	NetSavedTokens      int64   // 净省 = IndexSaved − IndexInjected
	ExpenseCalls        int64   // 当日模型调用次数
	ExpenseTokens       int64   // 当日模型 token 总量（prompt + completion）
	CostCNYMilli        int64   // 当日费用合计，单位 0.001 元
	Warn                bool    // 净省为负时为真
	WarnReason          string  // 告警原因（Warn 为真时非空）
}

// DailySavings 按账本算当日（UTC）省 token 核算。只读 ledger.db，不写。
// 净省为负时 Warn 为真，由调用方决定是否告警。
func (w *Watchdog) DailySavings() (Savings, error) {
	return w.DailySavingsFor(time.Now().UTC().Format("2006-01-02"))
}

// DailySavingsFor 按账本算指定 UTC 日期（YYYY-MM-DD）的省 token 核算。
// 只读 ledger.db，不写。日期格式非法时报错。
// 09:00 定时用它算前一天的完整账；DailySavings() 是算当天的快捷入口。
func (w *Watchdog) DailySavingsFor(date string) (Savings, error) {
	var sv Savings
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return sv, fmt.Errorf("watchdog: 日期格式非法 %q: %w", date, err)
	}
	if strings.TrimSpace(w.dbPath) == "" {
		return sv, errors.New("watchdog: 未指定 ledger.db 路径")
	}
	db, err := sql.Open("sqlite", "file:"+w.dbPath+"?mode=ro")
	if err != nil {
		return sv, fmt.Errorf("watchdog: open ledger.db: %w", err)
	}
	defer db.Close()
	sv.Date = date
	day := `date(ts) = '` + date + `'`
	if err := db.QueryRow(`SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN tier = 0 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(tokens_in), 0), COALESCE(SUM(tokens_out), 0)
		FROM triage_metrics WHERE `+day).Scan(
		&sv.TriageCount, &sv.Tier0Count, &sv.TriageTokensIn, &sv.TriageTokensOut); err != nil {
		return sv, fmt.Errorf("watchdog: summarize triage_metrics: %w", err)
	}
	if err := db.QueryRow(`SELECT COALESCE(SUM(tokens_injected), 0),
		COALESCE(SUM(tokens_saved_vs_full), 0)
		FROM index_metrics WHERE `+day).Scan(
		&sv.IndexInjected, &sv.IndexSaved); err != nil {
		return sv, fmt.Errorf("watchdog: summarize index_metrics: %w", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*),
		COALESCE(SUM(prompt_tokens + completion_tokens), 0),
		COALESCE(SUM(cost_cny_milli), 0)
		FROM expense_ledger WHERE `+day).Scan(
		&sv.ExpenseCalls, &sv.ExpenseTokens, &sv.CostCNYMilli); err != nil {
		return sv, fmt.Errorf("watchdog: summarize expense_ledger: %w", err)
	}
	if sv.TriageCount > 0 {
		sv.Tier0Ratio = float64(sv.Tier0Count) / float64(sv.TriageCount)
	}
	sv.TokensInjectedTotal = sv.TriageTokensIn + sv.IndexInjected
	sv.NetSavedTokens = sv.IndexSaved - sv.IndexInjected
	if sv.NetSavedTokens < 0 && (sv.IndexInjected > 0 || sv.IndexSaved > 0) {
		sv.Warn = true
		sv.WarnReason = fmt.Sprintf("当日净省 token 为负：省下 %d，注入 %d，净 %d",
			sv.IndexSaved, sv.IndexInjected, sv.NetSavedTokens)
	}
	return sv, nil
}

// QueueWatermark 是队列水位（调用方从网关的 Watermark() 映射过来）。
type QueueWatermark struct {
	Seen           int64 // 已见到的序号
	Acked          int64 // 已确认的序号
	OldestUnackedS int64 // 最久未确认的时长（秒）
}

// CheckQueue 是水位看门狗：seen>acked 且最久未确认 >1800 秒才告警。
// 只判断、不直接发通知。
func CheckQueue(wm QueueWatermark) (alert bool, reason string) {
	if wm.Seen > wm.Acked && wm.OldestUnackedS > UnackedAlertThresholdS {
		return true, fmt.Sprintf("seen=%d acked=%d，最久未确认 %d 秒 > %d 秒",
			wm.Seen, wm.Acked, wm.OldestUnackedS, UnackedAlertThresholdS)
	}
	return false, "水位正常"
}
