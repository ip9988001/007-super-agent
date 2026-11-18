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
//     不可逆动作仍过监工（执行层自带）。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"007-super-agent/internal/executor"
	"007-super-agent/internal/ledger"
	"007-super-agent/internal/llm"
	"007-super-agent/internal/registry"
	"007-super-agent/internal/triage"
)

// ModelCallFunc 与 llm.Client.CallTask 同签名：
// 冻结提示词（A 区，可命中缓存）+ 当轮尾巴，返回模型原话与 token 数。
type ModelCallFunc func(taskID, frozen, tail string) (reply string, tokensIn, tokensOut int64, err error)

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

// Agent 是主循环：一次 Run 跑完一个多步任务。
type Agent struct {
	Call       ModelCallFunc // 只用 flash（worker 模型），不用 reasoner
	Reg        *registry.Registry
	Led        *ledger.Ledger
	Ex         *executor.Executor
	DataDir    string // 产物落盘根目录（artifacts/<taskID>/）
	SessionID  string
	MaxIter    int           // 0 时用 DefaultMaxIter（最后防线，不是能力上限）
	TimeBudget time.Duration // 0 时用 DefaultTimeBudget（时间护栏）
}

// Action 是模型每轮的决定：调一个工具，或直接交活。
type Action struct {
	Action      string   `json:"action"` // "tool"、"answer" 或 "expand"（三层懒加载导航）
	ToolID      string   `json:"tool_id,omitempty"`
	Inputs      []string `json:"inputs,omitempty"`
	Text        string   `json:"text,omitempty"`
	Category    string   `json:"category,omitempty"`    // expand 用：一级大类
	Subcategory string   `json:"subcategory,omitempty"` // expand 用：二级子类
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
type RunResult struct {
	ReplyDraft     string
	Status         string // done / max_iterations / agent_error
	CompletedSteps int
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
- 展开（expand）只是导航，不算干活、不花钱；调工具（tool）才真正干活。展开后看到的列表会出现在“已跑步骤”里，接着挑就行。
- 已经知道 tool_id 时不用展开，直接调工具。
规则：
- 记忆优先：如果下面“相关记忆摘要”里已经有这道题的答案或关键结论（比如 wiki 里有现成的逆向方案、踩过的坑），优先用它，只补必要的验证，不要从零重新推导。记忆里有的结论直接引用，不用再去现查；
- 分析/调研类任务：先调查，再把结论写成一个交付文件（用写文件工具或 python 写到工作目录的 report.md），最后用 action=answer 给出摘要并说明文件在哪。不要只在心里分析、只在对话里说，必须落盘成文，否则等于零交付；
- 任务做完了就 action=answer，把结果写进 text，不要再调工具；不确定做没做完就再调一个工具确认，不要猜；
- 一步只调一个工具；tool_id 只能从展开后看到的工具清单里挑，清单外的不许编；不知道调哪个就先展开最可能的大类看看，不要猜 tool_id；
- inputs 的每个字符串就是命令行上的一个参数（argv 的一个元素），执行时直接拼在工具名后面、不经过 shell：不要写 shell 语法——不要写管道 |、重定向 > >>、分号 ;、&&、$变量、for 循环、反引号，也不要在 inputs 里再写一遍工具名；需要落盘文件时，把绝对路径当参数传给工具（比如 --out /绝对路径/文件），不要写重定向；路径直接写绝对路径，不要用 $变量缩写；
- 不确定的命令先跑 --help 确认用法，不要编参数（比如 playwright screenshot 没有 --out 这个选项，编了会直接报错）；
- 抓网页 HTML 时要带全套浏览器请求头（Accept、Accept-Language、Sec-Fetch-* 等），只带 User-Agent 常被回 40 字节空壳或验证页；拿到验证页（Cloudflare Turnstile、验证码、challenge）或空壳时，换反检测工具再抓——camoufox（反检测浏览器）或 scrapling 的 stealthy_fetch（TLS 指纹伪装），不要在空壳上反复试普通抓取；
- 工具返回失败（exit 非 0、无输出、参数报错）时，先看错误信息换个招再试，不要原样重发同一条命令；同一工具连续失败两次就换别的工具；同一条命令失败了就不要再发一模一样的；
- 引用历史结论、经验、教训时，只引用下面“相关记忆摘要”里有的，不在其中的不要编造。
`

// New 建一个主循环。调用方负责把 Call 接到真实模型
// （生产：llm.New(cfg.WorkerModel, key).CallTask，只用 flash）。
func New(call ModelCallFunc, reg *registry.Registry, led *ledger.Ledger, ex *executor.Executor, dataDir, sessionID string) *Agent {
	return &Agent{
		Call: call, Reg: reg, Led: led, Ex: ex,
		DataDir: dataDir, SessionID: sessionID,
		MaxIter: DefaultMaxIter,
	}
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

// parseAction 解析模型一轮的决定；非法 JSON 返回错误。
func parseAction(reply string) (Action, error) {
	var a Action
	if err := json.Unmarshal([]byte(extractJSON(reply)), &a); err != nil {
		return a, fmt.Errorf("agent: 决定非法 JSON: %w", err)
	}
	if a.Action != "tool" && a.Action != "answer" && a.Action != "expand" {
		return a, fmt.Errorf("agent: 未知 action %q", a.Action)
	}
	if a.Action == "tool" && a.ToolID == "" {
		return a, fmt.Errorf("agent: tool 决定缺 tool_id")
	}
	if a.Action == "expand" && a.Category == "" {
		return a, fmt.Errorf("agent: expand 决定缺 category")
	}
	return a, nil
}

// categoryList 是懒加载第一级：9 个大类，每类只给首句摘要。
// 模型先定大类，再 expand 展开。
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
	return b.String()
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
	return b.String(), nil
}

// toolList 是懒加载第三级：某子类的全文说明 + 它的工具（完整英文描述）。
// 只列可调的（available 且可执行且探针未失败），与 CompactToolList 同一口径。
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
		if !e.Available || !e.Executable() || a.Reg.ProbeFailed(e.ToolID) {
			continue
		}
		fmt.Fprintf(&b, "- %s: %s\n", e.ToolID, e.OneLiner)
		listed++
	}
	if listed == 0 {
		return "", fmt.Errorf("agent: 子类 %q/%q 下没有可用工具", category, subcategory)
	}
	return b.String(), nil
}

// buildTail 拼当轮尾巴：任务 + 记忆摘要 + 监工反馈（如有） + 已跑步骤的结果。
func buildTail(taskText, memSummary, feedback string, history []stepRecord) string {
	var b strings.Builder
	b.WriteString("任务：")
	b.WriteString(taskText)
	b.WriteString("\n")
	if memSummary != "" {
		b.WriteString(memSummary)
		b.WriteString("\n")
	}
	if feedback != "" {
		b.WriteString("监工上一轮的打回意见（必须照着改，不要重复同样的错误）：\n")
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
	b.WriteString("下一步只输出一个 JSON 对象。")
	return b.String()
}

// synthesizePromptHead 是综合撰稿的冻结提示词：只写结论和分析，不写执行日志。
const synthesizePromptHead = `你是 007 的综合撰稿人。根据下面的调查记录，写一份结构化的分析报告，直接输出报告全文（markdown），不要任何解释、不要代码围栏。
要求：
- 只写调查记录里有的结论和证据，不要编造；没有证据的写“未验证/推测”，不要写成事实；
- 报告结构：开头一段 200 字以内摘要；然后功能清单（逐项列出，每项注明证据来源）；然后实现原理（逐项说明）；然后复刻方案（模块划分、关键接口、技术选型、难点）；最后未验证的假设与下一步；
- 不要写执行日志、不要罗列步骤、不要贴大段原始 HTML，只写结论和分析。
`

// needsReport 判调查后是否还缺一份交付报告：
// 用过抓取/分析类工具且工作目录里还没有 report.md，就缺。
func (a *Agent) needsReport(taskID string, history []stepRecord) bool {
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
// 成功且有产出的步骤会打断连续计数；失败、无产出、被监工叫停都算失败。
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
	reply, _, _, err := a.Call(taskID, synthesizePromptHead, b.String())
	if err != nil {
		return "", fmt.Errorf("agent: 综合调用失败: %w", err)
	}
	// 综合输出是 markdown 全文，直接用原文；只去掉模型可能包的围栏。
	report := strings.TrimSpace(reply)
	if strings.HasPrefix(report, "```") {
		report = strings.TrimPrefix(report, "```markdown")
		report = strings.TrimPrefix(report, "```")
		report = strings.TrimSuffix(strings.TrimSpace(report), "```")
		report = strings.TrimSpace(report)
	}
	wd := a.WorkDir(taskID)
	if err := os.MkdirAll(wd, 0o755); err != nil {
		return "", fmt.Errorf("agent: 建工作目录失败: %w", err)
	}
	if err := os.WriteFile(filepath.Join(wd, "report.md"), []byte(report), 0o600); err != nil {
		return "", fmt.Errorf("agent: 写 report.md 失败: %w", err)
	}
	if err := os.WriteFile(filepath.Join(artDir, "reply.md"), []byte(report), 0o600); err != nil {
		return "", fmt.Errorf("agent: 写 reply.md 失败: %w", err)
	}
	return report, nil
}

