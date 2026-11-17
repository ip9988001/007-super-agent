// Package advisor 的适配层：把 Gemini 的 CollectEvidenceBundle / RunAdvisorCheck
// 接进 executor.Advisor 接口（CollectEvidenceBundle + Check）。
//
// 计数口径：第 7 轮规格——HALT 最多打回 2 轮，同因二连叫停早退。
// 本适配器按任务记 HALT 次数与上次 HALT 原因，传给 RunAdvisorCheck 做判定；
// executor 自己也在任务级计数，第 3 次 HALT 时返回 ErrAdvisorHaltFinal，
// 两层口径一致（2 轮打回后第 3 次即终局上报）。
package advisor

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"007-super-agent/internal/executor"
	"007-super-agent/internal/ledger"
	"007-super-agent/internal/llm"

	_ "modernc.org/sqlite"
)

// Advisor 是 executor.Advisor 的真实实现。
type Advisor struct {
	db      *sql.DB
	cfg     AdvisorConfig
	workDir string
	Ledger  *ledger.Ledger // 监工费用记账用（第 13 步）；为 nil 时不记账

	mu          sync.Mutex
	retry       map[string]int    // taskID -> 该任务已 HALT 次数
	prevReason  map[string]string // taskID -> 上次 HALT 原因
	lastTask    string            // 最近一次收证据的任务
	lastSession string            // 最近一次收证据的任务所属会话
}

// New 打开 ledger.db 的只读连接（看护包同款 mode=ro，不写库）。
func New(dbPath string, cfg AdvisorConfig, workDir string) (*Advisor, error) {
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("advisor open ledger: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("advisor ping ledger: %w", err)
	}
	db.SetMaxOpenConns(1)
	return &Advisor{db: db, cfg: cfg, workDir: workDir,
		retry: map[string]int{}, prevReason: map[string]string{}}, nil
}

// Close 关闭只读连接。
func (a *Advisor) Close() error { return a.db.Close() }

// CollectEvidenceBundle 从宿主机抓五件套硬证据，映射成 executor.Evidence。
// trigger 是审查触发点：pre-action 预审时证据包带上拟执行的命令与目标
// （动作尚未执行，只核目标与参数，不验证完成度）；删文件时把目标文件
// 也放进产物指纹，让监工看到目标存在与否。其他触发点保持原样。
func (a *Advisor) CollectEvidenceBundle(task executor.Task, trigger executor.Trigger) (executor.Evidence, error) {
	prompt := task.Text
	var artifacts []string
	if trigger == executor.TriggerPreAction && strings.TrimSpace(task.Command) != "" {
		prompt = "任务原话：" + task.Text + "\n拟执行命令：" + task.Command +
			"\n【预审】动作尚未执行：只核对拟执行命令与目标是否与任务原话一致，不验证完成度；不要以“无法证实已完成”为由告警。"
		if task.IrreversibleKind == executor.KindDeleteFile {
			if target := deleteTarget(task.Command); target != "" {
				artifacts = append(artifacts, target)
			}
		}
	}
	// 交活前审：把计划任务的工作目录落盘文件也放进产物指纹，否则监工
	// 看不到任何落盘证据。2026-11-18 ping0 第三轮：4 个文件已落盘，
	// 监工却因 artifact_stats 为空而写下“无任何落盘文件”。
	if trigger == executor.TriggerDelivery && strings.TrimSpace(task.WorkDir) != "" {
		if entries, err := os.ReadDir(task.WorkDir); err == nil {
			for i, de := range entries {
				if i >= 20 {
					break
				}
				artifacts = append(artifacts, filepath.Join(task.WorkDir, de.Name()))
			}
		} else {
			// 目录本身不存在也是一种证据：让监工看到工作目录根本没建。
			artifacts = append(artifacts, task.WorkDir)
		}
	}
	b, err := CollectEvidenceBundle(context.Background(), a.db, EvidenceRequest{
		TaskID:         task.TaskID,
		TaskPrompt:     prompt,
		WorkDir:        a.workDir,
		ArtifactPaths:  artifacts,
		ProposedOutput: task.Draft,
	})
	if err != nil {
		return executor.Evidence{}, err
	}
	a.mu.Lock()
	a.lastTask = task.TaskID
	a.lastSession = task.SessionID
	a.mu.Unlock()
	var cmds []string
	for _, l := range strings.Split(b.LedgerCommands, "\n") {
		if strings.TrimSpace(l) != "" {
			cmds = append(cmds, l)
		}
	}
	return executor.Evidence{
		TaskText:            b.TaskAndCriteria,
		Diff:                b.GitDiff,
		Commands:            cmds,
		ArtifactFingerprint: b.ArtifactStats,
		Draft:               b.ProposedOutput,
	}, nil
}

