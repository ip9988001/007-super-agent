// Package agent 是 007 的主循环：对齐二号的 Step 1 + Step 4。
//
// 二号没有规划器、没有核验器：主模型在正常推理里先判任务类型，
// 要动手/要查历史就先 recall，再一步一步调工具，看到结果再决定
// 下一步。这里就是同一套做法——
//   - 不做事前整张计划（规划器已摘），不做事前核验（核验器已摘）；
//   - 每轮只让模型决定“下一步调哪个工具 / 还是直接交活”，
//     调完把结果喂回去，再决定下一步；
//   - 记忆摘要（Recall 前 3 条）每轮都带上，工具清单只列可执行条目；
//   - 代码层兜底不变：工具可调用性（ResolveTool）、参数拼装
//     （BuildArgv）、产物校验（exit 0 但无输出算失败），
//     不可逆动作不再过监工（监工已下线；没主人明确指令的先停先报）。
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"007-super-agent/internal/executor"
	"007-super-agent/internal/forge"
	"007-super-agent/internal/ledger"
	"007-super-agent/internal/llm"
	"007-super-agent/internal/memory"
	"007-super-agent/internal/registry"
	"007-super-agent/internal/sanitize"
	"007-super-agent/internal/session"
	"007-super-agent/internal/triage"
)

// ModelCallFunc 与 llm.Client.CallTask 同签名：
// 冻结提示词（A 区，可命中缓存）+ 当轮尾巴，返回模型原话与 token 数。
type ModelCallFunc func(taskID, frozen, tail string) (reply string, tokensIn, tokensOut int64, err error)

// ModelCallMetaFunc 与 llm.Client.CallTaskWithMeta 同签名：
// 额外返回 finish_reason（P0-1：length 表示输出被截断，整轮作废）。
type ModelCallMetaFunc func(taskID, frozen, tail string) (reply, finishReason string, tokensIn, tokensOut int64, err error)

// ModelCallToolsFunc 与 llm.Client.CallTaskFull 同签名（P1）：
// 原生工具调用——tools 声明给模型，history 是结构化对话历史，
// 返回整条 assistant 消息（正文 + tool_calls）与 finish_reason。
// 模型不调工具只回正文时，ToolCalls 为空，正文就是它要停下交的答案。
type ModelCallToolsFunc func(taskID, frozen, tail string, tools []llm.ToolDefinition, history []llm.Message) (llm.Message, string, int64, int64, error)

// MaxConsecutiveFailures 是“卡住”判定：连续多少轮工具调用失败或无产出，
// 就认定模型卡住了、停下来综合已有成果。不是步数上限——
// 能产出成果的任务想跑多少轮就跑多少轮，只有卡住才停。
// MaxIter 是最后防线（防模型永不交活的极端死循环），设得很高，
// 正常任务碰不到它；碰到了说明模型出了问题，停下来上报。
// DefaultTimeBudget 是单任务时间预算（资源护栏，不是能力上限）：
// 复杂调研 5-10 分钟够用；900 秒给足余量，到点就停下来综合手头成果，
// 不让一个开放式任务无限烧下去。正常任务 1-3 分钟就完，碰不到它。
const MaxConsecutiveFailures = 5
const DefaultMaxIter = 200
const DefaultTimeBudget = 900 * time.Second

// MaxNoProgressRounds 是执行动态刹车：连续多少轮没有新产出或状态变化，
// 就停下来交短回执，不许硬撑。比“连续失败”更严：成功但输出与上一轮
// 一模一样（没拿到新东西）也算无进展。
const MaxNoProgressRounds = 3

// MaxBatchActions 是一轮最多打包的工具调用数（互不依赖的才打包）。
const MaxBatchActions = 3

// Agent 是主循环：一次 Run 跑完一个多步任务。
type Agent struct {
	Call         ModelCallFunc     // 只用 flash（worker 模型），不用 reasoner
	CallWithMeta ModelCallMetaFunc // 可空：返回 finish_reason；为空时用 Call（finish_reason 为空）
	// CallWithTools 是 P1 原生工具调用（可空）：非空时 Run 走原生路径
	// （工具声明给模型、模型回 tool_calls、结果原样回填）；为空时走旧的
	// 文本 JSON 路径（callModel + parseActions，测试与旧调用方用）。
	// 生产 main.go 一定接线 CallWithTools，原生路径是唯一生产路径。
	CallWithTools ModelCallToolsFunc
	// ToolIDs 是本单派单建议的工具编号（P1）：runAgent 从派单设置，
	// 用完恢复空值，与 TaskType/StepCap/ShardNo 同一模式。Run 开始时
	// 按它向模型声明工具（只声明这几把 + expand + 记忆工具），
	// 模型不用先展开导航就能直接调；派单没给工具时只声明 expand 与记忆工具，
	// 模型靠展开找工具（回退路径）。
	ToolIDs []string
	// MustReadMemories 是本单派单点名的必读记忆编号（2026-11-26 记忆召回整改）：
	// 前置查到方法/剧本类记忆后写进派单（最多 2 个，只写编号不贴全文）；
	// Run 开工时由代码直接读全文拼进首轮尾巴，不靠模型自己调 read_memory。
	// runAgent 从派单设置，用完恢复空值，与 TaskType/StepCap/ShardNo 同一模式。
	MustReadMemories []string
	// TaskType 是本单任务类型（P0-3）：execute（默认，动手产出）/
	// research（只调研，交付报告）。runAgent 从派单设置，用完恢复空值，
	// 与 StepCap/ShardNo 同一模式；空值按 execute。
	TaskType   string
	Reg        *registry.Registry
	Led        *ledger.Ledger
	Ex         *executor.Executor
	DataDir    string // 产物落盘根目录（artifacts/<taskID>/）
	SessionID  string
	Forge      *forge.Store  // 第 23 步缺口检测：nil 时关闭
	MaxIter    int           // 0 时用 DefaultMaxIter（最后防线，不是能力上限）
	TimeBudget time.Duration // 0 时用 DefaultTimeBudget（时间护栏）
	// StepCap 是派单步数上限（预估×2，最少 10 步、最多 50 步）；0 时
	// 用 MaxIter。StepCap 再小也不能突破 900 秒 / 200 步最后外墙。
	StepCap int
	// ShardNo 是分片号（第 15 项：0 表示不分片；>0 表示本 Run 是第几片）。
	// 每片是一个独立 Run，各有自己的 900 秒预算；Run 结束时按片号落盘
	// Stage 断点，下一片凭 LastStage 续跑。由 runAgent 按派单设置，
	// 跑完恢复 0（执行是串行的，不影响下一单）。
	ShardNo int
	// Mem 是记忆 Store（只读补搜工具 search_memory/read_memory 用；
	// 为空时两个工具返回“记忆未接线”）。main.go 经 WithMemoryStore
	// 接线；测试直接赋值。记忆摘要仍走 Run 的 memSummary 参数进首轮，
	// 与这两个工具互不干扰。
	Mem *memory.Store
	// memBudget 是本任务的补搜计数（每任务 search/read 各 3 次）：
	// Run 开始时新建（reset 点），任务结束即丢弃，新任务清零。
	// 网关串行 + 派单 worker 单线程，同一时刻只有一个任务在跑；
	// 计数本身在 memory.SearchBudget 内有锁。
	memBudget *memory.SearchBudget
	// PromptHead 是执行宪法文本：New() 时从 data/prompts/executor-AGENTS.md
	// 加载；为空时 Run 用内置 agentPromptHead 兜底（测试与旧调用方）。
	PromptHead string
	// Sess 是会话存盘入口（P2-a）：main.go 接线（d.sess）；为空时
	// 不做会话存盘（测试可不接线）。只存本步的对话历史与步状态，
	// 不做压缩、不判回执。
	Sess *session.Store
	// RunSessionID / RunStepNo 是本 Run 的会话归属（P2-a）：runAgent
	// 从派单设置（SessionID/StepNo），用完恢复空值/0，与
	// TaskType/StepCap/ShardNo 同一模式。RunSessionID 为空或
	// RunStepNo<=0 时本 Run 不做会话存盘（历史不读不写）。
	RunSessionID string
	RunStepNo    int
	// RunRedoReason 是本 Run 的重干原因（H-1）：runAgent 每 Run 临场
	// 从派单的 LastRedoReason 设置，用完恢复空值；为空表示不是重干。
	// 断点续跑首轮靠它把“上次错在哪”告诉模型，否则模型只看旧历史，
	// 根本不知道自己被打回了。
	RunRedoReason string
	// runResumed 是本 Run 是否读回了历史（H-1/方向 b）：Run 内在
	// LoadMessages 后设置，finishWithAnswer 靠它给 RunResult 打标。
	runResumed bool
	// RunPlanStep 是本 Run 是不是多步计划里的一步（H-5）：runAgent
	// 从派单的 PlanStep 字段设置。计划步的草稿写 step-<n>-reply.md，
	// 不写顶层 reply.md；单步派单仍写顶层 reply.md。
	RunPlanStep bool
}

// replyFileName 是本 Run 的草稿文件名（H-5）：计划步写
// step-<n>-reply.md（单步草稿，不算整单完成），其余写 reply.md。
// 看门狗只认顶层 reply.md——计划步的草稿不许出现在顶层，否则
// 后续步卡住/崩溃后看门狗见新鲜 reply.md 会误把整单标 DONE。
func (a *Agent) replyFileName() string {
	if a.RunPlanStep && a.RunStepNo > 0 {
		return fmt.Sprintf("step-%d-reply.md", a.RunStepNo)
	}
	return "reply.md"
}

// Action 是模型每轮的决定：调一个工具，或直接交活。
type Action struct {
	Action      string   `json:"action"` // "tool"、"answer" 或 "expand"（三层懒加载导航）
	ToolID      string   `json:"tool_id,omitempty"`
	Inputs      []string `json:"inputs,omitempty"`
	Text        string   `json:"text,omitempty"`
	Category    string   `json:"category,omitempty"`    // expand 用：一级大类
	Subcategory string   `json:"subcategory,omitempty"` // expand 用：二级子类
	// ToolCallID 是 P1 原生路径的调用编号：模型 tool_calls 里的 id，
	// 执行完要按它回填一条 tool 消息；文本路径为空。
	ToolCallID string `json:"-"`
}

// stepRecord 是一轮工具调用及其结果，喂给下一轮做决定。
type stepRecord struct {
	ToolID   string
	Inputs   []string
	ExitCode int
	Status   string
	Output   string // stdout 末尾（截断后），供模型判断
	NoOutput bool   // exit 0 但无任何输出：算失败，模型要换招
}

// RunResult 是 Run 的交活：最终回复文本 + 任务状态。
// Status 取值：stopped（模型停下，待网关验货；执行会话自己不标 done）、
// max_iterations / stalled / agent_error。done 只由 runAgent 的网关验货
// 通过后才给（见 cmd/super-agent/main.go verifyDelivery）。
type RunResult struct {
	ReplyDraft     string
	Status         string // stopped / max_iterations / agent_error / stalled
	CompletedSteps int
	ShardNo        int // 本片号（第 15 项：0 表示不分片）
	// Resumed 是本次执行是否触发了断点续跑（方向 b）：读回了本步历史
	// 为真；网关靠它决定硬底线①的续跑豁免与 PriorStepCalls 统计。
	Resumed bool
}

