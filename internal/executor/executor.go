// Package executor 是 007 超级 Agent 的直接执行层：原生执行 + 监工接线口。
//
// 设计依据（《超级Agent-全新设计方案》v1.5，第 2、4、7 轮）：
//   - 无沙箱无限制：主 Agent 直接跑命令 / 读写文件 / 调工具，完整 Login Shell
//     环境继承（修掉二号 27 次失败的缺 HOME 事故：HOME、PATH 缺失就明确报错）。
//   - 短任务 ≤30 秒内联跑；长任务 >30 秒走后台协程（最长 1800 秒，超了杀掉），
//     30 秒派单即返回纪律：Run 超过 30 秒的任务立刻返回任务句柄，不等结果。
//   - 执行后只记账不拦截：每次执行的结果与费用进 ledger.db execution_ledger；
//     写账本失败返回错误，不静默吞掉。
//   - 监工三触发（交活前审、不可逆动作前审、长任务分片后审），NOTE / CONCERN /
//     HALT 三级，HALT 最多打回 2 轮（初次 + 2 次重提，第 3 次 HALT 即终局，
//     停下并上报用户，不再重提）。
//
// 本包只定义监工接口与调用点，不写监工的具体实现：CollectEvidenceBundle 与
// Check 由 core/advisor.go 落位后接进来（Gemini 另一路在写），未接之前
// Advisor 为 nil 表示不审。
//
// 铁律：不可逆动作（删文件 / 改配置 / 对外发钱）执行前必须过监工；HALT 时
// 停止提交并把原因写进 execution_ledger 的 advisor_* 三字段；
// 不写任何密钥、Token。
package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"007-super-agent/internal/ledger"
)

// InlineThreshold 是内联执行的上限：≤30 秒等结果，>30 秒转后台协程。
const InlineThreshold = 30 * time.Second

// MaxTimeout 是后台任务的最长存活时间：1800 秒，超了杀掉并记账。
// 字段化是为了测试能把它调小，不用真等 1800 秒。
var MaxTimeout = 1800 * time.Second

var (
	ErrMissingEnv       = errors.New("executor: Login Shell 环境缺失")
	ErrInvalidTimeout   = errors.New("executor: 超时必须 > 0")
	ErrAdvisorHalt      = errors.New("executor: 监工 HALT，打回重做")
	ErrAdvisorHaltFinal = errors.New("executor: 监工第 3 次 HALT，终局，停下上报用户")
	ErrNoAdvisor        = errors.New("executor: 未接监工，无法审查")
)

// Verdict 是监工三级便签。
type Verdict string

const (
	VerdictNone    Verdict = "NONE"
	VerdictNote    Verdict = "NOTE"
	VerdictConcern Verdict = "CONCERN"
	VerdictHalt    Verdict = "HALT"
)

// Trigger 是监工的三处审查触发点（第 7 轮）。
type Trigger string

const (
	TriggerDelivery  Trigger = "delivery"   // 交活前审
	TriggerPreAction Trigger = "pre-action" // 不可逆动作前审
	TriggerShard     Trigger = "shard"      // 长任务分片后审
)

// Task 是监工要审的一件事。
type Task struct {
	TaskID           string
	SessionID        string
	ToolID           string
	Text             string // 任务原话
	Command          string // 实际要跑的命令（拼好的可读串）
	Irreversible     bool
	IrreversibleKind string // delete-file / modify-config / send-money
	Draft            string // 拟交付结论（交活前审用）
}

// Evidence 是监工收到的五件套硬证据（第 7 轮，硬封顶 ≤3000 token，
// 由 core/advisor.go 的 CollectEvidenceBundle 从宿主机直接抓，
// 不经过执行方的自述）。
type Evidence struct {
	TaskText            string   // 任务原话 + 验收 ≤300 token
	Diff                string   // git diff -U5 业务与测试源码 ≤1200 token
	Commands            []string // ledger 里真实命令与退出码 ≤500 token
	ArtifactFingerprint string   // 产物 os.Stat 指纹 ≤200 token
	Draft               string   // 拟交付结论 ≤400 token
}

