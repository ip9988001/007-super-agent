// Package front 是 007 的前置会话（第 1 项）：判断、分诊、派单、回话。
//
// 前置只干四件事，不亲自做执行活（F-01）。它是一个常驻模型循环，
// 冻结提示词 = 前置宪法（front-AGENTS.md）+ 本文件里的动作格式；
// 每轮模型输出一个 JSON 动作（或打包最多 3 个互不依赖的动作），
// 前置执行它并把结果喂回下一轮。
//
// 六个内置工具（F-07/F-11，宪法里的工具收口）：
// - recall：查记忆 1 次（每单只查 1 次，F-05），默认前 3 条碎片；
// - registry：查工具单 L1→L2，定工具编号（不读 L3 全文，F-07）；
// - read：读文件走 ctxread 压缩读（R0 偷看 ≤2KB，R1 直读 ≤8KB；超 8KB 给结构/签名，24KB 封顶）；
// - bash：只读 bash（F-11），命令白名单 + 危险字符/参数双重拦截，
// 每次调用记审计日志（只记命令与退出码，不记输出，防密钥进日志 F-22）；
// - dispatch：写 ≤3000 token 派单进内部通道，派完即返回（F-08/F-10）；
// - reply：直接回话（纯讨论 DIRECT），写 reply.md 交活。
//
// 刹车（F-12）：自查（read/bash）最多 3 次，总计 7 轮内收口；超了就
// 按手头的东西派单或问用户。派单后前置等回执（同步流，见 dispatch 包），
// 回执状态即本单状态；执行已把 reply.md 写好，前置不再覆盖。
package front

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"007-super-agent/internal/agent"
	"007-super-agent/internal/ctxread"
	"007-super-agent/internal/dispatch"
	"007-super-agent/internal/ledger"
	"007-super-agent/internal/llm"
	"007-super-agent/internal/memory"
	"007-super-agent/internal/registry"
	"007-super-agent/internal/sanitize"
	"007-super-agent/internal/session"
	"007-super-agent/internal/triage"
)

// ModelCallFunc 是前置的模型调用（与 agent/triage 同一签名：
// taskID、冻结提示词、尾巴 → 回复、输入 token、输出 token、错误）。
// 用 agent.ModelCallFunc 的别名，三处可直接互换。
type ModelCallFunc = agent.ModelCallFunc

// ModelCallToolsFunc 是前置的原生模型调用（与 llm.Client.CallTaskFull
// 同签名，P1 之后执行会话同款）：frozen 放 system（A 区），history 是
// 结构化对话历史，tail 放最后一条 user 消息；返回整条 assistant 消息
// （正文 + tool_calls）与 finish_reason。2 号前置原生用它。
type ModelCallToolsFunc = agent.ModelCallToolsFunc

// MaxFrontRounds 是前置每单最多轮数（F-12：总计 7 轮内收口；
// 规划 v2 从 5 放到 7：模型要查记忆 + 查工具单（L1→L2）再写
// plan_task，5 轮不够，查工具单就把轮数耗光了）。
const MaxFrontRounds = 7

// MaxSelfChecks 是前置每单最多自查次数（F-12：read/bash 合计 ≤3 次）。
const MaxSelfChecks = 3

// maxConsecutiveValidationErrors 是原生路径里连续派单校验失败的上限
// （Gemini 审阅中优 M2，2026-11-26）：模型连续 2 次派单校验失败还可以
// 再试一次，第 3 次直接停下写失败回信，不再重问模型烧前置预算。
// 注意：计划循环不走这条路（派单失败走统一失败状态机：重干≤2→改计划≤2→
// 停下），这里只管模型自己重试的原生路径，不重复加帽。
const maxConsecutiveValidationErrors = 2

// MaxBatchActions 是前置一轮最多打包动作数（与执行端一致：3 个）。
const MaxBatchActions = 3

// ReceiptWaitTimeout 是前置等执行回执的最长等待（900 秒外墙 + 60 秒余量）。
const ReceiptWaitTimeout = 960 * time.Second

// Front 是前置会话。Call 接真实模型（只用 flash）；Reg 是总账；
// Mem 是记忆（查记忆用）；Ch 是派单通道；DataDir 是数据目录
// （reply.md 落盘处）；SessionID 是本进程会话号；Led 是账本
// （派单预算事件用，为空时只记日志、不写账本）。
type Front struct {
	Call      ModelCallFunc
	Reg       *registry.Registry
	Mem       *memory.Store
	Ch        *dispatch.Channel
	DataDir   string
	SessionID string
	Prompt    string // 前置宪法文本（New 时从文件加载；为空用内置兜底）
	Led       *ledger.Ledger
	// sess 是会话存盘入口（P2-a）：派单时给本步在会话计划里占位
	// （步号 + 工具 + 验收），执行层按 (session, step) 存历史、断点续跑。
	// New 时从 DataDir + Led 建；为空时不做会话存盘（测试可不接线）。
	sess *session.Store

	// CallWithTools 是 2 号原生工具调用（可空）：非空时 Run 走原生路径
	// （工具声明给模型、模型回 tool_calls、结果结构化回填）；为空时走
	// 旧的文本 JSON 路径。生产 main.go 一定接线，原生是唯一生产路径。
	CallWithTools ModelCallToolsFunc

	// CallWithToolsJudge 是规划 v2 判回执的原生调用（可空）：与
	// CallWithTools 相同签名，但 main.go 按 front_judge 角色记账。
	// 为空时判回执退回 CallWithTools（测试与旧调用方）。
	CallWithToolsJudge ModelCallToolsFunc

	// CallWithToolsSummary 是最终汇报的原生调用（可空，H-3）：
	// 与 CallWithTools 相同签名，但 main.go 按 front_summary 角色
	// 记账（终报不再混进 front_judge）。为空时终报退回
	// CallWithToolsJudge（测试与旧调用方）。
	CallWithToolsSummary ModelCallToolsFunc

	// CallRollover 是 P2-b 翻新摘要的模型调用（可空，M-5）：main.go
	// 接线为带 ctx 与 rollover 角色的入口（透传 420 秒超时）。
	// 为空时 summarizer() 退回 f.Call（测试与旧调用方）。
	CallRollover func(ctx context.Context, taskID, prompt string) (string, error)

	// RenewDeadline 是规划 v2 的看门狗续期回调（可空）：main.go 接线，
	// 按 task_id 把 inbox RUNNING 行的 deadline 与 updated_ts 推后
	// 1800 秒。为空时不续期（测试可不接线）。
	RenewDeadline func(taskID string)
}

// FrontResult 是前置一单的结果。
type FrontResult struct {
	ReplyDraft string   // 直接回话的文本（DIRECT 时）；派单时为空（执行已写 reply.md）
	Status     string   // done / dispatched / stalled / failed
	Route      string   // DIRECT / TOOL / META / AGENT；失败回信时为 ERROR
	DispatchID string   // 派单编号（派单时）
	ToolIDs    []string // 派单带的工具编号（供 L1 日志）
	Receipt    *dispatch.Receipt
}

// New 建一个前置会话。Prompt 从 data/prompts/front-AGENTS.md 加载；
// 文件缺失时留空，Run 用内置 frontPromptHead 兜底。
// led 可为空（测试用）：为空时派单预算事件只记日志、不写账本。
func New(call ModelCallFunc, reg *registry.Registry, mem *memory.Store, ch *dispatch.Channel, dataDir, sessionID string, led *ledger.Ledger) *Front {
	return &Front{
		Call: call, Reg: reg, Mem: mem, Ch: ch,
		DataDir: dataDir, SessionID: sessionID,
		Prompt: loadFrontPrompt(dataDir), Led: led,
		sess: session.New(dataDir, led),
	}
}

// loadFrontPrompt 读前置宪法文件（定稿：讨论六 + 讨论七修订）。
// 文件缺失返回空串，调用方用内置 frontPromptHead 兜底并记日志。
func loadFrontPrompt(dataDir string) string {
	p := filepath.Join(dataDir, "prompts", "front-AGENTS.md")
	b, err := os.ReadFile(p)
	if err != nil {
		log.Printf("front: 读前置宪法 %s 失败，用内置提示词兜底: %v", p, err)
		return ""
	}
	return string(b)
}

// frontPromptHead 是前置宪法的内置兜底（文件缺失时用；只有一句话，
// 提醒模型按前置规矩办事，真正的 25 条在文件里）。
const frontPromptHead = `你是 007 的前置会话，只干四件事：判断、分诊、派单、回话。不亲自做执行活。派单即返回，不轮询、不自己接手。`

// frontActionFormat 是前置会话的机器输出格式（不进宪法文件，宪法只写
// 规矩，格式由代码定）：JSON 动作格式 + 六个内置工具的用法。
const frontActionFormat = `只输出一个 JSON 对象，不要任何解释、不要 markdown 代码围栏。对象格式：
{"action":"reply","text":"……（直接回给用户的话）"}
{"action":"recall","query":"……（要查的记忆关键词/问题）"}
{"action":"registry","level":"L1"}
{"action":"registry","level":"L2","category":"fetch"}
{"action":"read","path":"/绝对路径/文件"}
{"action":"bash","command":"ls /某目录"}
{"action":"dispatch","route":"AGENT","goal":"……","criteria":"……","tool_ids":["cap-fetch-curl-jq"],"plan":["1. 先……","2. 再……"],"memory":["碎片……"],"pointers":["/绝对路径/文件"],"constraints":["……"],"est_steps":5,"est_seconds":300,"shard_no":0,"resume_from":"","task_type":"execute","expected_artifacts":["/绝对路径/产物文件"],"verify_cmd":["命令","参数"]}
一轮可打包最多 3 个互不依赖的动作（发一个 JSON 数组）；reply 和 dispatch 必须单独发。
六个工具的用法：
- reply：纯讨论/轻自查直接答（DIRECT）。对外回复每轮 ≤200 字（代码块除外），每句一个原子事实，去废话，写清文件路径。
- recall：查记忆。每单只查 1 次（本单内再调直接返回上次的结果，不重复查）。默认取前 3 条，命中标来源。没有 recall 就不编历史、不说“没有记忆”。
- registry：查工具单。先 L1 看 9 个大类，再 L2 看某大类的子类与工具编号（一句话说明，不读 L3 全文）。定工具编号写进派单；标不可用的不许用。
- read：读小文件。偷看 ≤2KB，直读 ≤8KB；超 8KB 的文件不许硬读，只把路径写进派单的 pointers。
- bash：只读 bash，只用来看和查（ls/cat/grep/状态）。跑完机器上什么都不许变；要改东西一律写进派单。每单自查（read+bash 合计）最多 3 次。
- dispatch：写派单（TOOL 单工具简单派单 / META 元配置走 PR / AGENT 复杂多步）。派单内容：用户原话由代码自动带上，不用重复写；goal 目标、criteria 验收标准、tool_ids 工具编号、plan 步骤骨架、memory 记忆碎片（recall 查到的）、pointers 指针（路径/链接/记忆 ID，不贴全文）、constraints 约束、est_steps 预估步数、est_seconds 预估耗时；task_type 任务类型（execute 动手产出，默认；research 只调研交付报告）、expected_artifacts 产出文件清单（execute 任务写要产出的文件绝对路径，网关逐项验货；可空，网关有账本兜底）、verify_cmd 是可选的校验命令（想加就加、不加也行，只当参考证据，不由代码一票否决；必须是只读检查命令）。派单总量 ≤3000 token（保守口径）；超限时代码自动分级裁减（先减卡片，核心指令不动），只有核心指令本身超 3000 才打回。派完即返回，不轮询、不睡等。
流程：先判断——纯聊直接 reply；工具一眼能定、不涉旧事的简单派单，跳过 recall 和 registry 直接 dispatch；复杂活先 recall（1 次）+ registry（L1→L2）再 dispatch。互不相干的查询（如 recall + registry L1）可一轮打包最多 3 个。7 轮内必须收口（reply 或 dispatch）；超了就按手头的东西派单或问用户。
`

