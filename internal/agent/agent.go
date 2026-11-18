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

	"007-super-agent/internal/executor"
	"007-super-agent/internal/ledger"
	"007-super-agent/internal/llm"
	"007-super-agent/internal/registry"
	"007-super-agent/internal/triage"
)

// ModelCallFunc 与 llm.Client.CallTask 同签名：
// 冻结提示词（A 区，可命中缓存）+ 当轮尾巴，返回模型原话与 token 数。
type ModelCallFunc func(taskID, frozen, tail string) (reply string, tokensIn, tokensOut int64, err error)

// DefaultMaxIter 是单任务最多调工具轮数：防模型死循环。
// 正常任务 3-8 轮够用；20 轮是熔断，不是目标。
const DefaultMaxIter = 20

// Agent 是主循环：一次 Run 跑完一个多步任务。
type Agent struct {
	Call      ModelCallFunc // 只用 flash（worker 模型），不用 reasoner
	Reg       *registry.Registry
	Led       *ledger.Ledger
	Ex        *executor.Executor
	DataDir   string // 产物落盘根目录（artifacts/<taskID>/）
	SessionID string
	MaxIter   int // 0 时用 DefaultMaxIter
}

// Action 是模型每轮的决定：调一个工具，或直接交活。
type Action struct {
	Action string   `json:"action"` // "tool" 或 "answer"
	ToolID string   `json:"tool_id,omitempty"`
	Inputs []string `json:"inputs,omitempty"`
	Text   string   `json:"text,omitempty"`
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
只输出一个 JSON 对象，不要任何解释、不要 markdown 代码围栏。对象格式二选一：
{"action":"tool","tool_id":"cap-fetch-curl-jq","inputs":["-sS","-L","https://example.com"]}
{"action":"answer","text":"……（任务的最终回复，直接写给用户）"}
规则：
- 任务做完了就 action=answer，把结果写进 text，不要再调工具；不确定做没做完就再调一个工具确认，不要猜；
- 一步只调一个工具；tool_id 只能从下面可用工具清单里挑，清单外的不许编；不知道调哪个就先调一个最可能有用的，看结果再说；
- inputs 的每个字符串就是命令行上的一个参数（argv 的一个元素），执行时直接拼在工具名后面、不经过 shell：不要写 shell 语法——不要写管道 |、重定向 > >>、分号 ;、&&、$变量、for 循环、反引号，也不要在 inputs 里再写一遍工具名；需要落盘文件时，把绝对路径当参数传给工具（比如 --out /绝对路径/文件），不要写重定向；路径直接写绝对路径，不要用 $变量缩写；
- 不确定的命令先跑 --help 确认用法，不要编参数（比如 playwright screenshot 没有 --out 这个选项，编了会直接报错）；
- 抓网页 HTML 时要带全套浏览器请求头（Accept、Accept-Language、Sec-Fetch-* 等），只带 User-Agent 常被回 40 字节空壳或验证页；拿到验证页（Cloudflare Turnstile、验证码、challenge）或空壳时，换反检测工具再抓——camoufox（反检测浏览器）或 scrapling 的 stealthy_fetch（TLS 指纹伪装），不要在空壳上反复试普通抓取；
- 工具返回失败（exit 非 0、无输出、参数报错）时，先看错误信息换个招再试，不要原样重发同一条命令；同一工具连续失败两次就换别的工具；
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
	if a.Action != "tool" && a.Action != "answer" {
		return a, fmt.Errorf("agent: 未知 action %q", a.Action)
	}
	if a.Action == "tool" && a.ToolID == "" {
		return a, fmt.Errorf("agent: tool 决定缺 tool_id")
	}
	return a, nil
}

// buildTail 拼当轮尾巴：任务 + 记忆摘要 + 已跑步骤的结果。
func buildTail(taskText, memSummary string, history []stepRecord) string {
	var b strings.Builder
	b.WriteString("任务：")
	b.WriteString(taskText)
	b.WriteString("\n")
	if memSummary != "" {
		b.WriteString(memSummary)
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

// Run 跑完一个多步任务：主模型逐轮决定调工具或交活。
// memSummary 是 Recall 前 3 条（可为空）；taskText 是任务原话。
func (a *Agent) Run(ctx context.Context, taskID, taskText, memSummary string) (*RunResult, error) {
	maxIter := a.MaxIter
	if maxIter <= 0 {
		maxIter = DefaultMaxIter
	}
	artDir := filepath.Join(a.DataDir, "artifacts", taskID)
	if err := os.MkdirAll(artDir, 0o755); err != nil {
		return nil, fmt.Errorf("agent: 建产物目录失败: %w", err)
	}
	frozen := agentPromptHead + "可用工具清单（只许从中挑 tool_id，清单外的一律非法）：\n" +
		triage.CompactToolList(a.Reg, nil)
	history := []stepRecord{}
	completed := 0
	for iter := 1; iter <= maxIter; iter++ {
		reply, _, _, err := a.Call(taskID, frozen, buildTail(taskText, memSummary, history))
		if err != nil {
			return nil, fmt.Errorf("agent: 第 %d 轮模型调用失败: %w", iter, err)
		}
		act, perr := parseAction(reply)
		if perr != nil {
			// 坏 JSON 重试一次：把“只返回 JSON”追加进尾巴再调。
			retry, _, _, rerr := a.Call(taskID, frozen, buildTail(taskText, memSummary, history)+"\n注意：上一轮回复不是合法 JSON，只返回 JSON 对象，不要加任何解释文字。")
			if rerr != nil {
				return nil, fmt.Errorf("agent: 第 %d 轮模型调用失败: %w", iter, rerr)
			}
			act, perr = parseAction(retry)
			if perr != nil {
				return nil, fmt.Errorf("agent: 第 %d 轮决定非法: %w", iter, perr)
			}
		}
		if act.Action == "answer" {
			if err := os.WriteFile(filepath.Join(artDir, "reply.md"), []byte(act.Text), 0o600); err != nil {
				return nil, fmt.Errorf("agent: 写 reply.md 失败: %w", err)
			}
			return &RunResult{ReplyDraft: act.Text, Status: "done", CompletedSteps: completed}, nil
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
	// 熔断：到轮数上限还没交活，如实汇总已跑步骤，不编结论。
	var b strings.Builder
	b.WriteString("任务未在轮数上限内完成，已跑步骤如下：\n")
	for i, s := range history {
		fmt.Fprintf(&b, "%d. %s → exit=%d status=%s\n", i+1, s.ToolID, s.ExitCode, s.Status)
	}
	b.WriteString("请根据以上结果继续处理，或告诉用户还差什么。")
	return &RunResult{ReplyDraft: b.String(), Status: "max_iterations", CompletedSteps: completed}, nil
}

// WorkDir 返回任务的工作目录（与规划器旧口径一致，供监工证据用）。
func (a *Agent) WorkDir(taskID string) string {
	return filepath.Join(a.DataDir, "workspace", taskID)
}
