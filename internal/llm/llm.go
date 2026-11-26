// Package llm 是 007 超级 Agent 的 DeepSeek 模型客户端（接通大脑）。
//
// 设计依据（《超级Agent-全新设计方案》v1.5，第 1、2、4 轮）：
//   - Tier-1 分诊：固定前缀、短 JSON 输出；模型调用由 triage 包注入
//     ModelCallFunc，本包的 TriageCall 就是那个注入函数。
//   - DeepSeek 缓存分区：A 冻结前缀（宪法摘要 + L1 索引 + 分诊提示词头，
//     放最前、内容固定才能命中缓存）/ A2 换页摘要 / B 只追加历史 /
//     C 尾部懒加载。BuildMessages 保证这个顺序，绝不把可变内容插进 A 区。
//   - 费用按官方价算，整数毫分（0.001 元）记进 ledger.db expense_ledger：
//     V4.1 Flash 非高峰：输入缓存命中 ¥0.02/百万 token、缓存未命中 ¥1/百万、
//     输出 ¥4/百万（高峰 2 倍）；命中价是未命中价的 1/50。
//     费率出处：2026-11-17 实测的 DeepSeek 官方定价，见设计方案。
//   - 密钥只从环境变量或 600 权限 .env 读键名 DEEPSEEK_API_KEY，
//     值永远不进日志、不进账本、不进代码与测试数据。
//   - 调用失败返回错误，不伪造回复；参数化调用的参数只做最小校验
//     （空串、NUL 字节拒绝）。argv 经 exec 直接传给进程，不经过
//     /bin/sh，shell 元字符只是普通数据，不拦截（B-04 修复：
//     2026-11-18 ping0 真机测试里，正则参数里的 |() 曾被误杀）。
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"007-super-agent/internal/executor"
	"007-super-agent/internal/ledger"
	"007-super-agent/internal/memory"
	"007-super-agent/internal/registry"
)

// DeepSeek 兼容 OpenAI 的地址与默认超时。
const (
	DefaultBaseURL = "https://api.deepseek.com"
	DefaultTimeout = 120 * time.Second
)

// V4.1 Flash 定价（毫分/百万 token，1 毫分 = 0.001 元；2026-11-17 官方价）。
const (
	priceHitMilliPerM  = 20   // 输入缓存命中 ¥0.02/百万
	priceMissMilliPerM = 1000 // 输入缓存未命中 ¥1/百万
	priceOutMilliPerM  = 4000 // 输出 ¥4/百万
	tokensPerMillion   = 1000000
)

// V4 Pro 定价（毫分/百万 token，非高峰；官方价 $0.022/$0.66/$1.98 × 6.67）。
const (
	priceProHitMilliPerM  = 147   // 输入缓存命中 ¥0.147/百万
	priceProMissMilliPerM = 4402  // 输入缓存未命中 ¥4.402/百万
	priceProOutMilliPerM  = 13207 // 输出 ¥13.207/百万
)

// modelPrices 是各模型的非高峰价表（毫分/百万 token）。
// deepseek-reasoner / deepseek-chat 是已退役旧别名：实测核实（2026-11-23，
// 直接调 API，model=deepseek-reasoner 返回 200 且响应里 model=deepseek-flash），
// 两者仍被接受但由 V4.1 Flash 提供服务、按 Flash 价计费；未知模型名默认按
// Flash 价。不要按旧 R1 价 $0.55/$2.19 算，那是已下架的历史价；也不要按
// V4 Pro 价算——实测证明 reasoner 调用实际走的是 Flash，不是 Pro。
func modelPrices(model string) (hit, miss, out int64) {
	switch model {
	case "deepseek-v4-pro":
		return priceProHitMilliPerM, priceProMissMilliPerM, priceProOutMilliPerM
	case "deepseek-reasoner", "deepseek-chat":
		// 已退役别名，实测由 Flash 提供服务，按 Flash 价计费。
		return priceHitMilliPerM, priceMissMilliPerM, priceOutMilliPerM
	default: // deepseek-flash 及其他
		return priceHitMilliPerM, priceMissMilliPerM, priceOutMilliPerM
	}
}