// agentPromptHead 是主循环的冻结提示词（放 A 区，内容固定才能命中缓存）。
// 对齐二号 Step 1（判任务 + 按需检索，记忆摘要已在尾巴里）与 Step 4（执行）。
const agentPromptHead = `你是 007 超级 Agent。按二号的办法做事：先看任务，再一步一步调工具，看到结果再决定下一步。不要一次规划全部步骤。
只输出一个 JSON 对象，不要任何解释、不要 markdown 代码围栏。对象格式三选一：
{"action":"expand","category":"fetch"}
{"action":"expand","category":"fetch","subcategory":"browser"}
{"action":"tool","tool_id":"cap-fetch-curl-jq","inputs":["-sS","-L","https://example.com"]}
{"action":"answer","text":"……（任务的最终回复，直接写给用户）"}
工具是三层懒加载，不是一次全摊开：
- 第一级（下面只列 9 个大类）：先定大类。不知道用哪个大类，就挑最可能的一个展开看看。
- 第二级：expand 某大类后，看到它下面的子类（每个一句话说明）与该大类的完整说明，再挑一个子类展开。
- 第三级：expand 某子类后，看到它下面的工具（每个有完整英文描述），再挑一个工具调。
- 展开（expand）只是导航，不算干活、不花钱；调工具（tool）才真正干活。展开后看到的列表只在下一轮出现一次，接着挑就行；之后“已跑步骤”里只留一行摘要（带工具 ID），忘了描述可以重新展开。
- 已经知道 tool_id 时不用展开，直接调工具。
规则：
- 记忆优先：如果下面“相关记忆摘要”里已经有这道题的答案或关键结论（比如 wiki 里有现成的逆向方案、踩过的坑），优先用它，只补必要的验证，不要从零重新推导。
- 分析/调研类任务：先调查，再把结论写成一个交付文件（用写文件工具或 python 写到工作目录的 report.md），最后用 action=answer 给出摘要并说明文件在哪。不要只在心里分析、只在对话里说，必须落盘成文，否则等于零交付；
- 任务做完了就 action=answer，把结果写进 text，不要再调工具（answer 只是请求停下，完成与否由网关验产物决定，不是你自己说了算；没产出就不要交活）；不确定做没做完就再调一个工具确认，不要猜；
- 一步只调一个工具；tool_id 只能从展开后看到的工具清单里挑，清单外的不许编；不知道调哪个就先展开最可能的大类看看，不要猜 tool_id；
- 内置只读记忆工具不用展开、直接调（进程内执行）：search_memory（inputs ["查询词"]，可加 mode lexical/semantic/hybrid 与 limit 1–5，返回最多 5 张简介卡片）和 read_memory（inputs ["编号"]，按编号读一篇全文不截断），每个任务各最多 3 次；语义腿当前停用，hybrid 自动退化为词法+全文 BM25；编号用 search_memory 返回的 doc_id，没有编号不要猜。
- inputs 的每个字符串就是命令行上的一个参数（argv 的一个元素），执行时直接拼在工具名后面、不经过 shell：不要写 shell 语法——不要写管道 |、重定向 > >>、分号 ;、&&、$变量、for 循环、反引号，也不要在 inputs 里再写一遍工具名；需要落盘文件时，把绝对路径当参数传给工具（比如 --out /绝对路径/文件），不要写重定向；路径直接写绝对路径，不要用 $变量缩写；
- 不确定的命令先跑 --help 确认用法，不要编参数（比如 playwright screenshot 没有 --out 这个选项，编了会直接报错）；
- 抓网页 HTML 时要带全套浏览器请求头（Accept、Accept-Language、Sec-Fetch-* 等），只带 User-Agent 常被回 40 字节空壳或验证页；拿到验证页（Cloudflare Turnstile、验证码、challenge）或空壳时，换反检测工具再抓——camoufox（反检测浏览器）或 scrapling 的 stealthy_fetch（TLS 指纹伪装），不要在空壳上反复试普通抓取；
- 工具返回失败（exit 非 0、无输出、参数报错）时，先看错误信息换个招再试，不要原样重发同一条命令；同一工具连续失败两次就换别的工具；同一条命令失败了就不要再发一模一样的；

`

// New 建一个主循环。调用方负责把 Call 接到真实模型
// （生产：llm.New(cfg.WorkerModel, key).CallTask，只用 flash）。
// PromptHead 从 data/prompts/executor-AGENTS.md 加载；文件缺失时
// 留空，Run 用内置 agentPromptHead 兜底。
func New(call ModelCallFunc, reg *registry.Registry, led *ledger.Ledger, ex *executor.Executor, dataDir, sessionID string) *Agent {
	return &Agent{
		Call: call, Reg: reg, Led: led, Ex: ex,
		DataDir: dataDir, SessionID: sessionID,
		MaxIter:    DefaultMaxIter,
		PromptHead: loadExecutorPrompt(dataDir),
	}
}

// callModel 调一次模型：优先用 CallWithMeta（带 finish_reason），
// 否则用 Call（finish_reason 为空，不触发截断拦截；测试与旧调用方走这条）。
func (a *Agent) callModel(taskID, frozen, tail string) (reply, finishReason string, err error) {
	if a.CallWithMeta != nil {
		r, fr, _, _, err := a.CallWithMeta(taskID, frozen, tail)
		return r, fr, err
	}
	r, _, _, err := a.Call(taskID, frozen, tail)
	return r, "", err
}

// finishWithAnswer 是模型请求停下时的收尾（P0-2，文本 answer 与原生
// 无工具调用共用）：answer 不是完成，执行会话不给自己盖完成章，
// 只把草稿落盘（计划步写 step-<n>-reply.md，其余写 reply.md，
// 见 replyFileName）并返回 stopped；完成与否由 runAgent 的
// 网关验货决定（验产物/验账本），验过才标 done。
// 调研任务光一句短答不算交付：没有 report.md 就先综合一份。
func (a *Agent) finishWithAnswer(ctx context.Context, taskID, taskText, memSummary string, history []stepRecord, artDir, text string, completed int) (*RunResult, error) {
	// 调查类任务：光一句短答不算交付，没有 report.md 就先综合一份。
	if a.needsReport(taskID, history) {
		report, serr := a.synthesize(ctx, taskID, taskText, memSummary, history, artDir)
		if serr != nil {
			log.Printf("agent task=%s synthesize: %v", taskID, serr)
		} else {
			a.detectGapsAtEnd(taskID, taskText, history, true)
			a.checkpointShard(taskID, artDir)
			return &RunResult{ReplyDraft: report, Status: "stopped", CompletedSteps: completed, ShardNo: a.ShardNo, Resumed: a.runResumed}, nil
		}
	}
	// 出站硬扫描：answer 原文直接落盘（计划步写 step-<n>-reply.md，
	// 其余写 reply.md，见 replyFileName）并作为 ReplyDraft 回传。
	text, n, blocked := sanitize.ScrubOutbound(text)
	if n > 0 || blocked {
		log.Printf("agent task=%s 出站脱敏：掩码 %d 处，整封拦截=%v", taskID, n, blocked)
	}
	if err := os.WriteFile(filepath.Join(artDir, a.replyFileName()), []byte(text), 0o600); err != nil {
		return nil, fmt.Errorf("agent: 写 %s 失败: %w", a.replyFileName(), err)
	}
	a.detectGapsAtEnd(taskID, taskText, history, true)
	a.checkpointShard(taskID, artDir)
	return &RunResult{ReplyDraft: text, Status: "stopped", CompletedSteps: completed, ShardNo: a.ShardNo, Resumed: a.runResumed}, nil
}

// loadExecutorPrompt 读执行宪法文件（定稿：讨论六 + 讨论七修订）。
// 文件缺失返回空串，调用方用内置 agentPromptHead 兜底并记日志。
func loadExecutorPrompt(dataDir string) string {
	p := filepath.Join(dataDir, "prompts", "executor-AGENTS.md")
	b, err := os.ReadFile(p)
	if err != nil {
		log.Printf("agent: 读执行宪法 %s 失败，用内置提示词兜底: %v", p, err)
		return ""
	}
	return string(b)
}

// WithMemoryStore 接入记忆 Store（只读补搜工具用），链式调用。
func (a *Agent) WithMemoryStore(s *memory.Store) *Agent {
	a.Mem = s
	return a
}

// injectMustReadMemories 把必读记忆的全文读出来拼进记忆摘要
// （2026-11-26 记忆召回整改）：前置在派单 must_read_memories 里
// 点名的记忆编号（最多 2 个），执行开工时由代码直接调 ReadMemory
// 读全文，不靠模型自己调工具。读到的全文拼在摘要后面进首轮尾巴；
// 读不到（无编号/无记忆库/超限）只记日志，不阻塞任务。
// 每次读都记进 budget（与模型自己调 read_memory 同一个 3 次上限）。
func injectMustReadMemories(mem *memory.Store, budget *memory.SearchBudget, ids []string, summary string) string {
	if len(ids) == 0 || mem == nil {
		return summary
	}
	var b strings.Builder
	b.WriteString(summary)
	for i, id := range ids {
		if i >= 2 {
			break
		}
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		text, note, err := mem.ReadMemory(budget, id)
		if err != nil {
			log.Printf("agent must_read_memories 读 %q 失败: %v", id, err)
			continue
		}
		if note != "" {
			// 超限时 text 为空、note 是提示：不再继续读。
			log.Printf("agent must_read_memories 读 %q 被限: %s", id, note)
			break
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "必读记忆全文（%s）：\n%s\n", id, text)
	}
	return b.String()
}

// runMemoryTool 在进程内执行一个只读补搜工具（search_memory /
// read_memory），返回与 executor.Result 同形的执行结果，供主循环
// 共用产物落盘 + history 记账的尾巴。只读、无不可逆动作，不走
// executor、不走监工、不记 execution_ledger（记账在召回账本，
// 见 front.doRecall / memorySummary 的 RecordRecall）；失败时
// ExitCode=1、Status="failure"，模型看到错误换招再试。
// 次数由 a.memBudget 按任务计（Run 开始时新建，见 Run）。
func (a *Agent) runMemoryTool(act Action) *executor.Result {
	fail := func(msg string) *executor.Result {
		return &executor.Result{TaskID: "", ToolID: act.ToolID, ExitCode: 1, Status: "failure", StdoutTail: msg, StdoutFull: msg, ErrHead: msg}
	}
	if a.Mem == nil {
		return fail("记忆未接线：search_memory/read_memory 不可用")
	}
	if a.memBudget == nil {
		a.memBudget = &memory.SearchBudget{}
	}
	switch act.ToolID {
	case "search_memory":
		query, mode, limit := memory.ParseSearchMemoryInputs(act.Inputs)
		cards, note, err := a.Mem.SearchMemory(a.memBudget, query, mode, limit)
		if err != nil {
			// L-1：记具体 err（cf-012 的 search_memory exit=1 查不到
			// 原因就是没记日志）；失败仍是 exit=1。
			log.Printf("agent search_memory 失败: query=%q err=%v", query, err)
			return fail("search_memory 失败：" + err.Error())
		}
		var b strings.Builder
		if note != "" {
			b.WriteString(note + "\n")
		}
		if len(cards) == 0 && note == "" {
			// 0 条匹配是成功（exit=0），不是失败：明确告诉模型
			// 没找到，别让它当成错误重查。
			b.WriteString("未找到相关记忆\n")
		}
		for _, c := range cards {
			b.WriteString(c.Display() + "\n")
		}
		// 读全文提示（2026-11-26 记忆召回整改）：卡片只有一句话简介，
		// 不含具体步骤和踩坑点，动手前要调 read_memory 读全文。
		if len(cards) > 0 {
			b.WriteString("以上仅为简介卡片，动手前请调用 read_memory(doc_id) 读取完整正文\n")
		}
		text := strings.TrimSpace(b.String())
		return &executor.Result{ToolID: act.ToolID, ExitCode: 0, Status: "success", StdoutTail: text, StdoutFull: text}
	case "read_memory":
		docID := memory.ParseReadMemoryInputs(act.Inputs)
		text, note, err := a.Mem.ReadMemory(a.memBudget, docID)
		if err != nil {
			// L-1：记具体 err。
			log.Printf("agent read_memory 失败: doc=%q err=%v", docID, err)
			return fail("read_memory 失败：" + err.Error())
		}
		// 超限时 text 为空、note 是提示：当作一次成功调用回显
		// （模型看到提示就知道不用再调），不算失败。
		if note != "" {
			return &executor.Result{ToolID: act.ToolID, ExitCode: 0, Status: "success", StdoutTail: note, StdoutFull: note}
		}
		return &executor.Result{ToolID: act.ToolID, ExitCode: 0, Status: "success", StdoutTail: text, StdoutFull: text}
	default:
		return fail("未知内置工具 " + act.ToolID)
	}
}

