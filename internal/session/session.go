// Package session 是 007 的会话与计划状态存盘（P2-a：状态存盘与断点续跑）。
//
// 目录（data/sessions/<session_id>/ 下拆三类文件，前置计划状态与执行层
// 对话历史绝不混塞同一个文件）：
// - plan_state.json：计划事实源（计划表 + 当前步 + 重干次数 + 步间摘要），
// 给前置和网关用；写盘先写临时文件再原子改名。
// - front.jsonl：前置会话（跨轮才需要；单轮任务可不存在，P2-a 的前置不写）。
// - steps/step-<n>.jsonl：每步执行层的 msgHistory，只用于该步崩溃断点续跑；
// 跨步仍按步间摘要传递，不把上一步完整历史塞进下一步。
//
// 不做的（明确划走）：自动压缩续接（P2-b，照三号：20 万翻新、摘要 800–5000 字、
// 硬顶 6000、carryover、旧会话不删）、前置判回执与快路径（规划 v2）、
// 看门狗 WAITING_STEP（规划 v2）、expense_ledger 分 role 记账（规划 v2）。
// 计划 Status 的 DONE/FAILED/CANCELLED 流转由前置/规划 v2 驱动，
// 本包只保证存盘与读盘正确，不替调用方判计划终态（CompleteStep 只改步状态，
// 不自动把计划标 DONE/FAILED）。
package session

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"007-super-agent/internal/ledger"
)

// SessionIDPattern 是 session_id 字符白名单：字母、数字、下划线、连字符，
// 1–64 字。.. 与斜杠一律拒绝（防路径穿越）。
var SessionIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// InterruptedToolNote 是断点续跑时给未闭合 tool_calls 补的 tool 消息内容
// （规格 §4.4 原文）：调用状态未知，让模型先检查现状再继续。
const InterruptedToolNote = "上次进程中断，该次调用状态未知，请先检查现状再继续"

// MaxMessageBytes 是单条存盘消息的内容上限（防单条巨大 tool 输出撑爆磁盘）：
// 超了截断 content 并加标记。msgHistory 里的 tool 结果本身已截到 500 字，
// 这条是最后一道防线。
const MaxMessageBytes = 256 << 10

// MaxStepHistoryBytes 是单个步历史文件的大小上限：超了读回时直接报错，
// 不读进内存（防 OOM）。正常一步历史远小于这个数。
const MaxStepHistoryBytes = 32 << 20

// PlanStatus 是计划终态（规格 §2）：RUNNING / DONE / FAILED / CANCELLED。
// CANCELLED 是叫停态（规划 v2 的派发前检查点，P2-a 只预留字段）。
type PlanStatus string

const (
	PlanRunning PlanStatus = "RUNNING"
	// PlanRolledOver 计划已翻新：旧会话的水位超阈值，计划表已继承到新
	// 会话（P2-b）。终态——翻新后旧会话不再续跑，防止误恢复。
	PlanRolledOver PlanStatus = "ROLLED_OVER"
	PlanDone       PlanStatus = "DONE"
	PlanFailed     PlanStatus = "FAILED"
	PlanCancelled  PlanStatus = "CANCELLED"
)

// IsTerminal 判计划是否已终结（DONE/FAILED/CANCELLED/ROLLED_OVER）。
func (s PlanStatus) IsTerminal() bool {
	return s == PlanDone || s == PlanFailed || s == PlanCancelled || s == PlanRolledOver
}

// PlanPhase 是计划的运行阶段（规划 v2，Gemini 查缺补漏第 6 点）：
// RUNNING（正在跑步）/ WAITING_STEP（已派单，等执行回执）/
// RESUMABLE（进程重启后读回的未终结计划，可断点续跑）。
// Phase 是 Status 的子状态：只在 RUNNING 时有意义，终态计划的 Phase
// 保持最后一次的值，仅供观测，不参与终态判定。
type PlanPhase string

const (
	PhaseRunning     PlanPhase = "RUNNING"
	PhaseWaitingStep PlanPhase = "WAITING_STEP"
	PhaseResumable   PlanPhase = "RESUMABLE"
)

// StepStatus 是单步状态（规格 §2）：
// PENDING / RUNNING / DONE / FAILED / SKIPPED（仅非强依赖可选步可跳过）。
type StepStatus string

const (
	StepPending StepStatus = "PENDING"
	StepRunning StepStatus = "RUNNING"
	StepDone    StepStatus = "DONE"
	StepFailed  StepStatus = "FAILED"
	StepSkipped StepStatus = "SKIPPED"
)

