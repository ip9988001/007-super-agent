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
// - dispatch：写 ≤1000 token 派单进内部通道，派完即返回（F-08/F-10）；
// - reply：直接回话（纯讨论 DIRECT），写 reply.md 交活。
//
// 刹车（F-12）：自查（read/bash）最多 3 次，总计 5 轮内收口；超了就
// 按手头的东西派单或问用户。派单后前置等回执（同步流，见 dispatch 包），
// 回执状态即本单状态；执行已把 reply.md 写好，前置不再覆盖。
package front

import (
	"context"
	"encoding/json"
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
	"007-super-agent/internal/memory"
	"007-super-agent/internal/registry"
	"007-super-agent/internal/triage"
)

// ModelCallFunc 是前置的模型调用（与 agent/triage 同一签名：
// taskID、冻结提示词、尾巴 → 回复、输入 token、输出 token、错误）。
// 用 agent.ModelCallFunc 的别名，三处可直接互换。
type ModelCallFunc = agent.ModelCallFunc

// MaxFrontRounds 是前置每单最多轮数（F-12：总计 5 轮内收口）。
const MaxFrontRounds = 5

// MaxSelfChecks 是前置每单最多自查次数（F-12：read/bash 合计 ≤3 次）。
const MaxSelfChecks = 3

// MaxBatchActions 是前置一轮最多打包动作数（与执行端一致：3 个）。
const MaxBatchActions = 3

// ReceiptWaitTimeout 是前置等执行回执的最长等待（900 秒外墙 + 60 秒余量）。
const ReceiptWaitTimeout = 960 * time.Second

// Front 是前置会话。Call 接真实模型（只用 flash）；Reg 是总账；
// Mem 是记忆（查记忆用）；Ch 是派单通道；DataDir 是数据目录
// （reply.md 落盘处）；SessionID 是本进程会话号。
type Front struct {
	Call      ModelCallFunc
	Reg       *registry.Registry
	Mem       *memory.Store
	Ch        *dispatch.Channel
	DataDir   string
	SessionID string
	Prompt    string // 前置宪法文本（New 时从文件加载；为空用内置兜底）
}

// FrontResult 是前置一单的结果。
type FrontResult struct {
	ReplyDraft string   // 直接回话的文本（DIRECT 时）；派单时为空（执行已写 reply.md）
	Status     string   // done / dispatched / stalled / failed
	Route      string   // DIRECT / TOOL / META / AGENT
	DispatchID string   // 派单编号（派单时）
	ToolIDs    []string // 派单带的工具编号（供 L1 日志）
	Receipt    *dispatch.Receipt
}