// executorActionFormat 是执行会话的机器输出格式（不进宪法文件，
// 宪法只写规矩，格式由代码定）：JSON 动作格式 + 三层懒加载机制 +
// inputs / shell 规则。冻结提示词 = 宪法文本 + 本段 + 大类清单。
const executorActionFormat = `只输出一个 JSON 对象，不要任何解释、不要 markdown 代码围栏。对象格式三选一：
{"action":"expand","category":"fetch"}
{"action":"expand","category":"fetch","subcategory":"browser"}
{"action":"tool","tool_id":"cap-fetch-curl-jq","inputs":["-sS","-L","https://example.com"]}
{"action":"answer","text":"……（任务的最终回复，直接写给用户）"}
一轮可打包最多 3 个互不依赖的 tool/expand 动作（发一个 JSON 数组）；answer 必须单独发；删东西、发东西、花钱永不打包，一次只发一件。
工具是三层懒加载，不是一次全摊开：
- 第一级（下面只列 9 个大类）：先定大类。不知道用哪个大类，就挑最可能的一个展开看看。
- 第二级：expand 某大类后，看到它下面的子类（每个一句话说明）与该大类的完整说明，再挑一个子类展开。
- 第三级：expand 某子类后，看到它下面的工具（每个有完整英文描述），再挑一个工具调。
- 展开（expand）只是导航，不算干活；调工具（tool）才真正干活。展开后看到的列表只在下一轮出现一次，接着挑就行；之后“已跑步骤”里只留一行摘要（带工具 ID），忘了描述可以重新展开。
- 已经知道 tool_id 时不用展开，直接调工具。
内置只读记忆工具（不用展开、不用在工具清单里找，直接调；进程内执行，不走 executor）：
- {"action":"tool","tool_id":"search_memory","inputs":["查询词"]}
  只读补搜：返回最多 5 张简介卡片（编号+标题+一句话简介，不含全文）。inputs 可追加 mode（lexical/semantic/hybrid，默认 hybrid）与 limit（1–5，默认 5）。语义腿当前停用，hybrid 自动退化为词法+全文 BM25。每个任务最多调 3 次，第 4 次起返回“次数已用完”，不要再调。
- {"action":"tool","tool_id":"read_memory","inputs":["W-0001"]}
  按编号读一篇全文（不截断）。每个任务最多调 3 次。编号用 search_memory 返回的 doc_id；没有编号时不要猜，先 search_memory 再读。
规则：
- 记忆优先：如果下面“相关记忆摘要”里已经有这道题的答案或关键结论，优先用它，只补必要的验证，不要从零重新推导。
- 分析/调研类任务：先调查，再把结论写成一个交付文件（用写文件工具或 python 写到工作目录的 report.md），最后用 action=answer 给出摘要并说明文件在哪。不要只在心里分析、只在对话里说，必须落盘成文，否则等于零交付；
- 任务做完了就 action=answer，把结果写进 text，不要再调工具（answer 只是请求停下，完成与否由网关验产物决定，不是你自己说了算；没产出就不要交活）；不确定做没做完就再调一个工具确认，不要猜；
- 一步只调一个工具（打包时一轮最多 3 个互不依赖的）；tool_id 只能从展开后看到的工具清单里挑，清单外的不许编；不知道调哪个就先展开最可能的大类看看，不要猜 tool_id；
- inputs 的每个字符串就是命令行上的一个参数（argv 的一个元素），执行时直接拼在工具名后面、不经过 shell：不要写 shell 语法——不要写管道 |、重定向 > >>、分号 ;、&&、$变量、for 循环、反引号，也不要在 inputs 里再写一遍工具名；需要落盘文件时，把绝对路径当参数传给工具（比如 --out /绝对路径/文件），不要写重定向；路径直接写绝对路径，不要用 $变量缩写；
- 不确定的命令先跑 --help 确认用法，不要编参数（比如 playwright screenshot 没有 --out 这个选项，编了会直接报错）；
- 抓网页 HTML 时要带全套浏览器请求头（Accept、Accept-Language、Sec-Fetch-* 等），只带 User-Agent 常被回 40 字节空壳或验证页；拿到验证页（Cloudflare Turnstile、验证码、challenge）或空壳时，换反检测工具再抓——camoufox（反检测浏览器）或 scrapling 的 stealthy_fetch（TLS 指纹伪装），不要在空壳上反复试普通抓取；
- 工具返回失败（exit 非 0、无输出、参数报错）时，先看错误信息换个招再试，不要原样重发同一条命令；同一工具连续失败两次就换别的工具；同一条命令失败了就不要再发一模一样的；

`

// frozenPrompt 拼执行会话的冻结提示词（放 A 区）：宪法 + 输出格式 + 大类清单。
// PromptHead 为空时用内置 agentPromptHead（测试与旧调用方）。
func (a *Agent) frozenPrompt() string {
	head := a.PromptHead
	if strings.TrimSpace(head) == "" {
		head = agentPromptHead
		return head + a.categoryList()
	}
	return head + "\n" + executorActionFormat + a.categoryList()
}

// executorActionFormatNative 是 P1 原生路径的机器输出格式：不用 JSON，
// 直接调声明好的工具；不调工具只回正文就是请求停下。
// 规则与文本路径一致（记忆优先、调研落盘、失败换招、不编 tool_id），
// 只把“输出 JSON”换成“调工具”。
const executorActionFormatNative = `直接调声明好的工具完成任务，不要输出 JSON、不要写代码围栏。
- 每轮可以调一个或多个工具（互不依赖的才一起调，最多 3 个）；删东西、发东西、花钱永不打包，一次只调一件。
- 工具的参数只有一个 inputs 数组：每个字符串就是命令行上的一个参数（argv 的一个元素），执行时直接拼在工具名后面、不经过 shell：不要写 shell 语法——不要写管道 |、重定向 > >>、分号 ;、&&、$变量、for 循环、反引号，也不要在 inputs 里再写一遍工具名；需要落盘文件时，把绝对路径当参数传给工具（比如 --out /绝对路径/文件），不要写重定向；路径直接写绝对路径，不要用 $变量缩写。
- 派单里声明的工具就是这单要用的（名字和用法见工具说明），不用先展开就能直接调；声明里没有的工具不要编，想用别的就先调 expand 展开大类/子类找。
- expand 只是导航找工具，不算干活；调 expand 时传 category（大类），要看子类再传 subcategory。
- 内置只读记忆工具直接调（进程内执行）：search_memory（inputs ["查询词"]，可加 mode 与 limit，返回最多 5 张简介卡片）和 read_memory（inputs ["编号"]，按编号读一篇全文不截断），每个任务各最多 3 次；编号用 search_memory 返回的 doc_id，没有编号不要猜。
规则：
- 记忆优先：如果下面“相关记忆摘要”里已经有这道题的答案或关键结论，优先用它，只补必要的验证，不要从零重新推导。
- 分析/调研类任务：先调查，再把结论写成一个交付文件（用写文件工具或 python 写到工作目录的 report.md），最后不调工具、直接回正文给出摘要并说明文件在哪。不要只在心里分析、只在对话里说，必须落盘成文，否则等于零交付；
- 任务做完了就不再调工具，直接回正文告诉用户结果（不调工具只回正文就是请求停下，完成与否由网关验产物决定，不是你自己说了算；没产出就不要交活）；不确定做没做完就再调一个工具确认，不要猜；
- 工具返回失败（exit 非 0、无输出、参数报错）时，先看错误信息换个招再试，不要原样重发同一条命令；同一工具连续失败两次就换别的工具；同一条命令失败了就不要再发一模一样的；
- 不确定的命令先跑 --help 确认用法，不要编参数；
- 抓网页 HTML 时要带全套浏览器请求头（Accept、Accept-Language、Sec-Fetch-* 等），只带 User-Agent 常被回 40 字节空壳或验证页；拿到验证页（Cloudflare Turnstile、验证码、challenge）或空壳时，换反检测工具再抓——camoufox（反检测浏览器）或 scrapling 的 stealthy_fetch（TLS 指纹伪装），不要在空壳上反复试普通抓取；

`

// frozenPromptNative 拼原生路径的冻结提示词：宪法 + 原生格式 + 大类清单。
// 与 frozenPrompt 的区别只是输出格式段（调工具 vs 输出 JSON）。
func (a *Agent) frozenPromptNative() string {
	head := a.PromptHead
	if strings.TrimSpace(head) == "" {
		head = agentPromptHead
		// 回退分支也要拼原生格式（Gemini 审阅中危：旧分支漏拼，
		// 模型会按旧的 action=answer 文本 JSON 规矩回话）。
		return head + "\n" + executorActionFormatNative + a.categoryList()
	}
	return head + "\n" + executorActionFormatNative + a.categoryList()
}

// expandToolsToDeclare 取某子类下可用、可执行、未隐藏的工具（去重后
// 追加进原生 tools 声明）。原生路径下派单漏配工具时，模型靠 expand
// 找到工具，但 tools 声明只在任务开始时算一次，不把新发现的工具加
// 进去，模型就调不了它（真实 API 会拒调未声明的函数名，
// Gemini 审阅高危）。have 是已声明工具名集合，命中的记进去，
// 下次不再重复追加。
func (a *Agent) expandToolsToDeclare(category, subcategory string, have map[string]bool) []llm.ToolDefinition {
	var out []llm.ToolDefinition
	if a.Reg == nil {
		return out
	}
	for _, e := range a.Reg.EntriesByCategory(category) {
		if e.Subcategory != subcategory || e.ToolID == "" || have[e.ToolID] {
			continue
		}
		if e.Hidden || !e.Available || !e.Executable() || a.Reg.ProbeFailed(e.ToolID) {
			continue
		}
		desc := strings.TrimSpace(e.OneLiner)
		if desc == "" {
			desc = e.ToolID
		}
		out = append(out, llm.NewToolDefinition(e.ToolID, desc, llm.InputsSchema))
		have[e.ToolID] = true
	}
	return out
}

