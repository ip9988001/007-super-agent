// Package planner 是 007 超级 Agent 的多步骤规划器（第 21 步）。
//
// 流程：分诊判 PLAN 后，规划器（deepseek-flash）拆步、核验器
// （deepseek-reasoner，用户 2026-11-18 03:45 定）审步、执行循环
// 按审过的表跑。步骤数不设上限（15 步就跑 15 步）；笼子是进度保险
// （连续 3 步零进展即停）+ 50 步熔断（只防死循环）。
// 核验对照注册表真实状态（工具在不在不是模型说了算）；
// 宪法、分诊提示词、执行层代码一律不在本包改动。
package planner

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"007-super-agent/internal/executor"
	"007-super-agent/internal/ledger"
	"007-super-agent/internal/llm"
	"007-super-agent/internal/registry"
	"007-super-agent/internal/triage"
)

const (
	// MaxReviseRounds 是核验最多打回的轮数（与监工 HALT 封顶同口径）：
	// 初版 + 最多 2 轮修改；2 轮后仍不过，取最好一版执行或退回单步。
	MaxReviseRounds = 2
	// MaxExecutions 是熔断：一次任务总执行次数（含重试/重规划），
	// 只防死循环，不作设计上限。
	MaxExecutions = 50
	// NoProgressLimit 是进度保险：连续 3 步零进展（失败/跳过/被拦）即停。
	NoProgressLimit = 3
	// MaxSummaryRunes 是传给下一步的摘要上限（字符）。
	MaxSummaryRunes = 500
)

var (
	ErrInvalidPlan       = errors.New("planner: 步骤表非法")
	ErrInvalidVerify     = errors.New("planner: 核验回复非法")
	ErrPlanRejected      = errors.New("planner: 核验打回（单步就够或任务不成立）")
	ErrReviseExhausted   = errors.New("planner: 核验 2 轮后仍无可用修订版")
	ErrPlannerUnavaiable = errors.New("planner: 未接模型调用")
)

// ModelCallFunc 与 triage.ModelCallFunc 同型：frozen 放 system（A 区），
// tail 放 user（C 区），费用按 taskID 归属记账。生产上接 llm.Client
// 的 CallTask；测试时注入桩。
type ModelCallFunc func(taskID, frozen, tail string) (reply string, tokensIn, tokensOut int64, err error)

// Step 是步骤表里的一步。
type Step struct {
	StepNo          int      `json:"step_no"`
	ToolID          string   `json:"tool_id"`
	Inputs          []string `json:"inputs"`
	DependsOn       []int    `json:"depends_on"`
	ExpectedOutput  string   `json:"expected_output"`
	EstimatedTokens int64    `json:"estimated_tokens"`
}

// Plan 是一张审过的步骤表。
type Plan struct {
	PlanID   string `json:"plan_id"` // pl-YYYY-MM-DD-NNN，PlanAndVerify 里编号
	TaskID   string `json:"task_id"`
	TaskText string `json:"task_text"`
	Steps    []Step `json:"steps"`
	Rounds   int    `json:"rounds"` // 核验修改轮数
}

// Verdict 是核验结论三档。
type Verdict string

const (
	VerdictApprove Verdict = "APPROVE" // 直接执行
	VerdictRevise  Verdict = "REVISE"  // 按 findings 改
	VerdictReject  Verdict = "REJECT"  // 单步就够或任务不成立
)

// Finding 是核验的一条逐项结论。Question 只能是下面四个之一，
// 对应方案要求的四个必答问题。
type Finding struct {
	Question string `json:"question"` // necessity | rationality | reasons | suggestion
	Text     string `json:"text"`
}

// Verification 是一次核验的结论。
type Verification struct {
	Verdict     Verdict   `json:"verdict"`
	Findings    []Finding `json:"findings"`
	RevisedPlan []Step    `json:"revised_plan"`
}

// Planner 是规划器 + 核验器 + 执行循环的接线体。
// PlanCall 走 worker 模型（flash），VerifyCall 走 reasoner 模型，
// 两者共用同一把 DeepSeek key（llm 客户端各自建，密钥只读键名）。
type Planner struct {
	PlanCall   ModelCallFunc
	VerifyCall ModelCallFunc
	Reg        *registry.Registry
	Led        *ledger.Ledger
	Ex         *executor.Executor
	DataDir    string // 产物落盘根目录（artifacts/<taskID>/）
	SessionID  string
	Shadow     bool // 影子模式：只出规划+核验结论，RunPlan 拒绝执行
}

// New 建一个规划器。调用方负责把 PlanCall/VerifyCall 接到真实模型
// （生产：llm.New(cfg.WorkerModel, key).CallTask 与
// llm.New(cfg.AdvisorModel, key).CallTask）。
func New(planCall, verifyCall ModelCallFunc, reg *registry.Registry, led *ledger.Ledger, ex *executor.Executor, dataDir, sessionID string, shadow bool) *Planner {
	return &Planner{
		PlanCall: planCall, VerifyCall: verifyCall,
		Reg: reg, Led: led, Ex: ex,
		DataDir: dataDir, SessionID: sessionID, Shadow: shadow,
	}
}