// StepType 是单步类型（规格 §2）：execute（动手产出，必填
// expected_artifacts 或 verify_cmd——由规划器 validatePlan 强制，
// 本包 AllocateStep 不因缺验收拦步，只做 step_type 取值校验，
// 缺验收绝不能让断点续跑丢历史）/ research（只调研，verify 可空）。
type StepType string

const (
	StepExecute  StepType = "execute"
	StepResearch StepType = "research"
)

// PlanStep 是计划表里的一步。
type PlanStep struct {
	StepNo            int      `json:"step_no"`
	TaskID            string   `json:"task_id,omitempty"` // 归属任务（断点续跑去重用：同任务 RUNNING 步复用）
	ToolID            string   `json:"tool_id"`
	Inputs            []string `json:"inputs,omitempty"`
	DependsOn         []int    `json:"depends_on,omitempty"`
	StepType          StepType `json:"step_type"`
	ExpectedArtifacts []string `json:"expected_artifacts,omitempty"`
	VerifyCmd         []string `json:"verify_cmd,omitempty"`
	// ExpectedOutput 是这步做完应得到什么（M-2）：前置的 PlanStep
	// 原样带过来，派单时拼进目标；旧计划缺该字段时读作 ""。
	ExpectedOutput string `json:"expected_output,omitempty"`
	// LastRedoReason 是上一次判回执叫重干的原因（M-2）：重干时带给
	// 执行层，免得它原样重犯同一个错；每重干一次覆盖写一次。
	LastRedoReason string `json:"last_redo_reason,omitempty"`
	// FirstExecIDBefore 是本步首次派发前账本的最大执行行号（方向 b 严谨版）：
	// 重干时账本按步查历史调用的起点；只在首次派发前记录一次，
	// ReopenStep 重干绝不覆盖。老计划表缺该字段时读作 0。
	// 注意：0 不代表“没记过”——账本为空时首次记录就是 0；
	// “记没记过”以 FirstStartTime 是否已设置（非零）为准，
	// 两个字段由 RecordStepStart 原子一起写。
	FirstExecIDBefore int64 `json:"first_exec_id_before,omitempty"`
	// FirstStartTime 是本步首次派发的开始时间（H-2）：产物新鲜度基准
	// 在计划加载不到时可回溯到这里；同样只记首次，重干不覆盖。
	// 它同时是“步起点已记录”的信号（非零 = 已记过）。
	FirstStartTime time.Time `json:"first_start_time,omitempty"`
	// Optional 是该步是否可选（规划 v2）：重干 2 次仍不过时，可选且
	// 无依赖的步可跳过（SKIPPED），否则局部重规划或停下。由前置的
	// PlanStep.Optional 原样带过来，默认 false（不可跳过，宁严勿松）。
	// EstSteps 是这步预估要几轮工具调用（CF 诊断中优）：前置规划时
	// 给，缺省 0 时 stepToAction 按步类型给默认值（research=12，
	// execute=10），StepCap = EstSteps*2（上限 50）。
	EstSteps  int        `json:"est_steps,omitempty"`
	Optional  bool       `json:"optional,omitempty"`
	Status    StepStatus `json:"status"`
	Attempts  int        `json:"attempts"`
	Summary   string     `json:"summary,omitempty"`
	Artifacts []string   `json:"artifacts,omitempty"`
}

// PlanState 是 plan_state.json 的全部内容（规格 §2）。
type PlanState struct {
	PlanID    string `json:"plan_id"`
	SessionID string `json:"session_id"`
	TaskID    string `json:"task_id"`
	// Goal 是本计划的任务目标（M-2）：前置的 PlanArgs.Goal 原样存下，
	// 判回执与改计划时打印给模型看（不要拿 TaskID 当目标）。
	Goal   string     `json:"goal,omitempty"`
	Status PlanStatus `json:"status"`
	// RolledTo 是翻新前进指针（H-6）：本计划翻新后，新会话的 session_id
	// 写在这里；崩溃重启拿旧 session_id 恢复时，沿这条链追到最新活跃
	// 会话，不丢新会话的进度。未翻新时为空。
	RolledTo string `json:"rolled_to,omitempty"`
	// Phase 是运行阶段（规划 v2）：RUNNING / WAITING_STEP / RESUMABLE。
	// 只在 Status==RUNNING 时有意义；旧文件缺该字段时读作 ""，按 RUNNING 论。
	Phase       PlanPhase  `json:"phase,omitempty"`
	CreatedTS   string     `json:"created_ts"`
	UpdatedTS   string     `json:"updated_ts"`
	Steps       []PlanStep `json:"steps"`
	CurrentStep int        `json:"current_step"`
	TotalSteps  int        `json:"total_steps"`
	Failures    []string   `json:"failures"`
	Replans     int        `json:"replans"`
	// PrevPlanID / PrevSummaries：本计划是从一个已终结计划继承来的
	// （OpenSession 防覆写），只读历史摘要，供下一任务接续参考。
	PrevPlanID    string   `json:"prev_plan_id,omitempty"`
	PrevSummaries []string `json:"prev_summaries,omitempty"`
	// TokensUsed 是本会话的水位 = 单次模型调用最大的 prompt_tokens
	// （峰值上下文大小），不是累计和（P2-b）。每次调用的 prompt 已含
	// 全部历史，累加会重复计算；三号的 used 也是当前会话上下文大小。
	TokensUsed int64 `json:"tokens_used,omitempty"`
	// Carryover 是翻新摘要（P2-b）：上段会话的接续摘要全文（含前缀
	// “【上一段会话的接续摘要】”与末尾旧会话文件路径），新会话开头注入。
	Carryover string `json:"carryover,omitempty"`
	// Prev 是翻新链（P2-b）：旧会话记录，只留最近 8 条。
	Prev []PrevSession `json:"prev,omitempty"`
	// RolloverCount 是本会话链的翻新次数（P2-b，观测用）。
	RolloverCount int `json:"rollover_count,omitempty"`
}