// declaredTools 按派单 ToolIDs 向模型声明工具（P1）：
// 派单里有的工具（总账里找得到、可用才声明）+ expand 导航 +
// search_memory/read_memory 记忆工具。每个 registry 工具的参数统一
// 只有 inputs 数组（与 BuildArgv 的 argv 模型一致），说明用总账 one_liner。
// 派单没给工具时只声明 expand 与记忆工具，模型靠展开找工具（回退路径）。
func (a *Agent) declaredTools() []llm.ToolDefinition {
	seen := map[string]bool{}
	var out []llm.ToolDefinition
	for _, id := range a.ToolIDs {
		if seen[id] || id == "" {
			continue
		}
		seen[id] = true
		if a.Reg == nil {
			continue
		}
		e, ok := a.Reg.Entry(id)
		if !ok || !e.Available {
			log.Printf("agent: 派单工具 %q 不在总账或不可用，不声明", id)
			continue
		}
		desc := strings.TrimSpace(e.OneLiner)
		if desc == "" {
			desc = id
		}
		out = append(out, llm.NewToolDefinition(id, desc, llm.InputsSchema))
	}
	out = append(out,
		llm.NewToolDefinition("expand", "展开工具分类导航：先传 category 看子类，再传 subcategory 看工具清单。只导航，不干活。", llm.ExpandSchema),
		llm.NewToolDefinition("search_memory", "只读补搜记忆：返回最多 5 张简介卡片（编号+标题+一句话简介）。inputs[0] 是查询词，可追加 mode 与 limit。每个任务最多 3 次。", llm.InputsSchema),
		llm.NewToolDefinition("read_memory", "按编号读一篇记忆全文（不截断）。inputs[0] 是 search_memory 返回的 doc_id。每个任务最多 3 次。", llm.InputsSchema),
	)
	return out
}