// planPromptHead 是规划器的冻结提示词（放 A 区，内容固定才能命中缓存）。
// 上一步产物给下一步用的参数写占位符 {{stepN}}（产物文件路径）与
// {{stepN.summary}}（结果摘要 ≤500 字），执行循环负责代入。
const planPromptHead = `你是 007 超级 Agent 的多步骤规划器。把下面一个任务拆成按顺序执行的步骤表。
只输出一个 JSON 对象，不要任何解释、不要 markdown 代码围栏。对象格式：
{"steps":[{"step_no":1,"tool_id":"cap-code-rea","inputs":["https://example.com"],"depends_on":[],"expected_output":"一份逆向报告，列出该网站的全部功能与实现方式","estimated_tokens":2000}]}
规则：
- 一步只做一件事；tool_id 只能从下面可用工具清单里挑，清单外的不许编；
- inputs 是传给工具的参数（字符串数组），不要写空字符串；正则、URL 里的 & | ( ) 等字符可以直接写，执行时不经过 shell；
- depends_on 只写前面步骤的 step_no（依赖前面步骤的产物才能做的才写），不要循环依赖、不要依赖后面的步骤；
- expected_output 写这步要产出什么、后一步拿它做什么；
- estimated_tokens 估这步的 token 量（输入+输出，整数）；
- 步骤数不限，该拆几步就拆几步；一步能做完的不要硬拆；
- 上一步产物给下一步用的参数写占位符：{{stepN}} 表示第 N 步的产物文件路径，{{stepN.summary}} 表示第 N 步的结果摘要（≤500 字）。
`

// verifyPromptHead 是核验器的冻结提示词（放 A 区，内容固定才能命中缓存）。
// 角色是审查员不是同事；必须逐项回答四个问题，找不到问题也要写清楚
// 查过什么、为什么不能合并——不许只说“合理”或“通过”。
const verifyPromptHead = `你是 007 超级 Agent 的多步骤核验器。你是审查员，不是规划器的同事——你的工作是挑刺，不是附和。只输出一个 JSON 对象，不要任何解释、不要 markdown 代码围栏。对象格式：
{"verdict":"APPROVE|REVISE|REJECT","findings":[{"question":"necessity","text":"..."}],"revised_plan":[{"step_no":1,"tool_id":"...","inputs":[],"depends_on":[],"expected_output":"...","estimated_tokens":0}]}
你必须逐项回答下面四个问题，每个问题至少一条 finding，question 只能是 necessity / rationality / reasons / suggestion 四个之一：
① necessity：要不要这么多步骤？每一步是否必要？相邻两步能不能合并？不能合并的要写清楚为什么；
② rationality：合理吗？依赖对不对（后一步用的东西前一步真给得出吗）？有没有漏掉的步骤（比如部署了却没验证）？
③ reasons：不合理的原因是什么？逐条点名，哪一步、哪个工具、什么问题；
④ suggestion：建议如何规划步骤？给出修改后的步骤表（revised_plan），合并哪几步、补哪步、改哪个工具，都要写进表里。
规则：
- 找不到问题也要写，不许只说“合理”或“通过”：例如“已检查合并可能性：步骤 2 与 3 不能合并，因为步骤 2 的产物是步骤 3 的输入”。
- 每个工具必须对照下面注册表实核表核对（在册且可用才算数，模型说了不算）：不存在的工具、不可以调用的工具、参数缺失的步骤，一律打回。
- verdict 三档：APPROVE 直接执行（revised_plan 可空）；REVISE 按 findings 改（revised_plan 是完整步骤表，尽量给；给不出时调用方会带着 findings 重新规划）；REJECT 单步就够或任务本身不成立（revised_plan 可空）。
- 只输出 JSON 对象。
`

// synthPromptHead 是交活综合器的冻结提示词（放 A 区）。
const synthPromptHead = `你是 007 超级 Agent 的交活综合器。把下面多步骤任务的各步结果综合成给用户的最终回复。
只输出回复正文，不要解释、不要 markdown 代码围栏之外的废话。
结构：先说做完了什么（按步骤简述），再说结果与产物在哪里（给路径），最后说没做完或卡住的地方（没有就写“全部完成”）。
数字、路径、工具名必须与步骤摘要一致，不许编造；没做完的不要说做完了。
`

// truncateRunes 按字符截断到 n 个字（先转 []rune，防中文乱码）。
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// stripFences 去掉模型回复外围的 markdown 代码围栏（与精炼同口径）。
func stripFences(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

// parsePlanSteps 解析规划器的 JSON 回复：{"steps":[...]}。
func parsePlanSteps(reply string) ([]Step, error) {
	var raw struct {
		Steps []Step `json:"steps"`
	}
	if err := json.Unmarshal([]byte(stripFences(reply)), &raw); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidPlan, err)
	}
	if len(raw.Steps) == 0 {
		return nil, fmt.Errorf("%w: 步骤表为空", ErrInvalidPlan)
	}
	return raw.Steps, nil
}