// Store 是会话存盘的入口：dataDir 下的 sessions/<session_id>/ 目录。
// led 可为空（测试用）：为空时修复/续跑事件只记日志、不写账本。
// 同一进程内每个 session_id 一把互斥锁（写盘与追加历史串行化）。
type Store struct {
	dataDir string
	led     *ledger.Ledger

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// New 建一个会话存盘入口。dataDir 是数据根目录（sessions 落在其下）。
func New(dataDir string, led *ledger.Ledger) *Store {
	return &Store{dataDir: dataDir, led: led, locks: map[string]*sync.Mutex{}}
}

// ValidateSessionID 校验 session_id 是否合规（白名单，1–64 字）。
// 不合规返回错误，调用方不得静默改写、不得直接拼进路径。
func ValidateSessionID(id string) error {
	if !SessionIDPattern.MatchString(id) {
		return fmt.Errorf("session: session_id %q 非法（只允许字母、数字、下划线、连字符，1–64 字）", id)
	}
	return nil
}

// DefaultSessionID 把任务号映射成合法的默认 session_id（wake 信封没给
// session_id 时用，默认 = task_id）：非法字符换成 -，超长截断并在尾部
// 拼 7 位内容哈希防撞车（同一任务号每次映射结果相同，崩溃重放仍能找回）。
func DefaultSessionID(taskID string) string {
	var b strings.Builder
	for _, r := range taskID {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	s := b.String()
	if s == "" {
		sum := sha256.Sum256([]byte(taskID))
		return "sess-" + hex.EncodeToString(sum[:])[:7]
	}
	if len(s) > 64 {
		sum := sha256.Sum256([]byte(taskID))
		s = s[:56] + "-" + hex.EncodeToString(sum[:])[:7]
	}
	return s
}

// lockFor 取某会话的互斥锁（不存在即新建）。
func (s *Store) lockFor(sessionID string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.locks[sessionID]; ok {
		return l
	}
	l := &sync.Mutex{}
	s.locks[sessionID] = l
	return l
}

// SessionDir 返回某会话的目录（校验失败返回错误，绝不拼非法路径）。
func (s *Store) SessionDir(sessionID string) (string, error) {
	if err := ValidateSessionID(sessionID); err != nil {
		return "", err
	}
	return filepath.Join(s.dataDir, "sessions", sessionID), nil
}

// PlanPath 返回 plan_state.json 的绝对路径。
func (s *Store) PlanPath(sessionID string) (string, error) {
	dir, err := s.SessionDir(sessionID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "plan_state.json"), nil
}

// StepPath 返回某步历史文件的绝对路径（stepNo<=0 时是 front.jsonl）。
func (s *Store) StepPath(sessionID string, stepNo int) (string, error) {
	dir, err := s.SessionDir(sessionID)
	if err != nil {
		return "", err
	}
	if stepNo <= 0 {
		return filepath.Join(dir, "front.jsonl"), nil
	}
	return filepath.Join(dir, "steps", fmt.Sprintf("step-%03d.jsonl", stepNo)), nil
}

// MaxRolloverHops 是沿 RolledTo 链追踪的最多跳数（H-6，防环）：
// 正常链只有 1–2 跳；超过即认为链断了，停在原会话。
const MaxRolloverHops = 16

// loadPlanRaw 读某个会话号的 plan_state.json（不沿 RolledTo 链追踪）：
// 不存在返回 (nil, nil)；损坏返回错误。
func (s *Store) loadPlanRaw(sessionID string) (*PlanState, error) {
	p, err := s.PlanPath(sessionID)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("session: 读 plan_state 失败: %w", err)
	}
	var ps PlanState
	if err := json.Unmarshal(b, &ps); err != nil {
		return nil, fmt.Errorf("session: plan_state 解析失败: %w", err)
	}
	if ps.PlanID == "" || ps.SessionID == "" {
		return nil, errors.New("session: plan_state 缺 plan_id/session_id")
	}
	return &ps, nil
}

// ResolveActiveSession 沿 RolledTo 翻新链追踪到最新活跃会话（H-6）：
// 旧会话翻新后标 ROLLED_OVER 且记 RolledTo=新会话号；崩溃重启拿旧
// session_id 恢复时，沿链找到新会话，不丢进度。防环：最多 16 跳，
// 遇环或链断（指到的会话不存在）停在最后读到的会话号。
// 不持任何会话锁（只读盘），调用方拿到会话号后再取锁。
func (s *Store) ResolveActiveSession(sessionID string) (string, error) {
	if err := ValidateSessionID(sessionID); err != nil {
		return "", err
	}
	cur := sessionID
	seen := map[string]bool{cur: true}
	for i := 0; i < MaxRolloverHops; i++ {
		ps, err := s.loadPlanRaw(cur)
		if err != nil || ps == nil {
			// 读不到（损坏/不存在）：链断，停在当前号，调用方按旧逻辑处理。
			return cur, err
		}
		if ps.Status != PlanRolledOver || strings.TrimSpace(ps.RolledTo) == "" {
			return cur, nil
		}
		next := strings.TrimSpace(ps.RolledTo)
		if err := ValidateSessionID(next); err != nil {
			log.Printf("session %s RolledTo %q 非法，停在当前会话", cur, next)
			return cur, nil
		}
		if seen[next] {
			log.Printf("session %s 翻新链成环（%s 已见过），停在当前会话", cur, next)
			return cur, nil
		}
		seen[next] = true
		cur = next
	}
	log.Printf("session %s 翻新链超 %d 跳，停在 %s", sessionID, MaxRolloverHops, cur)
	return cur, nil
}

// LoadPlan 读 plan_state.json：先沿 RolledTo 链追到最新活跃会话
// （H-6：翻新后旧会话号仍能找回新会话的进度），再读它的计划。
// 不存在返回 (nil, nil)；损坏返回错误（调用方按规格 §4.2 分级处理，
// 不静默丢弃）。要读某个会话号的原始计划（不追踪）时用 loadPlanRaw。
func (s *Store) LoadPlan(sessionID string) (*PlanState, error) {
	resolved, err := s.ResolveActiveSession(sessionID)
	if err != nil {
		return nil, err
	}
	return s.loadPlanRaw(resolved)
}

// SavePlan 原子写 plan_state.json（先写临时文件再改名，防 kill -9 写半截），
// 写前把 UpdatedTS 刷成现在。写盘失败返回错误（调用方记日志，不杀任务）。
func (s *Store) SavePlan(ps *PlanState) error {
	if ps == nil {
		return errors.New("session: plan_state 为空")
	}
	if err := ValidateSessionID(ps.SessionID); err != nil {
		return err
	}
	l := s.lockFor(ps.SessionID)
	l.Lock()
	defer l.Unlock()
	return s.savePlanLocked(ps)
}

// savePlanLocked 是 SavePlan 的持锁版（Store 内部复用）。
func (s *Store) savePlanLocked(ps *PlanState) error {
	p, err := s.PlanPath(ps.SessionID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("session: 建会话目录失败: %w", err)
	}
	ps.UpdatedTS = time.Now().UTC().Format(time.RFC3339)
	b, err := json.MarshalIndent(ps, "", " ")
	if err != nil {
		return fmt.Errorf("session: 序列化 plan_state 失败: %w", err)
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("session: 写 plan_state 临时文件失败: %w", err)
	}
	if err := os.Rename(tmp, p); err != nil {
		return fmt.Errorf("session: plan_state 原子改名失败: %w", err)
	}
	return nil
}

// newPlanID 生成一个计划编号：plan-<UTC时间戳>-<6位随机hex>。
func newPlanID() string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("plan-%s-%s", time.Now().UTC().Format("20060102T150405Z"), hex.EncodeToString(b[:]))
}