// CostMilliForModel 按模型名算一次调用的费用（毫分，整数运算，
// 四舍五入到毫分；Peak 为真时高峰价 2 倍）。
func CostMilliForModel(model string, hit, miss, out int64, peak bool) int64 {
	ph, pm, po := modelPrices(model)
	total := hit*ph + miss*pm + out*po
	milli := (total + tokensPerMillion/2) / tokensPerMillion
	if peak {
		milli *= 2
	}
	return milli
}

// CostMilli 算一次调用的费用（毫分，整数运算，四舍五入到毫分；
// Peak 为真时高峰价 2 倍）。命中价是未命中价的 1/50。
// 保留给旧调用方：默认按 deepseek-flash 价。
func CostMilli(hit, miss, out int64, peak bool) int64 {
	return CostMilliForModel("deepseek-flash", hit, miss, out, peak)
}

// Message 是一条聊天消息（OpenAI 兼容格式）。
// P1：原生工具调用——assistant 消息可带 ToolCalls；tool 角色的消息
// 用 ToolCallID 指回它回答的那次调用，Content 是工具返回的结果。
// 对齐二号三号（2026-11-25）：Content 永远带上（空正文发 ""），
// 绝不用 omitempty——DeepSeek 要求每条消息都有 content 字段，
// 空正文被省掉会整单报 422（cf-register-gonglue-010 连着 3 次）。
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// ToolMessage 组装一条 tool 角色的结果消息：内容为空时保底填
// "(empty output)"（M-4：只读 bash 执行成功但无任何输出时，如 grep
// 无匹配、ls 空目录，Content 为 ""；部分 OpenAI 兼容网关/代理会
// 拒收空 content 或导致模型行为异常，保底填一句占位）。
func ToolMessage(toolCallID, content string) Message {
	if strings.TrimSpace(content) == "" {
		content = "(empty output)"
	}
	return Message{Role: "tool", ToolCallID: toolCallID, Content: content}
}

// ToolCall 是一次原生工具调用（模型返回的）：ID 供 tool 消息指回，
// Function.Name 是工具编号，Function.Arguments 是 JSON 字符串。
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ToolDefinition 是声明给模型的一个工具（发给 API 的 tools 参数）：
// 每个工具一个 function，参数统一只留 inputs 字符串数组（与现有
// BuildArgv 的 argv 模型一致，模型不用学每把工具的私有参数表）。
type ToolDefinition struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

// InputsSchema 是 registry 工具与内置记忆工具的统一参数表：
// 只有一个 inputs 数组（argv 的一个元素一个字符串），与 BuildArgv 一致。
var InputsSchema = json.RawMessage(`{"type":"object","properties":{"inputs":{"type":"array","items":{"type":"string"}}},"required":["inputs"]}`)

// ExpandSchema 是 expand 导航工具的参数表：category 必填，subcategory 可空。
var ExpandSchema = json.RawMessage(`{"type":"object","properties":{"category":{"type":"string"},"subcategory":{"type":"string"}},"required":["category"]}`)

// NewToolDefinition 按名字 + 说明 + 参数表造一个工具声明。
func NewToolDefinition(name, description string, parameters json.RawMessage) ToolDefinition {
	var t ToolDefinition
	t.Type = "function"
	t.Function.Name = name
	t.Function.Description = description
	t.Function.Parameters = parameters
	return t
}

// Zones 是缓存分区输入：A 冻结前缀放最前且内容固定；A2 换页摘要；
// B 历史只追加；C 尾部懒加载放最后。BuildMessages 保证这个顺序。
type Zones struct {
	Frozen    string    // A 区：宪法摘要 + L1 索引 + 分诊提示词头
	Carryover string    // A2 区：换页摘要（无则留空）
	History   []Message // B 区：只追加的历史（不得改写、不得插队）
	Tail      string    // C 区：本次用户原话 / 尾部懒加载内容
}