// toolCallsToActions 把模型的原生 tool_calls 转成执行动作（P1）：
// expand 按 {category, subcategory} 转导航动作；其余按 {inputs: [...]}
// 转调工具动作。参数解析失败的那次调用不进执行，直接返回一条
// 参数错误结果（调用方把它当 tool 消息回填，模型看到错误换招）。
func toolCallsToActions(tcs []llm.ToolCall) (actions []Action, argErrs map[string]string) {
	argErrs = map[string]string{}
	for _, tc := range tcs {
		name := tc.Function.Name
		if name == "" {
			argErrs[tc.ID] = "工具调用缺名字，无法执行。"
			continue
		}
		if name == "expand" {
			var v struct {
				Category    string `json:"category"`
				Subcategory string `json:"subcategory"`
			}
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &v); err != nil || strings.TrimSpace(v.Category) == "" {
				argErrs[tc.ID] = "expand 参数解析失败：要传 category（大类），看子类再传 subcategory。正确示例：{\"category\":\"fetch\"}"
				continue
			}
			actions = append(actions, Action{Action: "expand", Category: v.Category, Subcategory: v.Subcategory, ToolCallID: tc.ID})
			continue
		}
		var v struct {
			Inputs []string `json:"inputs"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &v); err != nil {
			argErrs[tc.ID] = "工具参数解析失败：inputs 要是一个字符串数组。正确示例：{\"inputs\":[\"参数1\",\"参数2\"]}"
			continue
		}
		actions = append(actions, Action{Action: "tool", ToolID: name, Inputs: v.Inputs, ToolCallID: tc.ID})
	}
	return actions, argErrs
}

// toolResultText 把一步的执行结果格式化成 tool 消息正文（P1）：
// exit 码 + 状态 + 输出（已截到 500 字），与文本路径尾巴里的格式一致，
// 模型凭它判断下一步。
func toolResultText(rec stepRecord) string {
	out := rec.Output
	if rec.NoOutput {
		out = "（exit 0 但没有任何输出，等于没拿到东西）"
	}
	return fmt.Sprintf("exit=%d status=%s\n%s", rec.ExitCode, rec.Status, out)
}

// buildTailNative 是原生路径的当轮尾巴（P1）：任务 + 记忆摘要（首轮）+
// 打回意见 + 刚展开的列表 + 只读提醒。不含已跑步骤——步骤结果在结构化
// 对话历史里（assistant 的 tool_calls + tool 的结果），不再用文字重发一遍。
func buildTailNative(taskText, memSummary, feedback, pendingListing string) string {
	var b strings.Builder
	b.WriteString("任务：")
	b.WriteString(taskText)
	b.WriteString("\n")
	if memSummary != "" {
		b.WriteString(memSummary)
		b.WriteString("\n")
	}
	if feedback != "" {
		b.WriteString("上一轮的打回意见（必须照着改，不要重复同样的错误）：\n")
		b.WriteString(feedback)
		b.WriteString("\n")
	}
	if pendingListing != "" {
		b.WriteString("刚展开的列表（只看这一次，挑一个工具调或继续展开）：\n")
		b.WriteString(pendingListing)
		b.WriteString("\n")
	}
	b.WriteString("下一步：调工具继续做，或不调工具直接回正文交活。")
	return b.String()
}

// buildTailNativeFollowup 拼原生路径第 2 轮起的尾巴：只含本轮新增信息
// （打回意见、刚展开的列表），不重复任务正文与记忆摘要——
// 它们已在 msgHistory 首条 user 消息里，第 2 轮起仍可见。
func buildTailNativeFollowup(feedback, pendingListing string) string {
	var b strings.Builder
	if feedback != "" {
		b.WriteString("上一轮的打回意见（必须照着改，不要重复同样的错误）：\n")
		b.WriteString(feedback)
		b.WriteString("\n")
	}
	if pendingListing != "" {
		b.WriteString("刚展开的列表（只看这一次，挑一个工具调或继续展开）：\n")
		b.WriteString(pendingListing)
		b.WriteString("\n")
	}
	b.WriteString("下一步：调工具继续做，或不调工具直接回正文交活。")
	return b.String()
}

// extractJSON 取最外层 JSON 对象（模型常在 JSON 外包一层解释文字）。
func extractJSON(s string) string {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end < 0 || end < start {
		return s
	}
	return s[start : end+1]
}

// extractJSONValue 取最外层 JSON 值（对象或数组）：模型常在 JSON 外包
// 一层解释文字；打包时外层是 [...]，单发时是 {...}。
func extractJSONValue(s string) string {
	startObj := strings.Index(s, "{")
	startArr := strings.Index(s, "[")
	start := -1
	isArr := false
	if startObj >= 0 && (startArr < 0 || startObj < startArr) {
		start = startObj
	} else if startArr >= 0 {
		start = startArr
		isArr = true
	}
	if start < 0 {
		return s
	}
	var end int
	if isArr {
		end = strings.LastIndex(s, "]")
	} else {
		end = strings.LastIndex(s, "}")
	}
	if end < 0 || end <= start {
		return s
	}
	return s[start : end+1]
}

// validateAction 校验单个决定（parseAction 与 parseActions 共用）。
func validateAction(a Action) error {
	if a.Action != "tool" && a.Action != "answer" && a.Action != "expand" {
		return fmt.Errorf("agent: 未知 action %q", a.Action)
	}
	if a.Action == "tool" && a.ToolID == "" {
		return fmt.Errorf("agent: tool 决定缺 tool_id")
	}
	if a.Action == "expand" && a.Category == "" {
		return fmt.Errorf("agent: expand 决定缺 category")
	}
	return nil
}

// parseActions 解析模型一轮的决定：单个 JSON 对象，或最多 3 个互不依赖
// 动作的 JSON 数组。answer 必须单独发，数组里混了 answer 直接报错。
// 打包上限 MaxBatchActions（3 个）；超了报错，由调用方把原因喂回模型。
func parseActions(reply string) ([]Action, error) {
	v := strings.TrimSpace(extractJSONValue(reply))
	if strings.HasPrefix(v, "[") {
		var arr []Action
		if err := json.Unmarshal([]byte(v), &arr); err != nil {
			return nil, fmt.Errorf("agent: 打包决定非法 JSON: %w", err)
		}
		if len(arr) == 0 {
			return nil, fmt.Errorf("agent: 打包决定为空数组")
		}
		if len(arr) > MaxBatchActions {
			return nil, fmt.Errorf("agent: 打包决定超过 %d 个（%d 个）", MaxBatchActions, len(arr))
		}
		for i, a := range arr {
			if err := validateAction(a); err != nil {
				return nil, fmt.Errorf("agent: 打包第 %d 个决定非法: %w", i+1, err)
			}
			if a.Action == "answer" {
				return nil, fmt.Errorf("agent: answer 必须单独发，不能打包")
			}
		}
		return arr, nil
	}
	a, err := parseAction(reply)
	if err != nil {
		return nil, err
	}
	return []Action{a}, nil
}

// parseAction 解析模型一轮的决定；非法 JSON 返回错误。
// 单动作兼容入口（测试与旧调用方用）；主循环用 parseActions。
func parseAction(reply string) (Action, error) {
	var a Action
	if err := json.Unmarshal([]byte(extractJSON(reply)), &a); err != nil {
		return a, fmt.Errorf("agent: 决定非法 JSON: %w", err)
	}
	if err := validateAction(a); err != nil {
		return a, err
	}
	return a, nil
}

// categoryList 是懒加载第一级：9 个大类，每类只给首句摘要。
// 模型先定大类，再 expand 展开。建好的清单记一次 index_metrics
// （注入 token 与相对全量省下的 token，供每日 09:00 核算）。
func (a *Agent) categoryList() string {
	var b strings.Builder
	b.WriteString("工具大类（9 个，先挑一个展开看子类）：\n")
	for _, c := range a.Reg.CategoryMetas() {
		desc := registry.FirstSentence(c.Description)
		if desc == "" {
			desc = c.Name
		}
		fmt.Fprintf(&b, "- %s: %s\n", c.ID, desc)
	}
	text := b.String()
	a.Reg.RecordIndexLoad("L1", "", registry.EstimateTokens(text))
	return text
}

// subcategoryList 是懒加载第二级：某大类的全文说明 + 它的子类（每类首句摘要）。
func (a *Agent) subcategoryList(category string) (string, error) {
	desc := a.Reg.CategoryDescription(category)
	if desc == "" {
		return "", fmt.Errorf("agent: 未知大类 %q", category)
	}
	subs := a.Reg.SubcategoriesOf(category)
	if len(subs) == 0 {
		return "", fmt.Errorf("agent: 大类 %q 下没有子类", category)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "大类 %s 说明：\n%s\n子类（挑一个展开看工具）：\n", category, desc)
	for _, s := range subs {
		d := registry.FirstSentence(s.Description)
		if d == "" {
			d = s.ID
		}
		fmt.Fprintf(&b, "- %s: %s\n", s.ID, d)
	}
	text := b.String()
	a.Reg.RecordIndexLoad("L2", "", registry.EstimateTokens(text))
	return text, nil
}

// toolList 是懒加载第三级：某子类的全文说明 + 它的工具（完整英文描述）。
// 只列可调的（available 且可执行且探针未失败），与 CompactToolList 同一口径；
// 默认隐藏（Hidden）的工具不列，模型没机会挑中未过试用期的锻造工具。
func (a *Agent) toolList(category, subcategory string) (string, error) {
	desc := a.Reg.SubcategoryDescription(category, subcategory)
	if desc == "" {
		return "", fmt.Errorf("agent: 未知子类 %q/%q", category, subcategory)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "子类 %s/%s 说明：\n%s\n工具（挑一个调）：\n", category, subcategory, desc)
	listed := 0
	for _, e := range a.Reg.EntriesByCategory(category) {
		if e.Subcategory != subcategory {
			continue
		}
		if e.Hidden {
			continue
		}
		if !e.Available || !e.Executable() || a.Reg.ProbeFailed(e.ToolID) {
			continue
		}
		fmt.Fprintf(&b, "- %s: %s\n", e.ToolID, e.OneLiner)
		listed++
	}
	if listed == 0 {
		return "", fmt.Errorf("agent: 子类 %q/%q 下没有可用工具", category, subcategory)
	}
	text := b.String()
	a.Reg.RecordIndexLoad("L3", category+"/"+subcategory, registry.EstimateTokens(text))
	return text, nil
}

// buildTail 拼当轮尾巴：任务 + 记忆摘要 + 打回意见（如有） + 已跑步骤的结果。
// pendingListing 是上一轮刚展开的完整列表（只展示一次）：模型展开后需要
// 在下一轮看到完整描述才能挑工具，但列表不进 history 全文——history 里
// 只留一行带 ID 的摘要，否则每轮都把几千字的列表重发一遍，提示词爆炸。
func buildTail(taskText, memSummary, feedback string, history []stepRecord, pendingListing string) string {
	var b strings.Builder
	b.WriteString("任务：")
	b.WriteString(taskText)
	b.WriteString("\n")
	if memSummary != "" {
		b.WriteString(memSummary)
		b.WriteString("\n")
	}
	if feedback != "" {
		b.WriteString("上一轮的打回意见（必须照着改，不要重复同样的错误）：\n")
		b.WriteString(feedback)
		b.WriteString("\n")
	}
	if len(history) > 0 {
		b.WriteString("已跑步骤（按顺序，看结果再决定下一步）：\n")
		for i, s := range history {
			out := s.Output
			if s.NoOutput {
				out = "（exit 0 但没有任何输出，等于没拿到东西）"
			}
			fmt.Fprintf(&b, "%d. %s %v → exit=%d status=%s\n   结果：%s\n", i+1, s.ToolID, s.Inputs, s.ExitCode, s.Status, out)
		}
	} else {
		b.WriteString("还没跑过任何工具。\n")
	}
	if pendingListing != "" {
		b.WriteString("刚展开的列表（只看这一次，挑一个工具调或继续展开）：\n")
		b.WriteString(pendingListing)
		b.WriteString("\n")
	}
	b.WriteString("下一步只输出一个 JSON 对象。")
	return b.String()
}

// expandSummary 给展开步骤在 history 里留一行摘要：只记展开了什么、
// 看到了哪些 ID，不贴完整描述。模型已经在展开后的那轮看过完整列表，
// 之后凭 ID 就能调工具，不需要每轮重看描述。
func (a *Agent) expandSummary(category, subcategory string) string {
	if subcategory == "" {
		subs := a.Reg.SubcategoriesOf(category)
		ids := make([]string, 0, len(subs))
		for _, s := range subs {
			ids = append(ids, s.ID)
		}
		return fmt.Sprintf("展开 %s：子类 %s（共 %d 个）", category, strings.Join(ids, "、"), len(ids))
	}
	var ids []string
	for _, e := range a.Reg.EntriesByCategory(category) {
		if e.Subcategory != subcategory {
			continue
		}
		if !e.Available || !e.Executable() || a.Reg.ProbeFailed(e.ToolID) {
			continue
		}
		ids = append(ids, e.ToolID)
	}
	return fmt.Sprintf("展开 %s/%s：工具 %s（共 %d 个）", category, subcategory, strings.Join(ids, "、"), len(ids))
}

// synthesizePromptHead 是综合撰稿的冻结提示词：只写结论和分析，不写执行日志。
const synthesizePromptHead = `你是 007 的综合撰稿人。根据下面的调查记录，写一份结构化的分析报告，直接输出报告全文（markdown），不要任何解释、不要代码围栏。
要求：
- 只写调查记录里有的结论和证据，不要编造；没有证据的写“未验证/推测”，不要写成事实；
- 报告结构：开头一段 200 字以内摘要；然后功能清单（逐项列出，每项注明证据来源）；然后实现原理（逐项说明）；然后复刻方案（模块划分、关键接口、技术选型、难点）；最后未验证的假设与下一步；
- 不要写执行日志、不要罗列步骤、不要贴大段原始 HTML，只写结论和分析。
`

// readOnlyToolPrefixes 是只读工具的前缀：读/查/取类工具，只读不写。
var readOnlyToolPrefixes = []string{
	"cap-ops-lean-ctx", "cap-code-rg", "cap-fetch-",
	"cap-design-", "cap-seo-", "cap-graph-", "cap-rea-",
}

// IsReadOnlyToolByName 是原始的名字口径，供网关验货共用：
// expand / search_memory / read_memory 与上面前缀的都算只读；
// cap-ops-python 单独算只读——名字无法证明它写没写文件。
// 网关账本口径不同（见 cmd/super-agent 的
// gatewaySubstantiveCall，2026-11-25 对齐改动）：python 是通用执行
// 工具，成功调用就算实质执行——否则靠 python 干活的步在没声明产物/
// 校验命令时会被账本兜底误判为“没动手”。
// verify-cmd 是网关自己的验货动作（runVerifyCmd 注释有言在先），
// 不算任务的实质执行，也进只读表——否则验货行自己就满足了“实质执行”
// 底线，等于自己给自己验货。
// 注意：名字口径只用于网关账本检查（硬底线判实质调用）。
// 只读刹车已于 2026-11-26 删掉（白名单 98 个工具全进后刹车对已知
// 工具名存实亡，只拦不住换着花样读文件，反而误伤正常调研）。
func IsReadOnlyToolByName(toolID string) bool {
	switch toolID {
	case "expand", "search_memory", "read_memory", "cap-ops-python", "verify-cmd":
		return true
	}
	for _, p := range readOnlyToolPrefixes {
		if strings.HasPrefix(toolID, p) {
			return true
		}
	}
	return false
}

// outputLimitForTool 给工具输出进模型上下文的单次上限（M4 中期，2026-11-26；
// 2026-11-26 放宽：默认 500→6000 字，文档档 3000→20000 字——
// 500/3000 字一刀切逼执行器把长文档分段重读，每段重读都叠加全量上下文，
// 反而更烧钱（CF-015 执行器 41 次调用里大半在重读）；上限留得很大，
// 正常活永远切不到，只防失控输出冲垮上下文。其余工具保持 6000 字。
// cap-ops-python 是读文档的主力（脚本按行/关键词定向提取后回吐
// 结果），也进放宽档。真正的大文件仍走全文落产物（step-N.txt），
// 模型按需再取，上下文不会无限胀。
const (
	defaultOutputLimitRunes = 6000
	longOutputLimitRunes    = 20000
)

var longOutputTools = map[string]bool{
	"read_memory": true, "search_memory": true, "expand": true,
	"cap-ops-python": true,
}

func outputLimitForTool(toolID string) int {
	if longOutputTools[toolID] {
		return longOutputLimitRunes
	}
	return defaultOutputLimitRunes
}

// sameCommandFailedTwice 判同一工具+同一参数是否已连续失败 2 次
// （P0-4：CF 005 同一条浏览器启动命令连发 8 次的死法，代码层强制执行
// 提示词里“同一工具连续失败两次就换别的工具”的规矩）。
// 只看 history 末尾连续两条：都是同一命令且都不是 success 才算。
func sameCommandFailedTwice(toolID string, inputs []string, history []stepRecord) bool {
	if len(history) < 2 {
		return false
	}
	want := strings.Join(inputs, "\x00")
	n := 0
	for i := len(history) - 1; i >= 0 && n < 2; i-- {
		s := history[i]
		if s.ToolID != toolID || strings.Join(s.Inputs, "\x00") != want {
			return false
		}
		if s.Status == "success" {
			return false
		}
		n++
	}
	return n == 2
}

// needsReport 判调查后是否还缺一份交付报告：
// 用过抓取/分析类工具且工作目录里还没有 report.md，就缺。
// P0-3：只给调研任务综合报告。执行任务的交付是产物本身，
// 不是一篇事后合成的报告；执行任务靠 needsReport 冒充交付是 004/007 的死法。
// TaskType 为空时按 execute（不综合）。
func (a *Agent) needsReport(taskID string, history []stepRecord) bool {
	if a.TaskType != "research" {
		return false
	}
	if _, err := os.Stat(filepath.Join(a.WorkDir(taskID), "report.md")); err == nil {
		return false
	}
	for _, s := range history {
		if s.Status != "success" {
			continue
		}
		for _, p := range []string{"cap-fetch-", "cap-ops-", "cap-code-", "cap-design-", "cap-seo-", "cap-graph-", "cap-rea-"} {
			if strings.HasPrefix(s.ToolID, p) {
				return true
			}
		}
	}
	return false
}

// trailingFailures 返回 history 末尾连续失败的步数：
// 成功且有产出的步骤会打断连续计数；失败、无产出都算失败。
func trailingFailures(history []stepRecord) int {
	n := 0
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Status == "success" && !history[i].NoOutput {
			break
		}
		n++
	}
	return n
}

// synthesize 根据调查记录写一份分析报告，落盘到工作目录 report.md，
// 同时作为 reply.md 返回。只调一次模型，失败则返回错误。
func (a *Agent) synthesize(ctx context.Context, taskID, taskText, memSummary string, history []stepRecord, artDir string) (string, error) {
	var b strings.Builder
	b.WriteString("任务：")
	b.WriteString(taskText)
	b.WriteString("\n")
	if memSummary != "" {
		b.WriteString(memSummary)
		b.WriteString("\n")
	}
	b.WriteString("调查记录（只含成功的步骤，按顺序）：\n")
	total := 0
	for i, s := range history {
		if s.Status != "success" {
			continue
		}
		// 优先读该步的全文产物（比 500 字摘要完整）。
		full := ""
		if data, err := os.ReadFile(filepath.Join(artDir, fmt.Sprintf("step-%d.txt", i+1))); err == nil {
			full = string(data)
		}
		if full == "" {
			full = s.Output
		}
		if len([]rune(full)) > 2000 {
			full = string([]rune(full)[:2000])
		}
		fmt.Fprintf(&b, "【步骤 %d】%s %v\n%s\n", i+1, s.ToolID, s.Inputs, full)
		total += len([]rune(full))
		if total > 12000 {
			break
		}
	}
	reply, finishReason, err := a.callModel(taskID, synthesizePromptHead, b.String())
	if err != nil {
		return "", fmt.Errorf("agent: 综合调用失败: %w", err)
	}
	// 综合输出被截断时直接报错（不写半截报告当交付），调用方按失败处理。
	if finishReason == "length" {
		return "", fmt.Errorf("agent: 综合输出被截断（输出超限），报告未生成")
	}
	// 综合输出是 markdown 全文，直接用原文；只去掉模型可能包的围栏。
	report := strings.TrimSpace(reply)
	if strings.HasPrefix(report, "```") {
		report = strings.TrimPrefix(report, "```markdown")
		report = strings.TrimPrefix(report, "```")
		report = strings.TrimSuffix(strings.TrimSpace(report), "```")
		report = strings.TrimSpace(report)
	}
	// 出站硬扫描（线 B 修复轮第 3 棒）：综合报告既落盘 report.md /
	// 草稿文件（计划步 step-<n>-reply.md，其余 reply.md），
	// 也作为 ReplyDraft 回传，落盘/回传前先过 ScrubOutbound；
	// 模型干活用的 history / memSummary 不动，密钥保持可用。
	report, n, blocked := sanitize.ScrubOutbound(report)
	if n > 0 || blocked {
		log.Printf("agent task=%s 出站脱敏：掩码 %d 处，整封拦截=%v", taskID, n, blocked)
	}
	wd := a.WorkDir(taskID)
	if err := os.MkdirAll(wd, 0o755); err != nil {
		return "", fmt.Errorf("agent: 建工作目录失败: %w", err)
	}
	if err := os.WriteFile(filepath.Join(wd, "report.md"), []byte(report), 0o600); err != nil {
		return "", fmt.Errorf("agent: 写 report.md 失败: %w", err)
	}
	if err := os.WriteFile(filepath.Join(artDir, a.replyFileName()), []byte(report), 0o600); err != nil {
		return "", fmt.Errorf("agent: 写 %s 失败: %w", a.replyFileName(), err)
	}
	return report, nil
}