// NewPlan 建一个空计划（不落盘，调用方 SavePlan）。
func (s *Store) NewPlan(sessionID, taskID string) *PlanState {
	now := time.Now().UTC().Format(time.RFC3339)
	return &PlanState{
		PlanID: newPlanID(), SessionID: sessionID, TaskID: taskID,
		Status: PlanRunning, CreatedTS: now, UpdatedTS: now,
		Steps: []PlanStep{}, Failures: []string{},
	}
}

// OpenSession 打开一个会话的计划（供前置派单与执行续跑共用）：
// - 先沿 RolledTo 翻新链追到最新活跃会话（H-6）：崩溃重启拿旧号
// 恢复时找回新会话的进度，不丢单；链断/损坏时退回原号走旧逻辑。
// - 无计划 → 新建并保存，返回 (plan, true)。
// - 计划未终结 → 返回原计划，(plan, false)。
// - 计划已终结（DONE/FAILED/CANCELLED）→ 新任务不得直接覆写续跑：
// 新开一个 plan_id，只继承只读历史摘要（上一计划已完成步的摘要），
// 返回 (newPlan, true)。
// - plan_state.json 损坏 → 记事件，按 steps/ 目录重建一个最小计划
// （发现 N 个步骤历史文件，CurrentStep=N+1），返回 (plan, true)。
func (s *Store) OpenSession(sessionID, taskID string) (*PlanState, bool, error) {
	if err := ValidateSessionID(sessionID); err != nil {
		return nil, false, err
	}
	// H-6：先追踪到最新活跃会话再取锁（锁要取在真正要写的会话上，
	// 不能锁旧号、写新会话的文件）。
	resolved, rerr := s.ResolveActiveSession(sessionID)
	if rerr != nil || resolved == "" {
		resolved = sessionID
	}
	l := s.lockFor(resolved)
	l.Lock()
	defer l.Unlock()
	return s.openSessionLocked(resolved, taskID)
}