// Check 调模型审一次。trigger 不进证据包（证据只含五件套硬事实），
// 触发点由调用方（executor）记账。模型回了无法识别的 verdict 时不悄悄
// 放行，降级为 CONCERN 并写明原值。
func (a *Advisor) Check(evidence executor.Evidence, trigger executor.Trigger) (executor.CheckResult, error) {
	_ = trigger
	a.mu.Lock()
	taskID := a.lastTask
	retry := a.retry[taskID]
	prev := a.prevReason[taskID]
	a.mu.Unlock()
	dec, err := RunAdvisorCheck(context.Background(), a.cfg, EvidenceBundle{
		TaskAndCriteria: evidence.TaskText,
		GitDiff:         evidence.Diff,
		LedgerCommands:  strings.Join(evidence.Commands, "\n"),
		ArtifactStats:   evidence.ArtifactFingerprint,
		ProposedOutput:  evidence.Draft,
	}, retry, prev)
	if err != nil {
		return executor.CheckResult{}, err
	}
	// 第 13 步：监工调用的费用进 expense_ledger（模型名、hit/miss、
	// 费用毫分齐全，按任务归属）。记账失败只记日志，不吞 verdict。
	a.recordAdvisorExpense(taskID, dec.Usage)
	res := executor.CheckResult{RetryCount: retry}
	switch dec.Verdict {
	case "NOTE":
		res.Verdict = executor.VerdictNote
	case "CONCERN":
		res.Verdict = executor.VerdictConcern
	case "HALT":
		res.Verdict = executor.VerdictHalt
	default:
		res.Verdict = executor.VerdictConcern
		res.Note = fmt.Sprintf("监工回了无法识别的 verdict %q，按 CONCERN 处理；原因：%s", dec.Verdict, dec.Reason)
		return res, nil
	}
	res.Note = dec.Reason
	if dec.FixDirective != "" {
		res.Note = dec.Reason + " | 改法：" + dec.FixDirective
	}
	if dec.Verdict == "HALT" {
		a.mu.Lock()
		a.retry[taskID]++
		a.prevReason[taskID] = dec.Reason
		res.RetryCount = a.retry[taskID]
		a.mu.Unlock()
		if dec.ShouldEscalate {
			res.Note = "ESCALATE（打回已封顶或同因二连叫停，停下上报用户）: " + res.Note
		}
	}
	return res, nil
}

// recordAdvisorExpense 把一次监工调用的费用记进 expense_ledger。
// Ledger 为 nil 时不记（测试桩或未接账本）。失败只记日志，不吞 verdict。
func (a *Advisor) recordAdvisorExpense(taskID string, u llm.Usage) {
	if a.Ledger == nil {
		return
	}
	a.mu.Lock()
	sessionID := a.lastSession
	a.mu.Unlock()
	cost := llm.CostMilliForModel(a.cfg.Model, u.CacheHitTokens, u.CacheMissTokens, u.CompletionTokens, false)
	_, err := a.Ledger.RecordExpense(ledger.ExpenseRecord{
		SessionID: sessionID, TaskID: taskID, Model: a.cfg.Model,
		PromptTokens: u.PromptTokens, CacheHitTokens: u.CacheHitTokens,
		CacheMissTokens: u.CacheMissTokens, CompletionTokens: u.CompletionTokens,
		CostCNYMilli: cost,
	})
	if err != nil {
		log.Printf("advisor: record expense failed: %v", err)
	}
}

// deleteTarget 从 "rm [-flags] <path>" 这类命令里取目标文件路径：
// 跳过命令名与 - 开头的选项，第一个剩下的字段就是目标。取不到返回 ""。
func deleteTarget(command string) string {
	fields := strings.Fields(command)
	for _, f := range fields[1:] {
		if strings.HasPrefix(f, "-") {
			continue
		}
		return f
	}
	return ""
}