// Action 是前置一轮的决定。
type Action struct {
	Action   string   `json:"action"`
	Text     string   `json:"text,omitempty"`
	Query    string   `json:"query,omitempty"`
	Level    string   `json:"level,omitempty"`
	Category string   `json:"category,omitempty"`
	Path     string   `json:"path,omitempty"`
	Command  string   `json:"command,omitempty"`
	Route    string   `json:"route,omitempty"`
	Goal     string   `json:"goal,omitempty"`
	Criteria string   `json:"criteria,omitempty"`
	ToolIDs  []string `json:"tool_ids,omitempty"`
	Plan     []string `json:"plan,omitempty"`
	Memory   []string `json:"memory,omitempty"`
	// MustReadMemories 是必读记忆编号（2026-11-26 记忆召回整改）：
	// 前置查到方法/剧本类记忆后写进派单（最多 2 个，只写编号不贴全文）；
	// 执行开工时代码自动读全文喂给模型。
	MustReadMemories []string `json:"must_read_memories,omitempty"`
	Pointers         []string `json:"pointers,omitempty"`
	Constraints      []string `json:"constraints,omitempty"`
	EstSteps         int      `json:"est_steps,omitempty"`
	EstSeconds       int      `json:"est_seconds,omitempty"`
	ShardNo          int      `json:"shard_no,omitempty"`    // 分片号（第 15 项：0 不分片）
	ResumeFrom       string   `json:"resume_from,omitempty"` // 续跑起点（上一片的断点指针）
	// TaskType 是任务类型（P0-3）：execute（默认，动手产出）/
	// research（只调研，交付报告）。未填按 execute。
	TaskType string `json:"task_type,omitempty"`
	// ExpectedArtifacts 是 execute 任务要产出的文件绝对路径清单（P0-2）：
	// 网关验货时逐项核对。research 任务不填。
	ExpectedArtifacts []string `json:"expected_artifacts,omitempty"`
	// VerifyCmd 是无文件产出时的校验命令（P0-2，argv 已拆分）：
	// 网关执行它，exit 0 才算过。research 任务不填。
	VerifyCmd []string `json:"verify_cmd,omitempty"`
	// PlanStepNo 是规划 v2 的计划步号：>0 表示这是计划里的第 N 步，
	// dispatch() 直接用它当会话步号，不再 AllocateStep 另占一位；
	// 0 表示普通派单（走 P2-a 的 AllocateStep）。
	PlanStepNo int `json:"plan_step_no,omitempty"`
	// FirstExecIDBefore 是本步首次派发前账本的最大执行行号（方向 b 严谨版）：
	// 只由 stepToAction 从计划步里带来（首次派发前已记下），
	// dispatch() 原样透传给派单；单步派单不填。
	FirstExecIDBefore int64 `json:"first_exec_id_before,omitempty"`
	// FirstStartTime 是本步首次派发的开始时间（H-2）：同上，
	// 只由 stepToAction 从计划步里带来。
	FirstStartTime time.Time `json:"first_start_time,omitempty"`
	// LastRedoReason 是上一次判回执叫重干的原因（H-1）：只由
	// stepToAction 从计划步里带来，dispatch() 原样透传给派单；
	// 执行层续跑时靠它知道自己错在哪。
	LastRedoReason string `json:"last_redo_reason,omitempty"`
}

// frozenPrompt 拼前置的冻结提示词（放 A 区）：宪法 + 输出格式。
func (f *Front) frozenPrompt() string {
	head := f.Prompt
	if strings.TrimSpace(head) == "" {
		head = frontPromptHead
	}
	return head + "\n" + frontActionFormat
}

// frontActionFormatNative 是原生路径的前置输出格式（不进宪法文件，宪法只写
// 规矩，格式由代码定）：直接调声明好的 6 个工具，不输出 JSON。
const frontActionFormatNative = `直接调声明好的工具完成分诊与派单，不要输出 JSON、不要写代码围栏。
- 工具（6 个）：recall 查记忆；registry 查工具单（先 L1 看 9 个大类，再 L2 看某大类的子类与工具编号）；read 读文件；bash 只读查看命令；dispatch 写派单；plan_task 规划多步任务（步骤表，每步工具编号与 inputs 由代码校验）。
- registry 省着用：最多查 2 次（L1 1 次 + L2 1 次），不要每个大类都翻；cap-ops-python 是通用工具（写文件、跑命令都能干），大多数活直接用它，不用翻遍工具单找“最合适”的。查完工具就动手写 plan_task / dispatch，不要一直查。
- 每轮可以调一个或多个工具（互不依赖的才一起调，最多 3 个）；dispatch 与 plan_task 必须单独发（不要跟别的工具打包）。
- 工具编号只能用 registry 工具返回的真实编号，不许编；派单/规划里的工具编号由代码逐个校验，非法编号会当成工具结果返回给你，换真实编号重发。
- 不调工具、只回正文 = 直接回话（纯讨论 DIRECT）；派单后前置等回执，执行已把 reply.md 写好，前置不再覆盖。
- 流程：先判断——纯聊直接回正文；一步就能做完的简单活（工具一眼能定、不涉旧事），跳过 recall 和 registry 直接 dispatch；**多步任务（2 步以上，或后一步要用到前一步的产物/结果）必须用 plan_task，不许用 dispatch 一次性派单**；复杂活先 recall（1 次）+ registry（L1→L2）再 dispatch 或 plan_task。7 轮内必须收口（回正文或派单）；超了就按手头的东西派单或问用户。
`

// frozenPromptNative 拼原生路径的冻结提示词（放 A 区）：宪法 + 原生格式。
func (f *Front) frozenPromptNative() string {
	head := f.Prompt
	if strings.TrimSpace(head) == "" {
		head = frontPromptHead
	}
	return head + "\n" + frontActionFormatNative
}

// 前置原生工具名（声明与执行共用，防手写串号）。
const (
	frontToolRecall   = "recall"
	frontToolRegistry = "registry"
	frontToolRead     = "read"
	frontToolBash     = "bash"
	frontToolDispatch = "dispatch"
	frontToolPlanTask = "plan_task"
)

// 前置 6 个原生工具的参数表（JSON schema）。
var recallSchema = json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"要查的记忆关键词/问题"}},"required":["query"]}`)

var registrySchema = json.RawMessage(`{"type":"object","properties":{"level":{"type":"string","enum":["L1","L2"],"description":"L1 看 9 个大类；L2 看某大类的子类与工具编号"},"category":{"type":"string","description":"L2 时必填：大类编号，如 fetch"}},"required":["level"]}`)

var readSchema = json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"要读的文件绝对路径"}},"required":["path"]}`)