// countStepFiles 数 steps/ 下已有多少个 step-<n>.jsonl 文件（重建用）。
func (s *Store) countStepFiles(sessionID string) int {
	dir, err := s.SessionDir(sessionID)
	if err != nil {
		return 0
	}
	entries, err := os.ReadDir(filepath.Join(dir, "steps"))
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "step-") && strings.HasSuffix(e.Name(), ".jsonl") {
			n++
		}
	}
	return n
}

// stepSummaryLine 把一步已完成记录写成一行接续摘要。
// （2026-11-26 放宽：摘要从 200 字放宽到 1000 字——200 字把上一步的
// 关键路径拦腰切断，执行层只能重读文件，CF-015 的教训。）
func stepSummaryLine(st PlanStep) string {
	line := fmt.Sprintf("步骤%d（%s）", st.StepNo, st.ToolID)
	if st.Summary != "" {
		line += "：" + truncateRunes(st.Summary, 1000)
	}
	if len(st.Artifacts) > 0 {
		line += "；已产出 " + strings.Join(st.Artifacts, "、")
	}
	return line
}

// AllocateStep 给一次派单在会话计划里占一位（前置派单时调）：
// - 同任务的上一 RUNNING 步还在 → 复用该步号（崩溃重放断点续跑，
// 不另开一步），Attempts+1，返回旧步号。
// - 否则追加新步（StepNo=len(Steps)+1，Status=RUNNING，Attempts=1），
// CurrentStep=新步号，TotalSteps=步数，保存，返回新步号。
// step_type 非法返回错误；execute 缺验收不拦（缺验收绝不能让断点续跑丢历史，
// 验收必填由规划器 validatePlan 强制）。分配失败不阻塞派单，
// 调用方记日志、用 StepNo=0 派单（本步不做会话存盘）。
func (s *Store) AllocateStep(sessionID, taskID string, step PlanStep) (int, error) {
	if err := ValidateSessionID(sessionID); err != nil {
		return 0, err
	}
	if step.StepType == "" {
		step.StepType = StepExecute
	}
	if step.StepType != StepExecute && step.StepType != StepResearch {
		return 0, fmt.Errorf("session: step_type %q 非法，只能是 execute/research", step.StepType)
	}
	step.TaskID = taskID
	// H-6：先沿翻新链追到最新活跃会话再取锁（崩溃重启拿旧号派单时，
	// 步要占在新会话的计划表里，不能占进已终结的旧会话）。
	resolved, rerr := s.ResolveActiveSession(sessionID)
	if rerr != nil || resolved == "" {
		resolved = sessionID
	}
	l := s.lockFor(resolved)
	l.Lock()
	defer l.Unlock()
	ps, _, err := s.openSessionLocked(resolved, taskID)
	if err != nil {
		return 0, err
	}
	if n := len(ps.Steps); n > 0 {
		last := &ps.Steps[n-1]
		if last.TaskID == taskID && last.Status == StepRunning {
			last.Attempts++
			ps.CurrentStep = last.StepNo
			if err := s.savePlanLocked(ps); err != nil {
				return 0, err
			}
			return last.StepNo, nil
		}
	}
	step.StepNo = len(ps.Steps) + 1
	step.Status = StepRunning
	step.Attempts = 1
	ps.Steps = append(ps.Steps, step)
	ps.CurrentStep = step.StepNo
	ps.TotalSteps = len(ps.Steps)
	if err := s.savePlanLocked(ps); err != nil {
		return 0, err
	}
	return step.StepNo, nil
}

