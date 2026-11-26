// Package dispatch 是前置会话与执行会话之间的内部通道（第 1 项）。
//
// 派单（Dispatch）是前置写给执行的唯一信物：用户原话、目标、验收标准、
// 工具编号、步骤骨架、记忆碎片、指针、约束、预估步数与耗时，
// 总量 ≤3000 token（线 A：F-08 从 1000 放宽到 3000，保守口径）。
// 执行只按派单干活，不重分诊、不重查记忆。
//
// 通道分两层：
//   - 落盘：每份派单写 data/dispatch/<id>.json，每份回执写
//     data/dispatch/<id>.receipt.json；进程重启后，未回执的派单
//     由 Worker 启动时重入队（不丢单）。
//   - 内存：Send 把派单推进缓冲 channel，Worker  goroutine 取出执行；
//     执行完把回执（Receipt）写回文件，并经 ReplyCh 交还给等它的
//     前置（同步流）；没人等时只落盘，前置下次唤醒再汇报（F-10）。
package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// MaxDispatchTokens 是派单的 token 硬上限（线 A：F-08 从 1000 改到 3000；
// 计数用 registry.EstimateDispatchTokens 保守口径，不是旧审计口径）。
const MaxDispatchTokens = 3000

// SoftAlertTokens 是派单的软告警阈值（线 A）：达到即记日志并写账本事件，
// 但不拦截，派单照常发送。
const SoftAlertTokens = 1200

// Dispatch 是前置写给执行的派单（F-09 内容）。
type Dispatch struct {
	ID           string   `json:"id"`            // 派单编号（任务号 + 时间戳，全局唯一）
	TaskID       string   `json:"task_id"`       // 归属任务
	Route        string   `json:"route"`         // TOOL / AGENT / META
	UserText     string   `json:"user_text"`     // 用户原话
	Goal         string   `json:"goal"`          // 目标
	Criteria     string   `json:"criteria"`      // 验收标准
	ToolIDs      []string `json:"tool_ids"`      // 工具编号（已分诊，执行不重分诊）
	PlanSkeleton []string `json:"plan_skeleton"` // 步骤骨架（工具编号 + 顺序 + 指针）
	Memory       []string `json:"memory"`        // 记忆碎片（前置已查到的前 3 条，只贴碎片）
	// MustReadMemories 是必读记忆编号（2026-11-26 记忆召回整改）：
	// 前置查到方法/剧本类记忆后写进派单（最多 2 个，只写编号不贴全文）；
	// 执行开工时代码自动读全文喂给模型，不靠模型自己调工具。
	MustReadMemories []string `json:"must_read_memories,omitempty"`
	Pointers         []string `json:"pointers"`    // 指针（路径/链接/记忆 ID，不贴全文）
	Constraints      []string `json:"constraints"` // 约束
	EstSteps         int      `json:"est_steps"`   // 预估步数
	EstSeconds       int      `json:"est_seconds"` // 预估耗时（秒）
	ShardNo          int      `json:"shard_no"`    // 分片号（第 15 项：0 表示不分片；>0 表示这是第几片）
	ResumeFrom       string   `json:"resume_from"` // 续跑起点（上一片的断点指针；第一片为空）
	CreatedAt        string   `json:"created_at"`  // RFC3339 UTC

	// TaskType 是任务类型（P0-2/P0-3）：execute（默认，动手产出）/
	// research（只调研，交付报告）。前置填，未填按 execute。
	TaskType string `json:"task_type,omitempty"`
	// ExpectedArtifacts 是 execute 任务要产出的文件绝对路径清单
	// （P0-2）：网关验货时逐项核对（存在/非空/新生成）。research 任务不填。
	ExpectedArtifacts []string `json:"expected_artifacts,omitempty"`
	// VerifyCmd 是无文件产出时的校验命令（P0-2，argv 已拆分）：
	// 网关执行它，exit 0 才算过；60 秒超时。research 任务不填。
	VerifyCmd []string `json:"verify_cmd,omitempty"`

	// ReplyCh 是执行完交回执的通道（进程内，不落盘）。为 nil 表示没人等，
	// 执行完只写回执文件。缓冲 1，执行方发送不阻塞。
	ReplyCh chan *Receipt `json:"-"`

	// SessionID 是会话号（P2-a）：本步归属的会话，执行层按
	// (session, step) 存对话历史、断点续跑。为空表示不做会话存盘。
	SessionID string `json:"session_id,omitempty"`
	// StepNo 是本步在会话计划里的步号（P2-a）：>0 时执行层按
	// steps/step-<n>.jsonl 存取本步历史；0 表示不做会话存盘。
	StepNo int `json:"step_no,omitempty"`
	// PlanStep 是不是多步计划里的一步（H-5）：只有 plan_task 派出的
	// 步才为真。计划步的执行草稿写 step-<n>-reply.md，不写顶层
	// reply.md——看门狗见新鲜顶层 reply.md 会误把整单标 DONE。
	// 单步派单（AllocateStep 占位来的）为假，草稿仍写顶层 reply.md。
	PlanStep bool `json:"plan_step,omitempty"`
	// FirstExecIDBefore 是本步首次派发前账本的最大执行行号（方向 b 严谨版）：
	// 网关按步查历史调用的起点；由前置在首次派发前记下并透传，
	// 重干时原样带过来，绝不更新。
	FirstExecIDBefore int64 `json:"first_exec_id_before,omitempty"`
	// FirstStartTime 是本步首次派发的开始时间（H-2）：产物新鲜度基准
	// 在计划加载不到时可回溯到这里；同样只记首次，重干不更新。
	FirstStartTime time.Time `json:"first_start_time,omitempty"`
	// LastRedoReason 是上一次判回执叫重干的原因（H-1/H-2）：前置从
	// 计划步里原样透传；执行层续跑时靠它知道自己错在哪。
	LastRedoReason string `json:"last_redo_reason,omitempty"`

	// FactList 是网关随回执附的免费事实清单（规划 v2）：执行层在
	// runAgent 里算一次（只读代码查，不花模型调用），buildReceipt
	// 原样抄进回执。进程内传递，不落盘（json:"-"）。
	FactList *FactList `json:"-"`
}