// Run 跑完一个多步任务：主模型逐轮决定调工具或交活。
// memSummary 是 Recall 前 3 条（可为空）；taskText 是任务原话；
// feedback 是监工上一轮打回的意见（重跑时才有，第一次为空）。
func (a *Agent) Run(ctx context.Context, taskID, taskText, memSummary, feedback string) (*RunResult, error) {
	maxIter := a.MaxIter
	if maxIter <= 0 {
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
	frozen := agentPromptHead + a.categoryList()
	history := []stepRecord{}
	completed := 0
	for iter := 1; iter <= maxIter; iter++ {
		// 时间护栏：到预算就停下来综合已有成果，不让开放式任务无限烧下去。
		if time.Since(start) > budget {
			log.Printf("agent task=%s 到时间预算 %v，停下来综合", taskID, budget)
			break
		}
		// 卡住判定：连续 5 轮失败/无产出就停下来综合已有成果，不再硬撑。
		// 能产出成果的任务不受步数限制，想跑多少轮就跑多少轮。
		if iter > 1 && trailingFailures(history) >= MaxConsecutiveFailures {
			log.Printf("agent task=%s 卡住：连续 %d 轮失败，停下来综合", taskID, MaxConsecutiveFailures)
			break
		}
		reply, _, _, err := a.Call(taskID, frozen, buildTail(taskText, memSummary, feedback, history))
		if err != nil {
			return nil, fmt.Errorf("agent: 第 %d 轮模型调用失败: %w", iter, err)
		}
		act, perr := parseAction(reply)
		if perr != nil {
			// 坏 JSON 重试一次：把“只返回 JSON”追加进尾巴再调。
			retry, _, _, rerr := a.Call(taskID, frozen, buildTail(taskText, memSummary, feedback, history)+"\n注意：上一轮回复不是合法 JSON，只返回 JSON 对象，不要加任何解释文字。")
			if rerr != nil {
				return nil, fmt.Errorf("agent: 第 %d 轮模型调用失败: %w", iter, rerr)
			}
			act, perr = parseAction(retry)
			if perr != nil {
				// 两次都不是合法 JSON：模型明显在直接输出答案（而非调工具），
				// 把重试的原话当答案收下，交给监工去审，而不是直接报错、
				// 丢掉已经调查到的成果。空串才算真失败。
				if strings.TrimSpace(retry) == "" {
					return nil, fmt.Errorf("agent: 第 %d 轮决定非法: %w", iter, perr)
				}
				act = Action{Action: "answer", Text: strings.TrimSpace(retry)}
			}
		}
		if act.Action == "answer" {
			// 调查类任务：光一句短答不算交付，没有 report.md 就先综合一份。
			if a.needsReport(taskID, history) {
				report, serr := a.synthesize(ctx, taskID, taskText, memSummary, history, artDir)
				if serr != nil {
					log.Printf("agent task=%s synthesize: %v", taskID, serr)
				} else {
					return &RunResult{ReplyDraft: report, Status: "done", CompletedSteps: completed}, nil
				}
			}
			if err := os.WriteFile(filepath.Join(artDir, "reply.md"), []byte(act.Text), 0o600); err != nil {
				return nil, fmt.Errorf("agent: 写 reply.md 失败: %w", err)
			}
			return &RunResult{ReplyDraft: act.Text, Status: "done", CompletedSteps: completed}, nil
		}
		if act.Action == "expand" {
			// 懒加载导航：只展开分类，不算干活、不走监工、不记账。
			// 展开成功记为 success（导航是进展，不是失败），这样连续失败
			// 计数不会把“一直在找工具”误判成卡住；时间预算与 200 步
			// 最后防线仍兜底无限展开。
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
			history = append(history, stepRecord{ToolID: "expand", Inputs: []string{act.Category, act.Subcategory}, ExitCode: 0, Status: "success", Output: listing})
			continue
		}
		// 调工具：先过代码层可调用性，再拼参数，再执行。
		if _, _, err := triage.ResolveTool(a.Reg, act.ToolID); err != nil {
			log.Printf("agent task=%s iter=%d resolve %s: %v", taskID, iter, act.ToolID, err)
			history = append(history, stepRecord{ToolID: act.ToolID, Inputs: act.Inputs, ExitCode: -1, Status: "resolve_error", Output: "工具不可用：" + err.Error()})
			continue
		}
		argv, err := llm.BuildArgv(a.Reg, act.ToolID, act.Inputs)
		if err != nil {
			log.Printf("agent task=%s iter=%d argv %s: %v", taskID, iter, act.ToolID, err)
			history = append(history, stepRecord{ToolID: act.ToolID, Inputs: act.Inputs, ExitCode: -1, Status: "argv_error", Output: "参数拼装失败：" + err.Error()})
			continue
		}
		res, h, err := a.Ex.Run(executor.RunRequest{
			TaskID: taskID, SessionID: a.SessionID, ToolID: act.ToolID,
			Argv: argv, Timeout: executor.MaxTimeout, Text: taskText,
		})
		if err != nil {
			if errors.Is(err, executor.ErrAdvisorHaltFinal) || errors.Is(err, executor.ErrAdvisorHalt) {
				// 监工 HALT：这步停下，把原因告诉模型，让它换招或交活。
				history = append(history, stepRecord{ToolID: act.ToolID, Inputs: act.Inputs, ExitCode: -1, Status: "advisor_halt", Output: "监工叫停：" + err.Error()})
				continue
			}
			log.Printf("agent task=%s iter=%d run %s: %v", taskID, iter, act.ToolID, err)
			history = append(history, stepRecord{ToolID: act.ToolID, Inputs: act.Inputs, ExitCode: -1, Status: "run_error", Output: "执行失败：" + err.Error()})
			continue
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
		artPath := filepath.Join(artDir, fmt.Sprintf("step-%d.txt", iter))
		if werr := os.WriteFile(artPath, []byte(content), 0o600); werr != nil {
			log.Printf("agent task=%s iter=%d 写产物失败: %v", taskID, iter, werr)
		}
		out := res.StdoutTail
		if len([]rune(out)) > 500 {
			out = string([]rune(out)[:500])
		}
		status := res.Status
		if noOutput {
			status = "no_output"
		} else if res.Status == "success" {
			completed++
		}
		history = append(history, stepRecord{
			ToolID: act.ToolID, Inputs: act.Inputs,
			ExitCode: res.ExitCode, Status: status, Output: out, NoOutput: noOutput,
		})
		log.Printf("agent task=%s iter=%d %s exit=%d status=%s", taskID, iter, act.ToolID, res.ExitCode, status)
	}
	// 停下：要么卡住（连续失败），要么碰到最后防线。先看调查后是否缺交付报告：
	// 缺就综合一份，综合出来算交付完成；综合失败或本来就不需要报告，
	// 才回步骤汇总。
	if a.needsReport(taskID, history) {
		report, serr := a.synthesize(ctx, taskID, taskText, memSummary, history, artDir)
		if serr == nil {
			log.Printf("agent task=%s 停下后综合出报告", taskID)
			return &RunResult{ReplyDraft: report, Status: "done", CompletedSteps: completed}, nil
		}
		log.Printf("agent task=%s synthesize: %v", taskID, serr)
	}
	var b strings.Builder
	b.WriteString("任务未完成，已跑步骤如下：\n")
	for i, s := range history {
		fmt.Fprintf(&b, "%d. %s → exit=%d status=%s\n", i+1, s.ToolID, s.ExitCode, s.Status)
	}
	b.WriteString("请根据以上结果继续处理，或告诉用户还差什么。")
	if err := os.WriteFile(filepath.Join(artDir, "reply.md"), []byte(b.String()), 0o600); err != nil {
		log.Printf("agent task=%s 写 reply.md 失败: %v", taskID, err)
	}
	return &RunResult{ReplyDraft: b.String(), Status: "max_iterations", CompletedSteps: completed}, nil
}

// WorkDir 返回任务的工作目录（与规划器旧口径一致，供监工证据用）。
func (a *Agent) WorkDir(taskID string) string {
	return filepath.Join(a.DataDir, "workspace", taskID)
}