// validateSteps 校验一张步骤表（规划器输出与核验修订版共用）：
// step_no 从 1 连续编号；tool_id 在册且可用；参数过白名单；
// depends_on 只许依赖前面的步骤；预估 token 不能为负。
func validateSteps(steps []Step, reg *registry.Registry) error {
	if len(steps) == 0 {
		return fmt.Errorf("%w: 步骤表为空", ErrInvalidPlan)
	}
	for i, s := range steps {
		if s.StepNo != i+1 {
			return fmt.Errorf("%w: 步骤 %d 的 step_no=%d，应为 %d",
				ErrInvalidPlan, i+1, s.StepNo, i+1)
		}
		if strings.TrimSpace(s.ToolID) == "" {
			return fmt.Errorf("%w: 步骤 %d 未指定 tool_id", ErrInvalidPlan, s.StepNo)
		}
		if reg == nil || !reg.Callable(s.ToolID) {
			return fmt.Errorf("%w: 步骤 %d 的工具 %q 不在册或不可用",
				ErrInvalidPlan, s.StepNo, s.ToolID)
		}
		if err := llm.ValidateArgs(s.Inputs); err != nil {
			return fmt.Errorf("%w: 步骤 %d 参数非法: %v", ErrInvalidPlan, s.StepNo, err)
		}
		for _, d := range s.DependsOn {
			if d < 1 || d >= s.StepNo {
				return fmt.Errorf("%w: 步骤 %d 的 depends_on=%d 非法（只许依赖前面的步骤）",
					ErrInvalidPlan, s.StepNo, d)
			}
		}
		if s.EstimatedTokens < 0 {
			return fmt.Errorf("%w: 步骤 %d 的 estimated_tokens 为负", ErrInvalidPlan, s.StepNo)
		}
	}
	return nil
}

// renumberSteps 把修订版/重规划的步骤表按 start 起重新编号（续接用）：
// step_no 平移，depends_on 跟着平移（新表内部编号；重规划反馈里
// 已写明 depends_on 只写本版新表内的步骤，从 1 开始）。
func renumberSteps(steps []Step, start int) []Step {
	out := make([]Step, len(steps))
	for i, s := range steps {
		s.StepNo = start + i
		for j, d := range s.DependsOn {
			s.DependsOn[j] = d + start - 1
		}
		out[i] = s
	}
	return out
}

// Plan 调一次 worker 模型（flash）拆步。feedback 非空时是上版计划的
// 核验意见（重规划用），原样拼进尾部，不参与冻结前缀。
// WorkDir 是本任务的落盘目录：DataDir/workspace/<taskID>，RunPlan
// 开跑前建好（2026-11-18 ping0 测试：规划器把文件写到三号的
// /data/workspace convention 下，测试机上该目录不存在，curl -D
// 撞上不存在的父目录直接段错误，步骤 -1 失败）。
func (p *Planner) WorkDir(taskID string) string {
	return filepath.Join(p.DataDir, "workspace", taskID)
}