// 缺失原因分类（FactList 缺失四数用的口径，main.go 分类时用）。
// a 早一步做的 / b 以前的旧文件：按文件 mtime 与任务开始时间比；
// c 不存在 / d 是空的：与时间无关。
const (
	MissingReasonEarlyTask   = iota // 这单里早一步做的：存在、非空，mtime 在任务开始之后、本轮派单之前
	MissingReasonOld                // 以前的旧文件：存在、非空，mtime 不晚于任务开始时间
	MissingReasonNonexistent        // 不存在：文件不在盘上（或是个目录）
	MissingReasonEmpty              // 是空的：存在但去空白后无内容
)

// ArtifactFact 是一项声明产物的事实（代码查的原始数据）。
type ArtifactFact struct {
	Path     string `json:"path"`              // 声明的绝对路径
	Exists   bool   `json:"exists"`            // 文件在不在
	Size     int64  `json:"size"`              // 字节数（不存在为 0）
	Fresh    bool   `json:"fresh"`             // mtime 不早于验货基线（允许 2 秒容差）
	NonEmpty bool   `json:"non_empty"`         // 去空白后非空
	Valid    bool   `json:"valid"`             // .json 能解析（非 .json 文件恒为真）
	Problem  string `json:"problem,omitempty"` // 不合格的原因（人话短句）
}

// VerifyCmdFact 是 verify_cmd 的执行事实。
type VerifyCmdFact struct {
	Cmd      []string `json:"cmd"`       // 校验命令 argv
	Ran      bool     `json:"ran"`       // 是否实际执行了（未声明时为假）
	ExitCode int      `json:"exit_code"` // 退出码（未执行时为 -1）
	Passed   bool     `json:"passed"`    // exit 0
	Detail   string   `json:"detail,omitempty"`
}