// openSessionLocked 是 OpenSession 的持锁版（Store 内部复用，调用方须已持锁）。
// 传入的 sessionID 须是调用方已沿 RolledTo 链追踪过的最新活跃会话号，
// 这里直接读原始文件（loadPlanRaw，不再二次追踪）。
func (s *Store) openSessionLocked(sessionID, taskID string) (*PlanState, bool, error) {
	ps, err := s.loadPlanRaw(sessionID)
	if err != nil {
		log.Printf("session %s plan_state 损坏，按 steps/ 重建: %v", sessionID, err)
		s.event(sessionID, taskID, ledger.DispatchEventSessionRepair,
			fmt.Sprintf("plan_state 损坏，按 steps/ 重建: %v", err))
		n := s.countStepFiles(sessionID)
		np := s.NewPlan(sessionID, taskID)
		np.CurrentStep = n + 1
		np.PrevSummaries = []string{fmt.Sprintf("旧 plan_state 损坏，已按 steps/ 重建；发现 %d 个步骤历史文件", n)}
		if err := s.savePlanLocked(np); err != nil {
			return nil, false, err
		}
		return np, true, nil
	}
	if ps == nil {
		np := s.NewPlan(sessionID, taskID)
		if err := s.savePlanLocked(np); err != nil {
			return nil, false, err
		}
		return np, true, nil
	}
	if ps.Status.IsTerminal() {
		np := s.NewPlan(sessionID, taskID)
		np.PrevPlanID = ps.PlanID
		for _, st := range ps.Steps {
			if st.Status == StepDone {
				np.PrevSummaries = append(np.PrevSummaries, stepSummaryLine(st))
			}
		}
		if err := s.savePlanLocked(np); err != nil {
			return nil, false, err
		}
		return np, true, nil
	}
	return ps, false, nil
}