// workDirListing 列出本任务工作目录的实际落盘文件（名 + 大小），供综合
// 与监工核对：2026-11-18 ping0 第三轮，综合稿与监工都写了“没有落盘文件”，
// 但原计划第 1 步的 4 个文件其实已落盘——两边的证据里都没有工作目录清单。
func (p *Planner) workDirListing(taskID string) string {
	dir := p.WorkDir(taskID)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "工作目录 " + dir + "：不存在或不可读（" + err.Error() + "）"
	}
	if len(ents) == 0 {
		return "工作目录 " + dir + "：空，无落盘文件"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "工作目录 %s 落盘文件（共 %d 个）：\n", dir, len(ents))
	for i, de := range ents {
		if i >= 20 {
			fmt.Fprintf(&b, "- …还有 %d 个（仅列前 20 个）\n", len(ents)-20)
			break
		}
		size := int64(-1)
		if info, err := de.Info(); err == nil {
			size = info.Size()
		}
		fmt.Fprintf(&b, "- %s（%dB）\n", de.Name(), size)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func (p *Planner) Plan(taskID, taskText, memSummary, feedback string) (*Plan, error) {
	if p.PlanCall == nil {
		return nil, ErrPlannerUnavaiable
	}
	if p.Reg == nil {
		return nil, fmt.Errorf("%w: 未接注册表", ErrInvalidPlan)
	}
	frozen := planPromptHead +
		"可用工具清单（只许从中挑 tool_id，清单外的一律非法）：\n" +
		triage.CompactToolList(p.Reg, nil)
	var tail strings.Builder
	tail.WriteString("任务：" + strings.TrimSpace(taskText) + "\n")
	tail.WriteString("工作目录：" + p.WorkDir(taskID) + "（已建好；所有落盘文件一律写这个目录下，不要写 /data/... 或别的绝对路径）\n")
	if strings.TrimSpace(memSummary) != "" {
		tail.WriteString(memSummary)
	}
	if strings.TrimSpace(feedback) != "" {
		tail.WriteString("上版计划的核验意见（必须按此修改，不要重复已完成的步骤）：\n" + feedback + "\n")
	}
	reply, _, _, err := p.PlanCall(taskID, frozen, tail.String())
	if err != nil {
		return nil, fmt.Errorf("planner plan: %w", err)
	}
	steps, err := parsePlanSteps(reply)
	if err != nil {
		return nil, err
	}
	if err := validateSteps(steps, p.Reg); err != nil {
		return nil, err
	}
	return &Plan{TaskID: taskID, TaskText: taskText, Steps: steps}, nil
}

// registryCheck 是核验的代码实核：逐项对照注册表真实状态，
// 工具不存在/不可用、依赖非法都逐条点名。模型说了不算，
// 这张表进核验输入，也合并进核验结论。
func (p *Planner) registryCheck(plan *Plan) (findings []Finding, table string) {
	var tb strings.Builder
	tb.WriteString("注册表实核（在册且可用才算数）：\n")
	for _, s := range plan.Steps {
		status := "在册可用"
		if p.Reg == nil {
			status = "未接注册表，无法核对"
		} else if _, ok := p.Reg.Entry(s.ToolID); !ok {
			status = "不存在"
			findings = append(findings, Finding{
				Question: "rationality",
				Text:     fmt.Sprintf("步骤 %d 的工具 %q 在注册表里不存在，必须换成在册工具或去掉这步", s.StepNo, s.ToolID),
			})
		} else if !p.Reg.Callable(s.ToolID) {
			status = "在册但不可用"
			findings = append(findings, Finding{
				Question: "rationality",
				Text:     fmt.Sprintf("步骤 %d 的工具 %q 在册但不可用（available:false），必须换成可用工具或去掉这步", s.StepNo, s.ToolID),
			})
		}
		for _, d := range s.DependsOn {
			if d < 1 || d >= s.StepNo {
				findings = append(findings, Finding{
					Question: "rationality",
					Text:     fmt.Sprintf("步骤 %d 的 depends_on=%d 非法（只许依赖前面的步骤）", s.StepNo, d),
				})
			}
		}
		fmt.Fprintf(&tb, "- 步骤 %d：%s（%s）\n", s.StepNo, s.ToolID, status)
	}
	return findings, tb.String()
}

// parseVerification 解析核验器的 JSON 回复并严格校验：
// verdict 三档；findings 非空、question 四键齐全、每条有内容；
// REVISE 的 revised_plan 在 Verify 里另过 validateSteps（这里只解析）。
func parseVerification(reply string) (*Verification, error) {
	var ver Verification
	if err := json.Unmarshal([]byte(stripFences(reply)), &ver); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidVerify, err)
	}
	switch ver.Verdict {
	case VerdictApprove, VerdictRevise, VerdictReject:
	default:
		return nil, fmt.Errorf("%w: 非法 verdict %q", ErrInvalidVerify, ver.Verdict)
	}
	if len(ver.Findings) == 0 {
		return nil, fmt.Errorf("%w: findings 为空（四个问题必须逐项回答）", ErrInvalidVerify)
	}
	need := map[string]bool{"necessity": false, "rationality": false, "reasons": false, "suggestion": false}
	for _, f := range ver.Findings {
		if _, ok := need[f.Question]; !ok {
			return nil, fmt.Errorf("%w: 非法 question %q", ErrInvalidVerify, f.Question)
		}
		if strings.TrimSpace(f.Text) == "" {
			return nil, fmt.Errorf("%w: finding 内容为空", ErrInvalidVerify)
		}
		need[f.Question] = true
	}
	for q, ok := range need {
		if !ok {
			return nil, fmt.Errorf("%w: 缺问题 %q 的结论", ErrInvalidVerify, q)
		}
	}
	return &ver, nil
}

// findingsText 把 findings 拼成给重规划的反馈文本。
func findingsText(findings []Finding) string {
	var b strings.Builder
	for _, f := range findings {
		fmt.Fprintf(&b, "- [%s] %s\n", f.Question, f.Text)
	}
	return b.String()
}

// Verify 调一次 reasoner 模型核验一张计划：先代码实核（注册表真实状态），
// 再调模型，最后合并代码发现——模型说 APPROVE 但实核有问题的，
// 降级为 REVISE；REVISE 的修订版再过代码校验，不过就置空（调用方重规划）。
func (p *Planner) Verify(taskID string, plan *Plan) (*Verification, error) {
	if p.VerifyCall == nil {
		return nil, ErrPlannerUnavaiable
	}
	codeFindings, table := p.registryCheck(plan)
	planJSON, err := json.Marshal(plan.Steps)
	if err != nil {
		return nil, fmt.Errorf("planner verify: 步骤表序列化失败: %w", err)
	}
	var tail strings.Builder
	tail.WriteString("任务：" + strings.TrimSpace(plan.TaskText) + "\n")
	tail.WriteString("步骤表：\n" + string(planJSON) + "\n")
	tail.WriteString(table)
	reply, _, _, err := p.VerifyCall(taskID, verifyPromptHead, tail.String())
	if err != nil {
		return nil, fmt.Errorf("planner verify: %w", err)
	}
	ver, err := parseVerification(reply)
	if err != nil {
		return nil, err
	}
	if len(codeFindings) > 0 {
		ver.Findings = append(ver.Findings, codeFindings...)
		if ver.Verdict == VerdictApprove {
			ver.Verdict = VerdictRevise
		}
	}
	if ver.Verdict == VerdictRevise && len(ver.RevisedPlan) > 0 {
		if err := validateSteps(ver.RevisedPlan, p.Reg); err != nil {
			ver.RevisedPlan = nil
			ver.Findings = append(ver.Findings, Finding{
				Question: "reasons",
				Text:     "核验给出的修订版仍有问题（" + err.Error() + "），需重新规划",
			})
		}
	}
	return ver, nil
}