// FactList 是网关随回执附的免费事实清单（规划 v2，Gemini 查缺补漏第 1 点）：
// 代码查的原始事实，不花模型调用；是前置判回执的参考证据，不是通过门。
// HardFloorRed 只当一条记录供前置看（为真表示有问题：0 次成功调用 /
// execute 步无产物全合格、无校验命令真实通过、且 0 次实质调用 /
// 声明产物缺失或空或过期），不再由代码一票否决——前置看事实清单自己判
// （2026-11-26 用户定：一票否决已去掉）。verify_cmd 不进硬底线（2026-11-25
// 对齐二号三号）：声明了且跑了的结果记在这里当参考证据，只供前置
// 模型看，不由代码一票否决。
// AllGreen 是快路径条件：有机器检查且全部事实都绿。
// LastPythonCmd 是本步最后一次 cap-ops-python 成功调用的命令摘要
// （M-3：python 是通用执行工具，光数“跑没跑”分不出真干活还是空跑
// 一句只读探测；摘要供前置模型一眼识别是不是真干了活）。
type FactList struct {
	Artifacts        []ArtifactFact `json:"artifacts,omitempty"`
	VerifyCmd        *VerifyCmdFact `json:"verify_cmd,omitempty"`
	SubstantiveCalls int            `json:"substantive_calls"` // 成功且非只读的工具调用次数
	SuccessfulCalls  int            `json:"successful_calls"`  // 成功工具调用总次数（不含 verify-cmd）
	HasMachineCheck  bool           `json:"has_machine_check"` // 声明了 expected_artifacts 或 verify_cmd
	HardFloorRed     bool           `json:"hard_floor_red"`
	HardFloorReason  string         `json:"hard_floor_reason,omitempty"`
	AllGreen         bool           `json:"all_green"`
	LastPythonCmd    string         `json:"last_python_cmd,omitempty"` // 最后一次 python 成功调用的命令摘要（截 200 字）
	// PriorStepCalls 是本步之前尝试在账本中的成功调用数（方向 b 严谨版）：
	// 只数 FirstExecIDBefore < e.ID <= execIDBefore 的行，绝不含别步；
	// 仅用于硬底线①的续跑豁免与事实展示，绝不算进本轮 SuccessfulCalls，
	// 兜底放行也不认它（fail-closed）。
	PriorStepCalls int `json:"prior_step_calls,omitempty"`
	// PriorStepSubstantiveCalls 是本步之前尝试的实质调用数（同上口径）。
	PriorStepSubstantiveCalls int `json:"prior_step_substantive_calls,omitempty"`
	// Resumed 是本次执行是否触发了断点续跑（读回了本步历史）。
	Resumed bool `json:"resumed,omitempty"`
	// ArtifactNote 是产物新鲜度的补充说明（H-2）：续跑时产物上次修改
	// 于本步首轮，供前置模型看，不改硬底线。
	ArtifactNote string `json:"artifact_note,omitempty"`
	// 置信度（2026-11-26 用户定，两点各 50%）：
	// ①工具调用：调了 ≥1 次得 50 分，没调 0 分；回执没声明成果文件
	// （ClaimedFiles==0）时这点占 100 分（调了 100，没调 0）。
	// ②成果文件核对：回执“成果在哪里”列的文件逐个核（存在、去空白后
	// 非空、mtime 在本步接单时间之后），核对有 M 个 / 回执说 N 个，
	// 这点 = M/N×50 分；N==0 时这点 0 分。
	// 置信度 = 两点相加（0–100）。回执状态不进百分比，前置自己看状态。
	Confidence           int      `json:"confidence"`                    // 0–100
	ConfidenceToolPoints int      `json:"confidence_tool_points"`        // 工具调用分（0/50/100）
	ConfidenceFilePoints int      `json:"confidence_file_points"`        // 成果文件核对分（0–50）
	ToolCallBreakdown    string   `json:"tool_call_breakdown,omitempty"` // 成功调用按工具分，如 cap-ops-python×41
	ClaimedFiles         int      `json:"claimed_files"`                 // 回执“成果在哪里”列出的文件数
	VerifiedFiles        int      `json:"verified_files"`                // 核对有的文件数
	MissingFiles         []string `json:"missing_files,omitempty"`       // 核对缺失的文件路径
	// TaskStartTime 是任务开始时间（任务首次派单的时间）：
	// Summary 第 5 行“任务开始时间”与缺失原因分类（“这单里早一步做的”
	// 指 mtime 在任务开始之后、本轮派单之前）都靠它；为零表示取不到
	// （老任务/无派单文件），Summary 里写“未知”，分类时按
	// fail-closed 判成“以前的旧文件”。
	TaskStartTime time.Time `json:"task_start_time,omitempty"`
	// 缺失原因聚合（2026-11-26 用户定：Summary 第 4 行只报各类个数，
	// 不报单个文件名——任务文件几十几百个时逐个报名字会把派单 token
	// 撑爆）。由 computeFactList 按文件 mtime 与任务开始时间、
	// 与本轮派单时间分类填入；四数之和 = 缺失文件数。
	MissingEarlyTask   int `json:"missing_early_task,omitempty"`  // 缺失里“这单里早一步做的”个数
	MissingOld         int `json:"missing_old,omitempty"`         // 缺失里“以前的旧文件”个数
	MissingNonexistent int `json:"missing_nonexistent,omitempty"` // 缺失里“不存在”个数
	MissingEmpty       int `json:"missing_empty,omitempty"`       // 缺失里“是空的”个数
}