// New 建一个前置会话。Prompt 从 data/prompts/front-AGENTS.md 加载；
// 文件缺失时留空，Run 用内置 frontPromptHead 兜底。
func New(call ModelCallFunc, reg *registry.Registry, mem *memory.Store, ch *dispatch.Channel, dataDir, sessionID string) *Front {
	return &Front{
		Call: call, Reg: reg, Mem: mem, Ch: ch,
		DataDir: dataDir, SessionID: sessionID,
		Prompt: loadFrontPrompt(dataDir),
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
{"action":"dispatch","route":"AGENT","goal":"……","criteria":"……","tool_ids":["cap-fetch-curl-jq"],"plan":["1. 先……","2. 再……"],"memory":["碎片……"],"pointers":["/绝对路径/文件"],"constraints":["……"],"est_steps":5,"est_seconds":300,"shard_no":0,"resume_from":""}
一轮可打包最多 3 个互不依赖的动作（发一个 JSON 数组）；reply 和 dispatch 必须单独发。
六个工具的用法：
- reply：纯讨论/轻自查直接答（DIRECT）。对外回复每轮 ≤200 字（代码块除外），每句一个原子事实，去废话，写清文件路径。
- recall：查记忆。每单只查 1 次（本单内再调直接返回上次的结果，不重复查）。默认取前 3 条，命中标来源。没有 recall 就不编历史、不说“没有记忆”。
- registry：查工具单。先 L1 看 9 个大类，再 L2 看某大类的子类与工具编号（一句话说明，不读 L3 全文）。定工具编号写进派单；标不可用的不许用。
- read：读小文件。偷看 ≤2KB，直读 ≤8KB；超 8KB 的文件不许硬读，只把路径写进派单的 pointers。
- bash：只读 bash，只用来看和查（ls/cat/grep/状态）。跑完机器上什么都不许变；要改东西一律写进派单。每单自查（read+bash 合计）最多 3 次。
- dispatch：写派单（TOOL 单工具简单派单 / META 元配置走 PR / AGENT 复杂多步）。派单内容：用户原话由代码自动带上，不用重复写；goal 目标、criteria 验收标准、tool_ids 工具编号、plan 步骤骨架、memory 记忆碎片（recall 查到的）、pointers 指针（路径/链接/记忆 ID，不贴全文）、constraints 约束、est_steps 预估步数、est_seconds 预估耗时。派单总量 ≤1000 token，超了会被打回重写。派完即返回，不轮询、不睡等。
流程：先判断——纯聊直接 reply；工具一眼能定、不涉旧事的简单派单，跳过 recall 和 registry 直接 dispatch；复杂活先 recall（1 次）+ registry（L1→L2）再 dispatch。互不相干的查询（如 recall + registry L1）可一轮打包最多 3 个。5 轮内必须收口（reply 或 dispatch）；超了就按手头的东西派单或问用户。
`

// Action 是前置一轮的决定。
type Action struct {
	Action      string   `json:"action"`
	Text        string   `json:"text,omitempty"`
	Query       string   `json:"query,omitempty"`
	Level       string   `json:"level,omitempty"`
	Category    string   `json:"category,omitempty"`
	Path        string   `json:"path,omitempty"`
	Command     string   `json:"command,omitempty"`
	Route       string   `json:"route,omitempty"`
	Goal        string   `json:"goal,omitempty"`
	Criteria    string   `json:"criteria,omitempty"`
	ToolIDs     []string `json:"tool_ids,omitempty"`
	Plan        []string `json:"plan,omitempty"`
	Memory      []string `json:"memory,omitempty"`
	Pointers    []string `json:"pointers,omitempty"`
	Constraints []string `json:"constraints,omitempty"`
	EstSteps    int      `json:"est_steps,omitempty"`
	EstSeconds  int      `json:"est_seconds,omitempty"`
	ShardNo     int      `json:"shard_no,omitempty"`    // 分片号（第 15 项：0 不分片）
	ResumeFrom  string   `json:"resume_from,omitempty"` // 续跑起点（上一片的断点指针）
}

// frozenPrompt 拼前置的冻结提示词（放 A 区）：宪法 + 输出格式。
func (f *Front) frozenPrompt() string {
	head := f.Prompt
	if strings.TrimSpace(head) == "" {
		head = frontPromptHead
	}
	return head + "\n" + frontActionFormat
}

// extractJSONValue 取最外层 JSON 值（对象或数组）。
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
func (f *Front) Run(ctx context.Context, taskID, taskText string) (*FrontResult, error) {
	frozen := f.frozenPrompt()
	artDir := filepath.Join(f.DataDir, "artifacts", taskID)
	if err := os.MkdirAll(artDir, 0o755); err != nil {
		return nil, fmt.Errorf("front: 建产物目录失败: %w", err)
	}
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
				// 两次都不是合法 JSON：模型明显在直接输出答案，把重试的
				// 原话当直接回话收下。空串才算真失败。
				if strings.TrimSpace(retry) == "" {
					return nil, fmt.Errorf("front: 第 %d 轮决定非法: %w", round, perr)
				}
				return f.reply(artDir, taskID, strings.TrimSpace(retry), "DIRECT")
			}
		}
		// reply/dispatch 只能单发（parseActions 已保证）。
		if len(actions) == 1 && actions[0].Action == "reply" {
			return f.reply(artDir, taskID, actions[0].Text, "DIRECT")
		}
		if len(actions) == 1 && actions[0].Action == "dispatch" {
			return f.dispatch(ctx, artDir, taskID, taskText, actions[0], memFragments)
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
				frags, err := f.doRecall(act.Query)
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
	// 5 轮未收口（F-12）：按手头的东西派单（AGENT，工具未定则空着，
	// 执行按派单骨架自行定工具），派单失败才问用户。
	d := &dispatch.Dispatch{
		Route: "AGENT", Goal: "完成用户任务（前置 5 轮未收口，按手头信息派单）",
		Criteria: "按用户原话验收", Memory: memFragments,
		EstSteps: 5, EstSeconds: 300,
	}
	return f.dispatch(ctx, artDir, taskID, taskText, Action{
		Route: "AGENT", Goal: d.Goal, Criteria: d.Criteria,
		Memory: memFragments, EstSteps: 5, EstSeconds: 300,
	}, memFragments)
}

// reply 写直接回话的 reply.md 并返回。
func (f *Front) reply(artDir, taskID, text, route string) (*FrontResult, error) {
	if err := os.WriteFile(filepath.Join(artDir, "reply.md"), []byte(text), 0o600); err != nil {
		return nil, fmt.Errorf("front: 写 reply.md 失败: %w", err)
	}
	return &FrontResult{ReplyDraft: text, Status: "done", Route: route}, nil
}

// dispatch 校验并发送派单，派完等回执。回执状态即本单状态。
func (f *Front) dispatch(ctx context.Context, artDir, taskID, taskText string, act Action, memFragments []string) (*FrontResult, error) {
	// 工具编号逐个过 ResolveTool（分诊不编造工具的硬守卫，见 triage）：
	// 不在册、不可用、默认隐藏的直接打回，不进执行。
	for _, toolID := range act.ToolIDs {
		if _, _, err := triage.ResolveTool(f.Reg, toolID); err != nil {
			return nil, fmt.Errorf("front: 派单工具 %q 不可用: %w（请换工具编号重派）", toolID, err)
		}
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
		Pointers: act.Pointers, Constraints: act.Constraints,
		EstSteps: estSteps, EstSeconds: estSeconds,
		ShardNo: act.ShardNo, ResumeFrom: act.ResumeFrom,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		ReplyCh:   make(chan *dispatch.Receipt, 1),
	}
	// 派单 ≤1000 token（F-08）：超了打回，不发送，让模型缩短重派。
	if n := dispatchTokens(d); n > dispatch.MaxDispatchTokens {
		return nil, fmt.Errorf("front: 派单 %d token，超 1000 上限，请缩短（只留目标、验收、工具编号、指针）", n)
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

// dispatchTokens 估算一份派单的 token 数（EstimateTokens 口径，
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
	for _, s := range d.Pointers {
		b.WriteString(s)
	}
	for _, s := range d.Constraints {
		b.WriteString(s)
	}
	return registry.EstimateTokens(b.String())
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
func (f *Front) doRecall(query string) ([]string, error) {
	if f.Mem == nil {
		return nil, fmt.Errorf("记忆未接线")
	}
	rs, err := f.Mem.Recall(query)
	if err != nil {
		return nil, fmt.Errorf("查记忆失败: %w", err)
	}
	var out []string
	for _, r := range rs {
		out = append(out, fmt.Sprintf("【%s】%s", r.Source, r.Text))
	}
	return out, nil
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