// beijingDate 取北京时间 YYYY-MM-DD（plan_id 编号用；与第 20 步同口径）。
func beijingDate() string {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		loc = time.FixedZone("CST", 8*3600)
	}
	return time.Now().In(loc).Format("2006-01-02")
}

// nextPlanID 编下一个 plan_id：pl-<北京日期>-NNN（账本计数 +1）。
func (p *Planner) nextPlanID() string {
	date := beijingDate()
	n := 0
	if p.Led != nil {
		if c, err := p.Led.CountPlanRunsByDate(date); err == nil {
			n = c
		}
	}
	return fmt.Sprintf("pl-%s-%03d", date, n+1)
}

// recordPlanRun 把一次 PlanAndVerify 的结论写进 plan_runs。
// 写失败只记日志，不让一次好计划作废（与分诊记账同口径）。
func (p *Planner) recordPlanRun(plan *Plan, ver *Verification) {
	if p.Led == nil {
		return
	}
	planJSON, _ := json.Marshal(plan.Steps)
	var total int64
	for _, s := range plan.Steps {
		total += s.EstimatedTokens
	}
	// 预估费用按 Flash 缓存未命中价算（上限口径：全按未命中输入计）。
	estCost := llm.CostMilliForModel("deepseek-flash", 0, total, 0, false)
	_, err := p.Led.RecordPlanRun(ledger.PlanRun{
		PlanID: plan.PlanID, TaskID: plan.TaskID, TaskText: plan.TaskText,
		Steps: len(plan.Steps), PlanJSON: string(planJSON),
		Verdict: string(ver.Verdict), ReviseRounds: plan.Rounds,
		EstimatedCostMilli: estCost,
	})
	if err != nil {
		log.Printf("planner record plan run %s: %v", plan.PlanID, err)
	}
}

// PlanAndVerify 是规划 + 核验的主入口：拆步 → 核验 → REVISE 时最多改 2 轮。
// APPROVE 直接返回计划；REJECT 返回 ErrPlanRejected（调用方退回单步）；
// 2 轮后仍 REVISE：有可用修订版就取最好一版执行，否则返回 ErrReviseExhausted。
func (p *Planner) PlanAndVerify(taskID, taskText, memSummary string) (*Plan, *Verification, error) {
	plan, err := p.Plan(taskID, taskText, memSummary, "")
	if err != nil {
		return nil, nil, err
	}
	plan.PlanID = p.nextPlanID()
	rounds := 0
	var bestRevised []Step
	for {
		ver, err := p.Verify(taskID, plan)
		if err != nil {
			return nil, nil, fmt.Errorf("planner verify: %w", err)
		}
		switch ver.Verdict {
		case VerdictApprove:
			plan.Rounds = rounds
			p.recordPlanRun(plan, ver)
			return plan, ver, nil
		case VerdictReject:
			plan.Rounds = rounds
			p.recordPlanRun(plan, ver)
			return nil, ver, ErrPlanRejected
		case VerdictRevise:
			if len(ver.RevisedPlan) > 0 {
				bestRevised = ver.RevisedPlan
			}
			if rounds >= MaxReviseRounds {
				if bestRevised != nil {
					plan.Steps = renumberSteps(bestRevised, 1)
					plan.Rounds = rounds
					p.recordPlanRun(plan, ver)
					return plan, ver, nil
				}
				plan.Rounds = rounds
				p.recordPlanRun(plan, ver)
				return nil, ver, ErrReviseExhausted
			}
			rounds++
			if len(ver.RevisedPlan) > 0 {
				plan.Steps = renumberSteps(ver.RevisedPlan, 1)
			} else {
				// 核验没给修订版：带着 findings 重新规划。
				np, err := p.Plan(taskID, taskText, memSummary, findingsText(ver.Findings))
				if err != nil {
					return nil, nil, err
				}
				np.PlanID = plan.PlanID
				plan = np
			}
		}
	}
}

// RunResult 是一次执行循环的结果。
type RunResult struct {
	Status         string // completed / stopped_no_progress / stopped_fuse / stopped_halt / stopped_replan_failed
	Summary        string // 各步一句话汇总
	ReplyDraft     string // 综合回复（Synthesize 产物；失败时为 Summary）
	CompletedSteps int
	SkippedSteps   []int
	StoppedAtStep  int
}

// outcome 是单步尝试的结果。
const (
	outcomeSuccess   = "success"
	outcomeFailed    = "failed"
	outcomeBlocked   = "blocked"    // 监工 HALT，不重试
	outcomeHaltFinal = "halt-final" // 监工第 3 次 HALT，终局
)

// substituteInputs 把步骤参数里的占位符换成真实值：
// {{stepN}} → 第 N 步的产物文件路径（该步必须已完成且产物在位）；
// {{stepN.summary}} → 第 N 步的结果摘要。
// 引用不存在/失败步骤的产物时返回错误，这步不跑。
var placeholderRe = regexp.MustCompile(`\{\{step(\d+)(\.summary)?\}\}`)