var bashSchema = json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"只读查看命令，如 ls /某目录；不许改东西"}},"required":["command"]}`)

var dispatchSchema = json.RawMessage(`{"type":"object","properties":{"route":{"type":"string","enum":["TOOL","AGENT","META"],"description":"TOOL 单工具简单派单；AGENT 单步复杂任务（一步做完；多步任务改用 plan_task）；META 元配置"},"goal":{"type":"string","description":"目标，一句话"},"criteria":{"type":"string","description":"验收标准"},"tool_ids":{"type":"array","items":{"type":"string"},"description":"工具编号，必须是 registry 工具返回的真实编号，不许编"},"plan":{"type":"array","items":{"type":"string"},"description":"单步任务的执行骨架（一步内的要点，不要写多步）"},"memory":{"type":"array","items":{"type":"string"},"description":"记忆碎片"},"must_read_memories":{"type":"array","items":{"type":"string"},"description":"必读记忆编号：方法/剧本类记忆的 doc_id（如 W-0001），最多 2 个；执行开工时代码自动读全文，不贴全文"},"pointers":{"type":"array","items":{"type":"string"},"description":"指针：路径/链接/记忆 ID，不贴全文"},"constraints":{"type":"array","items":{"type":"string"},"description":"约束"},"est_steps":{"type":"integer","description":"预估步数"},"est_seconds":{"type":"integer","description":"预估耗时（秒）"},"shard_no":{"type":"integer","description":"分片号，0 不分片"},"resume_from":{"type":"string","description":"续跑起点，上一片的断点指针"},"task_type":{"type":"string","enum":["execute","research"],"description":"execute 动手产出（默认）；research 只调研交付报告"},"expected_artifacts":{"type":"array","items":{"type":"string"},"description":"execute 任务要产出的文件绝对路径清单（可空；网关有账本兜底）"},"verify_cmd":{"type":"array","items":{"type":"string"},"description":"可选的校验命令（argv 已拆分），只当参考证据，不由代码一票否决；必须是只读检查命令"}},"required":["route","goal"]}`)

var planTaskSchema = json.RawMessage(`{"type":"object","properties":{"goal":{"type":"string","description":"任务目标，一句话"},"criteria":{"type":"string","description":"验收标准"},"task_type":{"type":"string","enum":["execute","research"],"description":"execute 动手产出（默认）；research 只调研"},"expected_artifacts":{"type":"array","items":{"type":"string"},"description":"整单要产出的文件绝对路径清单"},"verify_cmd":{"type":"array","items":{"type":"string"},"description":"可选的校验命令（只读），只当参考证据，不由代码一票否决"},"pointers":{"type":"array","items":{"type":"string"},"description":"指针：路径/链接/记忆 ID"},"must_read_memories":{"type":"array","items":{"type":"string"},"description":"必读记忆编号：方法/剧本类记忆的 doc_id（如 W-0001），最多 2 个；每步执行开工时代码自动读全文，不贴全文"},"constraints":{"type":"array","items":{"type":"string"},"description":"约束"},"steps":{"type":"array","items":{"type":"object","properties":{"step_no":{"type":"integer","description":"步骤号，从 1 开始"},"tool_id":{"type":"string","description":"必须是 registry 工具返回的真实编号，不许编"},"inputs":{"type":"array","items":{"type":"string"},"description":"argv 数组：每个字符串是命令行上的一个参数；不要写 shell 语法（管道/重定向/分号/&&/$变量），不要写 ./ 或 ../ 相对路径，不要把工具名再写一遍"},"depends_on":{"type":"array","items":{"type":"integer"},"description":"依赖的步骤号"},"expected_output":{"type":"string","description":"这步做完应得到什么"},"expected_artifacts":{"type":"array","items":{"type":"string"},"description":"这步产出的文件绝对路径（可空）"},"verify_cmd":{"type":"array","items":{"type":"string"},"description":"这步可选的校验命令（只读），只当参考证据，不由代码一票否决"},"step_type":{"type":"string","enum":["execute","research"],"description":"这步类型：execute 动手产出（默认）；research 只查/只读"},"est_steps":{"type":"integer","description":"预估本步要几轮工具调用：research 调研步默认 12（StepCap 24），execute 动手步默认 10（StepCap 20）；要翻很多文件的调研步可以往上估；不填用默认值"},"optional":{"type":"boolean","description":"这步是否可跳过：true 表示失败时可跳过（仅当无其他步依赖它）；默认 false"}},"required":["step_no","tool_id","expected_output"]}}},"required":["goal","steps"]}`)

// declaredFrontTools 向模型声明前置的 6 个原生工具（2 号）：
// recall 查记忆、registry 查工具单、read 读文件、bash 只读查看、
// dispatch 写派单、plan_task 规划多步。只声明这 6 个，不塞注册表
// 全部约 90 个工具（会炸 token、挑花眼）；注册表的工具由 registry
// 工具以文本形式查，模型从返回的真实清单里挑编号。
func (f *Front) declaredFrontTools() []llm.ToolDefinition {
	return []llm.ToolDefinition{
		llm.NewToolDefinition(frontToolRecall, "查记忆：按关键词/问题查相关记忆碎片，每单只查 1 次，默认取前 3 条。", recallSchema),
		llm.NewToolDefinition(frontToolRegistry, "查工具单：先 L1 看 9 个大类，再 L2 看某大类的子类与工具编号（一句话说明）。工具编号只能从这里返回的真实清单里挑，不许编。", registrySchema),
		llm.NewToolDefinition(frontToolRead, "读文件：读一个绝对路径的文件（小文件全文，超 8KB 给结构/签名）。只读，什么都不改。", readSchema),
		llm.NewToolDefinition(frontToolBash, "只读 bash：只用来看和查（ls/cat/grep/状态），跑完机器上什么都不许变；要改东西一律写进派单。", bashSchema),
		llm.NewToolDefinition(frontToolDispatch, "写派单：把一步就能做完的简单活派给执行会话（只做一步）。多步任务（2 步以上，或后一步要用到前一步的产物/结果）不许用这个，改用 plan_task。工具编号必须是 registry 工具返回的真实编号，代码逐个校验，非法编号打回重发。派完即返回，等回执。", dispatchSchema),
		llm.NewToolDefinition(frontToolPlanTask, "规划多步任务（多步任务的首选）：把任务拆成步骤表（每步工具编号、inputs、预期产出、验收），代码逐项校验（工具编号须真实、inputs 不许 shell 语法与相对路径），通过后前置一步步派单、每步看回执判做完没有。", planTaskSchema),
	}
}

// DispatchArgs 是 dispatch 原生工具的参数（与 Action 的派单字段一一对应）。
type DispatchArgs struct {
	Route             string   `json:"route"`
	Goal              string   `json:"goal"`
	Criteria          string   `json:"criteria"`
	ToolIDs           []string `json:"tool_ids"`
	Plan              []string `json:"plan"`
	Memory            []string `json:"memory"`
	MustReadMemories  []string `json:"must_read_memories"`
	Pointers          []string `json:"pointers"`
	Constraints       []string `json:"constraints"`
	EstSteps          int      `json:"est_steps"`
	EstSeconds        int      `json:"est_seconds"`
	ShardNo           int      `json:"shard_no"`
	ResumeFrom        string   `json:"resume_from"`
	TaskType          string   `json:"task_type"`
	ExpectedArtifacts []string `json:"expected_artifacts"`
	VerifyCmd         []string `json:"verify_cmd"`
}

// toAction 把 dispatch 工具参数转成内部 Action（复用 f.dispatch）。
func (d DispatchArgs) toAction() Action {
	return Action{
		Action: "dispatch", Route: d.Route, Goal: d.Goal, Criteria: d.Criteria,
		ToolIDs: d.ToolIDs, Plan: d.Plan, Memory: d.Memory, Pointers: d.Pointers,
		MustReadMemories: d.MustReadMemories,
		Constraints:      d.Constraints, EstSteps: d.EstSteps, EstSeconds: d.EstSeconds,
		ShardNo: d.ShardNo, ResumeFrom: d.ResumeFrom, TaskType: d.TaskType,
		ExpectedArtifacts: d.ExpectedArtifacts, VerifyCmd: d.VerifyCmd,
	}
}

// PlanStep 是 plan_task 步骤表里的一步。
// PlanStep 是 plan_task 里的单步（规划 v2）：
//   - StepType：execute（动手产出，verify 必填）/ research（只调研，
//     verify 可空，底线是 ≥1 次成功工具调用 + 非空步间摘要）；
//     为空时按整单 TaskType 论（再空按 execute）。
//   - Optional：该步是否可选——重干 2 次仍不过时，可选且无依赖的步
//     可跳过（SKIPPED），否则局部重规划或停下诚实汇报。
type PlanStep struct {
	StepNo            int      `json:"step_no"`
	ToolID            string   `json:"tool_id"`
	Inputs            []string `json:"inputs"`
	DependsOn         []int    `json:"depends_on"`
	ExpectedOutput    string   `json:"expected_output"`
	ExpectedArtifacts []string `json:"expected_artifacts"`
	VerifyCmd         []string `json:"verify_cmd"`
	StepType          string   `json:"step_type,omitempty"`
	Optional          bool     `json:"optional,omitempty"`
	// EstSteps 是这步预估要几轮工具调用（CF 诊断中优）：模型规划时
	// 可填，不填（0）时 stepToAction 按步类型给默认值（research=12，
	// execute=10），StepCap = EstSteps*2（上限 50）。
	EstSteps int `json:"est_steps,omitempty"`
	// LastRedoReason：上次判回执叫重干的原因（M-2，会话步的
	// LastRedoReason 还原到这里），不进模型参数 JSON，只进派单骨架。
	LastRedoReason string `json:"-"`
	// FirstExecIDBefore / FirstStartTime：本步首次派发前账本的最大
	// 执行行号与开始时间（方向 b 严谨版 / H-2，会话步还原到这里），
	// 不进模型参数 JSON，只进派单透传。
	FirstExecIDBefore int64     `json:"-"`
	FirstStartTime    time.Time `json:"-"`
}

// PlanArgs 是 plan_task 原生工具的参数。
type PlanArgs struct {
	Goal              string     `json:"goal"`
	Criteria          string     `json:"criteria"`
	TaskType          string     `json:"task_type"`
	ExpectedArtifacts []string   `json:"expected_artifacts"`
	VerifyCmd         []string   `json:"verify_cmd"`
	Pointers          []string   `json:"pointers"`
	MustReadMemories  []string   `json:"must_read_memories"`
	Constraints       []string   `json:"constraints"`
	Steps             []PlanStep `json:"steps"`
}

// shellOperatorTokens 是 inputs 里当独立词出现即判为 shell 语法的词。
// 只拦“整个词就是操作符”的情况：正则参数里的 |()（如 "(a|b)" 是一个词，
// 不是独立的 |）不误杀（B-04 教训：正则里的 |() 是合法参数）。
var shellOperatorTokens = map[string]bool{
	"|": true, "||": true, "&&": true, ";": true,
	">": true, ">>": true, "<": true, "2>": true, "&": true,
}

// validatePlanInputs 校验 plan_task 某步的 inputs（argv 数组）：
// 不许 shell 语法、不许 ./ 或 ../ 或 ~/ 相对路径、不许重复写工具名。
// 只拦 shell 语法与 ./、../、~/（Gemini 审阅），普通参数不误杀。
func validatePlanInputs(toolID string, inputs []string) error {
	if len(inputs) > 0 && inputs[0] == toolID {
		return fmt.Errorf("inputs 里不要重复写工具名 %q（去掉第一个参数）", toolID)
	}
	for _, in := range inputs {
		if in == "." || in == ".." || in == "~" || strings.HasPrefix(in, "./") || strings.HasPrefix(in, "../") || strings.HasPrefix(in, "~/") {
			return fmt.Errorf("inputs 里不许相对路径 %q（用绝对路径）", in)
		}
		for _, tok := range strings.Fields(in) {
			if shellOperatorTokens[tok] {
				return fmt.Errorf("inputs 里不要写 shell 语法 %q（argv 直接传给进程，不经过 shell；要传多个参数就拆成 inputs 数组的多个字符串）", tok)
			}
			if strings.HasPrefix(tok, "$") || strings.Contains(tok, "`") || strings.Contains(tok, "$(") {
				return fmt.Errorf("inputs 里不要写 shell 变量/命令替换 %q", tok)
			}
		}
	}
	return nil
}

// maxPlanSteps 是 plan_task 一单最多步数（规划 v2 粗粒度）：
// 计划要粗粒度（3–5 步，不是十几个 shell 命令——执行层自己的循环
// 一步里能跑多轮工具调用）；超过 10 步说明拆得太细，请合并。
const maxPlanSteps = 10

// validatePlan 校验 plan_task 的整单计划：目标、步骤数、每步的工具编号
// （注册表白名单）、inputs、step_type。验收（expected_artifacts /
// verify_cmd）全是可选的（对齐二号三号：不强制验证），不校验。
// 非法时返回包着 ErrDispatchValidation 的错误，
// 原生路径回填给模型重试。
func (f *Front) validatePlan(p PlanArgs) error {
	if strings.TrimSpace(p.Goal) == "" {
		return fmt.Errorf("%w：plan_task 缺 goal（任务目标）", ErrDispatchValidation)
	}
	if len(p.Steps) == 0 {
		return fmt.Errorf("%w：plan_task 的 steps 为空（至少 1 步）", ErrDispatchValidation)
	}
	if len(p.Steps) > maxPlanSteps {
		return fmt.Errorf("%w：plan_task 的 steps %d 步，超 %d 步上限（计划要粗粒度 3–5 步，请把多个 shell 命令合并成一步）", ErrDispatchValidation, len(p.Steps), maxPlanSteps)
	}
	taskType := p.TaskType
	if taskType == "" {
		taskType = "execute"
	}
	if taskType != "execute" && taskType != "research" {
		return fmt.Errorf("%w：plan_task 的 task_type %q 非法，只能是 execute/research", ErrDispatchValidation, p.TaskType)
	}
	for i, s := range p.Steps {
		where := fmt.Sprintf("第 %d 步", i+1)
		if s.StepNo <= 0 {
			return fmt.Errorf("%w：%s 的 step_no 须 ≥1", ErrDispatchValidation, where)
		}
		if strings.TrimSpace(s.ToolID) == "" {
			return fmt.Errorf("%w：%s 缺 tool_id", ErrDispatchValidation, where)
		}
		if _, _, err := triage.ResolveTool(f.Reg, s.ToolID); err != nil {
			return fmt.Errorf("%w：%s 的工具 %q 不可用：%v（请换真实工具编号重发）", ErrDispatchValidation, where, s.ToolID, err)
		}
		if err := validatePlanInputs(s.ToolID, s.Inputs); err != nil {
			return fmt.Errorf("%w：%s %v", ErrDispatchValidation, where, err)
		}
		if strings.TrimSpace(s.ExpectedOutput) == "" {
			return fmt.Errorf("%w：%s 缺 expected_output（这步做完应得到什么）", ErrDispatchValidation, where)
		}
		// step_type：每步可单独标 execute/research；为空按整单 TaskType 论。
		// verify_cmd 是可选的（对齐二号三号：不强制验证）：前置想给某步
		// 加个校验命令可以加，不想加就不加；execute 步即使既无
		// expected_artifacts 又无 verify_cmd 也放行——网关有账本兜底
		// （实质调用 ≥1）与硬底线（0 次调用 / 产物问题）把关，
		// 不靠前置在计划里把验收写全。
		stepType := s.StepType
		if stepType == "" {
			stepType = taskType
		}
		if stepType != "execute" && stepType != "research" {
			return fmt.Errorf("%w：%s 的 step_type %q 非法，只能是 execute/research", ErrDispatchValidation, where, s.StepType)
		}
	}
	return nil
}

// ErrDispatchValidation 是派单校验失败的哨兵错误（工具编号非法/不可用、
// task_type 非法、计划非法等）：模型可修的错，原生路径回填给模型重试，
// 不当致命错；文本路径照常返回错误。
var ErrDispatchValidation = errors.New("front: 派单校验失败")