// CompleteStep 更新一步的终态（执行完由 executeDispatch 调）：
// Status/Summary（截 1000 字，2026-11-26 从 500 放宽）/Artifacts 写进该步；FAILED 时把 failure
// 记进计划 Failures（最多留 20 条）；CurrentStep 进到下一步。
// 计划 Status 不自动流转（由前置/规划 v2 判终态，见包说明）。
// 找不到步返回错误。
func (s *Store) CompleteStep(sessionID string, stepNo int, status StepStatus, summary string, artifacts []string, failure string) error {
	if err := ValidateSessionID(sessionID); err != nil {
		return err
	}
	l := s.lockFor(sessionID)
	l.Lock()
	defer l.Unlock()
	ps, err := s.LoadPlan(sessionID)
	if err != nil {
		return err
	}
	if ps == nil {
		return fmt.Errorf("session: 会话 %q 无计划，步 %d 无处更新", sessionID, stepNo)
	}
	found := false
	for i := range ps.Steps {
		if ps.Steps[i].StepNo == stepNo {
			ps.Steps[i].Status = status
			ps.Steps[i].Summary = truncateRunes(summary, 1000)
			ps.Steps[i].Artifacts = artifacts
			if status == StepFailed && failure != "" {
				ps.Failures = append(ps.Failures, truncateRunes(failure, 300))
				if len(ps.Failures) > 20 {
					ps.Failures = ps.Failures[len(ps.Failures)-20:]
				}
			}
			ps.CurrentStep = stepNo + 1
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("session: 会话 %q 计划里没有步 %d", sessionID, stepNo)
	}
	return s.savePlanLocked(ps)
}

// ReopenStep 把一步重新置为 RUNNING（规划 v2 判回执后决定重干时用）：
// 步状态回 RUNNING、尝试次数 +1、计划当前步回到该步、阶段回 RUNNING。
// RecordStepStart 记下本步首次派发的账本起点与开始时间（方向 b 严谨版）：
// 仅当 FirstStartTime 未设置（零值，从未记过）时写入；已记过则原样不动，
// 重干绝不覆盖。调用方（前置派发前）负责传当前账本最大行号与当前时间。
// 注意：不用 FirstExecIDBefore==0 当“没记过”的判断——账本为空时
// 首次记录就是 0，再调一次会误把起点往后挪、丢掉本步历史调用。
func (s *Store) RecordStepStart(sessionID string, stepNo int, execIDBefore int64, startTime time.Time) error {
	if err := ValidateSessionID(sessionID); err != nil {
		return err
	}
	l := s.lockFor(sessionID)
	l.Lock()
	defer l.Unlock()
	ps, err := s.LoadPlan(sessionID)
	if err != nil {
		return err
	}
	if ps == nil {
		return fmt.Errorf("session: 会话 %q 无计划，步 %d 无起点可记", sessionID, stepNo)
	}
	for i := range ps.Steps {
		if ps.Steps[i].StepNo == stepNo {
			if !ps.Steps[i].FirstStartTime.IsZero() {
				return nil // 已记过，不覆盖
			}
			ps.Steps[i].FirstExecIDBefore = execIDBefore
			ps.Steps[i].FirstStartTime = startTime
			return s.savePlanLocked(ps)
		}
	}
	return fmt.Errorf("session: 会话 %q 计划里没有步 %d", sessionID, stepNo)
}

// reason 是上一次判回执叫重干的原因（M-2）：写进该步的 LastRedoReason，
// 下次派单带给执行层，免得它原样重犯同一个错；reason 为空时不覆盖旧值。
// CompleteStep 总把 CurrentStep 推到 stepNo+1，不适用于重干——
// 重干要原地再跑一次，CurrentStep 必须回到该步。
func (s *Store) ReopenStep(sessionID string, stepNo int, reason string) error {
	if err := ValidateSessionID(sessionID); err != nil {
		return err
	}
	l := s.lockFor(sessionID)
	l.Lock()
	defer l.Unlock()
	ps, err := s.LoadPlan(sessionID)
	if err != nil {
		return err
	}
	if ps == nil {
		return fmt.Errorf("session: 会话 %q 无计划，步 %d 无处重开", sessionID, stepNo)
	}
	found := false
	for i := range ps.Steps {
		if ps.Steps[i].StepNo == stepNo {
			ps.Steps[i].Status = StepRunning
			ps.Steps[i].Attempts++
			if strings.TrimSpace(reason) != "" {
				ps.Steps[i].LastRedoReason = truncateRunes(reason, 300)
			}
			// 防锚定兜底（低危）：第 2 次尝试（Attempts == 2）时把旧的
			// step-<n>.jsonl 归档改名，让这次重干在干净上下文中带着
			// LastRedoReason 重新开始；旧历史留作排查，不删。
			// 归档失败只记日志，不阻塞重干。
			if ps.Steps[i].Attempts == 2 {
				if p, perr := s.StepPath(sessionID, stepNo); perr == nil {
					if _, serr := os.Stat(p); serr == nil {
						archived := strings.TrimSuffix(p, ".jsonl") + ".attempt-1.jsonl"
						if rerr := os.Rename(p, archived); rerr != nil {
							log.Printf("session: 步 %d 历史归档失败（不阻塞重干）: %v", stepNo, rerr)
						} else {
							log.Printf("session: 步 %d 第 2 次重干，旧历史已归档 %s", stepNo, archived)
						}
					}
				}
			}
			ps.CurrentStep = stepNo
			ps.Phase = PhaseRunning
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("session: 会话 %q 计划里没有步 %d", sessionID, stepNo)
	}
	return s.savePlanLocked(ps)
}

// SetPhase 改计划的运行阶段（规划 v2）：RUNNING / WAITING_STEP /
// RESUMABLE。只改 Phase，不动 Status 与步状态；终态计划调它无效
// （终态不再流转，Phase 保持最后一次的值）。
func (s *Store) SetPhase(sessionID string, phase PlanPhase) error {
	if err := ValidateSessionID(sessionID); err != nil {
		return err
	}
	l := s.lockFor(sessionID)
	l.Lock()
	defer l.Unlock()
	ps, err := s.LoadPlan(sessionID)
	if err != nil {
		return err
	}
	if ps == nil {
		return fmt.Errorf("session: 会话 %q 无计划，无法改阶段", sessionID)
	}
	if ps.Status.IsTerminal() {
		return fmt.Errorf("session: 会话 %q 计划已终结（%s），阶段不再流转", sessionID, ps.Status)
	}
	ps.Phase = phase
	return s.savePlanLocked(ps)
}

// CancelPlan 把计划标为 CANCELLED（规划 v2 叫停态）：前置在每步派发前
// 查 Status，CANCELLED 即停下诚实汇报，不再派新步。已跑完的步不动，
// 正在跑的步由执行层自己收尾（回执回来后前置不再推进）。
// reason 记进 Failures（截 300 字），便于事后查为什么停。
func (s *Store) CancelPlan(sessionID, reason string) error {
	if err := ValidateSessionID(sessionID); err != nil {
		return err
	}
	l := s.lockFor(sessionID)
	l.Lock()
	defer l.Unlock()
	ps, err := s.LoadPlan(sessionID)
	if err != nil {
		return err
	}
	if ps == nil {
		return fmt.Errorf("session: 会话 %q 无计划，无法叫停", sessionID)
	}
	if ps.Status.IsTerminal() {
		return fmt.Errorf("session: 会话 %q 计划已终结（%s），无法叫停", sessionID, ps.Status)
	}
	ps.Status = PlanCancelled
	ps.Phase = PhaseRunning
	if reason != "" {
		ps.Failures = append(ps.Failures, truncateRunes(reason, 300))
		if len(ps.Failures) > 20 {
			ps.Failures = ps.Failures[len(ps.Failures)-20:]
		}
	}
	return s.savePlanLocked(ps)
}

// ListActivePlans 列出所有未终结的计划（L-1：启动时断点续跑的观测入口）：
// 读 sessions/ 下每个会话目录的原始计划（loadPlanRaw，不沿 RolledTo
// 追踪——追踪会在旧目录上读出新会话的计划，造成重复），Status 未终结
// 的返回。调用方（main 启动时）在网关 ReplayInbox 之后调它，只记日志
// 告诉值班的人“这几个计划还活着、停在第几步”；真正把计划拉起来跑
// 靠网关重放未确认的 inbox 行（外部重投递），不是本函数——
// 进程重启后没有 inbox 行的计划不会被自动唤醒，这是设计如此，
// 不是漏扫（WAITING_STEP/RESUMABLE 只表示“可续跑”，唤醒靠重投递）。
func (s *Store) ListActivePlans() ([]*PlanState, error) {
	root := filepath.Join(s.dataDir, "sessions")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("session: 读 sessions 目录失败: %w", err)
	}
	var out []*PlanState
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		if err := ValidateSessionID(id); err != nil {
			continue
		}
		ps, err := s.loadPlanRaw(id)
		if err != nil || ps == nil {
			continue
		}
		if ps.Status.IsTerminal() {
			continue
		}
		out = append(out, ps)
	}
	return out, nil
}