func substituteInputs(inputs []string, artifacts map[int]string, summaries map[int]string) ([]string, error) {
	out := make([]string, len(inputs))
	for i, in := range inputs {
		var subErr error
		s := placeholderRe.ReplaceAllStringFunc(in, func(m string) string {
			if subErr != nil {
				return m
			}
			parts := placeholderRe.FindStringSubmatch(m)
			var n int
			fmt.Sscanf(parts[1], "%d", &n)
			if parts[2] == ".summary" {
				txt, ok := summaries[n]
				if !ok {
					subErr = fmt.Errorf("引用了不存在的步骤 %d 的摘要", n)
					return m
				}
				return txt
			}
			path, ok := artifacts[n]
			if !ok {
				subErr = fmt.Errorf("引用了不存在或未完成的步骤 %d 的产物", n)
				return m
			}
			return path
		})
		if subErr != nil {
			return nil, subErr
		}
		out[i] = s
	}
	return out, nil
}

// dependsOnLater 判某步是否被后面的步骤依赖。
func dependsOnLater(steps []Step, fromIdx int, stepNo int) bool {
	for _, s := range steps[fromIdx+1:] {
		for _, d := range s.DependsOn {
			if d == stepNo {
				return true
			}
		}
	}
	return false
}

// attemptStep 跑一步（单次尝试）：解析工具、拼参数、执行、成功则落产物并记 Stage 行。
// 返回 (result, outcome)：outcome 为 success / failed / blocked / halt-final；
// result 为 nil 表示没跑起来（解析/拼参数失败）。记账失败返回 error。
func (p *Planner) attemptStep(plan *Plan, st Step, inputs []string, taskText, artDir string, executions *int) (*executor.Result, string, error) {
	_, _, err := triage.ResolveTool(p.Reg, st.ToolID)
	if err != nil {
		// 解析/探针失败是确定性失败：记一行（探针失败记账与主程序同口径），不重试。
		if errors.Is(err, triage.ErrProbeFailed) {
			if _, rerr := p.Led.RecordExecution(ledger.ExecutionRecord{
				SessionID: p.SessionID, TaskID: plan.TaskID, ToolID: st.ToolID,
				Command: "probe " + st.ToolID, ExitCode: 1, Status: "failure",
				ErrHead: truncateRunes(err.Error(), 600),
			}); rerr != nil {
				return nil, outcomeFailed, rerr
			}
		}
		log.Printf("planner step %d resolve %s: %v", st.StepNo, st.ToolID, err)
		return nil, outcomeFailed, nil
	}
	argv, err := llm.BuildArgv(p.Reg, st.ToolID, inputs)
	if err != nil {
		log.Printf("planner step %d argv %s: %v", st.StepNo, st.ToolID, err)
		return nil, outcomeFailed, nil
	}
	*executions++
	res, h, err := p.Ex.Run(executor.RunRequest{
		TaskID: plan.TaskID, SessionID: p.SessionID, ToolID: st.ToolID,
		Argv: argv, Timeout: executor.MaxTimeout, Text: taskText,
	})
	if err != nil {
		if errors.Is(err, executor.ErrAdvisorHaltFinal) {
			return nil, outcomeHaltFinal, nil
		}
		if errors.Is(err, executor.ErrAdvisorHalt) {
			return nil, outcomeBlocked, nil
		}
		// 其他执行层错误（如记账失败）也算这步失败，不重试同一种错误。
		log.Printf("planner step %d run %s: %v", st.StepNo, st.ToolID, err)
		return nil, outcomeFailed, nil
	}
	if h != nil {
		// 长步骤：等后台跑完（计划内步骤必须等结果，下一步依赖它）。
		r, werr := h.Wait()
		if werr != nil {
			return nil, outcomeFailed, nil
		}
		res = r
	}
	if res == nil || res.Status != "success" {
		return res, outcomeFailed, nil
	}
	// 成功：全文落产物 + Stage 行（第 14 步 B-06 的 Stage 边界落盘）。
	artPath := filepath.Join(artDir, fmt.Sprintf("step-%d.txt", st.StepNo))
	content := res.StdoutFull
	if strings.TrimSpace(res.StderrFull) != "" {
		content += "\n--- stderr ---\n" + res.StderrFull
	}
	if err := os.WriteFile(artPath, []byte(content), 0600); err != nil {
		return res, outcomeFailed, fmt.Errorf("planner step %d 写产物失败: %w", st.StepNo, err)
	}
	if _, err := p.Led.RecordStage(ledger.StageRecord{
		SessionID: p.SessionID, TaskID: plan.TaskID, ToolID: st.ToolID,
		StageNo: st.StepNo, ArtifactPath: artPath,
		Command: fmt.Sprintf("plan %s step %d: %s", plan.PlanID, st.StepNo, st.ToolID),
	}); err != nil {
		return res, outcomeFailed, fmt.Errorf("planner step %d 落 Stage 行失败: %w", st.StepNo, err)
	}
	return res, outcomeSuccess, nil
}