// CheckResult 是一次审查的结果。
type CheckResult struct {
	Verdict    Verdict
	Note       string // ≤600 字，HALT 时写明打回原因
	RetryCount int    // 该任务累计 HALT 次数（0..3）
}

// Advisor 是监工的接口：收证据 + 审查。具体实现在 core/advisor.go。
type Advisor interface {
	CollectEvidenceBundle(task Task) (Evidence, error)
	Check(evidence Evidence, trigger Trigger) (CheckResult, error)
}

// IrreversibleKind 是三类不可逆动作。
const (
	KindDeleteFile   = "delete-file"   // 删文件
	KindModifyConfig = "modify-config" // 改配置
	KindSendMoney    = "send-money"    // 对外发钱
)

// ClassifyIrreversible 按命令名粗判是否不可逆：rm / shred 这类删文件的
// 命令即使调用方没标也必须审；改配置与发钱由调用方显式标记（命令形态
// 太多，靠名字猜不准，不硬猜）。
func ClassifyIrreversible(argv []string) (bool, string) {
	if len(argv) == 0 {
		return false, ""
	}
	switch argv[0] {
	case "rm", "shred", "unlink":
		return true, KindDeleteFile
	}
	return false, ""
}

// Result 是一次执行的结果。
type Result struct {
	TaskID            string
	SessionID         string
	ToolID            string
	Command           string
	ExitCode          int
	StdoutTail        string // 标准输出末尾 20 行
	ErrHead           string // 错误头 600 字（按字符截断）
	DurationMs        int64
	Status            string // success / failure
	AdvisorVerdict    Verdict
	AdvisorRetryCount int
	AdvisorNote       string
	Killed            bool // 是否被超时杀掉
}

// Handle 是后台任务的句柄：Run 立刻返回它，不等结果。
type Handle struct {
	TaskID    string
	StartedAt time.Time
	done      chan struct{}
	result    *Result
	err       error
	cancel    context.CancelFunc
	mu        sync.Mutex
}

// Done 在任务结束时关闭。
func (h *Handle) Done() <-chan struct{} { return h.done }

// Wait 等后台任务结束并返回结果。
func (h *Handle) Wait() (*Result, error) {
	<-h.done
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.result, h.err
}

// Cancel 杀掉后台任务（测试与看护用）。
func (h *Handle) Cancel() { h.cancel() }

// RunRequest 是一次执行的请求。
type RunRequest struct {
	TaskID           string
	SessionID        string
	ToolID           string
	Argv             []string      // 要跑的命令与参数
	Timeout          time.Duration // ≤30s 内联，>30s 后台；>1800s 截到 1800s
	Irreversible     bool
	IrreversibleKind string
	Text             string // 任务原话（监工证据用）

	// preVerdict / preNote 由 Run 在不可逆动作前审通过后填入，runCmd
	// 随执行结果一起记账（NOTE / CONCERN 也要留痕）。
	preVerdict Verdict
	preNote    string
}

// Executor 是直接执行层。
type Executor struct {
	Ledger  *ledger.Ledger
	Advisor Advisor  // nil 表示未接监工，不审
	Env     []string // nil 时继承 os.Environ()；测试可注入缺 HOME 的环境

	mu    sync.Mutex
	halts map[string]int // taskID -> 累计 HALT 次数
}

// truncateRunes 按字符截断到 n 个字（第 6 轮坑 2：先转 []rune）。
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