// PlanSummary 给下一任务的接续摘要（runAgent 拼进记忆摘要用）：
// 上一计划的只读摘要 + 本计划已完成步（产物路径为主）。
// P2-b：翻新摘要（carryover）放最前面、原样带上（生成时已硬顶
// 6000 字，这里不再截）；步摘要部分仍截到 2000 字。
// 无内容返回 ""。
func (s *Store) PlanSummary(sessionID string) (string, error) {
	if err := ValidateSessionID(sessionID); err != nil {
		return "", err
	}
	ps, err := s.LoadPlan(sessionID)
	if err != nil {
		return "", err
	}
	if ps == nil {
		return "", nil
	}
	var parts []string
	if strings.TrimSpace(ps.Carryover) != "" {
		// 翻新摘要（接续摘要）原样带上，不截（已在生成时硬顶 6000 字）。
		parts = append(parts, ps.Carryover)
	}
	var lines []string
	for _, sum := range ps.PrevSummaries {
		lines = append(lines, sum)
	}
	done := 0
	for _, st := range ps.Steps {
		if st.Status == StepDone {
			lines = append(lines, stepSummaryLine(st))
			done++
		}
	}
	if len(lines) > 0 {
		out := fmt.Sprintf("本会话已完成 %d 步：\n- %s", done, strings.Join(lines, "\n- "))
		parts = append(parts, truncateRunes(out, 2000))
	}
	if len(parts) == 0 {
		return "", nil
	}
	return strings.Join(parts, "\n"), nil
}

// event 记一笔 dispatch_events（修复/续跑事件用；led 为空时只记日志）。
func (s *Store) event(sessionID, taskID, kind, detail string) {
	if s.led == nil {
		log.Printf("session %s task=%s %s: %s", sessionID, taskID, kind, detail)
		return
	}
	if _, err := s.led.RecordDispatchEvent(ledger.DispatchEvent{
		SessionID: sessionID, TaskID: taskID, Kind: kind, Detail: detail,
	}); err != nil {
		log.Printf("session %s task=%s 记事件失败: %v", sessionID, taskID, err)
	}
}

// truncateRunes 按字符截断到 n 个字（先转 []rune，防中文乱码）。
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