// validateDispatchAction 校验派单动作里模型可修的部分（工具编号逐个过
// 注册表白名单、task_type 二选一）：非法时返回包着 ErrDispatchValidation
// 的错误。返回归一化后的 task_type（未填按 execute）。
func (f *Front) validateDispatchAction(act Action) (string, error) {
	// 工具编号逐个过 ResolveTool（分诊不编造工具的硬守卫，见 triage）：
	// 不在册、不可用、默认隐藏的直接打回，不进执行。
	for _, toolID := range act.ToolIDs {
		if _, _, err := triage.ResolveTool(f.Reg, toolID); err != nil {
			return "", fmt.Errorf("%w：派单工具 %q 不可用：%v（请换真实工具编号重派）", ErrDispatchValidation, toolID, err)
		}
	}
	// 任务类型校验（P0-3）：只认 execute/research；未填按 execute；
	// 别的都打回，不进执行。
	taskType := act.TaskType
	if taskType == "" {
		taskType = "execute"
	}
	if taskType != "execute" && taskType != "research" {
		return "", fmt.Errorf("%w：派单 task_type %q 非法，只能是 execute/research", ErrDispatchValidation, act.TaskType)
	}
	// CF-013 根治：cap-fetch-camoufox 只是查浏览器版本的命令行工具，
	// 打不开网页，派它等于没派；浏览器活一律走 cap-ops-python +
	// Camoufox Python API。这里直接打回，逼模型换 Python 重派
	// （注册表与 F-27 已写明，但 CF-013 证明光写提示词拦不住模型，
	// 必须代码层拦）。
	for _, toolID := range act.ToolIDs {
		if toolID == "cap-fetch-camoufox" {
			return "", fmt.Errorf("%w：派单工具 %q 只是查浏览器版本的命令行，打不开网页；浏览器活（开网页/填表/点按钮/过 Turnstile）一律用 cap-ops-python + Camoufox Python API 重派", ErrDispatchValidation, toolID)
		}
	}
	return taskType, nil
}

// isRetryableDispatchError 判一个派单错误是不是模型可修的（回填给模型
// 重试）：校验失败与 token 超限都是；通道发送失败等基础设施错不是。
func isRetryableDispatchError(err error) bool {
	return errors.Is(err, ErrDispatchValidation) || errors.Is(err, ErrTokenOverflow)
}

// extractJSONValue 取最外层 JSON 值（对象或数组）：从每个候选起点
// （{ 或 [）依次试解码，第一个能完整解出的即为 JSON 值。
// 模型常在 JSON 前加 [任务·…] 小标题、markdown 围栏，后面跟
// <ds_safety>[…]…</ds_safety> 这类带方括号的块；旧实现取首个 [/{ 到
// 末个 ]/}，会把这些括号也吞进来，拼出非法 JSON。
// （2026-11-24 线 B 验收 s5-s6a/6b/6b3：长任务两次解析失败后，兜底
// 把原话当 DIRECT 回信，造成假 DONE。）
func extractJSONValue(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] != '{' && s[i] != '[' {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(s[i:]))
		var v any
		if err := dec.Decode(&v); err != nil {
			continue
		}
		if n := int(dec.InputOffset()); n > 0 {
			return s[i : i+n]
		}
	}
	return s
}

// validateAction 校验单个前置决定。
func validateAction(a Action) error {
	switch a.Action {
	case "reply":
		if strings.TrimSpace(a.Text) == "" {
			return fmt.Errorf("front: reply 决定缺 text")
		}
	case "recall":
		if strings.TrimSpace(a.Query) == "" {
			return fmt.Errorf("front: recall 决定缺 query")
		}
	case "registry":
		if a.Level != "L1" && a.Level != "L2" {
			return fmt.Errorf("front: registry 决定 level 须为 L1 或 L2")
		}
		if a.Level == "L2" && strings.TrimSpace(a.Category) == "" {
			return fmt.Errorf("front: registry L2 决定缺 category")
		}
	case "read":
		if strings.TrimSpace(a.Path) == "" {
			return fmt.Errorf("front: read 决定缺 path")
		}
	case "bash":
		if strings.TrimSpace(a.Command) == "" {
			return fmt.Errorf("front: bash 决定缺 command")
		}
	case "dispatch":
		if a.Route != "TOOL" && a.Route != "AGENT" && a.Route != "META" {
			return fmt.Errorf("front: dispatch 决定 route 须为 TOOL/AGENT/META")
		}
		if strings.TrimSpace(a.Goal) == "" {
			return fmt.Errorf("front: dispatch 决定缺 goal")
		}
	default:
		return fmt.Errorf("front: 未知 action %q", a.Action)
	}
	return nil
}

// parseActions 解析前置一轮的决定：单个 JSON 对象，或最多 3 个
// 互不依赖动作的 JSON 数组。reply/dispatch 必须单独发。
func parseActions(reply string) ([]Action, error) {
	v := strings.TrimSpace(extractJSONValue(reply))
	if strings.HasPrefix(v, "[") {
		var arr []Action
		if err := json.Unmarshal([]byte(v), &arr); err != nil {
			return nil, fmt.Errorf("front: 打包决定非法 JSON: %w", err)
		}
		if len(arr) == 0 {
			return nil, fmt.Errorf("front: 打包决定为空数组")
		}
		if len(arr) > MaxBatchActions {
			return nil, fmt.Errorf("front: 打包决定超过 %d 个（%d 个）", MaxBatchActions, len(arr))
		}
		for i, a := range arr {
			if err := validateAction(a); err != nil {
				return nil, fmt.Errorf("front: 打包第 %d 个决定非法: %w", i+1, err)
			}
			if a.Action == "reply" || a.Action == "dispatch" {
				return nil, fmt.Errorf("front: reply/dispatch 必须单独发，不能打包")
			}
		}
		return arr, nil
	}
	var a Action
	if err := json.Unmarshal([]byte(v), &a); err != nil {
		return nil, fmt.Errorf("front: 决定非法 JSON: %w", err)
	}
	if err := validateAction(a); err != nil {
		return nil, err
	}
	return []Action{a}, nil
}

// Run 跑完前置一单：判断、分诊、派单或回话。
// 纯讨论直接回话写 reply.md；任务派单后等回执，回执状态即本单状态。
// CallWithTools 接线时走 2 号原生路径（runNative），否则走旧的文本
// JSON 路径（runText）。生产 main.go 一定接线，原生是唯一生产路径。
func (f *Front) Run(ctx context.Context, taskID, taskText string) (*FrontResult, error) {
	return f.RunWithSession(ctx, taskID, taskText, "")
}

// RunWithSession 与 Run 相同，但带上本次任务的会话号（P2-a）：
// sessionID 为空时派单按 taskID 归属会话（main.go 传进来的是已解析过的，
// 空值由 dispatch() 再按 taskID 兜底）。会话号决定派单的步历史落在哪，
// 与进程级 SessionID（f.SessionID）是两回事，别混。
func (f *Front) RunWithSession(ctx context.Context, taskID, taskText, sessionID string) (*FrontResult, error) {
	frozen := f.frozenPrompt()
	if f.CallWithTools != nil {
		frozen = f.frozenPromptNative()
	}
	artDir := filepath.Join(f.DataDir, "artifacts", taskID)
	if err := os.MkdirAll(artDir, 0o755); err != nil {
		return nil, fmt.Errorf("front: 建产物目录失败: %w", err)
	}
	if f.CallWithTools != nil {
		return f.runNative(ctx, taskID, taskText, artDir, frozen, sessionID)
	}
	return f.runText(ctx, taskID, taskText, artDir, frozen, sessionID)
}

// runText 是旧的文本 JSON 路径：模型输出 JSON 动作，前置解析后执行。
// 只留给测试与旧调用方；生产走 runNative。
func (f *Front) runText(ctx context.Context, taskID, taskText, artDir, frozen, sessionID string) (*FrontResult, error) {
	// recalled：本单是否已查过记忆（F-05：每单只查 1 次）；memFragments
	// 是查到的碎片（派单时带上，本单内不再重复查）。
	recalled := false
	var memFragments []string
	selfChecks := 0
	// history：每轮的动作结果摘要（只留短行，不贴全文），供下一轮参考。
	var history []string
	for round := 1; round <= MaxFrontRounds; round++ {
		tail := buildTail(taskText, memFragments, history)
		reply, _, _, err := f.Call(taskID, frozen, tail)
		if err != nil {
			return nil, fmt.Errorf("front: 第 %d 轮模型调用失败: %w", round, err)
		}
		actions, perr := parseActions(reply)
		if perr != nil {
			// 坏 JSON 重试一次：把“只返回 JSON”追加进尾巴再调。
			retry, _, _, rerr := f.Call(taskID, frozen, tail+"\n注意：上一轮回复不是合法 JSON，只返回 JSON 对象（单发一个对象，或打包最多 3 个互不依赖的 recall/registry/read/bash 动作的数组），不要加任何解释文字。")
			if rerr != nil {
				return nil, fmt.Errorf("front: 第 %d 轮模型调用失败: %w", round, rerr)
			}
			actions, perr = parseActions(retry)
			if perr != nil {
				// 两次都不是合法 JSON：直接标 FAILED 并写失败回信，
				// 绝不把原话当 DIRECT 回信标 DONE（假 DONE，Gemini
				// 审阅第十节 A 清单第 1 项；2026-11-24 线 B 验收
				// s5-s6a/6b/6b3 曾因此把未解析的原话当成完成回信）。
				// 失败回信只写人话原因，不贴模型原话。
				log.Printf("front task=%s 第 %d 轮决定两次解析失败，标 FAILED: %v", taskID, round, perr)
				return f.failReply(artDir, taskID, fmt.Sprintf("前置第 %d 轮两次解析模型回复均失败（回复不是合法 JSON，无法判断是直接回话还是派单）", round))
			}
		}
		// reply/dispatch 只能单发（parseActions 已保证）。
		if len(actions) == 1 && actions[0].Action == "reply" {
			return f.reply(artDir, taskID, actions[0].Text, "DIRECT")
		}
		if len(actions) == 1 && actions[0].Action == "dispatch" {
			return f.dispatch(ctx, artDir, taskID, taskText, actions[0], memFragments, sessionID)
		}
		// 打包执行（recall/registry/read/bash，1–3 个）。
		for _, act := range actions {
			switch act.Action {
			case "recall":
				if recalled {
					history = append(history, "recall：本单已查过，不重复查；碎片见上文。")
					continue
				}
				recalled = true
				frags, err := f.doRecall(taskID, act.Query)
				if err != nil {
					history = append(history, "recall 失败："+err.Error()+"；没有 recall 就不编历史。")
					continue
				}
				memFragments = frags
				if len(frags) == 0 {
					history = append(history, "recall：无命中（不编历史，不说“没有记忆”）。")
				} else {
					history = append(history, fmt.Sprintf("recall：命中 %d 条，碎片已记下（派单时带上）。", len(frags)))
				}
			case "registry":
				out, err := f.doRegistry(act.Level, act.Category)
				if err != nil {
					history = append(history, "registry 失败："+err.Error())
					continue
				}
				history = append(history, "registry "+act.Level+"：\n"+out)
			case "read":
				if selfChecks >= MaxSelfChecks {
					history = append(history, "read：自查已满 3 次，不再查；按手头的东西派单或问用户。")
					continue
				}
				selfChecks++
				out, err := f.doRead(act.Path)
				if err != nil {
					history = append(history, "read 失败："+err.Error())
					continue
				}
				history = append(history, "read "+act.Path+"：\n"+out)
			case "bash":
				if selfChecks >= MaxSelfChecks {
					history = append(history, "bash：自查已满 3 次，不再查；按手头的东西派单或问用户。")
					continue
				}
				selfChecks++
				out, err := f.doBash(ctx, act.Command)
				if err != nil {
					history = append(history, "bash 失败："+err.Error())
					continue
				}
				history = append(history, "bash "+act.Command+"：\n"+out)
			}
		}
	}
	// 7 轮未收口（F-12）：按手头的东西派单（AGENT，工具未定则空着，
	// 执行按派单骨架自行定工具），派单失败才问用户。
	d := &dispatch.Dispatch{
		Route: "AGENT", Goal: "完成用户任务（前置 7 轮未收口，按手头信息派单）",
		Criteria: "按用户原话验收", Memory: memFragments,
		EstSteps: 5, EstSeconds: 300,
	}
	return f.dispatch(ctx, artDir, taskID, taskText, Action{
		Route: "AGENT", Goal: d.Goal, Criteria: d.Criteria,
		Memory: memFragments, EstSteps: 5, EstSeconds: 300,
	}, memFragments, sessionID)
}

