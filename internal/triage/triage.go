// Package triage 是 007 超级 Agent 的分诊（大脑核心）：四选一 + 工具定位。
//
// 设计依据（《超级Agent-全新设计方案》v1.5，第 1、4 轮）：
//   - 四选一：DIRECT 不用工具直接答；TOOL 要动工具走执行；META 元配置自己做；
//     PLAN 多步任务走规划器（第 21 步：先拆步、核验、再按步执行）。
//   - Tier-0 规则 0 token 先行：已注册前缀 / 定时任务；未命中走 Tier-1。
//   - Tier-1 模型分诊：固定前缀、短 JSON 输出，只回一行
//     {"route":"DIRECT|TOOL|META","tool_id":"...","reason":"..."}。
//   - 工具定位最多 1–2 次模型调用：Tier-0 直取 L3；自然语言先取 L2 再取 L3。
//   - 分诊①类无计数器是旧系统未验证的缺口：每次分诊都写
//     ledger.db triage_metrics，不靠模型自觉记账。
//
// 前缀多对一的真实情况（第 1 步总账实测，逐条数过）：/rea、/audit 各只对 1 条，
// /graph 对 2 条，/seo 对 6 条，/design 对 3 条，/ui 对 4 条。
// 因此 Tier-0 只在前缀唯一对应 1 条可用工具时直接定 TOOL；
// 多候选时只收窄候选集，交给 Tier-1 在候选集里挑，绝不猜一个。
//
// 铁律：定位不到返回明确错误，绝不硬凑一个工具；DIRECT 不查表，
// 分诊本身不取 L3（取 L3 是调用方在 TOOL 之后另调 ResolveTool）；
// 写账本失败只记日志，不阻塞分诊；不写任何密钥、Token。
package triage

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
	"unicode"

	"007-super-agent/internal/ledger"
	"007-super-agent/internal/registry"
)

var (
	ErrNoModel        = errors.New("triage: Tier-0 未命中且未配置模型调用")
	ErrInvalidReply   = errors.New("triage: 模型分诊回复非法")
	ErrToolNotFound   = errors.New("triage: 工具定位不到")
	ErrToolNotAllowed = errors.New("triage: 工具不可调用")
	ErrProbeFailed    = errors.New("triage: 工具预检探针失败")
)

// Route 是分诊四选一。
type Route string

const (
	RouteDirect Route = "DIRECT" // 不用工具直接答
	RouteTool   Route = "TOOL"   // 要动工具走执行
	RouteMeta   Route = "META"   // 元配置自己做
	RoutePlan   Route = "PLAN"   // 多步任务走规划器（第 21 步）
)

// Input 是一次分诊的输入。
type Input struct {
	Text            string // 用户原话
	TaskID          string
	SessionID       string
	Scheduled       bool   // 定时任务标记（由网关/调度器打上）
	ScheduledToolID string // 定时任务预注册的工具（有才直出）
}

// Decision 是一次分诊的结论。
type Decision struct {
	Route      Route
	ToolID     string   // TOOL 时必填；DIRECT/META 为空
	Tier       int      // 0=规则直出，1=模型分诊
	Reason     string   // 规则或模型给出的理由
	Candidates []string // Tier-0 多候选时收窄的候选集
	LatencyMs  int64
}

// ModelCallFunc 是 Tier-1 的模型调用注入：生产上接 DeepSeek，
// 返回模型原文 + 输入/输出 token 数。测试时注入桩。
// frozen 是冻结前缀（分诊提示词头 + 可用工具清单，放缓存分区 A 区，
// 跨调用内容固定才能命中缓存）；tail 是本次用户原话（C 区）；
// taskID 用于费用按任务归属记账。
type ModelCallFunc func(taskID, frozen, tail string) (reply string, tokensIn, tokensOut int64, err error)

// Tier0Result 是 Tier-0 的结论：Matched 为真表示前缀/定时任务命中。
// ToolID 只有唯一候选时才有值；多候选时只给 Candidates。
type Tier0Result struct {
	Matched    bool
	Prefix     string
	Candidates []string
	ToolID     string
	Scheduled  bool
}