// replan 是失败阶梯第三档：回规划器重新规划剩余工作（只审一轮，
// 有可用修订版就用；没有就用重规划版——它已过 Plan 校验）。
// 重规划的结论也记一行 plan_runs（plan_id 后缀 -r1，与原计划区分）。
func (p *Planner) replan(plan *Plan, taskText, memSummary string, summaries []string, reason string) ([]Step, error) {
	feedback := "上版计划执行中出问题，原因：" + reason +
		"\n已完成步骤摘要（不要重复已完成的步骤；要用已完成步骤的产物就写 {{stepN}} 占位符）：\n" +
		strings.Join(summaries, "\n") +
		"\ndepends_on 只写本版新表内的步骤编号（从 1 开始）。"
	np, err := p.Plan(plan.TaskID, taskText, memSummary, feedback)
	if err != nil {
		return nil, err
	}
	ver, err := p.Verify(plan.TaskID, np)
	if err != nil {
		return nil, err
	}
	if ver.Verdict == VerdictReject {
		return nil, ErrPlanRejected
	}
	steps := np.Steps
	if ver.Verdict == VerdictRevise && len(ver.RevisedPlan) > 0 {
		steps = ver.RevisedPlan
	}
	// 重规划结论记账（plan_id 后缀 -r1；原计划行不动）。
	if p.Led != nil {
		planJSON, _ := json.Marshal(steps)
		var total int64
		for _, s := range steps {
			total += s.EstimatedTokens
		}
		_, _ = p.Led.RecordPlanRun(ledger.PlanRun{
			PlanID: plan.PlanID + "-r1", TaskID: plan.TaskID, TaskText: plan.TaskText,
			Steps: len(steps), PlanJSON: string(planJSON),
			Verdict: string(ver.Verdict), ReviseRounds: 0,
			EstimatedCostMilli: llm.CostMilliForModel("deepseek-flash", 0, total, 0, false),
		})
	}
	return steps, nil
}

// stepSummary 取一步结果的摘要（≤500 字，供下一步与综合用）。
func stepSummary(st Step, res *executor.Result) string {
	if res == nil {
		return fmt.Sprintf("步骤%d（%s）：无结果", st.StepNo, st.ToolID)
	}
	out := strings.TrimSpace(res.StdoutTail)
	if out == "" {
		out = "（无输出）"
	}
	return fmt.Sprintf("步骤%d（%s）：%s", st.StepNo, st.ToolID, truncateRunes(out, MaxSummaryRunes))
}

// failAction 是失败阶梯的处置结果。
type failAction struct {
	stop     string // 非空时停下（stopped_* 状态）
	newSteps []Step // 非空时已重规划，调用方替换剩余步骤后继续
}

// handleFailure 实现失败阶梯的后三档（第一档“重试一次”在调用方）。
// 进度保险优先：连续零进展到上限就停，不再重规划。
// 否则：后面步骤依赖这步、或是最后一步 → 重规划一次（只许一次）；
// 不依赖则跳过。记账失败返回 error。
func (p *Planner) handleFailure(plan *Plan, steps []Step, i int, st Step, reason string, summaries []string, taskText, memSummary string, replanned *bool, noProgress int) (failAction, error) {
	if noProgress >= NoProgressLimit {
		return failAction{stop: "stopped_no_progress"}, nil
	}
	if dependsOnLater(steps, i, st.StepNo) || i == len(steps)-1 {
		if !*replanned {
			*replanned = true
			newSteps, rerr := p.replan(plan, taskText, memSummary, summaries, reason)
			if rerr != nil {
				log.Printf("planner replan task=%s step=%d failed: %v", plan.TaskID, st.StepNo, rerr)
				return failAction{stop: "stopped_replan_failed"}, nil
			}
			return failAction{newSteps: renumberSteps(newSteps, st.StepNo+1)}, nil
		}
		return failAction{stop: "stopped_replan_failed"}, nil
	}
	// 不依赖：跳过，记一行 skipped（失败行执行层已记）。
	if _, rerr := p.Led.RecordExecution(ledger.ExecutionRecord{
		SessionID: p.SessionID, TaskID: plan.TaskID, ToolID: st.ToolID,
		Command:  fmt.Sprintf("skip step %d: %s (%s)", st.StepNo, st.ToolID, reason),
		ExitCode: 0, Status: "skipped",
	}); rerr != nil {
		return failAction{}, rerr
	}
	return failAction{}, nil
}