// runNative 是 2 号原生路径：6 个工具声明给模型，模型回 tool_calls，
// 结果以 tool 消息结构化回填进下一轮；不调工具只回正文 = 直接回话。
// 截断（finish_reason=length）整轮作废：不执行其中的工具调用、不把
// 截断的 assistant 消息写进历史，提醒进下一轮尾巴重发（P0-1 同款，
// 执行会话的截断拦截；提醒放尾巴里，不当 user 消息塞进 msgHistory）。
func (f *Front) runNative(ctx context.Context, taskID, taskText, artDir, frozen, sessionID string) (*FrontResult, error) {
	tools := f.declaredFrontTools()
	// msgHistory 是原生路径的结构化对话历史（assistant 的 tool_calls +
	// tool 的结果，原样追加，不得改写）；首轮 user 消息也在历史里，
	// 消息顺序为 system → user → assistant，符合规范、利好 KV 缓存。
	var msgHistory []llm.Message
	recalled := false
	var memFragments []string
	selfChecks := 0
	// validationErrs：本单连续派单校验失败次数（M2 上限用）；只有派单/
	// plan_task 校验成功才清零，查记忆/查工具单等中间动作不碰它——模型
	// 卡在同一个非法派单上，中间查再多东西也还是卡住。
	validationErrs := 0
	// truncationNotice：上一轮被截断的提醒，进下一轮尾巴，用一次即清。
	truncationNotice := ""
	// blockedNotice：上一轮被内容过滤/空回复的提醒，进下一轮尾巴，用一次即清。
	blockedNotice := ""
	for round := 1; round <= MaxFrontRounds; round++ {
		var tail string
		if round == 1 {
			// 首轮：用户原话进尾巴，同时作为首条 user 消息写进
			// msgHistory；尾巴本身不再重发（BuildMessages 会跳过
			// 空尾巴，不会发两遍）。记忆摘要首轮为空（recall 还没
			// 查到），查到后以 tool 结果形式进历史，不再进尾巴。
			tail = buildTail(taskText, memFragments, nil)
			msgHistory = append(msgHistory, llm.Message{Role: "user", Content: tail})
			tail = ""
		}
		if truncationNotice != "" {
			tail += "\n" + truncationNotice
			truncationNotice = ""
		}
		if blockedNotice != "" {
			tail += "\n" + blockedNotice
			blockedNotice = ""
		}
		msg, finishReason, promptTokens, _, err := f.CallWithTools(taskID, frozen, tail, tools, msgHistory)
		if err != nil {
			return nil, fmt.Errorf("front: 第 %d 轮模型调用失败: %w", round, err)
		}
		// P2-b：记会话水位（单次 prompt_tokens 的峰值），翻新阈值用。
		// 失败只记日志、不阻塞任务。
		if f.sess != nil && sessionID != "" && promptTokens > 0 {
			if rerr := f.sess.RecordTokens(sessionID, promptTokens); rerr != nil {
				log.Printf("front task=%s 会话水位记录失败: %v", taskID, rerr)
			}
		}
		// 诊断日志（规划 v2）：每轮模型调了哪些工具、回了什么正文。
		{
			var names []string
			for _, tc := range msg.ToolCalls {
				names = append(names, tc.Function.Name)
			}
			log.Printf("front task=%s 第 %d 轮：工具=%v 正文=%.120s", taskID, round, names, strings.TrimSpace(msg.Content))
		}
		// 截断拦截：本轮整轮作废，不执行、不交活、不进历史。
		if finishReason == "length" {
			log.Printf("front task=%s 第 %d 轮输出被截断，本轮作废", taskID, round)
			truncationNotice = "上一轮你的输出被截断了，本轮作废；请用更短的回复重发，不要重复已发出的工具调用，不要把截断的内容当答案。"
			continue
		}
		// 内容过滤/空回复拦截：finish_reason 是 content_filter，或正文
		// 为空且没调工具，都不当作交活——直接交活会写一份空 reply.md。
		// 记一轮作废，让模型换个说法重发；7 轮上限由外层循环兜底。
		if finishReason == "content_filter" || (len(msg.ToolCalls) == 0 && strings.TrimSpace(msg.Content) == "") {
			log.Printf("front task=%s 第 %d 轮回复被内容过滤拦截或为空，本轮作废", taskID, round)
			blockedNotice = "上一轮你的回复被内容过滤拦截或为空，本轮作废；换个说法重发，不要交空回复。"
			continue
		}
		// 不调工具只回正文 = 直接回话（DIRECT）。
		if len(msg.ToolCalls) == 0 {
			return f.reply(artDir, taskID, msg.Content, "DIRECT")
		}
		// 调了工具：assistant 消息（带 tool_calls）进结构化历史，
		// 再逐个执行调用、把结果以 tool 消息回填。
		msgHistory = append(msgHistory, msg)
		res, terminal, err := f.execNativeCalls(ctx, taskID, taskText, artDir, msg.ToolCalls, &msgHistory, &recalled, &memFragments, &selfChecks, &validationErrs, sessionID)
		if err != nil {
			return nil, err
		}
		if terminal {
			return res, nil
		}
	}
	// 7 轮未收口（F-12）：按手头的东西派单（AGENT，工具未定则空着，
	// 执行按派单骨架自行定工具），派单失败才问用户。
	return f.dispatch(ctx, artDir, taskID, taskText, Action{
		Route: "AGENT", Goal: "完成用户任务（前置 7 轮未收口，按手头信息派单）",
		Criteria: "按用户原话验收", Memory: memFragments, EstSteps: 5, EstSeconds: 300,
	}, memFragments, sessionID)
}

// stopOnValidationCap 在连续派单校验失败超限时写失败回信并终结本单
// （M2）：返回 (result, true, nil)；写回信失败时返回 (nil, false, err)。
func (f *Front) stopOnValidationCap(artDir, taskID string, validationErrs *int, err error) (*FrontResult, bool, error) {
	fr, ferr := f.failReply(artDir, taskID, fmt.Sprintf("派单连续 %d 次校验失败，已停下不再重试：%s", *validationErrs, err))
	if ferr != nil {
		return nil, false, ferr
	}
	return fr, true, nil
}

// execNativeCalls 执行原生路径一轮的工具调用（最多 3 个），逐个把结果
// 以 tool 消息回填进 msgHistory。dispatch/plan_task 是终结动作：
// 执行它（校验→派单→等回执）并返回 (result, true, nil)；校验失败等
// 模型可修的错回填成工具结果、不终结，让模型换真实编号重发；
// 通道发送失败等基础设施错返回 (nil, false, err)，整单失败。
// 其余工具（recall/registry/read/bash）的结果与错误都回填成 tool 消息，
// 模型下一轮接着干。
func (f *Front) execNativeCalls(ctx context.Context, taskID, taskText, artDir string, calls []llm.ToolCall, msgHistory *[]llm.Message, recalled *bool, memFragments *[]string, selfChecks *int, validationErrs *int, sessionID string) (*FrontResult, bool, error) {
	for _, tc := range calls {
		switch tc.Function.Name {
		case frontToolRecall:
			var args struct {
				Query string `json:"query"`
			}
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				*msgHistory = append(*msgHistory, toolResultMsg(tc.ID, "recall 参数解析失败："+err.Error()+"；请按 {\"query\":\"……\"} 重发。"))
				continue
			}
			if strings.TrimSpace(args.Query) == "" {
				*msgHistory = append(*msgHistory, toolResultMsg(tc.ID, "recall 缺 query（要查的记忆关键词/问题）；请补上重发。"))
				continue
			}
			if *recalled {
				*msgHistory = append(*msgHistory, toolResultMsg(tc.ID, "recall：本单已查过，不重复查；碎片见上文。"))
				continue
			}
			*recalled = true
			frags, err := f.doRecall(taskID, args.Query)
			if err != nil {
				*msgHistory = append(*msgHistory, toolResultMsg(tc.ID, "recall 失败："+err.Error()+"；没有 recall 就不编历史。"))
				continue
			}
			*memFragments = frags
			if len(frags) == 0 {
				*msgHistory = append(*msgHistory, toolResultMsg(tc.ID, "recall：无命中（不编历史，不说“没有记忆”）。"))
			} else {
				*msgHistory = append(*msgHistory, toolResultMsg(tc.ID, fmt.Sprintf("recall：命中 %d 条：\n%s", len(frags), strings.Join(frags, "\n"))))
			}
		case frontToolRegistry:
			var args struct {
				Level    string `json:"level"`
				Category string `json:"category"`
			}
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				*msgHistory = append(*msgHistory, toolResultMsg(tc.ID, "registry 参数解析失败："+err.Error()+"；请按 {\"level\":\"L1\"} 或 {\"level\":\"L2\",\"category\":\"大类\"} 重发。"))
				continue
			}
			if args.Level != "L1" && args.Level != "L2" {
				*msgHistory = append(*msgHistory, toolResultMsg(tc.ID, "registry 的 level 须为 L1 或 L2；请重发。"))
				continue
			}
			if args.Level == "L2" && strings.TrimSpace(args.Category) == "" {
				*msgHistory = append(*msgHistory, toolResultMsg(tc.ID, "registry L2 缺 category（大类编号）；请补上重发。"))
				continue
			}
			out, err := f.doRegistry(args.Level, args.Category)
			if err != nil {
				*msgHistory = append(*msgHistory, toolResultMsg(tc.ID, "registry 失败："+err.Error()))
				continue
			}
			*msgHistory = append(*msgHistory, toolResultMsg(tc.ID, "registry "+args.Level+"：\n"+out))
		case frontToolRead:
			var args struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				*msgHistory = append(*msgHistory, toolResultMsg(tc.ID, "read 参数解析失败："+err.Error()+"；请按 {\"path\":\"/绝对路径/文件\"} 重发。"))
				continue
			}
			if *selfChecks >= MaxSelfChecks {
				*msgHistory = append(*msgHistory, toolResultMsg(tc.ID, "read：自查已满 3 次，不再查；按手头的东西派单或问用户。"))
				continue
			}
			if strings.TrimSpace(args.Path) == "" {
				*msgHistory = append(*msgHistory, toolResultMsg(tc.ID, "read 缺 path（要读的文件绝对路径）；请补上重发。"))
				continue
			}
			*selfChecks++
			out, err := f.doRead(args.Path)
			if err != nil {
				*msgHistory = append(*msgHistory, toolResultMsg(tc.ID, "read 失败："+err.Error()))
				continue
			}
			*msgHistory = append(*msgHistory, toolResultMsg(tc.ID, "read "+args.Path+"：\n"+out))
		case frontToolBash:
			var args struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				*msgHistory = append(*msgHistory, toolResultMsg(tc.ID, "bash 参数解析失败："+err.Error()+"；请按 {\"command\":\"ls /某目录\"} 重发。"))
				continue
			}
			if *selfChecks >= MaxSelfChecks {
				*msgHistory = append(*msgHistory, toolResultMsg(tc.ID, "bash：自查已满 3 次，不再查；按手头的东西派单或问用户。"))
				continue
			}
			if strings.TrimSpace(args.Command) == "" {
				*msgHistory = append(*msgHistory, toolResultMsg(tc.ID, "bash 缺 command（只读查看命令）；请补上重发。"))
				continue
			}
			*selfChecks++
			out, err := f.doBash(ctx, args.Command)
			if err != nil {
				*msgHistory = append(*msgHistory, toolResultMsg(tc.ID, "bash 失败："+err.Error()))
				continue
			}
			*msgHistory = append(*msgHistory, toolResultMsg(tc.ID, "bash "+args.Command+"：\n"+out))
		case frontToolDispatch:
			var args DispatchArgs
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				*msgHistory = append(*msgHistory, toolResultMsg(tc.ID, "dispatch 参数解析失败："+err.Error()+"；请按 dispatch 参数表重发（route 与 goal 必填）。"))
				continue
			}
			res, err := f.dispatch(ctx, artDir, taskID, taskText, args.toAction(), *memFragments, sessionID)
			if err != nil {
				if isRetryableDispatchError(err) {
					// M2：连续派单校验失败记数，超 2 次直接停下写失败回信，
					// 不再重问模型（模型钻牛角尖反复派同一个错误工具时，
					// 每轮都是一次前置模型调用，烧预算）。
					if errors.Is(err, ErrDispatchValidation) {
						*validationErrs++
						if *validationErrs > maxConsecutiveValidationErrors {
							log.Printf("front task=%s 派单连续 %d 次校验失败，停下不再重问模型: %v", taskID, *validationErrs, err)
							return f.stopOnValidationCap(artDir, taskID, validationErrs, err)
						}
					}
					*msgHistory = append(*msgHistory, toolResultMsg(tc.ID, "dispatch 失败："+err.Error()))
					continue
				}
				return nil, false, err
			}
			// 校验通过并派单成功：连续失败计数清零。
			*validationErrs = 0
			return res, true, nil
		case frontToolPlanTask:
			var args PlanArgs
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				*msgHistory = append(*msgHistory, toolResultMsg(tc.ID, "plan_task 参数解析失败："+err.Error()+"；请按 plan_task 参数表重发（goal 与 steps 必填）。"))
				continue
			}
			if err := f.validatePlan(args); err != nil {
				// M2：plan_task 校验失败同样是 ErrDispatchValidation，
				// 与 dispatch 共用同一个连续失败计数。
				if errors.Is(err, ErrDispatchValidation) {
					*validationErrs++
					if *validationErrs > maxConsecutiveValidationErrors {
						log.Printf("front task=%s plan_task 连续 %d 次校验失败，停下不再重问模型: %v", taskID, *validationErrs, err)
						return f.stopOnValidationCap(artDir, taskID, validationErrs, err)
					}
				}
				*msgHistory = append(*msgHistory, toolResultMsg(tc.ID, "plan_task 校验失败："+err.Error()))
				continue
			}
			// 规划 v2：前置判回执的规划循环——校验通过后一步步派单、
			// 每步看回执判做完没有，好则下一步，不好叫重干（每步最多
			// 2 次），全做完大白话汇报用户。runPlan 已写好 reply.md
			// 并终结本单：直接返回，不再继续模型轮次。
			// （单次整单派单 planToAction 已废弃：它把全部步一次丢给
			// 执行层，前置看不到中间回执，判不了每一步。）
			res := f.runPlan(ctx, taskID, taskText, artDir, *memFragments, args, sessionID)
			return res, true, nil
		default:
			*msgHistory = append(*msgHistory, toolResultMsg(tc.ID, fmt.Sprintf("未知工具 %q：前置只有 recall、registry、read、bash、dispatch、plan_task 这 6 个工具；工具编号请走 registry 查，不要直接调。", tc.Function.Name)))
		}
	}
	return nil, false, nil
}

