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
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
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

// DefaultProbe 是默认探针：entry_point 首词含 "/" 时看文件在不在，
// 否则在 PATH 里找命令。只做存在性检查，不执行。
func DefaultProbe(e registry.Entry) error {
	ep := strings.TrimSpace(e.EntryPoint)
	if ep == "" {
		return errors.New("entry_point 为空")
	}
	first := strings.Fields(ep)[0]
	if strings.Contains(first, "/") {
		if _, err := os.Stat(first); err != nil {
			return fmt.Errorf("entry_point %q 不存在: %v", first, err)
		}
		return nil
	}
	if _, err := exec.LookPath(first); err != nil {
		return fmt.Errorf("命令 %q 不在 PATH: %v", first, err)
	}
	return nil
}

// Watchdog 是看护：体检写账本用 led，核算只读 dbPath 指定的 ledger.db。
// led 为 nil 时体检只出报告、不记账；dbPath 为空时核算报错。
type Watchdog struct {
	led    *ledger.Ledger
	dbPath string
	probe  ProbeFunc
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
// L3 的 SOP 三要素齐全）。坏条目记进 execution_ledger（状态 failed，
// 原因写清），不删总账、不改总账。记账失败会汇总进返回的错误。
func (w *Watchdog) CheckRegistry(dataDir string) (CheckResult, error) {
	var res CheckResult
	rg, err := registry.Load(dataDir)
	if err != nil {
		return res, fmt.Errorf("watchdog: load registry: %w", err)
	}
	var recordErrs []string
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
			if w.led != nil {
				_, err := w.led.RecordExecution(ledger.ExecutionRecord{
					SessionID: "watchdog",
					TaskID:    "watchdog-daily-check",
					ToolID:    e.ToolID,
					Command:   "watchdog check " + e.ToolID,
					ExitCode:  1,
					Status:    "failure",
					ErrHead:   reason,
				})
				if err != nil {
					recordErrs = append(recordErrs, fmt.Sprintf("%s 记账失败：%v", e.ToolID, err))
				}
			}
		}
	}
	if len(recordErrs) > 0 {
		return res, fmt.Errorf("watchdog: %s", strings.Join(recordErrs, "；"))
	}
	return res, nil
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

// DailySavings 按账本算当日省 token 核算。只读 ledger.db，不写。
// 净省为负时 Warn 为真，由调用方决定是否告警。
func (w *Watchdog) DailySavings() (Savings, error) {
	var sv Savings
	if strings.TrimSpace(w.dbPath) == "" {
		return sv, errors.New("watchdog: 未指定 ledger.db 路径")
	}
	db, err := sql.Open("sqlite", "file:"+w.dbPath+"?mode=ro")
	if err != nil {
		return sv, fmt.Errorf("watchdog: open ledger.db: %w", err)
	}
	defer db.Close()
	sv.Date = time.Now().UTC().Format("2006-01-02")
	today := `date(ts) = date('now')`
	if err := db.QueryRow(`SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN tier = 0 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(tokens_in), 0), COALESCE(SUM(tokens_out), 0)
		FROM triage_metrics WHERE `+today).Scan(
		&sv.TriageCount, &sv.Tier0Count, &sv.TriageTokensIn, &sv.TriageTokensOut); err != nil {
		return sv, fmt.Errorf("watchdog: summarize triage_metrics: %w", err)
	}
	if err := db.QueryRow(`SELECT COALESCE(SUM(tokens_injected), 0),
		COALESCE(SUM(tokens_saved_vs_full), 0)
		FROM index_metrics WHERE `+today).Scan(
		&sv.IndexInjected, &sv.IndexSaved); err != nil {
		return sv, fmt.Errorf("watchdog: summarize index_metrics: %w", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*),
		COALESCE(SUM(prompt_tokens + completion_tokens), 0),
		COALESCE(SUM(cost_cny_milli), 0)
		FROM expense_ledger WHERE `+today).Scan(
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