// Summary 把事实清单压成六行摘要（2026-11-26 用户定，两次改版）：
// 第 1 行置信度、第 2 行工具调用、第 3 行回执内成果文件、
// 第 4 行成果文件核对结果（只报各类个数，不报单个文件名）、
// 第 5 行任务开始时间（北京时间）、第 6 行提醒前置核实的注意句。
// 回执没声明成果文件时第 4 行省略（共五行）。
// 格式：
//
//	结果置信度：90%（工具 50% + 成果 40%）
//	工具调用：41 次（cap-ops-python×41）
//	回执内成果文件：10 个
//	成果文件核对结果（回执 10/实际 8/缺失 2）：缺失的 2 个都是这单里早一步做的，所以判为 成果文件缺失！
//	任务开始时间：2026-11-26 15:57:32
//	注意命令：以上事实信息仅供参考。如果置信度低于 100%，必须先读这步的详细报告并核对成果文件，确认后再判！
func (f *FactList) Summary() string {
	if f == nil {
		return "事实清单缺失（执行层未算）"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "结果置信度：%d%%（工具 %d%% + 成果 %d%%）\n", f.Confidence, f.ConfidenceToolPoints, f.ConfidenceFilePoints)
	if f.ToolCallBreakdown != "" {
		fmt.Fprintf(&b, "工具调用：%d 次（%s）\n", f.SuccessfulCalls, f.ToolCallBreakdown)
	} else {
		fmt.Fprintf(&b, "工具调用：%d 次\n", f.SuccessfulCalls)
	}
	if f.ClaimedFiles == 0 {
		fmt.Fprintf(&b, "回执内成果文件：0 个（回执没声明成果文件）\n")
	} else {
		fmt.Fprintf(&b, "回执内成果文件：%d 个\n", f.ClaimedFiles)
		fmt.Fprintf(&b, "成果文件核对结果（回执 %d/实际 %d/缺失 %d）：%s\n", f.ClaimedFiles, f.VerifiedFiles, len(f.MissingFiles), f.verificationReason())
	}
	if f.TaskStartTime.IsZero() {
		fmt.Fprintf(&b, "任务开始时间：未知\n")
	} else {
		fmt.Fprintf(&b, "任务开始时间：%s\n", f.TaskStartTime.UTC().Add(8*time.Hour).Format("2006-01-02 15:04:05"))
	}
	fmt.Fprintf(&b, "注意命令：以上事实信息仅供参考。如果置信度低于 100%%，必须先读这步的详细报告并核对成果文件，确认后再判！")
	return b.String()
}