// toolResultMsg 造一条 tool 角色的结果消息（回填给模型）。
// 空内容保底填 "(empty output)"（M-4），见 llm.ToolMessage。
func toolResultMsg(toolCallID, content string) llm.Message {
	return llm.ToolMessage(toolCallID, content)
}

// writeFileAtomic 写文件：先写同目录临时文件再 os.Rename 原子替换，
// 避免看门狗或验收逻辑读到写了一半的 reply.md（P0 修复）。
// 临时文件失败时清理，不留半截文件。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".reply-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// reply 写直接回话的 reply.md 并返回（原子落盘，防读到半截文件）。
func (f *Front) reply(artDir, taskID, text, route string) (*FrontResult, error) {
	// 出站硬扫描（线 B 修复轮第 3 棒）：DIRECT 回话直接落盘 reply.md
	// 并作为 ReplyDraft 返回，落盘/回传前先过 ScrubOutbound。
	clean, n, blocked := sanitize.ScrubOutbound(text)
	if n > 0 || blocked {
		log.Printf("front task=%s 出站脱敏：掩码 %d 处，整封拦截=%v", taskID, n, blocked)
	}
	if err := writeFileAtomic(filepath.Join(artDir, "reply.md"), []byte(clean), 0o600); err != nil {
		return nil, fmt.Errorf("front: 写 reply.md 失败: %w", err)
	}
	return &FrontResult{ReplyDraft: clean, Status: "done", Route: route}, nil
}

// failReply 写一封失败回信并返回 FAILED 结果（不标 DONE）。
// 只写人话原因，不贴模型原话、不贴日志；reply.md 落盘失败时返回错误，
// 由网关兜底（ensureFailureReply）再写一封失败回信并标 FAILED。
func (f *Front) failReply(artDir, taskID, reason string) (*FrontResult, error) {
	text := fmt.Sprintf("任务未能完成。\n\n原因：%s\n时间：%s\n", reason, time.Now().UTC().Format(time.RFC3339))
	if err := writeFileAtomic(filepath.Join(artDir, "reply.md"), []byte(text), 0o600); err != nil {
		return nil, fmt.Errorf("front: 写失败回信失败: %w", err)
	}
	return &FrontResult{ReplyDraft: text, Status: "failed", Route: "ERROR"}, nil
}

// summarizer 返回翻新摘要的模型调用（P2-b）：用前置的 plain-text
// 模型调用，调 deepseek-flash（会话同一个模型，不换模型）。
// 优先用 CallRollover（M-5：main.go 接线为带 ctx 与 rollover 角色的
// 入口——透传 420 秒超时，费用不计入规划 ¥0.05 熔断、但仍记总账本）；
// 未接线时退回 f.Call（测试与旧调用方，ctx 不透传）。
// 两者都为空时返回 nil（MaybeRollover 走 tailDigest 降级）。
// 摘要调用的 token 不记水位（摘要发生在翻新期间，新会话从 0 开始）。
func (f *Front) summarizer() session.Summarizer {
	if f.CallRollover != nil {
		rollover := f.CallRollover
		return func(ctx context.Context, taskID, prompt string) (string, error) {
			return rollover(ctx, taskID, prompt)
		}
	}
	if f.Call == nil {
		return nil
	}
	call := f.Call
	return func(ctx context.Context, taskID, prompt string) (string, error) {
		reply, _, _, err := call(taskID, "", prompt)
		return reply, err
	}
}

// ErrTokenOverflow 是派单超限拒绝的哨兵错误（线 A）：核心指令本身
// 超过 MaxDispatchTokens，或可裁减部分清空后仍超限。调用方用
// errors.Is 识别，映射为网关终态 REJECTED（前置校验拒绝），
// 绝不能当成执行失败、更不能标 DONE。
var ErrTokenOverflow = errors.New("front: 派单超限拒绝")

