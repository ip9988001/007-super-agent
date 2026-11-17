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
	"strings"
	"sync"

	"007-super-agent/internal/executor"

	_ "modernc.org/sqlite"
)

// Advisor 是 executor.Advisor 的真实实现。
type Advisor struct {
	db      *sql.DB
	cfg     AdvisorConfig
	workDir string

	mu         sync.Mutex
	retry      map[string]int    // taskID -> 该任务已 HALT 次数
	prevReason map[string]string // taskID -> 上次 HALT 原因
	lastTask   string            // 最近一次收证据的任务
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
func (a *Advisor) CollectEvidenceBundle(task executor.Task) (executor.Evidence, error) {
	b, err := CollectEvidenceBundle(context.Background(), a.db, EvidenceRequest{
		TaskID:         task.TaskID,
		TaskPrompt:     task.Text,
		WorkDir:        a.workDir,
		ProposedOutput: task.Draft,
	})
	if err != nil {
		return executor.Evidence{}, err
	}
	a.mu.Lock()
	a.lastTask = task.TaskID
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