// verificationReason 拼第 4 行冒号后的原因句（只报个数，不报文件名）。
// 缺失 0 个：文件都在本轮派单之后，判有文件；缺失 >0 个：按四类个数
// 聚合，判成果文件缺失；四类个数全为 0 但缺失 >0（老数据/手工构造
// 的 FactList 没分类）时只报个数，不编原因。
func (f *FactList) verificationReason() string {
	missing := len(f.MissingFiles)
	if missing == 0 {
		return "有文件存在，最后修改/操作时间在本轮派单之后，所以判为 有 文件！"
	}
	early, old, nonexistent, empty := f.MissingEarlyTask, f.MissingOld, f.MissingNonexistent, f.MissingEmpty
	if early+old+nonexistent+empty == 0 {
		return fmt.Sprintf("缺失的 %d 个，所以判为 成果文件缺失！", missing)
	}
	// 只一类非零：缺失全属这一类；多类非零：按 a→b→c→d 顺序列个数。
	nonZero := 0
	for _, n := range []int{early, old, nonexistent, empty} {
		if n > 0 {
			nonZero++
		}
	}
	if nonZero == 1 {
		switch {
		case early > 0:
			return fmt.Sprintf("缺失的 %d 个都是这单里早一步做的，所以判为 成果文件缺失！", missing)
		case old > 0:
			return fmt.Sprintf("缺失的 %d 个都是以前的旧文件，所以判为 成果文件缺失！", missing)
		case nonexistent > 0:
			return fmt.Sprintf("缺失的 %d 个都不存在，所以判为 成果文件缺失！", missing)
		default:
			return fmt.Sprintf("缺失的 %d 个都是空的，所以判为 成果文件缺失！", missing)
		}
	}
	var parts []string
	if early > 0 {
		parts = append(parts, fmt.Sprintf("%d 个是这单里早一步做的", early))
	}
	if old > 0 {
		parts = append(parts, fmt.Sprintf("%d 个是以前的旧文件", old))
	}
	if nonexistent > 0 {
		parts = append(parts, fmt.Sprintf("%d 个不存在", nonexistent))
	}
	if empty > 0 {
		parts = append(parts, fmt.Sprintf("%d 个是空的", empty))
	}
	return fmt.Sprintf("缺失的 %d 个里，%s，所以判为 成果文件缺失！", missing, strings.Join(parts, "，"))
}

// Receipt 是执行交回的短回执（F-10：成败只看回执）。
type Receipt struct {
	DispatchID    string   `json:"dispatch_id"`
	TaskID        string   `json:"task_id"`
	Status        string   `json:"status"`         // done / stalled / failed / max_iterations
	Conclusion    string   `json:"conclusion"`     // 结论（人话短句，不贴日志）
	Artifacts     []string `json:"artifacts"`      // 产物指针（路径）
	CostCNYMilli  int64    `json:"cost_cny_milli"` // 本单费用，单位 0.001 元
	Steps         int      `json:"steps"`          // 已跑步数
	NextStep      string   `json:"next_step"`      // 下一步建议（可空）
	AdvisorNote   string   `json:"advisor_note"`   // 监工 CONCERN（有则填，无则空）
	ShardNo       int      `json:"shard_no"`       // 本片号（第 15 项：0 表示不分片）
	ResumePointer string   `json:"resume_pointer"` // 续跑指针（第 15 项：下一片从这里接续；本片做完则为空）
	// FactList 是网关随回执附的免费事实清单（规划 v2）：产物在不在/
	// 多大/新不新、verify_cmd 过没过、实质调用几次。前置判回执的证据。
	FactList  *FactList `json:"fact_list,omitempty"`
	CreatedAt string    `json:"created_at"` // RFC3339 UTC
}

// ExecuteFunc 是执行派单的回调（由 main.go 接线：TOOL 走 runTool，
// AGENT/META 走 runAgent）。返回回执；出错时返回 error，Worker 会把
// error 包成 failed 回执落盘。
type ExecuteFunc func(ctx context.Context, d *Dispatch) (*Receipt, error)

// Channel 是派单通道：落盘 + 内存缓冲。
type Channel struct {
	dataDir string
	queue   chan *Dispatch
	mu      sync.Mutex
	seq     int
}

// New 建一个派单通道（缓冲 16）。dataDir 是数据目录（data/dispatch 落盘处）。
func New(dataDir string) *Channel {
	return &Channel{dataDir: dataDir, queue: make(chan *Dispatch, 16)}
}

// Queue 返回 worker 取派单的通道（只读）。
func (c *Channel) Queue() <-chan *Dispatch { return c.queue }

// dispatchFilePath 是某派单的落盘路径。
func (c *Channel) dispatchFilePath(id string) string {
	return filepath.Join(c.dataDir, "dispatch", id+".json")
}

// receiptFilePath 是某派单回执的落盘路径。
func (c *Channel) receiptFilePath(id string) string {
	return filepath.Join(c.dataDir, "dispatch", id+".receipt.json")
}

// NewID 生成一个派单编号：任务号 + 纳秒时间戳，全局唯一。
func (c *Channel) NewID(taskID string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	return fmt.Sprintf("%s-%d-%d", taskID, time.Now().UnixNano(), c.seq)
}