// dispatch 校验并发送派单，派完等回执。回执状态即本单状态。
// 线 A：先算核心指令（用户原话+目标+验收+工具编号+约束，锁死不动），
// 核心本身超 3000 直接拒绝（FAILED_TOKEN_OVERFLOW）；其余部分超限
// 时按四级逐级裁减（去冗余→卡片 5→3→仅编号加标题→0 卡片+补搜指引），
// 每减一级重新计数，够了就停；最终 token 超 1200 记软告警事件。
func (f *Front) dispatch(ctx context.Context, artDir, taskID, taskText string, act Action, memFragments []string, sessionID string) (*FrontResult, error) {
	// 派单校验（工具编号白名单、task_type 二选一）：非法时返回包着
	// ErrDispatchValidation 的错误，原生路径回填给模型重试。
	taskType, err := f.validateDispatchAction(act)
	if err != nil {
		return nil, err
	}
	// 记忆碎片：派单里带的优先用模型给的；模型没给就用本单 recall 查到的。
	memory := act.Memory
	if len(memory) == 0 {
		memory = memFragments
	}
	estSteps := act.EstSteps
	if estSteps <= 0 {
		estSteps = 5
	}
	estSeconds := act.EstSeconds
	if estSeconds <= 0 {
		estSeconds = 300
	}
	d := &dispatch.Dispatch{
		ID: f.Ch.NewID(taskID), TaskID: taskID, Route: act.Route,
		UserText: taskText, Goal: act.Goal, Criteria: act.Criteria,
		ToolIDs: act.ToolIDs, PlanSkeleton: act.Plan, Memory: memory,
		MustReadMemories: act.MustReadMemories,
		Pointers:         act.Pointers, Constraints: act.Constraints,
		EstSteps: estSteps, EstSeconds: estSeconds,
		ShardNo: act.ShardNo, ResumeFrom: act.ResumeFrom,
		TaskType: taskType, ExpectedArtifacts: act.ExpectedArtifacts, VerifyCmd: act.VerifyCmd,
		// H-5：多步计划的步（PlanStepNo>0）是计划步——执行层草稿写
		// step-<n>-reply.md，不写顶层 reply.md；单步派单 PlanStepNo
		// 为 0，仍写顶层 reply.md。
		PlanStep: act.PlanStepNo > 0,
		// 方向 b / H-1 / H-2：本步的账本起点、首轮时间、重干原因，
		// 由 stepToAction 从计划步里带来，原样透传给派单；
		// 单步派单不填（零值），网关按严格当前窗口口径算。
		FirstExecIDBefore: act.FirstExecIDBefore,
		FirstStartTime:    act.FirstStartTime,
		LastRedoReason:    act.LastRedoReason,
		CreatedAt:         time.Now().UTC().Format(time.RFC3339),
		ReplyCh:           make(chan *dispatch.Receipt, 1),
	}
	// 核心指令超限直接拒绝：目标、验收标准、红线约束、工具编号
	// 加用户原话锁死不动，不裁核心、不硬塞、不静默失败。
	if n := coreTokens(d); n > dispatch.MaxDispatchTokens {
		f.recordDispatchEvent(taskID, d.ID, ledger.DispatchEventOverflowRejected, n,
			fmt.Sprintf("核心指令 %d token，超 %d 上限，拒绝派单", n, dispatch.MaxDispatchTokens))
		return nil, fmt.Errorf("%w：核心指令 %d token，超 %d 上限（目标/验收/约束/工具编号不动，不裁核心）",
			ErrTokenOverflow, n, dispatch.MaxDispatchTokens)
	}
	// 可裁减部分超限：四级逐级裁减，够了就停。
	if levels, before, after := trimDispatch(d); levels > 0 {
		f.recordDispatchEvent(taskID, d.ID, ledger.DispatchEventTrimmed, after,
			fmt.Sprintf("派单 %d token，裁减 %d 级后 %d token", before, levels, after))
	}
	// 补搜指引（2026-11-26 记忆召回整改）：所有模式都附，不只
	// 0 卡片时才附；有余额才附，超限自动撤回（tryAttachSearchHint
	// 内处理；裁减时已附过的不会重复附）。
	tryAttachSearchHint(d)
	n := dispatchTokens(d)
	if n > dispatch.MaxDispatchTokens {
		// 可裁减部分已清空仍超限（指针异常大等）：同样拒绝。
		f.recordDispatchEvent(taskID, d.ID, ledger.DispatchEventOverflowRejected, n,
			fmt.Sprintf("派单 %d token，超 %d 上限，可裁减部分已清空", n, dispatch.MaxDispatchTokens))
		return nil, fmt.Errorf("%w：派单 %d token，超 %d 上限（可裁减部分已清空）",
			ErrTokenOverflow, n, dispatch.MaxDispatchTokens)
	}
	// 软告警：超 1200 记日志并写账本事件，但不拦截，派单照常发送。
	if n > dispatch.SoftAlertTokens {
		log.Printf("front task=%s 派单 %s %d token，超软告警阈值 %d（不拦截）", taskID, d.ID, n, dispatch.SoftAlertTokens)
		f.recordDispatchEvent(taskID, d.ID, ledger.DispatchEventSoftAlert, n,
			fmt.Sprintf("派单 %d token，超软告警阈值 %d，已发送，未拦截", n, dispatch.SoftAlertTokens))
	}
	// P2-a：会话存盘——给本步在会话计划里占一位（步号 + 工具 + 验收），
	// 执行层按 (session, step) 存对话历史、断点续跑。分配失败只记日志，
	// 不阻塞派单（StepNo=0 表示本步不做会话存盘；执行层见 StepNo<=0
	// 即关闭本步历史存取，计划步状态也不更新）。
	// 规划 v2：PlanStepNo>0 表示这是计划里的第 N 步（前置已在计划表里
	// 占好位），直接用该步号，不再 AllocateStep 另占一位（否则重干
	// 同一步会重复占位，计划表被撑乱）。
	sessID := sessionID
	if sessID == "" {
		sessID = taskID
	}
	// P2-b：翻新检查（派单前）：水位 >200K 先翻新再开工，派单进新会话。
	// 翻新失败记日志、不阻塞派单。runPlan 循环里已先查过一次（那次会
	// 更新循环的 sessID 并重读计划）；这里是单步派单的兜底——runPlan
	// 路径到这儿水位已清零，本次检查是空操作。
	if f.sess != nil {
		if newID, rolled, rerr := f.sess.MaybeRollover(ctx, sessID, taskID, act.Goal, session.DefaultRolloverConfig(), f.summarizer()); rerr != nil {
			log.Printf("front task=%s 派单前翻新检查失败，继续用旧会话: %v", taskID, rerr)
		} else if rolled {
			log.Printf("front task=%s 派单前翻新：会话 %s → %s", taskID, sessID, newID)
			sessID = newID
		}
	}
	if act.PlanStepNo > 0 {
		if err := session.ValidateSessionID(sessID); err != nil {
			log.Printf("front task=%s session_id 非法，跳过会话存盘: %v", taskID, err)
		} else {
			d.SessionID = sessID
			d.StepNo = act.PlanStepNo
		}
	} else if err := session.ValidateSessionID(sessID); err != nil {
		log.Printf("front task=%s session_id 非法，跳过会话存盘: %v", taskID, err)
	} else if f.sess != nil {
		stepType := session.StepExecute
		if taskType == "research" {
			stepType = session.StepResearch
		}
		toolID := ""
		if len(act.ToolIDs) > 0 {
			toolID = act.ToolIDs[0]
		}
		if n, aerr := f.sess.AllocateStep(sessID, taskID, session.PlanStep{
			ToolID: toolID, StepType: stepType,
			ExpectedArtifacts: act.ExpectedArtifacts, VerifyCmd: act.VerifyCmd,
		}); aerr != nil {
			log.Printf("front task=%s 会话步分配失败，跳过会话存盘: %v", taskID, aerr)
		} else {
			d.SessionID = sessID
			d.StepNo = n
		}
	}
	if err := f.Ch.Send(ctx, d); err != nil {
		return nil, fmt.Errorf("front: 发送派单失败: %w", err)
	}
	log.Printf("front task=%s 派单 %s route=%s 工具=%v 预估 %d 步/%d 秒", taskID, d.ID, d.Route, d.ToolIDs, d.EstSteps, d.EstSeconds)
	// 派完等回执（同步流）：执行已把 reply.md 写好，前置只看回执状态。
	waitCtx, cancel := context.WithTimeout(ctx, ReceiptWaitTimeout)
	defer cancel()
	select {
	case r := <-d.ReplyCh:
		status := r.Status
		if status == "" {
			status = "failed"
		}
		return &FrontResult{Status: status, Route: act.Route, DispatchID: d.ID, ToolIDs: act.ToolIDs, Receipt: r}, nil
	case <-waitCtx.Done():
		log.Printf("front task=%s 等回执超时（%v），派单 %s 仍在执行，回执落盘后下次唤醒再汇报", taskID, ReceiptWaitTimeout, d.ID)
		return &FrontResult{Status: "stalled", Route: act.Route, DispatchID: d.ID, ToolIDs: act.ToolIDs}, nil
	}
}

// dispatchTokens 估算一份派单的 token 数（EstimateDispatchTokens 保守口径，
// 只算派单正文，不算信封字段）。
func dispatchTokens(d *dispatch.Dispatch) int {
	var b strings.Builder
	b.WriteString(d.UserText)
	b.WriteString(d.Goal)
	b.WriteString(d.Criteria)
	for _, s := range d.ToolIDs {
		b.WriteString(s)
	}
	for _, s := range d.PlanSkeleton {
		b.WriteString(s)
	}
	for _, s := range d.Memory {
		b.WriteString(s)
	}
	for _, s := range d.MustReadMemories {
		b.WriteString(s)
	}
	for _, s := range d.Pointers {
		b.WriteString(s)
	}
	for _, s := range d.Constraints {
		b.WriteString(s)
	}
	return registry.EstimateDispatchTokens(b.String())
}

// coreTokens 估算一份派单里锁死不动的核心指令 token 数：用户原话、目标、
// 验收标准、工具编号、红线约束。核心超限无法靠裁减解决，只能拒绝。
func coreTokens(d *dispatch.Dispatch) int {
	var b strings.Builder
	b.WriteString(d.UserText)
	b.WriteString(d.Goal)
	b.WriteString(d.Criteria)
	for _, s := range d.ToolIDs {
		b.WriteString(s)
	}
	for _, s := range d.Constraints {
		b.WriteString(s)
	}
	return registry.EstimateDispatchTokens(b.String())
}

// memorySearchHint 是附在步骤骨架里的只读补搜指引：模型开工后
// 如需更多记忆，自行用 search_memory 补查，拿到卡片后调 read_memory
// 读全文再动手。指引只在预算有余额时才附，绝不因指引本身导致超限拒单。
const memorySearchHint = "如需更多记忆，开工后用只读补搜工具 search_memory 自行补查（拿到卡片后调 read_memory 读全文）"

// tryAttachSearchHint 有余额才附补搜指引：附上后仍不超限就保留，
// 附上后超限就撤回。调用方须在调用前已把派单裁到当前级。
// 已附过时直接返回 true，不重复附。
func tryAttachSearchHint(d *dispatch.Dispatch) bool {
	for _, s := range d.PlanSkeleton {
		if s == memorySearchHint {
			return true
		}
	}
	d.PlanSkeleton = append(d.PlanSkeleton, memorySearchHint)
	if dispatchTokens(d) <= dispatch.MaxDispatchTokens {
		return true
	}
	d.PlanSkeleton = d.PlanSkeleton[:len(d.PlanSkeleton)-1]
	return false
}

// trimDispatch 对一份超限派单做四级逐级裁减（线 A，v2.1 §8），每减一级
// 重新计数，够了就停。核心字段（用户原话/目标/验收/工具编号/约束）
// 绝不动。返回裁减级数与裁减前后 token 数；未超限返回 0 级。
// 第一级删历史对话冗余（去重：空条目、与核心指令重复的抄写、
// 同一列表内的重复条目）；第二级卡片 5 张减到 3 张；第三级卡片缩成
// 仅编号加标题（有余额时附补搜指引）；第四级 0 卡片（纯 0 卡片，
// 仅在有余额时附补搜指引；纯核心本身 ≤3000 时绝不因指引超限而拒单）。
func trimDispatch(d *dispatch.Dispatch) (levels, before, after int) {
	before = dispatchTokens(d)
	if before <= dispatch.MaxDispatchTokens {
		return 0, before, before
	}
	trimLevel1(d)
	if after = dispatchTokens(d); after <= dispatch.MaxDispatchTokens {
		return 1, before, after
	}
	// 第二级：卡片只留前 3 张。Memory 入队时已按召回相关度降序排列
	// （召回侧按分数降序返回；模型自带的碎片保持模型给出的顺序），
	// 因此直接截前 3 张即保留最相关的 3 张，不再重排。
	if len(d.Memory) > 3 {
		d.Memory = d.Memory[:3]
	}
	if after = dispatchTokens(d); after <= dispatch.MaxDispatchTokens {
		return 2, before, after
	}
	for i, m := range d.Memory {
		d.Memory[i] = cardTitleOnly(m)
	}
	if after = dispatchTokens(d); after <= dispatch.MaxDispatchTokens {
		// 第三级已够：有余额就附补搜指引（P0 修复：指引前移到第 3 级）。
		tryAttachSearchHint(d)
		after = dispatchTokens(d)
		return 3, before, after
	}
	// 第四级：0 卡片。纯核心本身 ≤3000 时绝不拒单（P0 修复的边界保护）：
	// 先算纯核心，够就放行；指引只在有余额时才附，附上超限就撤回。
	d.Memory = nil
	after = dispatchTokens(d)
	if after <= dispatch.MaxDispatchTokens {
		tryAttachSearchHint(d)
		after = dispatchTokens(d)
	}
	return 4, before, after
}

// trimLevel1 删历史对话冗余：删空条目；删与核心指令重复的抄写
// （模型把目标/验收又抄进步骤或碎片）；删同一列表内的重复条目。
func trimLevel1(d *dispatch.Dispatch) {
	core := d.UserText + "\n" + d.Goal + "\n" + d.Criteria
	d.PlanSkeleton = dedupeStrings(d.PlanSkeleton, core)
	d.Memory = dedupeStrings(d.Memory, core)
	d.Pointers = dedupeStrings(d.Pointers, core)
}