// checkEnv 校验 Login Shell 环境齐全：HOME、PATH 缺失就明确报错，
// 不许静默跑下去重演二号 27 次失败的事故。
func (e *Executor) checkEnv() error {
	env := e.Env
	if env == nil {
		env = os.Environ()
	}
	var missing []string
	for _, k := range []string{"HOME", "PATH"} {
		found := false
		for _, kv := range env {
			if strings.HasPrefix(kv, k+"=") && len(kv) > len(k)+1 {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: 缺 %s", ErrMissingEnv, strings.Join(missing, "、"))
	}
	return nil
}

// record 写一行执行回执。失败返回错误，不静默吞掉。
func (e *Executor) record(r *Result) error {
	if e.Ledger == nil {
		return nil
	}
	verdict := string(r.AdvisorVerdict)
	if verdict == "" {
		verdict = string(VerdictNone)
	}
	_, err := e.Ledger.RecordExecution(ledger.ExecutionRecord{
		SessionID:         r.SessionID,
		TaskID:            r.TaskID,
		ToolID:            r.ToolID,
		Command:           r.Command,
		ExitCode:          r.ExitCode,
		StdoutTail:        r.StdoutTail,
		ErrHead:           r.ErrHead,
		DurationMs:        r.DurationMs,
		Status:            r.Status,
		AdvisorVerdict:    verdict,
		AdvisorRetryCount: r.AdvisorRetryCount,
		AdvisorNote:       r.AdvisorNote,
	})
	return err
}

// Check 在一处触发点跑一次审查，执行 HALT 最多打回 2 轮的封顶：
// 同一任务第 3 次 HALT 返回 ErrAdvisorHaltFinal，调用方必须停下上报用户。
func (e *Executor) Check(task Task, trigger Trigger) (CheckResult, error) {
	if e.Advisor == nil {
		return CheckResult{Verdict: VerdictNone}, ErrNoAdvisor
	}
	ev, err := e.Advisor.CollectEvidenceBundle(task)
	if err != nil {
		return CheckResult{}, fmt.Errorf("executor: 收证据失败: %w", err)
	}
	res, err := e.Advisor.Check(ev, trigger)
	if err != nil {
		return CheckResult{}, fmt.Errorf("executor: 审查失败: %w", err)
	}
	if res.Verdict == VerdictHalt {
		e.mu.Lock()
		if e.halts == nil {
			e.halts = map[string]int{}
		}
		e.halts[task.TaskID]++
		res.RetryCount = e.halts[task.TaskID]
		e.mu.Unlock()
		if res.RetryCount >= 3 {
			return res, ErrAdvisorHaltFinal
		}
		return res, ErrAdvisorHalt
	}
	return res, nil
}

// ReviewDelivery 是触发点 ①：交活前审。HALT 时停止提交，原因记账。
func (e *Executor) ReviewDelivery(task Task) (CheckResult, error) {
	res, err := e.Check(task, TriggerDelivery)
	if err != nil && !errors.Is(err, ErrAdvisorHalt) && !errors.Is(err, ErrAdvisorHaltFinal) {
		return res, err
	}
	if res.Verdict == VerdictHalt {
		_ = e.record(&Result{
			TaskID: task.TaskID, SessionID: task.SessionID, ToolID: task.ToolID, Command: task.Command,
			ExitCode: -1, Status: "failure",
			AdvisorVerdict: VerdictHalt, AdvisorRetryCount: res.RetryCount,
			AdvisorNote: res.Note,
		})
	}
	return res, err
}

// ReviewShard 是触发点 ③：长任务分片后审。
func (e *Executor) ReviewShard(task Task) (CheckResult, error) {
	return e.Check(task, TriggerShard)
}

// Run 执行一次命令：≤30 秒内联等结果，>30 秒立刻返回任务句柄。
func (e *Executor) Run(req RunRequest) (*Result, *Handle, error) {
	if err := e.checkEnv(); err != nil {
		return nil, nil, err
	}
	if req.Timeout <= 0 {
		return nil, nil, ErrInvalidTimeout
	}
	timeout := req.Timeout
	if timeout > MaxTimeout {
		timeout = MaxTimeout
	}
	if len(req.Argv) == 0 {
		return nil, nil, fmt.Errorf("executor: 命令为空")
	}
	// 不可逆动作：调用方没标也按命令名粗判，漏标也必须审。
	irrev, kind := req.Irreversible, req.IrreversibleKind
	if !irrev {
		if b, k := ClassifyIrreversible(req.Argv); b {
			irrev, kind = true, k
		}
	}
	cmdStr := strings.Join(req.Argv, " ")
	task := Task{
		TaskID: req.TaskID, SessionID: req.SessionID, ToolID: req.ToolID,
		Text: req.Text, Command: cmdStr,
		Irreversible: irrev, IrreversibleKind: kind,
	}
	// 触发点 ②：不可逆动作前审。HALT 时不跑，原因记账。
	if irrev && e.Advisor != nil {
		res, err := e.Check(task, TriggerPreAction)
		if err != nil && !errors.Is(err, ErrAdvisorHalt) && !errors.Is(err, ErrAdvisorHaltFinal) {
			return nil, nil, err
		}
		if res.Verdict == VerdictHalt {
			r := &Result{
				TaskID: req.TaskID, SessionID: req.SessionID, ToolID: req.ToolID, Command: cmdStr,
				ExitCode: -1, Status: "failure",
				AdvisorVerdict: VerdictHalt, AdvisorRetryCount: res.RetryCount,
				AdvisorNote: res.Note,
			}
			if recErr := e.record(r); recErr != nil {
				return nil, nil, recErr
			}
			return r, nil, err // err 为 ErrAdvisorHalt 或 ErrAdvisorHaltFinal
		}
		task.Draft = res.Note // NOTE / CONCERN 的便签随执行结果一起记账
		req.preVerdict = res.Verdict
		req.preNote = res.Note
	}

	if timeout <= InlineThreshold {
		r, err := e.runInline(req, cmdStr, timeout)
		if err != nil {
			return nil, nil, err
		}
		return r, nil, nil
	}
	h := e.runBackground(req, cmdStr, timeout)
	return nil, h, nil
}

// runInline 内联跑一条短命令，等结果。
func (e *Executor) runInline(req RunRequest, cmdStr string, timeout time.Duration) (*Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return e.runCmd(ctx, req, cmdStr, timeout, cancel)
}

// runBackground 起一条长任务，立刻返回句柄。
func (e *Executor) runBackground(req RunRequest, cmdStr string, timeout time.Duration) *Handle {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	h := &Handle{TaskID: req.TaskID, StartedAt: time.Now(), done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(h.done)
		r, err := e.runCmd(ctx, req, cmdStr, timeout, cancel)
		h.mu.Lock()
		h.result, h.err = r, err
		h.mu.Unlock()
	}()
	return h
}

// runCmd 真正跑命令：建进程组（超时杀整组）、抓输出、记账。
func (e *Executor) runCmd(ctx context.Context, req RunRequest, cmdStr string, timeout time.Duration, cancel context.CancelFunc) (*Result, error) {
	start := time.Now()
	cmd := exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if e.Env != nil {
		cmd.Env = e.Env
	} else {
		cmd.Env = os.Environ()
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	dur := time.Since(start)
	r := &Result{
		TaskID: req.TaskID, SessionID: req.SessionID, ToolID: req.ToolID, Command: cmdStr,
		StdoutTail:     lastLines(stdout.String(), 20),
		ErrHead:        truncateRunes(stderr.String(), 600),
		DurationMs:     dur.Milliseconds(),
		AdvisorVerdict: req.preVerdict, AdvisorNote: req.preNote,
	}
	if runErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			r.Killed = true
			r.ExitCode = -1
			r.Status = "failure"
			r.ErrHead = truncateRunes(fmt.Sprintf("executor: 超时 %s 已杀掉", timeout), 600)
		} else if ee, ok := runErr.(*exec.ExitError); ok {
			r.ExitCode = ee.ExitCode()
			r.Status = "failure"
		} else {
			r.ExitCode = -1
			r.Status = "failure"
			r.ErrHead = truncateRunes(fmt.Sprintf("executor: 启动失败: %v", runErr), 600)
		}
	} else {
		r.ExitCode = 0
		r.Status = "success"
	}
	if recErr := e.record(r); recErr != nil {
		return nil, recErr
	}
	return r, nil
}