// BuildMessages 按 A → A2 → B → C 拼消息。A 区是 system，
// 换页摘要单独一条 system（内容固定才能命中缓存），历史原样追加，
// 尾部是本次 user。Tail 为空时不拼最后一条。
func BuildMessages(z Zones) []Message {
	msgs := make([]Message, 0, len(z.History)+3)
	if strings.TrimSpace(z.Frozen) != "" {
		msgs = append(msgs, Message{Role: "system", Content: z.Frozen})
	}
	if strings.TrimSpace(z.Carryover) != "" {
		msgs = append(msgs, Message{Role: "system", Content: "换页摘要（上轮会话压缩，200K 换页链）：\n" + z.Carryover})
	}
	msgs = append(msgs, z.History...)
	if strings.TrimSpace(z.Tail) != "" {
		msgs = append(msgs, Message{Role: "user", Content: z.Tail})
	}
	return msgs
}

// Usage 是一次调用的 token 用量（直接取自 API 返回的 usage）。
type Usage struct {
	PromptTokens     int64
	CacheHitTokens   int64
	CacheMissTokens  int64
	CompletionTokens int64
}

// Client 是 DeepSeek 客户端。
// Role 是本次调用的记账角色（规划 v2）：front_plan / front_judge /
// executor；空串表示未分角色。Client 是值字段为主的结构（无互斥锁），
// withRole 浅拷贝后改 Role 再调用是安全的（并发任务各用各的拷贝）。
type Client struct {
	Model       string
	BaseURL     string
	APIKey      string
	Timeout     time.Duration
	Peak        bool    // 高峰价 2 倍，默认 false（非高峰价）
	Temperature float64 // 默认 0.2，分诊要确定性
	Role        string  // 记账角色（规划 v2 分角色记账）
	Ledger      *ledger.Ledger
	SessionID   string
	TaskID      string
	HTTP        *http.Client // nil 时用默认（带 Timeout）
}

// New 建一个客户端。APIKey 为空时 Chat 调用会明确报错，不静默。
func New(model, apiKey string) *Client {
	return &Client{
		Model: model, APIKey: apiKey,
		BaseURL: DefaultBaseURL, Timeout: DefaultTimeout,
		Temperature: 0.2,
	}
}

// ResolveAPIKey 找密钥：先环境变量 DEEPSEEK_API_KEY，再按
// $SUPER_AGENT_ENV → /etc/super-agent/.env → ./.env 的顺序读 .env。
// 只返回“找到与否”，值本身只交给调用方，绝不写日志。
func ResolveAPIKey() (string, bool) {
	if v := os.Getenv("DEEPSEEK_API_KEY"); v != "" {
		return v, true
	}
	for _, cand := range []string{os.Getenv("SUPER_AGENT_ENV"), "/etc/super-agent/.env", "./.env"} {
		if cand == "" {
			continue
		}
		kv, err := parseEnvFile(cand)
		if err != nil || kv == nil {
			continue
		}
		if v, ok := kv["DEEPSEEK_API_KEY"]; ok && v != "" {
			return v, true
		}
	}
	return "", false
}