// dedupeStrings 去重：空条目、重复条目、整条原样出现在核心文本
// 里的（重复抄写）都删掉。判定是严格的整条原文包含匹配，
// 不是关键词模糊匹配：只共享关键词、但带自己结论的长条目不会被误删；
// 短条目（<10 字）不做包含判断，防误杀。
func dedupeStrings(in []string, core string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		t := strings.TrimSpace(s)
		if t == "" {
			continue
		}
		if seen[t] {
			continue
		}
		seen[t] = true
		if len([]rune(t)) >= 10 && strings.Contains(core, t) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// cardTitleOnly 把一张记忆卡片缩成“仅编号加标题”：保留开头的【来源】标记；
// 没有标记的取前 12 字作标题 stub。
func cardTitleOnly(s string) string {
	if strings.HasPrefix(s, "【") {
		if i := strings.Index(s, "】"); i > 0 {
			return s[:i+len("】")]
		}
	}
	r := []rune(s)
	if len(r) > 12 {
		return string(r[:12]) + "…"
	}
	return s
}

// recordDispatchEvent 写一行派单预算事件（软告警/裁减/超限拒绝）。
// Led 为空时只记日志（测试用）；写账本失败只记日志，不阻塞派单。
func (f *Front) recordDispatchEvent(taskID, dispatchID, kind string, dispatchEstTokens int, detail string) {
	if f.Led == nil {
		log.Printf("front task=%s dispatch event %s dispatch_est_tokens=%d: %s", taskID, kind, dispatchEstTokens, detail)
		return
	}
	if _, err := f.Led.RecordDispatchEvent(ledger.DispatchEvent{
		SessionID: f.SessionID, TaskID: taskID, DispatchID: dispatchID,
		Kind: kind, DispatchEstTokens: dispatchEstTokens, Detail: detail,
	}); err != nil {
		log.Printf("front task=%s dispatch event %s 写账本失败: %v", taskID, kind, err)
	}
}

// buildTail 拼前置每轮的尾巴：用户原话 + 记忆碎片（查到才带）+ 动作结果摘要。
func buildTail(taskText string, memFragments, history []string) string {
	var b strings.Builder
	b.WriteString("用户原话：\n" + taskText + "\n")
	if len(memFragments) > 0 {
		b.WriteString("相关记忆摘要（供参考，不在其中不要编造）：\n")
		for _, m := range memFragments {
			b.WriteString("- " + m + "\n")
		}
	}
	if len(history) > 0 {
		b.WriteString("已做步骤：\n")
		for i, h := range history {
			fmt.Fprintf(&b, "%d. %s\n", i+1, h)
		}
	}
	return b.String()
}

// doRecall 查记忆 1 次，返回前 3 条碎片（“文本”，文本已截断 ≤240 字）。
// 无命中返回空片（不报错）；召回不可用才用文本搜兜底——文本搜是
// memory.Store 的一部分，Recall 内部已处理，这里只调 Recall。
//
// 线 B 第二阶段第 3 棒：接了简介主库且 TwoStageRecallEnabled 时，
// AGENT 路径开工前这一次召回走新两阶段召回（0～5 张卡片，
// 每张只含标题、一句话简介和编号）；新路径出错时回落到旧 Recall。
// 脱敏口径（用户 2026-11-24 09:12 纠正）：交付给模型前绝不掩码
// （旧节点 4 已取消），模型照常看、照常用密钥；记账在 RecordRecall
// 里脱敏（节点 3），与 memorySummary 的记账口径一致。
func (f *Front) doRecall(taskID, query string) ([]string, error) {
	if f.Mem == nil {
		return nil, fmt.Errorf("记忆未接线")
	}
	start := time.Now()
	if memory.TwoStageRecallEnabled && f.Mem.HasSummaryStore() {
		res, err := f.Mem.RecallTwoStage(query)
		if err == nil {
			out := make([]string, 0, len(res.Cards))
			delivered := make([]string, 0, len(res.Cards))
			tokensEst := 0
			for _, c := range res.Cards {
				out = append(out, c.Display())
				delivered = append(delivered, c.SourcePath)
				tokensEst += c.Tokens()
			}
			candStrs := make([]string, 0, len(res.Pool))
			for _, c := range res.Pool {
				candStrs = append(candStrs, fmt.Sprintf("%s=%d", c.SourcePath, c.PoolRank))
			}
			f.recordRecall(taskID, query, candStrs, delivered, tokensEst, time.Since(start))
			// 置信度日志（2026-11-24 调参）：recall_log 加列需正规迁移，
			// 本轮只做到结果结构返回 + 日志打印，见 TwoStageResult。
			log.Printf("front doRecall 两阶段召回：confidence=%s top1_sem=%.2f top1_bm25=%.1f rejected=%d delivered=%d tokens=%d",
				res.ConfidenceBand, res.Top1SemCos, res.Top1BM25, res.RejectedCount, len(res.Cards), tokensEst)
			return out, nil
		}
		log.Printf("front doRecall 两阶段召回失败，回落旧 Recall: %v", err)
	}
	rs, err := f.Mem.Recall(query)
	if err != nil {
		return nil, fmt.Errorf("查记忆失败: %w", err)
	}
	var out []string
	delivered := make([]string, 0, len(rs))
	tokensEst := 0
	for _, r := range rs {
		out = append(out, fmt.Sprintf("【%s】%s", r.Source, r.Text))
		delivered = append(delivered, r.Source)
		tokensEst += registry.EstimateTokens(r.Source + r.Text)
	}
	// 回落路径记账：候选取前 40（与 memorySummary 旧路径一致）；
	// 再跑一次 RecallCandidates 才能拿到完整候选表（Recall 只回前 3 条）。
	if cands, cerr := f.Mem.RecallCandidates(query, 40); cerr == nil {
		candStrs := make([]string, 0, len(cands))
		for _, r := range cands {
			candStrs = append(candStrs, fmt.Sprintf("%s=%d", r.Source, r.Score))
		}
		f.recordRecall(taskID, query, candStrs, delivered, tokensEst, time.Since(start))
	} else {
		log.Printf("front doRecall 回落记账取候选失败（不阻塞）: %v", cerr)
	}
	return out, nil
}

// recordRecall 写一行召回账（节点 3：RecordRecall 内脱敏；
// Led 为空时跳过；失败只记日志，不阻塞前置）。
func (f *Front) recordRecall(taskID, query string, candidates, delivered []string, tokensEst int, latency time.Duration) {
	if f.Led == nil {
		return
	}
	if _, err := f.Led.RecordRecall(ledger.RecallRecord{
		SessionID: f.SessionID, TaskID: taskID,
		Query: query, Candidates: candidates,
		Delivered: delivered, TokensEst: int64(tokensEst),
		LatencyMs: latency.Milliseconds(),
	}); err != nil {
		log.Printf("front doRecall recall_log 记账失败（不阻塞）: %v", err)
	}
}

// doRegistry 查工具单：L1 返回 9 个大类（首句摘要）；L2 返回某大类的
// 子类与工具编号（一句话说明，不读 L3 全文）。默认隐藏/不可用/
// 探针失败的工具不列。
func (f *Front) doRegistry(level, category string) (string, error) {
	if level == "L1" {
		var b strings.Builder
		b.WriteString("工具大类（9 个，先挑一个看 L2）：\n")
		for _, c := range f.Reg.CategoryMetas() {
			desc := registry.FirstSentence(c.Description)
			if desc == "" {
				desc = c.Name
			}
			fmt.Fprintf(&b, "- %s: %s\n", c.ID, desc)
		}
		return b.String(), nil
	}
	desc := f.Reg.CategoryDescription(category)
	if desc == "" {
		return "", fmt.Errorf("未知大类 %q", category)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "大类 %s 说明：\n%s\n子类与工具（挑工具编号写进派单）：\n", category, desc)
	for _, s := range f.Reg.SubcategoriesOf(category) {
		d := registry.FirstSentence(s.Description)
		if d == "" {
			d = s.ID
		}
		fmt.Fprintf(&b, "子类 %s: %s\n", s.ID, d)
		for _, e := range f.Reg.EntriesByCategory(category) {
			if e.Subcategory != s.ID {
				continue
			}
			if e.Hidden || !e.Available || !e.Executable() || f.Reg.ProbeFailed(e.ToolID) {
				continue
			}
			fmt.Fprintf(&b, " - %s: %s\n", e.ToolID, e.OneLiner)
		}
	}
	return b.String(), nil
}

// doRead 读文件走 ctxread 压缩读（2026-11-24，E-12 明令）：
// R0/R1 小文件全文；超 8KB 给结构/签名，24KB 封顶；二进制、设备文件、
// 不存在的文件由 ctxread 拒绝。相对路径仍拒绝（只读绝对路径）。
func (f *Front) doRead(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("只读绝对路径：%q", path)
	}
	res, err := ctxread.Read(path, ctxread.ModeAuto)
	if err != nil {
		return "", fmt.Errorf("读文件失败: %w", err)
	}
	return res.Text, nil
}

// readOnlyCommands 是只读 bash 允许的命令（第一个词）。只用来看和查。
var readOnlyCommands = map[string]bool{
	"ls": true, "cat": true, "grep": true, "wc": true, "head": true,
	"tail": true, "df": true, "du": true, "ps": true, "pwd": true,
	"free": true, "uptime": true, "hostname": true, "whoami": true,
	"lsblk": true, "mount": true, "find": true, "stat": true,
	"file": true, "sort": true, "uniq": true, "cut": true,
	"systemctl": true, "git": true,
}

// deniedTokens 是只读 bash 里一律拒绝的词（写文件/改状态/执行任意命令的
// 开关）。与危险字符拦截互补：sed -i 这类不含 shell 元字符的写操作
// 也能拦住。
var deniedTokens = []string{
	"-i", "--in-place", "-exec", "-delete", "-ok", "-okdir",
	">", ">>", "<", "|", "&", ";", "$", "`", "(", ")", "{", "}",
	"*", "?", "~", "#", "!", "\n", "\r",
}

// doBash 跑一条只读命令（F-11）：白名单命令 + 危险字符/参数双重拦截，
// 超时 30 秒，输出各截到 2000 字。每次调用记审计日志（只记命令与
// 退出码，不记输出内容，防密钥进日志 F-22）。
func (f *Front) doBash(ctx context.Context, command string) (string, error) {
	cmd := strings.TrimSpace(command)
	if cmd == "" {
		return "", fmt.Errorf("命令为空")
	}
	for _, tok := range deniedTokens {
		if strings.Contains(cmd, tok) {
			return "", fmt.Errorf("只读 bash 拒绝含 %q 的命令：%s", tok, cmd)
		}
	}
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return "", fmt.Errorf("命令为空")
	}
	verb := fields[0]
	if !readOnlyCommands[verb] {
		return "", fmt.Errorf("只读 bash 只许 %v 这类查看命令，不许 %q", readOnlyCommandNames(), verb)
	}
	// git 与 systemctl 只许读状态的子命令。
	if verb == "git" {
		if len(fields) < 2 || !map[string]bool{
			"status": true, "log": true, "diff": true, "branch": true,
			"remote": true, "show": true, "ls-files": true,
		}[fields[1]] {
			return "", fmt.Errorf("只读 bash 里 git 只许 status/log/diff/branch/remote/show/ls-files")
		}
	}
	if verb == "systemctl" {
		if len(fields) < 2 || fields[1] != "status" {
			return "", fmt.Errorf("只读 bash 里 systemctl 只许 status")
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c := exec.CommandContext(runCtx, fields[0], fields[1:]...)
	out, err := c.Output()
	exitCode := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
			out = append(out, ee.Stderr...)
		} else {
			log.Printf("front bash 审计：命令=%q 退出码=-1（启动失败）", cmd)
			return "", fmt.Errorf("只读命令启动失败: %w", err)
		}
	}
	// 审计日志：只记命令与退出码，不记输出内容（输出可能含密钥）。
	log.Printf("front bash 审计：命令=%q 退出码=%d 输出 %d 字节", cmd, exitCode, len(out))
	if exitCode != 0 {
		return "", fmt.Errorf("只读命令退出码 %d", exitCode)
	}
	text := string(out)
	if len([]rune(text)) > 2000 {
		text = string([]rune(text)[:2000]) + "\n……（输出已截到 2000 字；要看全文用 read（ctxread）分段读，别用 cat 整篇倒）"
	}
	return text, nil
}

// readOnlyCommandNames 返回白名单命令名（报错提示用）。
func readOnlyCommandNames() []string {
	names := make([]string, 0, len(readOnlyCommands))
	for n := range readOnlyCommands {
		names = append(names, n)
	}
	return names
}