// Tier0 做 0 token 规则分诊：定时任务优先，已注册前缀次之。
// 前缀命中但候选多于 1 条时 Matched 为真、ToolID 为空，调用方收窄后走 Tier-1。
func Tier0(reg *registry.Registry, in Input) Tier0Result {
	if in.Scheduled && in.ScheduledToolID != "" {
		if e, ok := reg.Entry(in.ScheduledToolID); ok && e.Available {
			return Tier0Result{Matched: true, ToolID: e.ToolID, Scheduled: true}
		}
		// 预注册工具不存在/不可用：不硬定，落到 Tier-1 重新定。
	}
	text := strings.TrimSpace(in.Text)
	for _, prefix := range registeredPrefixes(reg) {
		if matchPrefix(text, prefix) {
			cands := prefixCandidates(reg, prefix)
			if len(cands) == 0 {
				// 前缀命中但名下无可用工具：当没命中，走 Tier-1。
				return Tier0Result{}
			}
			r := Tier0Result{Matched: true, Prefix: prefix, Candidates: cands}
			if len(cands) == 1 {
				r.ToolID = cands[0]
			}
			return r
		}
	}
	return Tier0Result{}
}

// registeredPrefixes 从总账收集全部去重后的 trigger_prefix，按字典序排，
// 保证多前缀共存时（如 /seo 与 /seo-xxx）匹配顺序稳定。
func registeredPrefixes(reg *registry.Registry) []string {
	seen := map[string]bool{}
	for _, cat := range registry.Categories() {
		for _, e := range reg.EntriesByCategory(cat) {
			if p := strings.TrimSpace(e.TriggerPrefix); p != "" {
				seen[p] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// prefixCandidates 取某前缀名下全部可用工具 ID，按字典序排。
func prefixCandidates(reg *registry.Registry, prefix string) []string {
	var out []string
	for _, cat := range registry.Categories() {
		for _, e := range reg.EntriesByCategory(cat) {
			if e.TriggerPrefix == prefix && e.Available {
				out = append(out, e.ToolID)
			}
		}
	}
	sort.Strings(out)
	return out
}

// matchPrefix 判原话是否以某前缀开头：前缀后须是空白、结尾，
// 或直接跟中文（"/audit审一下" 是中文用户的常见写法）；
// "/auditX" 这种前缀后直接跟 ASCII 字母的不算命中，防误判。
func matchPrefix(text, prefix string) bool {
	if !strings.HasPrefix(text, prefix) {
		return false
	}
	rest := text[len(prefix):]
	if rest == "" {
		return true
	}
	r := []rune(rest)[0]
	return unicode.IsSpace(r) || r >= 0x80
}

// triagePromptHead 是冻结的分诊提示词头：短、固定、只要求一行 JSON。
// 生产调用时由调用方在前面再拼常驻 L1（一级索引已在上下文里，不在这里重复）。
// 第 21 步：三选一改为四选一，新增 PLAN——需要多个工具按顺序分步完成，
// 或一个工具一次做不完、必须拆成多步的任务（先做 A、再做 B、最后汇总）。
// PLAN 只定去向，不挑具体工具，步骤表由规划器另出；单步路径行为不变。
const triagePromptHead = `你是 007 超级 Agent 的分诊器。只按下面四选一判，不解释、不多说。
DIRECT：不用任何工具，凭已有知识直接答（闲聊、常识、判断、解释）。
TOOL：必须动工具（取数、跑代码、动文件、出成品）才能做，且一步就能做完。
META：改 Agent 自身配置、定时任务、行为规则，不动外部工具。
PLAN：需要多个工具按顺序分步完成，或一个工具一次做不完、必须拆成多步的任务（先做 A、再做 B、最后汇总）。
只回一行 JSON，不许加别的内容：{"route":"DIRECT|TOOL|META|PLAN","tool_id":"...","reason":"..."}
TOOL 时 tool_id 必填且必须是总账里真实存在的工具；DIRECT/META/PLAN 时 tool_id 留空。
`

// CompactToolList 生成紧凑工具清单：每条一行「tool_id: 一句话」。
// 只列可执行的工具（cli/service；doc/sop/tool/mcp/data 是知识条目，
// 不进清单，模型没机会把它当步骤派去执行）。available:false 的也不进。
// candidates 非空时只列候选集（Tier-0 收窄用）；为空时列全量。
// 收窄后被滤空时回落到全量可执行清单（不给模型一张空表）。
// 顺序固定（总账 6 类顺序 + 类内文件顺序），跨调用内容不变，
// 冻结前缀才能命中缓存。one_liner 截到 80 字，防清单过长。
func CompactToolList(reg *registry.Registry, candidates []string) string {
	if len(candidates) > 0 {
		var b strings.Builder
		listed := 0
		for _, id := range candidates {
			e, ok := reg.Entry(id)
			if !ok || !e.Available || !e.Executable() {
				continue
			}
			fmt.Fprintf(&b, "- %s: %s\n", e.ToolID, truncateRunes(e.OneLiner, 80))
			listed++
		}
		if listed > 0 {
			return b.String()
		}
	}
	var b strings.Builder
	for _, cat := range registry.Categories() {
		for _, e := range reg.EntriesByCategory(cat) {
			if !e.Available || !e.Executable() {
				continue
			}
			fmt.Fprintf(&b, "- %s: %s\n", e.ToolID, truncateRunes(e.OneLiner, 80))
		}
	}
	return b.String()
}

// truncateRunes 按字符截断到 n 个字（先转 []rune，防中文乱码），超了加 …。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// BuildTriageFrozen 生成 Tier-1 分诊的冻结前缀（放缓存分区 A 区）：
// 提示词头 + 可用工具清单。清单放冻结区，不放每次变的用户区，
// 跨调用内容固定才能命中缓存。candidates 非空时只列候选集。
func BuildTriageFrozen(reg *registry.Registry, candidates []string) string {
	return triagePromptHead +
		"可用工具清单（只许从中挑 tool_id，清单外的一律非法；没有合适的工具就回 DIRECT 或 META，不要编 tool_id）：\n" +
		CompactToolList(reg, candidates)
}

// BuildTriageTail 生成 Tier-1 分诊的尾部（放缓存分区 C 区）：本次用户原话。
func BuildTriageTail(text string) string {
	return "用户原话：" + strings.TrimSpace(text) + "\n"
}

// BuildTriagePrompt 生成 Tier-1 分诊提示词（无候选收窄时用）：
// 冻结前缀 + 用户原话。调用方要走缓存分区时请拆成
// BuildTriageFrozen + BuildTriageTail 分别传 system / user。
func BuildTriagePrompt(reg *registry.Registry, text string) string {
	return BuildTriageFrozen(reg, nil) + BuildTriageTail(text)
}

// BuildTriagePromptNarrowed 生成收窄后的 Tier-1 提示词：
// 模型只许在 Tier-0 给出的候选集里挑工具，不许另编 tool_id。
func BuildTriagePromptNarrowed(reg *registry.Registry, text string, candidates []string) string {
	return BuildTriageFrozen(reg, candidates) + BuildTriageTail(text)
}

// ParseTriageReply 解析并校验模型的一行 JSON 回复。
// 校验：route 必须是四选一；TOOL 必须带 tool_id 且在总账里且可用；
// DIRECT/META/PLAN 带了不存在的 tool_id 也算错（已知 tool_id 则忽略，不进结论）。
func ParseTriageReply(reply string, reg *registry.Registry) (Decision, error) {
	start := strings.Index(reply, "{")
	end := strings.LastIndex(reply, "}")
	if start < 0 || end < 0 || end <= start {
		return Decision{}, fmt.Errorf("%w: reply has no JSON object", ErrInvalidReply)
	}
	var raw struct {
		Route  string `json:"route"`
		ToolID string `json:"tool_id"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(reply[start:end+1]), &raw); err != nil {
		return Decision{}, fmt.Errorf("%w: %v", ErrInvalidReply, err)
	}
	route := Route(strings.ToUpper(strings.TrimSpace(raw.Route)))
	switch route {
	case RouteDirect, RouteMeta, RoutePlan:
		if id := strings.TrimSpace(raw.ToolID); id != "" {
			if _, ok := reg.Entry(id); !ok {
				return Decision{}, fmt.Errorf("%w: unknown tool_id %q", ErrInvalidReply, id)
			}
		}
		return Decision{Route: route, Tier: 1, Reason: raw.Reason}, nil
	case RouteTool:
		id := strings.TrimSpace(raw.ToolID)
		if id == "" {
			return Decision{}, fmt.Errorf("%w: TOOL without tool_id", ErrInvalidReply)
		}
		e, ok := reg.Entry(id)
		if !ok {
			return Decision{}, fmt.Errorf("%w: unknown tool_id %q", ErrInvalidReply, id)
		}
		if !e.Available {
			return Decision{}, fmt.Errorf("%w: tool %q is available:false", ErrToolNotAllowed, id)
		}
		return Decision{Route: route, ToolID: e.ToolID, Tier: 1, Reason: raw.Reason}, nil
	default:
		return Decision{}, fmt.Errorf("%w: unknown route %q", ErrInvalidReply, raw.Route)
	}
}

// ResolveTool 按 L1→L2→L3 的顺序定位并取回一条工具的 L3 说明。
// 每一步都校验：L1 可读且该类在 6 类之内、L2 含该条、L3 可读且可用；
// 任一步失败返回错误，绝不换一条凑数。DIRECT 路径不调本函数。
// 第 14 步 B-03：verified=false 的工具先跑预检探针（存在性检查，
// 不真跑命令），通过才标记已验证并继续；失败返回 ErrProbeFailed，
// 调用方负责记账（失败行进 execution_ledger），不禁用、不静默跳过。
func ResolveTool(reg *registry.Registry, toolID string) (*registry.Entry, string, error) {
	if strings.TrimSpace(toolID) == "" {
		return nil, "", fmt.Errorf("%w: empty tool_id", ErrToolNotFound)
	}
	e, ok := reg.Entry(toolID)
	if !ok {
		return nil, "", fmt.Errorf("%w: %q", ErrToolNotFound, toolID)
	}
	if !e.Available {
		return nil, "", fmt.Errorf("%w: %q", ErrToolNotAllowed, toolID)
	}
	if reg.NeedsProbe(toolID) {
		if err := reg.Probe(toolID); err != nil {
			return nil, "", fmt.Errorf("%w: %q: %v", ErrProbeFailed, toolID, err)
		}
	}
	// L1：读常驻索引（已在 Load 时校验 ≤25 行），确认该类在 6 类之内。
	// 注：L1 行文用中文类名，slug 级核对走总账的固定 6 类表，不做字符串硬搜。
	if _, err := reg.L1(); err != nil {
		return nil, "", err
	}
	known := false
	for _, c := range registry.Categories() {
		if c == e.Category {
			known = true
			break
		}
	}
	if !known {
		return nil, "", fmt.Errorf("%w: category %q not in L1", ErrToolNotFound, e.Category)
	}
	// L2：确认该条在该类二级索引里。
	l2, err := reg.L2(e.Category)
	if err != nil {
		return nil, "", err
	}
	if !strings.Contains(l2, e.ToolID) {
		return nil, "", fmt.Errorf("%w: %q not listed in L2 %s", ErrToolNotFound, e.ToolID, e.Category)
	}
	// L3：取单条说明（registry 内部再校验 available 与 1600 字符上限）。
	l3, err := reg.L3(e.ToolID)
	if err != nil {
		return nil, "", err
	}
	return e, l3, nil
}

// Triage 跑完整分诊：Tier-0 先行，命中唯一工具 0 token 直出；
// 多候选收窄后走 Tier-1；未命中走 Tier-1。每次结论写 triage_metrics。
// led 为 nil 时跳过记账；modelCall 为 nil 且 Tier-0 未命中时返回 ErrNoModel。
func Triage(reg *registry.Registry, led *ledger.Ledger, in Input, modelCall ModelCallFunc) (Decision, error) {
	start := time.Now()
	if r := Tier0(reg, in); r.Matched {
		if r.ToolID != "" {
			d := Decision{Route: RouteTool, ToolID: r.ToolID, Tier: 0, Reason: tier0Reason(r)}
			d.LatencyMs = time.Since(start).Milliseconds()
			recordMetric(led, in, d, 0, 0)
			return d, nil
		}
		return triageTier1(reg, led, in, r.Candidates, modelCall, start)
	}
	return triageTier1(reg, led, in, nil, modelCall, start)
}

func tier0Reason(r Tier0Result) string {
	if r.Scheduled {
		return "Tier-0 定时任务预注册工具直出"
	}
	return "Tier-0 前缀 " + r.Prefix + " 唯一命中"
}

func triageTier1(reg *registry.Registry, led *ledger.Ledger, in Input, candidates []string, modelCall ModelCallFunc, start time.Time) (Decision, error) {
	if modelCall == nil {
		return Decision{}, ErrNoModel
	}
	prompt := BuildTriageFrozen(reg, candidates)
	tail := BuildTriageTail(in.Text)
	reply, tokIn, tokOut, err := modelCall(in.TaskID, prompt, tail)
	if err != nil {
		return Decision{}, fmt.Errorf("triage: model call: %w", err)
	}
	d, err := ParseTriageReply(reply, reg)
	if err != nil {
		return Decision{}, err
	}
	d.Tier = 1
	d.Candidates = candidates
	d.LatencyMs = time.Since(start).Milliseconds()
	recordMetric(led, in, d, tokIn, tokOut)
	return d, nil
}

// recordMetric 写一行 triage_metrics；账本失败只记日志，绝不阻塞分诊。
func recordMetric(led *ledger.Ledger, in Input, d Decision, tokIn, tokOut int64) {
	if led == nil {
		return
	}
	_, err := led.RecordTriage(ledger.TriageRecord{
		SessionID: in.SessionID, TaskID: in.TaskID,
		Route: string(d.Route), Tier: d.Tier, ToolID: d.ToolID,
		TokensIn: tokIn, TokensOut: tokOut, LatencyMs: d.LatencyMs,
	})
	if err != nil {
		log.Printf("triage: record metric failed: %v", err)
	}
}