// checkpointShard 落盘本片的 Stage 断点（第 15 项）：分片任务的 Run
// 正常结束（做完、撞预算、无进展停）时调用，下一片凭 LastStage 续跑。
// ShardNo 为 0（不分片）或执行器为空时不做任何事；落盘失败只记日志，
// 不掩盖本次 Run 的结果。
func (a *Agent) checkpointShard(taskID, artDir string) {
	if a.ShardNo <= 0 || a.Ex == nil {
		return
	}
	if err := a.Ex.CheckpointStage(taskID, a.SessionID, "agent", a.ShardNo, artDir); err != nil {
		log.Printf("agent task=%s shard %d 落盘断点失败: %v", taskID, a.ShardNo, err)
		return
	}
	log.Printf("agent task=%s shard %d 断点已落盘（产物：%s）", taskID, a.ShardNo, artDir)
}

// Run 跑完一个多步任务：主模型逐轮决定调工具或交活。
// memSummary 是 Recall 前 3 条（可为空）；taskText 是任务原话；
// feedback 是上一轮打回的意见（重跑时才有，第一次为空）。
// 记忆摘要只在首轮进尾巴（每单只带一次），之后各轮不再重复带。
// StepCap（派单预估×2，10–50）是本单步数上限；900 秒 / 200 步是最后外墙。
func (a *Agent) Run(ctx context.Context, taskID, taskText, memSummary, feedback string) (*RunResult, error) {
	maxIter := a.MaxIter
	if maxIter <= 0 {
		maxIter = DefaultMaxIter
	}
	// 派单步数上限优先（预估×2，已由派单方 clamp 到 10–50），
	// 但绝不突破 200 步最后外墙。
	if a.StepCap > 0 && a.StepCap < maxIter {
		maxIter = a.StepCap
	}
	if maxIter > DefaultMaxIter {
		maxIter = DefaultMaxIter
	}
	budget := a.TimeBudget
	if budget <= 0 {
		budget = DefaultTimeBudget
	}
	start := time.Now()
	artDir := filepath.Join(a.DataDir, "artifacts", taskID)
	if err := os.MkdirAll(artDir, 0o755); err != nil {
		return nil, fmt.Errorf("agent: 建产物目录失败: %w", err)
	}
	// native：CallWithTools 接线时走 P1 原生路径（工具声明给模型、
	// 模型回 tool_calls、结果结构化回填）；否则走旧的文本 JSON 路径。
	// 生产 main.go 一定接线，原生是唯一生产路径；文本路径只留给测试与旧调用方。
	native := a.CallWithTools != nil
	frozen := a.frozenPrompt()
	if native {
		frozen = a.frozenPromptNative()
	}
	// msgHistory 是原生路径的结构化对话历史（assistant 的 tool_calls +
	// tool 的结果，原样追加，不得改写）；文本路径不用。
	var msgHistory []llm.Message
	// P2-a：断点续跑——本步有存盘历史时读回接着跑（只原生路径）。
	// persisted 是已落盘的消息数（续跑时等于读回的条数，增量只追加新消息）；
	// resumed 为真表示读回了历史，首轮不再重复追加 user 消息。
	persisted := 0
	resumed := false
	if native && a.Sess != nil && a.RunSessionID != "" && a.RunStepNo > 0 {
		restored, rep, rerr := a.Sess.LoadMessages(a.RunSessionID, a.RunStepNo, taskID)
		if rerr != nil {
			log.Printf("agent task=%s 会话历史读回失败，从头开始: %v", taskID, rerr)
		} else if len(restored) > 0 {
			if restored[0].Role != "user" {
				// 存盘历史首条不是 user 消息：形状不对，丢弃整段从头开始
				// （任务正文本轮会重建，不丢任务本身）。
				log.Printf("agent task=%s 会话历史首条不是 user 消息，丢弃后从头开始", taskID)
			} else {
				msgHistory = restored
				persisted = len(restored)
				resumed = true
				detail := fmt.Sprintf("步 %d 断点续跑：读回 %d 条历史", a.RunStepNo, len(restored))
				if rep != nil {
					log.Printf("agent task=%s 会话历史修复后读回 %d 条：截断 %d 行，补闭合 %d 个调用，丢弃 %d 条",
						taskID, len(restored), rep.TruncatedLines, rep.ClosedToolCalls, rep.DroppedMessages)
					detail = fmt.Sprintf("步 %d 断点续跑：读回 %d 条历史（修复：截断 %d 行，补闭合 %d 个调用，丢弃 %d 条）", a.RunStepNo, len(restored), rep.TruncatedLines, rep.ClosedToolCalls, rep.DroppedMessages)
				} else {
					log.Printf("agent task=%s 断点续跑：读回 %d 条历史", taskID, len(restored))
				}
				if a.Led != nil {
					if _, err := a.Led.RecordDispatchEvent(ledger.DispatchEvent{
						SessionID: a.RunSessionID, TaskID: taskID,
						Kind:   ledger.DispatchEventSessionResume,
						Detail: detail,
					}); err != nil {
						log.Printf("agent task=%s 记续跑事件失败: %v", taskID, err)
					}
				}
			}
		}
	}
	// runResumed 随本 Run 走：finishWithAnswer 靠它给 RunResult 打标
	// （方向 b：网关据此决定硬底线①的续跑豁免）。
	a.runResumed = resumed
	// persistHistory 把本轮新进 msgHistory 的部分追加存盘（失败只记日志，
	// 不阻塞任务）。defer 兜底所有返回点；循环顶部每轮也调一次，
	// 这样 kill -9 时最多丢一轮（正在跑的那轮）。
	persistHistory := func() {
		if a.Sess == nil || a.RunSessionID == "" || a.RunStepNo <= 0 {
			return
		}
		if len(msgHistory) <= persisted {
			return
		}
		if err := a.Sess.AppendMessages(a.RunSessionID, a.RunStepNo, taskID, msgHistory[persisted:]); err != nil {
			log.Printf("agent task=%s 会话历史存盘失败: %v", taskID, err)
			return
		}
		persisted = len(msgHistory)
	}
	defer persistHistory()
	// tools 是原生路径声明给模型的工具（派单 ToolIDs + expand + 记忆工具），
	// 每任务只算一次（ToolIDs 在 Run 内不变）；但 expand 展开子类后发现
	// 的新工具会动态追加（见 expandToolsToDeclare），所以 tools 是可变
	// 的——每轮都用当前这份 tools 调模型。
	var tools []llm.ToolDefinition
	// declaredToolNames 是 tools 里已声明的工具名（动态补声明时去重用）。
	declaredToolNames := map[string]bool{}
	if native {
		tools = a.declaredTools()
		for _, t := range tools {
			declaredToolNames[t.Function.Name] = true
		}
	}
	history := []stepRecord{}
	completed := 0
	// memBudget reset 点：每任务（每次 Run）新建一个补搜计数，
	// search/read 各 3 次；任务结束即丢弃，新任务清零。
	a.memBudget = &memory.SearchBudget{}
	// 必读记忆全文预注入（2026-11-26 记忆召回整改）：
	// 派单 must_read_memories 点名的记忆，执行开工时由代码直接读全文
	// 拼进记忆摘要（进首轮尾巴，与摘要一起只带一次），不靠模型自己调工具。
	// 每次读都记进 memBudget（与模型自己调 read_memory 同一个 3 次上限）。
	if len(a.MustReadMemories) > 0 && a.Mem != nil {
		memSummary = injectMustReadMemories(a.Mem, a.memBudget, a.MustReadMemories, memSummary)
	}
	// stepNo：实际执行过的工具步数（产物 step-N.txt 的编号；打包后一轮
	// 可能跑多个工具，不能再用轮次号当文件名）。
	stepNo := 0
	expandGapReported := false // 规则 1：每任务只报一次
	// pendingListing：上一轮刚展开的完整列表，只在下一轮的尾巴里出现一次。
	// 模型看过就能挑工具，之后 history 里只留一行带 ID 的摘要，不再重发全文。
	pendingListing := ""
	// tailMem：记忆摘要只在首轮进尾巴，首轮用完即清掉（每单只带一次）。
	tailMem := memSummary
	// noProgress：连续无进展轮数（失败、无输出、重复输出都算无进展）；
	// 到 MaxNoProgressRounds（3 轮）即停手交回执，不许硬撑。
	noProgress := 0
	stalled := false
	// consecutiveArgErrs：连续参数解析失败的轮数（CF 诊断中优）：
	// 偶尔一次解析失败不算无进展，连续 ≥2 次才计 noProgress++，
	// 给模型一次按示例纠正的机会。
	consecutiveArgErrs := 0
	// truncationNotice：上一轮被截断的提醒，进下一轮尾巴，用一次即清
	// （Gemini 审阅中危：不要当 user 消息塞进 msgHistory——下一轮尾巴
	// 本身也是 user 消息，两条连续 user 会打乱对话轮次；且截断轮的
	// tailMem 已清空，提醒放尾巴里才不会丢）。
	truncationNotice := ""
	// lastOutputHash：每个工具最近一次成功输出的哈希（截断前原始输出），
	// 用于判“重复输出”。原来比截断后 500 字的前缀——两个不同输出
	// 前 500 字相同会被误判“重复”（CF 诊断中优）。
	lastOutputHash := map[string]string{}
	for iter := 1; iter <= maxIter; iter++ {
		// P2-a：每轮开头把上一轮新进历史的部分落盘（kill -9 时最多丢一轮）。
		persistHistory()
		// 时间护栏：到预算就停下来综合已有成果，不让开放式任务无限烧下去。
		if time.Since(start) > budget {
			log.Printf("agent task=%s 到时间预算 %v，停下来综合", taskID, budget)
			break
		}
		// 动态刹车：连续 3 轮无新产出或状态变化就停下来交回执。
		if iter > 1 && noProgress >= MaxNoProgressRounds {
			log.Printf("agent task=%s 无进展：连续 %d 轮无新产出，停下来交回执", taskID, MaxNoProgressRounds)
			stalled = true
			break
		}
		// 卡住判定（后备）：连续 5 轮失败/无产出就停下来综合已有成果。
		// 能产出成果的任务不受步数限制，想跑多少轮就跑多少轮。
		if iter > 1 && trailingFailures(history) >= MaxConsecutiveFailures {
			log.Printf("agent task=%s 卡住：连续 %d 轮失败，停下来综合", taskID, MaxConsecutiveFailures)
			break
		}
		// 本轮尾巴：原生路径不含已跑步骤（结果在结构化历史里），
		// 文本路径含已跑步骤（buildTail）。
		var tail string
		if native {
			if iter == 1 {
				if resumed {
					// 断点续跑：存盘历史里已有首条 user 消息（任务正文 +
					// 记忆摘要），不再重复追加；本轮尾巴为空，模型直接
					// 看历史接着干。
					tail = ""
					// H-1（判错重干）：续跑且本轮是被打回重干时，新派单
					// 正文（含 LastRedoReason）必须作为新 user 消息追加
					// 到 msgHistory 末尾——否则模型只看到旧的“已完成”
					// 历史，根本不知道自己被打回、错在哪，会原地复读
					// 交活、0 新调用死循环。纯崩溃续跑（无重干原因）不
					// 追加，模型直接看历史接着干。
					if strings.TrimSpace(a.RunRedoReason) != "" {
						redoMsg := "【重干提醒】上次这步没过关，被打回重干了，原因：" + strings.TrimSpace(a.RunRedoReason) + "。这次重新做，针对这个原因改，不要原样重犯；已经做对的部分不用重做。"
						msgHistory = append(msgHistory, llm.Message{Role: "user", Content: redoMsg})
					}
				} else {
					// 首轮：任务正文 + 记忆摘要进尾巴，同时作为首条 user
					// 消息写进 msgHistory（Gemini 审阅中危）——第 2 轮起
					// 记忆摘要仍在历史里可见，且消息顺序为 system → user →
					// assistant，符合规范、利好 KV 缓存；尾巴本身不再重发
					// （BuildMessages 会跳过空尾巴，不会发两遍）。
					tail = buildTailNative(taskText, tailMem, feedback, pendingListing)
					msgHistory = append(msgHistory, llm.Message{Role: "user", Content: tail})
					tail = ""
				}
			} else {
				// 第 2 轮起：只发新增信息，不重复任务正文与记忆摘要。
				tail = buildTailNativeFollowup(feedback, pendingListing)
			}
		} else {
			tail = buildTail(taskText, tailMem, feedback, history, pendingListing)
		}
		tailMem = ""        // 记忆摘要只带首轮：本轮用完即清，不再进下一轮
		pendingListing = "" // 列表只展示一次：本轮看过后就清掉，不再进下一轮
		// 截断提醒：上一轮被截断的提醒，进本轮尾巴，用一次即清。
		if truncationNotice != "" {
			tail += "\n" + truncationNotice
			truncationNotice = ""
		}
		var actions []Action
		if native {
			// 原生路径（P1）：工具声明给模型，模型回 tool_calls。
			msg, finishReason, promptTokens, _, err := a.CallWithTools(taskID, frozen, tail, tools, msgHistory)
			if err != nil {
				return nil, fmt.Errorf("agent: 第 %d 轮模型调用失败: %w", iter, err)
			}
			// P2-b：记会话水位（单次 prompt_tokens 的峰值），翻新阈值用。
			// 失败只记日志、不阻塞任务；文本路径（callModel）不是生产
			// 路径、不记（prompt_tokens 在 callModel 内部丢掉了）。
			if a.Sess != nil && a.RunSessionID != "" && promptTokens > 0 {
				if rerr := a.Sess.RecordTokens(a.RunSessionID, promptTokens); rerr != nil {
					log.Printf("agent task=%s 会话水位记录失败: %v", taskID, rerr)
				}
			}
			// 截断拦截（P0-1）：本轮整轮作废，不执行、不交活。
			// 截断提醒进下一轮尾巴（truncationNotice，用一次即清），
			// 不当 user 消息塞进 msgHistory——本轮的 assistant 消息
			// 作废、不进历史；提醒放尾巴里，下一轮模型用更短的回复重发。
			// 首轮 user 消息已在 msgHistory 里，记忆摘要不会因截断丢失。
			if finishReason == "length" {
				history = append(history, stepRecord{ToolID: "model", Inputs: nil, ExitCode: -1, Status: "truncated", Output: "模型输出被截断（输出超限），本轮作废：不要重复已发出的工具调用，不要把截断的内容当答案；用更短的回复重发。"})
				truncationNotice = "上一轮你的输出被截断了，本轮作废；请用更短的回复重发，不要重复已发出的工具调用，不要把截断的内容当答案。"
				noProgress++
				continue
			}
			// 内容过滤/空回复拦截（Gemini 审阅中危）：finish_reason 是
			// content_filter，或正文为空且没调工具，都不当作交活——
			// 直接交活会写一份空 reply.md 并返回 stopped。记一轮无进展，
			// 让模型换个说法重发；连续 3 轮无进展由 noProgress 刹车停下。
			if finishReason == "content_filter" || (len(msg.ToolCalls) == 0 && strings.TrimSpace(msg.Content) == "") {
				history = append(history, stepRecord{ToolID: "model", Inputs: nil, ExitCode: -1, Status: "content_blocked", Output: "模型回复被内容过滤拦截或为空，本轮作废：换个说法重发，不要交空回复。"})
				noProgress++
				continue
			}
			// 不调工具只回正文 = 请求停下（P0-2 语义，与文本 answer 同）。
			if len(msg.ToolCalls) == 0 {
				// H-1：答复消息先追加进 msgHistory 再交活——否则答复
				// 从不落盘，下次续跑读回的条数纹丝不动（prod-real-002
				// 两次重试均 7 条的根因之一）。
				msgHistory = append(msgHistory, msg)
				return a.finishWithAnswer(ctx, taskID, taskText, memSummary, history, artDir, msg.Content, completed)
			}
			// 调了工具：assistant 消息（带 tool_calls）进结构化历史，
			// 再转执行动作；参数解析失败的调用直接回填错误结果。
			msgHistory = append(msgHistory, msg)
			var argErrs map[string]string
			actions, argErrs = toolCallsToActions(msg.ToolCalls)
			for id, errmsg := range argErrs {
				history = append(history, stepRecord{ToolID: "model", Inputs: nil, ExitCode: -1, Status: "argv_error", Output: errmsg})
				msgHistory = append(msgHistory, llm.ToolMessage(id, errmsg))
			}
			if len(actions) == 0 {
				// 本轮全是参数错误的调用：没有可执行的动作。
				// 温和处理（CF 诊断中优）：偶尔一次解析失败不算无进展，
				// 连续 ≥2 次解析失败才计 noProgress++（给模型一次
				// 按示例纠正的机会，别一次小错就加速撞刹车）。
				consecutiveArgErrs++
				if consecutiveArgErrs >= 2 {
					noProgress++
				}
				continue
			}
			// 本轮有可执行的动作：解析失败连败清零。
			consecutiveArgErrs = 0
		} else {
			// 文本路径（旧）：模型回 JSON 文本，解析成动作。
			reply, finishReason, err := a.callModel(taskID, frozen, tail)
			if err != nil {
				return nil, fmt.Errorf("agent: 第 %d 轮模型调用失败: %w", iter, err)
			}
			// 截断拦截（P0-1）：输出被截断时本轮整轮作废——不解析、不执行、
			// 不交活。把截断原因喂回模型，让它用更短的回复重发；计入失败，
			// 连续截断会撞刹车停下，绝不接受半截工具调用。
			if finishReason == "length" {
				history = append(history, stepRecord{ToolID: "model", Inputs: nil, ExitCode: -1, Status: "truncated", Output: "模型输出被截断（输出超限），本轮作废：不要重复已发出的工具调用，不要把截断的内容当答案；用更短的回复重发。"})
				noProgress++
				continue
			}
			var perr error
			actions, perr = parseActions(reply)
			if perr != nil {
				// 坏 JSON 重试一次：把“只返回 JSON”追加进尾巴再调。
				retry, retryFR, rerr := a.callModel(taskID, frozen, tail+"\n注意：上一轮回复不是合法 JSON，只返回 JSON 对象（单发一个对象，或打包最多 3 个互不依赖的 tool/expand 动作的数组），不要加任何解释文字。")
				if rerr != nil {
					return nil, fmt.Errorf("agent: 第 %d 轮模型调用失败: %w", iter, rerr)
				}
				if retryFR == "length" {
					history = append(history, stepRecord{ToolID: "model", Inputs: nil, ExitCode: -1, Status: "truncated", Output: "模型输出被截断（输出超限），本轮作废：不要重复已发出的工具调用，不要把截断的内容当答案；用更短的回复重发。"})
					noProgress++
					continue
				}
				actions, perr = parseActions(retry)
				if perr != nil {
					// 两次都不是合法 JSON：直接返回错误，任务失败。
					// 绝不把模型原话当答案收下（006 的死法：非法 JSON 回执
					// 被当成答案标 DONE）。Run 返回 error 时 runAgent 返回
					// agent_error，网关标 FAILED，handle() 补写失败回信。
					// 错误里不贴模型原话（可能含密钥），只写原因。
					return nil, fmt.Errorf("agent: 第 %d 轮决定两次解析失败（非合法 JSON），任务失败: %w", iter, perr)
				}
			}
			// 单发 answer：模型请求停下（P0-2）。
			if len(actions) == 1 && actions[0].Action == "answer" {
				return a.finishWithAnswer(ctx, taskID, taskText, memSummary, history, artDir, actions[0].Text, completed)
			}
		}
		// 原生路径回填用（P1）：本轮执行前 history 的长度（执行后新增的
		// 记录按顺序一一对应本轮的动作），以及本轮 expand 的完整列表
		// （按 tool_call_id 暂存，执行完回填进 tool 消息）。
		historyBefore := len(history)
		expandListings := map[string]string{}
		// 打包执行（1–3 个 tool/expand 动作；answer 只能单发，已在上面处理）。
		// 先预解析全部 tool 动作：可调用性 + 参数拼装 + 是否不可逆。
		// 不可逆动作绝不打包：包里只要有一个不可逆，整包拒掉、什么都不跑，
		// 把原因喂回模型，让它单发重走。
		type preparedTool struct {
			act          Action
			argv         []string
			irreversible bool
			builtin      bool  // 内置只读记忆工具（search_memory/read_memory）
			preErr       error // resolve/argv 失败的原因（执行时直接记账，不再重跑）
		}
		prepared := make([]preparedTool, len(actions))
		batchHasIrreversible := false
		for i, act := range actions {
			if act.Action != "tool" {
				continue
			}
			// 内置只读记忆工具：进程内执行（见 runMemoryTool），不走
			// ResolveTool/BuildArgv/executor——它们不在注册表里
			// （注册表只列 executor 能跑的 CLI/service 工具），
			// 也不参与不可逆打包判断（只读，永不打包拦截）。
			if act.ToolID == "search_memory" || act.ToolID == "read_memory" {
				prepared[i] = preparedTool{act: act, builtin: true}
				continue
			}
			if _, _, err := triage.ResolveTool(a.Reg, act.ToolID); err != nil {
				prepared[i] = preparedTool{act: act, preErr: fmt.Errorf("工具不可用：%w", err)}
				continue
			}
			argv, err := llm.BuildArgv(a.Reg, act.ToolID, act.Inputs)
			if err != nil {
				prepared[i] = preparedTool{act: act, preErr: fmt.Errorf("参数拼装失败：%w", err)}
				continue
			}
			irr, _ := executor.ClassifyIrreversible(argv)
			prepared[i] = preparedTool{act: act, argv: argv, irreversible: irr}
			if irr {
				batchHasIrreversible = true
			}
		}
		if len(actions) > 1 && batchHasIrreversible {
			log.Printf("agent task=%s iter=%d 打包里有不可逆动作，整包拒掉", taskID, iter)
			history = append(history, stepRecord{ToolID: "batch", Inputs: nil, ExitCode: -1, Status: "batch_rejected", Output: "打包里含不可逆动作（删东西/发东西/花钱），请单发"})
			if native {
				// 原生路径：整包被拒也要给每个 tool_call 回填结果再
				// continue——assistant 消息（带 tool_calls）已进
				// msgHistory，缺了 tool 回应下一轮调 API 直接报 400
				// 协议错（Gemini 审阅高危）。回填内容与下面主回填块
				// 的 batch 分支一致（每个调用同一条拒因）。
				for _, act := range actions {
					if act.ToolCallID == "" {
						continue
					}
					msgHistory = append(msgHistory, llm.ToolMessage(act.ToolCallID, "打包里含不可逆动作（删东西/发东西/花钱），请单发"))
				}
			}
			noProgress++
			continue
		}
		// 本轮进展：任一动作产出新东西（展开成功、工具新输出）即算有进展。
		roundProgress := false
		for i, act := range actions {
			if act.Action == "expand" {
				// 懒加载导航：只展开分类，不算干活、不走监工、不记账。
				// 展开成功记为 success（导航是进展，不是失败），这样连续失败
				// 计数不会把“一直在找工具”误判成卡住；时间预算与步数外墙
				// 仍兜底无限展开。
				var listing string
				var err error
				if act.Subcategory == "" {
					listing, err = a.subcategoryList(act.Category)
				} else {
					listing, err = a.toolList(act.Category, act.Subcategory)
				}
				if err != nil {
					log.Printf("agent task=%s iter=%d expand %s/%s: %v", taskID, iter, act.Category, act.Subcategory, err)
					history = append(history, stepRecord{ToolID: "expand", Inputs: []string{act.Category, act.Subcategory}, ExitCode: -1, Status: "expand_error", Output: "展开失败：" + err.Error()})
					continue
				}
				history = append(history, stepRecord{ToolID: "expand", Inputs: []string{act.Category, act.Subcategory}, ExitCode: 0, Status: "success", Output: a.expandSummary(act.Category, act.Subcategory)})
				a.detectExpandGap(taskID, taskText, history, &expandGapReported)
				if native && act.ToolCallID != "" {
					// 原生路径：完整列表回填进 tool 消息（模型调 expand
					// 就是为了看列表，列表就是这次调用的结果），
					// 不再走尾巴的 pendingListing（避免重复发两遍）。
					expandListings[act.ToolCallID] = listing
					// 动态补声明（Gemini 审阅高危）：展开到子类后，把该
					// 子类下可用工具追加进 tools，下一轮模型才能真的调
					// 它们——不补的话 tools 里没有这些工具名，真实 API
					// 会拒调未声明的函数。
					if act.Subcategory != "" {
						tools = append(tools, a.expandToolsToDeclare(act.Category, act.Subcategory, declaredToolNames)...)
					}
				} else {
					pendingListing = listing // 完整列表只在下一轮展示一次，之后只留摘要
				}
				// 展开产出新列表，是“有新输出”，按 noProgress 定义算进展。
				roundProgress = true
				continue
			}
			// 调工具：预解析已过可调用性与参数拼装，这里只执行。
			pre := prepared[i]
			if pre.preErr != nil {
				log.Printf("agent task=%s iter=%d %s 预解析失败: %v", taskID, iter, act.ToolID, pre.preErr)
				status := "resolve_error"
				if strings.HasPrefix(pre.preErr.Error(), "参数拼装失败") {
					status = "argv_error"
				}
				history = append(history, stepRecord{ToolID: act.ToolID, Inputs: act.Inputs, ExitCode: -1, Status: status, Output: pre.preErr.Error()})
				continue
			}
			// 重复执行拦截（P0-4）：同一工具+同一参数已连续失败 2 次，第 3 次
			// 不再跑，逼模型换招或说明阻塞（CF 005 同一条命令连发 8 次的死法）。
			if sameCommandFailedTwice(act.ToolID, act.Inputs, history) {
				log.Printf("agent task=%s iter=%d %s 参数相同已连续失败 2 次，不再重跑", taskID, iter, act.ToolID)
				history = append(history, stepRecord{ToolID: act.ToolID, Inputs: act.Inputs, ExitCode: -1, Status: "repeat_rejected", Output: "同一命令已连续失败 2 次，不再重跑；换别的工具、改参数，或说明被什么阻塞了。"})
				continue
			}
			var res *executor.Result
			var h *executor.Handle
			if pre.builtin {
				// 内置只读记忆工具：进程内同步调用（见 runMemoryTool），
				// 没有后台句柄、不走监工；下面与 executor 结果共用
				// 同一条产物落盘 + history 记账尾巴。
				res = a.runMemoryTool(act)
			} else {
				var err error
				res, h, err = a.Ex.Run(executor.RunRequest{
					TaskID: taskID, SessionID: a.SessionID, ToolID: act.ToolID,
					Argv: pre.argv, Timeout: executor.MaxTimeout, Text: taskText,
				})
				if err != nil {
					// 监工已下线：不再有 HALT 打回；执行失败直接记 run_error。
					log.Printf("agent task=%s iter=%d run %s: %v", taskID, iter, act.ToolID, err)
					history = append(history, stepRecord{ToolID: act.ToolID, Inputs: act.Inputs, ExitCode: -1, Status: "run_error", Output: "执行失败：" + err.Error()})
					continue
				}
			}
			if h != nil {
				// 长步骤：等后台跑完（下一步可能依赖它）。
				r, werr := h.Wait()
				if werr != nil {
					history = append(history, stepRecord{ToolID: act.ToolID, Inputs: act.Inputs, ExitCode: -1, Status: "wait_error", Output: "等后台失败：" + werr.Error()})
					continue
				}
				res = r
			}
			if res == nil {
				history = append(history, stepRecord{ToolID: act.ToolID, Inputs: act.Inputs, ExitCode: -1, Status: "no_result", Output: "执行无返回"})
				continue
			}
			// 产物校验：exit 0 但 stdout 为空且无落盘，等于没拿到东西，记为失败。
			noOutput := res.Status == "success" && strings.TrimSpace(res.StdoutTail) == "" && strings.TrimSpace(res.StdoutFull) == ""
			if noOutput {
				log.Printf("agent task=%s iter=%d %s exit=0 但无输出", taskID, iter, act.ToolID)
			}
			// 全文落产物，供复查。
			content := res.StdoutFull
			if strings.TrimSpace(res.StderrFull) != "" {
				content += "\n--- stderr ---\n" + res.StderrFull
			}
			stepNo++
			artPath := filepath.Join(artDir, fmt.Sprintf("step-%d.txt", stepNo))
			if werr := os.WriteFile(artPath, []byte(content), 0o600); werr != nil {
				log.Printf("agent task=%s iter=%d 写产物失败: %v", taskID, iter, werr)
			}
			out := res.StdoutTail
			// M4 中期（2026-11-26；2026-11-26 放宽）：输出上限按工具分档——
			// 文档/wiki/记忆类读取工具放宽到 20000 字（见 outputLimitForTool），
			// 其余保持 6000 字。
			limit := outputLimitForTool(act.ToolID)
			if len([]rune(out)) > limit {
				out = string([]rune(out)[:limit])
				// 第二道锁（2026-11-24，ctxread 装机）：输出被截时提醒模型——
				// 别用 cat/sed 整篇倒大文件，用 ctxread 先看结构/签名再按行号取原文。
				// 固定文案（不带步骤号），不破坏“重复输出判无进展”。
				out += fmt.Sprintf("\n……（输出已截到 %d 字；要看全文用 ctxread：先 ctxread <文件> 看结构，再 ctxread lines <起> <止> <文件> 取原文）", limit)
			}
			status := res.Status
			if noOutput {
				status = "no_output"
			} else if res.Status == "success" {
				completed++
				// 重复输出判无进展：同一工具成功但输出与上次一字不差，
				// 说明模型在原地打转，不算进展。比的是截断前原始输出
				// 的哈希（CF 诊断中优），不是截断后 500 字的前缀。
				rawHash := sha256.Sum256([]byte(res.StdoutFull + "\x00" + res.StderrFull))
				h := hex.EncodeToString(rawHash[:])
				if prev, ok := lastOutputHash[act.ToolID]; ok && prev == h {
					log.Printf("agent task=%s iter=%d %s 输出与上次相同，算无进展", taskID, iter, act.ToolID)
				} else {
					lastOutputHash[act.ToolID] = h
					roundProgress = true
				}
			}
			history = append(history, stepRecord{
				ToolID: act.ToolID, Inputs: act.Inputs,
				ExitCode: res.ExitCode, Status: status, Output: out, NoOutput: noOutput,
			})
			log.Printf("agent task=%s iter=%d %s exit=%d status=%s", taskID, iter, act.ToolID, res.ExitCode, status)
		}
		// 原生路径：执行结果按 tool_call_id 回填进结构化历史（P1）。
		// 执行循环里每个动作恰好产生一条 history 记录（顺序一致），
		// 按顺序一一对应；expand 的回填内容是完整列表（不是摘要），
		// 模型调 expand 就是为了看它。
		// 整包被拒时只有一条 batch 记录，每个调用都回填同一条拒因。
		// 动作数与记录数对不上（不应发生）时，每个调用回填一条通用结果，
		// 保证每个 tool_call 都有回应，不丢协议。
		if native {
			newEntries := history[historyBefore:]
			if len(newEntries) == 1 && newEntries[0].ToolID == "batch" {
				for _, act := range actions {
					if act.ToolCallID == "" {
						continue
					}
					msgHistory = append(msgHistory, llm.ToolMessage(act.ToolCallID, newEntries[0].Output))
				}
			} else if len(newEntries) == len(actions) {
				for i, act := range actions {
					if act.ToolCallID == "" {
						continue
					}
					content := toolResultText(newEntries[i])
					if act.Action == "expand" {
						if listing, ok := expandListings[act.ToolCallID]; ok {
							content = listing
						}
					}
					msgHistory = append(msgHistory, llm.ToolMessage(act.ToolCallID, content))
				}
			} else {
				for _, act := range actions {
					if act.ToolCallID == "" {
						continue
					}
					msgHistory = append(msgHistory, llm.ToolMessage(act.ToolCallID, "执行结果未记录，请换招或说明阻塞。"))
				}
			}
		}
		if roundProgress {
			noProgress = 0
		} else {
			noProgress++
		}
	}
	// 停下：要么卡住（连续失败），要么碰到最后防线。先看调查后是否缺交付报告：
	// 缺就综合一份；综合出来的报告同样只算停下（stopped），完成与否由
	// runAgent 的网关验货决定。综合失败或本来就不需要报告，才回步骤汇总。
	if a.needsReport(taskID, history) {
		report, serr := a.synthesize(ctx, taskID, taskText, memSummary, history, artDir)
		if serr == nil {
			log.Printf("agent task=%s 停下后综合出报告", taskID)
			a.detectGapsAtEnd(taskID, taskText, history, true)
			a.checkpointShard(taskID, artDir)
			return &RunResult{ReplyDraft: report, Status: "stopped", CompletedSteps: completed, ShardNo: a.ShardNo, Resumed: resumed}, nil
		}
		log.Printf("agent task=%s synthesize: %v", taskID, serr)
	}
	var b strings.Builder
	b.WriteString("任务未完成，已跑步骤如下：\n")
	for i, s := range history {
		fmt.Fprintf(&b, "%d. %s → exit=%d status=%s\n", i+1, s.ToolID, s.ExitCode, s.Status)
	}
	b.WriteString("请根据以上结果继续处理，或告诉用户还差什么。")
	// 出站硬扫描：步骤汇总落盘（计划步写 step-<n>-reply.md，其余写
	// reply.md，见 replyFileName）并作为 ReplyDraft 回传。
	replyText, n, blocked := sanitize.ScrubOutbound(b.String())
	if n > 0 || blocked {
		log.Printf("agent task=%s 出站脱敏：掩码 %d 处，整封拦截=%v", taskID, n, blocked)
	}
	if err := os.WriteFile(filepath.Join(artDir, a.replyFileName()), []byte(replyText), 0o600); err != nil {
		log.Printf("agent task=%s 写 %s 失败: %v", taskID, a.replyFileName(), err)
	}
	a.detectGapsAtEnd(taskID, taskText, history, false)
	status := "max_iterations"
	if stalled {
		status = "stalled" // 动态刹车：连续 3 轮无新产出，停下来交回执
	}
	a.checkpointShard(taskID, artDir)
	return &RunResult{ReplyDraft: replyText, Status: status, CompletedSteps: completed, ShardNo: a.ShardNo, Resumed: resumed}, nil
}

// WorkDir 返回任务的工作目录（与规划器旧口径一致）。
func (a *Agent) WorkDir(taskID string) string {
	return filepath.Join(a.DataDir, "workspace", taskID)
}