// RunPlan 按审过的表跑执行循环。影子模式拒绝执行（调用方也不应在影子模式调它）。
// 同步跑完整个计划才返回：计划内步骤必须等结果，下一步依赖它。
func (p *Planner) RunPlan(plan *Plan, taskText, memSummary string) (*RunResult, error) {
	if p.Shadow {
		return nil, errors.New("planner: 影子模式不执行")
	}
	if p.Ex == nil || p.Reg == nil || p.Led == nil {
		return nil, errors.New("planner: 未接线执行层/总账/账本")
	}
	// 入口校验：PlanAndVerify 出来的表一定合法；手工传入的非法表在这里拦下。
	if err := validateSteps(plan.Steps, p.Reg); err != nil {
		return nil, err
	}
	artDir := filepath.Join(p.DataDir, "artifacts", plan.TaskID)
	if err := os.MkdirAll(artDir, 0700); err != nil {
		return nil, fmt.Errorf("planner mkdir artifacts: %w", err)
	}
	if err := os.MkdirAll(p.WorkDir(plan.TaskID), 0700); err != nil {
		return nil, fmt.Errorf("planner mkdir workspace: %w", err)
	}
	summaries := []string{}
	artifacts := map[int]string{}
	summariesByStep := map[int]string{}
	skipped := []int{}
	completed := 0
	noProgress := 0
	executions := 0
	replanned := false
	status := "completed"
	stoppedAt := 0

	steps := append([]Step(nil), plan.Steps...)

loop:
	for i := 0; i < len(steps); i++ {
		st := steps[i]
		if executions >= MaxExecutions {
			status = "stopped_fuse"
			stoppedAt = st.StepNo
			break loop
		}
		// 1. 代入上一步产物（占位符）；引用失败步骤的产物时这步没法跑。
		inputs, err := substituteInputs(st.Inputs, artifacts, summariesByStep)
		if err != nil {
			reason := fmt.Sprintf("步骤%d（%s）未跑：%v", st.StepNo, st.ToolID, err)
			log.Printf("planner step %d substitute: %v", st.StepNo, err)
			noProgress++
			summaries = append(summaries, reason)
			summariesByStep[st.StepNo] = reason
			act, ferr := p.handleFailure(plan, steps, i, st, reason, summaries, taskText, memSummary, &replanned, noProgress)
			if ferr != nil {
				return nil, ferr
			}
			if act.stop != "" {
				status = act.stop
				stoppedAt = st.StepNo
				break loop
			}
			if len(act.newSteps) > 0 {
				steps = append(steps[:i+1], act.newSteps...)
				summaries = append(summaries, reason+"；已重规划，继续执行新步骤")
				continue
			}
			skipped = append(skipped, st.StepNo)
			continue
		}
		// 2. 跑一步；失败重试一次（监工 HALT 不重试）。
		res, outcome, err := p.attemptStep(plan, st, inputs, taskText, artDir, &executions)
		if err != nil {
			return nil, err
		}
		if outcome == outcomeFailed && executions < MaxExecutions {
			res, outcome, err = p.attemptStep(plan, st, inputs, taskText, artDir, &executions)
			if err != nil {
				return nil, err
			}
		}
		switch outcome {
		case outcomeSuccess:
			completed++
			noProgress = 0
			s := stepSummary(st, res)
			summaries = append(summaries, s)
			summariesByStep[st.StepNo] = s
			artifacts[st.StepNo] = filepath.Join(artDir, fmt.Sprintf("step-%d.txt", st.StepNo))
		case outcomeHaltFinal:
			status = "stopped_halt"
			stoppedAt = st.StepNo
			summaries = append(summaries, fmt.Sprintf("步骤%d（%s）监工第 3 次 HALT，终局停下", st.StepNo, st.ToolID))
			break loop
		default: // failed / blocked：走失败阶梯
			noProgress++
			reason := fmt.Sprintf("步骤%d（%s）失败", st.StepNo, st.ToolID)
			if outcome == outcomeBlocked {
				reason = fmt.Sprintf("步骤%d（%s）被监工 HALT 拦下", st.StepNo, st.ToolID)
			}
			if res != nil && strings.TrimSpace(res.ErrHead) != "" {
				reason += "：" + truncateRunes(res.ErrHead, 200)
			}
			summaries = append(summaries, reason)
			summariesByStep[st.StepNo] = reason
			act, ferr := p.handleFailure(plan, steps, i, st, reason, summaries, taskText, memSummary, &replanned, noProgress)
			if ferr != nil {
				return nil, ferr
			}
			if act.stop != "" {
				status = act.stop
				stoppedAt = st.StepNo
				break loop
			}
			if len(act.newSteps) > 0 {
				steps = append(steps[:i+1], act.newSteps...)
				summaries = append(summaries, reason+"；已重规划，继续执行新步骤")
				continue
			}
			skipped = append(skipped, st.StepNo)
		}
	}

	// 综合回复（模型失败时用步骤汇总原文兜底，不伪造）。
	draft, err := p.Synthesize(plan.TaskID, taskText, summaries)
	if err != nil {
		log.Printf("planner synthesize %s: %v", plan.PlanID, err)
		draft = strings.Join(summaries, "\n")
	}
	replyPath := filepath.Join(artDir, "reply.md")
	_ = os.WriteFile(replyPath, []byte(draft), 0600)
	return &RunResult{
		Status: status, Summary: strings.Join(summaries, "\n"), ReplyDraft: draft,
		CompletedSteps: completed, SkippedSteps: skipped, StoppedAtStep: stoppedAt,
	}, nil
}

// Synthesize 调一次 worker 模型（flash）把各步结果综合成给用户的最终回复。
func (p *Planner) Synthesize(taskID, taskText string, summaries []string) (string, error) {
	if p.PlanCall == nil {
		return "", ErrPlannerUnavaiable
	}
	var tail strings.Builder
	tail.WriteString("任务：" + strings.TrimSpace(taskText) + "\n各步结果：\n")
	for i, s := range summaries {
		fmt.Fprintf(&tail, "%d. %s\n", i+1, s)
	}
	tail.WriteString("工作目录实际落盘文件（以此为准：不要声称不存在的文件已落盘，也不要否认已落盘的文件）：\n" + p.workDirListing(taskID) + "\n")
	reply, _, _, err := p.PlanCall(taskID, synthPromptHead, tail.String())
	if err != nil {
		return "", fmt.Errorf("planner synthesize: %w", err)
	}
	reply = strings.TrimSpace(reply)
	reply = strings.TrimPrefix(reply, "```")
	reply = strings.TrimSuffix(reply, "```")
	return strings.TrimSpace(reply), nil
}