// parseEnvFile 读 KEY=VALUE 文件（与 config 包同口径：跳过空行与 # 注释，
// 值两侧引号剥掉）。只给 ResolveAPIKey 用。
func parseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && v[0] == v[len(v)-1] && (v[0] == '"' || v[0] == '\'') {
			v = v[1 : len(v)-1]
		}
		if k != "" {
			out[k] = v
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

type chatRequest struct {
	Model       string           `json:"model"`
	Messages    []Message        `json:"messages"`
	Temperature float64          `json:"temperature"`
	Tools       []ToolDefinition `json:"tools,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content   string     `json:"content"`
			ToolCalls []ToolCall `json:"tool_calls"`
		} `json:"message"`
		// FinishReason 是模型停下的原因（stop / length / tool_calls /
		// content_filter）。length 表示输出被截断：本轮内容不可信，
		// 必须整轮作废、不得执行其中的工具调用、不得当答案收下。
		// 注意：DeepSeek 有时不带 finish_reason（实测 7 回合里有 1 回合
		// 缺失，Python 流式脚本测的；Go 生产代码走非流式 POST），缺失按
		// “未知”处理，不当成 stop；但截断时 length 会返回
		// （max_tokens=1 实测），截断拦截不受影响。
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		PromptCacheHit   int64 `json:"prompt_cache_hit_tokens"`
		PromptCacheMiss  int64 `json:"prompt_cache_miss_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`
}

// Chat 调一次 /v1/chat/completions，返回模型原文与用量。
// APIKey 为空、非 200、choices 为空都返回错误，不伪造回复。
// 成功后把费用如实记进 expense_ledger（task_id 取 c.TaskID）；
// 记账失败只记日志，不吞掉回复。
// 要按任务归属记账时请用 ChatTask（传本次 taskID），不要共用 c.TaskID。
func (c *Client) Chat(ctx context.Context, msgs []Message) (string, Usage, error) {
	return c.chatWithTask(ctx, msgs, c.TaskID)
}

// ChatTask 与 Chat 相同，但费用行按传入的 taskID 归属。
// 分诊、精炼、监工等每次调用都要带本次 taskID，费用才能按任务查。
func (c *Client) ChatTask(ctx context.Context, msgs []Message, taskID string) (string, Usage, error) {
	return c.chatWithTask(ctx, msgs, taskID)
}

// chatWithTask 是 Chat 的真实实现（不返回 finish_reason 的兼容入口）。
func (c *Client) chatWithTask(ctx context.Context, msgs []Message, taskID string) (string, Usage, error) {
	reply, _, u, err := c.chatWithTaskMeta(ctx, msgs, taskID)
	return reply, u, err
}

// chatWithTaskMeta 与 chatWithTask 相同，但额外返回 finish_reason：
// length 表示输出被截断，调用方必须整轮作废，不得执行、不得当答案收下。
// finish_reason 缺失时按“未知”处理（不触发截断拦截；调用方按有无
// tool_calls 决定是执行还是交活，不当成 stop 的保证）。
func (c *Client) chatWithTaskMeta(ctx context.Context, msgs []Message, taskID string) (string, string, Usage, error) {
	m, fr, u, err := c.chatWithTaskMetaTools(ctx, msgs, taskID, nil)
	if err != nil {
		return "", "", Usage{}, err
	}
	return m.Content, fr, u, nil
}

// chatWithTaskMetaTools 是带原生工具声明的调用：tools 非空时发给 API
// 的 tools 参数，模型回 tool_calls；返回整条 assistant 消息（正文 +
// tool_calls）与 finish_reason。tools 为空时与 chatWithTaskMeta 等价。
func (c *Client) chatWithTaskMetaTools(ctx context.Context, msgs []Message, taskID string, tools []ToolDefinition) (Message, string, Usage, error) {
	if strings.TrimSpace(c.APIKey) == "" {
		return Message{}, "", Usage{}, errors.New("llm: 未配置 DEEPSEEK_API_KEY")
	}
	if len(msgs) == 0 {
		return Message{}, "", Usage{}, errors.New("llm: 消息为空")
	}
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	body, err := json.Marshal(chatRequest{Model: c.Model, Messages: msgs, Temperature: c.Temperature, Tools: tools})
	if err != nil {
		return Message{}, "", Usage{}, fmt.Errorf("llm: 组请求失败: %w", err)
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Message{}, "", Usage{}, fmt.Errorf("llm: 建请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: timeout}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return Message{}, "", Usage{}, fmt.Errorf("llm: 调用失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Message{}, "", Usage{}, fmt.Errorf("llm: 读响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Message{}, "", Usage{}, fmt.Errorf("llm: API 返回 %d: %s", resp.StatusCode, truncateRunes(string(raw), 600))
	}
	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return Message{}, "", Usage{}, fmt.Errorf("llm: 响应解析失败: %w", err)
	}
	if len(cr.Choices) == 0 {
		return Message{}, "", Usage{}, errors.New("llm: 响应无 choices")
	}
	u := Usage{
		PromptTokens:     cr.Usage.PromptTokens,
		CacheHitTokens:   cr.Usage.PromptCacheHit,
		CacheMissTokens:  cr.Usage.PromptCacheMiss,
		CompletionTokens: cr.Usage.CompletionTokens,
	}
	c.recordExpense(u, taskID)
	m := Message{
		Role:      "assistant",
		Content:   cr.Choices[0].Message.Content,
		ToolCalls: cr.Choices[0].Message.ToolCalls,
	}
	// M-4：模型回的 tool_call 若 Arguments 为空串 ""（不是 "{}"），
	// DeepSeek 也会报 422；这里统一保底填 "{}"，调用方解析不再报错。
	for i := range m.ToolCalls {
		if strings.TrimSpace(m.ToolCalls[i].Function.Arguments) == "" {
			m.ToolCalls[i].Function.Arguments = "{}"
		}
	}
	return m, cr.Choices[0].FinishReason, u, nil
}

// recordExpense 把一次调用的费用记进 expense_ledger（按 taskID 归属）。
// 失败只记日志，不阻塞、不吞回复（账本坏了不该让一次好回答作废）。
// Role 随行记账（规划 v2 分角色记账；空串表示未分角色）。
func (c *Client) recordExpense(u Usage, taskID string) {
	if c.Ledger == nil {
		return
	}
	cost := CostMilliForModel(c.Model, u.CacheHitTokens, u.CacheMissTokens, u.CompletionTokens, c.Peak)
	_, err := c.Ledger.RecordExpense(ledger.ExpenseRecord{
		SessionID: c.SessionID, TaskID: taskID, Model: c.Model, Role: c.Role,
		PromptTokens: u.PromptTokens, CacheHitTokens: u.CacheHitTokens,
		CacheMissTokens: u.CacheMissTokens, CompletionTokens: u.CompletionTokens,
		CostCNYMilli: cost,
	})
	if err != nil {
		log.Printf("llm: record expense failed: %v", err)
	}
}

// withRole 返回一个 Role 被改成指定值的浅拷贝客户端（规划 v2）：
// Client 无互斥锁，浅拷贝安全；原客户端的 Role 不变，并发任务各用
// 各的拷贝，不会互相串角色。费用行按拷贝的 Role 记账。
func (c *Client) withRole(role string) *Client {
	cp := *c
	cp.Role = role
	return &cp
}

// CallTaskRole 与 CallTask 相同，但本次调用按指定角色记账（规划 v2）。
func (c *Client) CallTaskRole(taskID, frozen, tail, role string) (string, int64, int64, error) {
	return c.withRole(role).CallTask(taskID, frozen, tail)
}

// CallTaskWithMetaRole 与 CallTaskWithMeta 相同，但按指定角色记账。
func (c *Client) CallTaskWithMetaRole(taskID, frozen, tail, role string) (string, string, int64, int64, error) {
	return c.withRole(role).CallTaskWithMeta(taskID, frozen, tail)
}

// CallTaskFullRole 与 CallTaskFull 相同，但按指定角色记账（规划 v2）。
func (c *Client) CallTaskFullRole(taskID, frozen, tail string, tools []ToolDefinition, history []Message, role string) (Message, string, int64, int64, error) {
	return c.withRole(role).CallTaskFull(taskID, frozen, tail, tools, history)
}

// CallTaskContext 与 CallTask 相同，但调用方透传 ctx（超时由调用方定）。
// Gemini 审阅 M-5：翻新摘要的 420s 超时要真正生效，不能走
// context.Background() + 客户端默认超时。
func (c *Client) CallTaskContext(ctx context.Context, taskID, frozen, tail string) (string, int64, int64, error) {
	msgs := BuildMessages(Zones{Frozen: frozen, Tail: tail})
	reply, u, err := c.chatWithTask(ctx, msgs, taskID)
	if err != nil {
		return "", 0, 0, err
	}
	return reply, u.PromptTokens, u.CompletionTokens, nil
}

// CallTaskContextRole 与 CallTaskContext 相同，但本次调用按指定角色记账。
func (c *Client) CallTaskContextRole(ctx context.Context, taskID, frozen, tail, role string) (string, int64, int64, error) {
	return c.withRole(role).CallTaskContext(ctx, taskID, frozen, tail)
}

// CallTask 是通用的带任务归属的模型调用：frozen（冻结提示词 + 清单）
// 放 system 消息最前（缓存分区 A 区），tail 放最后一条 user 消息
// （C 区）。费用按 taskID 归属记账。TriageCall、规划器的 Plan/Verify
// 调用都走这里，不各写一套 HTTP 逻辑。
func (c *Client) CallTask(taskID, frozen, tail string) (string, int64, int64, error) {
	msgs := BuildMessages(Zones{Frozen: frozen, Tail: tail})
	reply, u, err := c.chatWithTask(context.Background(), msgs, taskID)
	if err != nil {
		return "", 0, 0, err
	}
	return reply, u.PromptTokens, u.CompletionTokens, nil
}

// CallTaskWithMeta 与 CallTask 相同，但额外返回 finish_reason（P0-1：
// 执行会话用它拦截截断。length 时调用方必须整轮作废）。
func (c *Client) CallTaskWithMeta(taskID, frozen, tail string) (string, string, int64, int64, error) {
	msgs := BuildMessages(Zones{Frozen: frozen, Tail: tail})
	reply, fr, u, err := c.chatWithTaskMeta(context.Background(), msgs, taskID)
	if err != nil {
		return "", "", 0, 0, err
	}
	return reply, fr, u.PromptTokens, u.CompletionTokens, nil
}

// CallTaskFull 是 P1 的原生工具调用：frozen 放 system（A 区），history
// 是结构化对话历史（assistant 的 tool_calls + tool 的结果，原样追加，
// 不得改写），tail 放最后一条 user 消息（C 区）；tools 声明给模型。
// 返回整条 assistant 消息（正文 + tool_calls）、finish_reason 与用量。
// 模型不调工具只回正文时，ToolCalls 为空，正文就是它要停下交的答案。
func (c *Client) CallTaskFull(taskID, frozen, tail string, tools []ToolDefinition, history []Message) (Message, string, int64, int64, error) {
	msgs := BuildMessages(Zones{Frozen: frozen, History: history, Tail: tail})
	m, fr, u, err := c.chatWithTaskMetaTools(context.Background(), msgs, taskID, tools)
	if err != nil {
		return Message{}, "", 0, 0, err
	}
	return m, fr, u.PromptTokens, u.CompletionTokens, nil
}

// TriageCall 是给 triage.Triage 注入的 ModelCallFunc：
// frozen（分诊提示词头 + 可用工具清单）放 system 消息最前（缓存分区 A 区，
// 跨调用内容固定才能命中缓存），tail（本次用户原话）放最后一条 user
// 消息（C 区）。费用按 taskID 归属记账。非法 JSON 由 triage.ParseTriageReply 校验。
func (c *Client) TriageCall(taskID, frozen, tail string) (string, int64, int64, error) {
	return c.CallTask(taskID, frozen, tail)
}

// TriageCallZones 是带缓存分区的分诊调用：A 区放冻结前缀（分诊提示词头
// 由 BuildTriagePrompt 自带，这里只补宪法摘要 + L1 索引），C 区放用户原话。
// 费用按 c.TaskID 归属；要按任务归属请用 TriageCall。
func (c *Client) TriageCallZones(frozen, carryover string, history []Message, tail string) (string, int64, int64, error) {
	msgs := BuildMessages(Zones{Frozen: frozen, Carryover: carryover, History: history, Tail: tail})
	reply, u, err := c.chatWithTask(context.Background(), msgs, c.TaskID)
	if err != nil {
		return "", 0, 0, err
	}
	return reply, u.PromptTokens, u.CompletionTokens, nil
}

var (
	// ErrBadArg 是参数非法：空串或含 NUL 字节（exec 传不进去）。
	// 注意：shell 元字符（; & | ` $ () 与换行）不在此列——argv 经
	// exec.Command 直接传给进程，不经过 /bin/sh，这些字符只是普通
	// 数据。2026-11-18 ping0 真机测试：规划器步骤里的正则参数
	// "(?<=src=\")[^\"]+\\.js..." 含 |() 曾被旧版白名单误杀，
	// 导致整个多步任务 plan_error。不可逆动作由监工把关，不由这里把关。
	ErrBadArg = errors.New("llm: 参数非法，拒绝执行")
	// ErrEmptyEntryPoint 是总账 entry_point 为空。
	ErrEmptyEntryPoint = errors.New("llm: 工具 entry_point 为空")
)

// ValidateArgs 做最小校验：空参数串拒绝（防空串拼出歧义命令）；
// 含 NUL 字节的拒绝（exec 系统调用传不进去）。
// shell 元字符不拦：见 ErrBadArg 上的说明。
func ValidateArgs(args []string) error {
	for _, a := range args {
		if a == "" {
			return fmt.Errorf("%w: 空参数", ErrBadArg)
		}
		if strings.ContainsRune(a, 0) {
			return fmt.Errorf("%w: 参数含 NUL 字节: %q", ErrBadArg, a)
		}
	}
	return nil
}

// BuildArgv 按总账 entry_point + 模型按 L3 拼出的参数拼命令。
// 参数先过白名单；entry_point 为空或工具不可用都报错，不硬凑。
func BuildArgv(reg *registry.Registry, toolID string, args []string) ([]string, error) {
	e, ok := reg.Entry(toolID)
	if !ok {
		return nil, fmt.Errorf("llm: 工具 %q 不在总账里", toolID)
	}
	if !e.Available {
		return nil, fmt.Errorf("llm: 工具 %q 不可调用 (available:false)", toolID)
	}
	argv := strings.Fields(e.EntryPoint)
	if len(argv) == 0 {
		return nil, ErrEmptyEntryPoint
	}
	// 模型常把工具名也写进 inputs（照着 L3 里的整行命令抄），
	// 首参数与命令名重复时直接去掉——argv[0] 已是工具名，
	// 再拼一遍会让它变成传给工具的第一个参数（比如 Chrome 会
	// 把 "chrome-nosandbox" 当 URL 去打开）。只去一个，且只当
	// 首参数与命令名字面相等时才去，别的参数原样保留。
	if len(args) > 0 && args[0] == argv[0] {
		args = args[1:]
	}
	if err := ValidateArgs(args); err != nil {
		return nil, err
	}
	return append(argv, args...), nil
}

// RunToolWithArgs 是参数化调用：拼好命令交给 executor 跑。
// 只拼、不直接跑 shell：argv 按 exec 语义传，不经过 /bin/sh。
// 超时默认 30 秒内联；>30 秒 executor 按既定纪律转后台。
func RunToolWithArgs(reg *registry.Registry, ex *executor.Executor, toolID string, args []string, taskID, sessionID, text string) (*executor.Result, *executor.Handle, error) {
	argv, err := BuildArgv(reg, toolID, args)
	if err != nil {
		return nil, nil, err
	}
	return ex.Run(executor.RunRequest{
		TaskID: taskID, SessionID: sessionID, ToolID: toolID,
		Argv: argv, Timeout: 30 * time.Second, Text: text,
	})
}

// refineSystemPrompt 是每日精炼的冻结提示词（放 A 区，内容固定才能命中缓存）：
// 只输出 JSON 数组，每条一个 Wiki 词条；不写解释、不写多余文字。
const refineSystemPrompt = `你是超级 Agent 的记忆精炼器。把下面一天的 L1 日志提炼成 Wiki 词条。
只输出一个 JSON 数组，不要任何解释、不要 markdown 代码围栏。数组每条的格式：
{"kind":"concepts","title":"词条名","summary":"一句话摘要","body":"正文，多行文字"}
kind 只能是 entities/concepts/sources/synthesis 四个之一；title 不含 / 与 ..；
只提炼值得长期记住的事实、结论、约定，流水、重复、无意义的行跳过；
没有值得记的内容就输出空数组 []。`

// refineL1Entry 是模型回的单条词条（先按这个结构解析，再转 memory.WikiEntry）。
type refineL1Entry struct {
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Summary string `json:"summary"`
	Body    string `json:"body"`
}

// MaxRefineRunes 是单次精炼送进模型的 L1 文本上限（字符，先转 []rune 切），
// 防一天日志过长导致费用失控；超了只取末尾（最近的内容最值得提炼）。
const MaxRefineRunes = 12000

// RefineL1 把一天的 L1 日志文本提炼成 Wiki 条目：调一次 Chat，
// 提示词放 A 区（冻结）、L1 文本放 C 区。模型回的 JSON 数组解析后返回；
// kind/title 非法由 WriteWiki 再拦一遍，这里只做解析，不伪造条目。
// L1 文本为空时直接回空数组，不调模型。费用按 taskID 归属记账
// （调用方传 refine-YYYY-MM-DD）。
func (c *Client) RefineL1(ctx context.Context, taskID, l1Text string) ([]memory.WikiEntry, error) {
	if strings.TrimSpace(l1Text) == "" {
		return nil, nil
	}
	if utf8.RuneCountInString(l1Text) > MaxRefineRunes {
		l1Text = string([]rune(l1Text)[utf8.RuneCountInString(l1Text)-MaxRefineRunes:])
	}
	msgs := BuildMessages(Zones{Frozen: refineSystemPrompt, Tail: l1Text})
	reply, _, err := c.chatWithTask(ctx, msgs, taskID)
	if err != nil {
		return nil, err
	}
	reply = strings.TrimSpace(reply)
	reply = strings.TrimPrefix(reply, "```json")
	reply = strings.TrimPrefix(reply, "```")
	reply = strings.TrimSuffix(reply, "```")
	reply = strings.TrimSpace(reply)
	var raw []refineL1Entry
	if err := json.Unmarshal([]byte(reply), &raw); err != nil {
		return nil, fmt.Errorf("llm: 精炼回复不是合法 JSON 数组: %w", err)
	}
	out := make([]memory.WikiEntry, 0, len(raw))
	for _, e := range raw {
		out = append(out, memory.WikiEntry{
			Kind:    memory.WikiKind(e.Kind),
			Title:   e.Title,
			Summary: e.Summary,
			Body:    e.Body,
		})
	}
	return out, nil
}

// reviewSelfEvolvePrompt 是每日自查的冻结提示词（放 A 区，内容固定才能命中缓存）：
// 只输出一个 JSON 对象，不要任何解释、不要 markdown 代码围栏。
// 模型只点评信号，不编造信号之外的提议；阈值（同一签名 7 天内 ≥3 次）
// 与分档（哪些能自动应用）由调用方代码强制执行，不由模型决定。
const reviewSelfEvolvePrompt = `你是超级 Agent 的自进化审查器。只看下面 7 天账本聚合信号（每行“签名: 次数”，费用突增是天级信号），输出一个 JSON 对象，不要任何解释、不要 markdown 代码围栏。
对象格式：
{"lessons":["教训1","教训2"],"proposals":[{"signature":"exec_fail:cap-code-run-audit","kind":"registry_entrypoint_fix","tool_id":"cap-code-run-audit","new_entry_point":"tools/scripts/run-audit.sh","text":"提议内容一句话"}]}
规则：
- lessons：每条一句话，写值得长期记住的教训；没有就空数组。
- proposals：只给出现 3 次及以上的问题提建议；kind 只能是 registry_entrypoint_fix（修注册表 entry_point 路径）、registry_verified（工具已修好、重新探针验证）、constitution（改宪法）、triage_prompt（改分诊提示词）、code（改执行层/网关代码）、other；registry_entrypoint_fix 必须给 tool_id 与 new_entry_point；与信号无关的提议不许编。
- 只输出 JSON 对象。`

// MaxReviewRunes 是单次自查送进模型的信号文本上限（字符，先转 []rune 切），
// 防信号过多导致费用失控；超了只留计数行，丢错误样本。
const MaxReviewRunes = 8000

// ReviewSelfEvolve 调一次 worker 模型做每日自查点评：提示词放 A 区（冻结）、
// 信号文本放 C 区。返回模型原文（JSON 对象，调用方解析）；费用按 taskID
// 归属记账（调用方传 selfevolve-YYYY-MM-DD）。信号文本为空时直接报错，
// 不调模型、不花钱。
func (c *Client) ReviewSelfEvolve(ctx context.Context, taskID, signalsText string) (string, error) {
	if strings.TrimSpace(signalsText) == "" {
		return "", errors.New("llm: 自查信号为空")
	}
	if utf8.RuneCountInString(signalsText) > MaxReviewRunes {
		signalsText = string([]rune(signalsText)[:MaxReviewRunes])
	}
	msgs := BuildMessages(Zones{Frozen: reviewSelfEvolvePrompt, Tail: signalsText})
	reply, _, err := c.chatWithTask(ctx, msgs, taskID)
	if err != nil {
		return "", err
	}
	return reply, nil
}

// truncateRunes 按字符截断到 n 个字（先转 []rune，防中文乱码）。
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