// Send 落盘并入队。ReplyCh 为 nil 表示没人等回执（只落盘）。
// 入队时若缓冲满则阻塞——调用方是前置会话，派单即返回前必须确保
// 派单已进队；缓冲 16 且 worker 常驻，正常不阻塞。
func (c *Channel) Send(ctx context.Context, d *Dispatch) error {
	if strings.TrimSpace(d.ID) == "" {
		return fmt.Errorf("dispatch: 派单缺 id")
	}
	if strings.TrimSpace(d.TaskID) == "" {
		return fmt.Errorf("dispatch: 派单缺 task_id")
	}
	dir := filepath.Join(c.dataDir, "dispatch")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("dispatch: 建 dispatch 目录失败: %w", err)
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("dispatch: 序列化派单失败: %w", err)
	}
	if err := os.WriteFile(c.dispatchFilePath(d.ID), b, 0o600); err != nil {
		return fmt.Errorf("dispatch: 写派单文件失败: %w", err)
	}
	select {
	case c.queue <- d:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("dispatch: 入队被取消: %w", ctx.Err())
	}
}

// WriteReceipt 落盘一份回执，并（ReplyCh 非空时）把回执交还给等它的前置。
// 回执文件与派单文件同目录，文件名 <id>.receipt.json。
func (c *Channel) WriteReceipt(d *Dispatch, r *Receipt) error {
	if r == nil {
		return fmt.Errorf("dispatch: 回执为空")
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("dispatch: 序列化回执失败: %w", err)
	}
	if err := os.WriteFile(c.receiptFilePath(d.ID), b, 0o600); err != nil {
		return fmt.Errorf("dispatch: 写回执文件失败: %w", err)
	}
	if d.ReplyCh != nil {
		select {
		case d.ReplyCh <- r:
		default: // 没人等或已取走，不阻塞
		}
	}
	return nil
}

// PendingDispatches 列出落盘了但还没有回执的派单（供 Worker 启动时重入队）。
func (c *Channel) PendingDispatches() ([]*Dispatch, error) {
	dir := filepath.Join(c.dataDir, "dispatch")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("dispatch: 读 dispatch 目录失败: %w", err)
	}
	var out []*Dispatch
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".receipt.json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		if _, err := os.Stat(c.receiptFilePath(id)); err == nil {
			continue // 已有回执，不重入队
		}
		b, err := os.ReadFile(c.dispatchFilePath(id))
		if err != nil {
			log.Printf("dispatch: 读待重入队派单 %s 失败: %v", id, err)
			continue
		}
		var d Dispatch
		if err := json.Unmarshal(b, &d); err != nil {
			log.Printf("dispatch: 解析待重入队派单 %s 失败: %v", id, err)
			continue
		}
		out = append(out, &d)
	}
	return out, nil
}

// Worker 是执行端的工作循环：从通道取派单，调 ExecuteFunc 执行，
// 把回执落盘并交还。启动时先把落盘未回执的派单重入队（不丢单）。
// ctx 取消时退出（正在执行的派单由 ExecuteFunc 自行处理取消）。
func Worker(ctx context.Context, ch *Channel, exec ExecuteFunc) {
	pending, err := ch.PendingDispatches()
	if err != nil {
		log.Printf("dispatch worker: 列待重入队派单失败: %v", err)
	} else if len(pending) > 0 {
		log.Printf("dispatch worker: 重启重入队 %d 份未回执派单", len(pending))
		for _, d := range pending {
			select {
			case ch.queue <- d:
			case <-ctx.Done():
				return
			}
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case d := <-ch.queue:
			r, err := exec(ctx, d)
			if err != nil {
				log.Printf("dispatch worker: 执行派单 %s 失败: %v", d.ID, err)
				r = &Receipt{
					DispatchID: d.ID, TaskID: d.TaskID, Status: "failed",
					Conclusion: "执行失败：" + err.Error(),
					CreatedAt:  time.Now().UTC().Format(time.RFC3339),
				}
			}
			if r != nil {
				if werr := ch.WriteReceipt(d, r); werr != nil {
					log.Printf("dispatch worker: 写回执 %s 失败: %v", d.ID, werr)
				}
			}
		}
	}
}
