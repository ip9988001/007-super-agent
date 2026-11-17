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
//   - 调用失败返回错误，不伪造回复；参数化调用的参数先过白名单，
//     含 shell 元字符直接拒绝、不执行。
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

// CostMilli 算一次调用的费用（毫分，整数运算，四舍五入到毫分；
// Peak 为真时高峰价 2 倍）。命中价是未命中价的 1/50。
func CostMilli(hit, miss, out int64, peak bool) int64 {
	total := hit*priceHitMilliPerM + miss*priceMissMilliPerM + out*priceOutMilliPerM
	milli := (total + tokensPerMillion/2) / tokensPerMillion
	if peak {
		milli *= 2
	}
	return milli
}

// Message 是一条聊天消息（OpenAI 兼容格式）。
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
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
type Client struct {
	Model       string
	BaseURL     string
	APIKey      string
	Timeout     time.Duration
	Peak        bool    // 高峰价 2 倍，默认 false（非高峰价）
	Temperature float64 // 默认 0.2，分诊要确定性
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
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
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
// 成功后把费用如实记进 expense_ledger；记账失败只记日志，不吞掉回复。
func (c *Client) Chat(ctx context.Context, msgs []Message) (string, Usage, error) {
	if strings.TrimSpace(c.APIKey) == "" {
		return "", Usage{}, errors.New("llm: 未配置 DEEPSEEK_API_KEY")
	}
	if len(msgs) == 0 {
		return "", Usage{}, errors.New("llm: 消息为空")
	}
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	body, err := json.Marshal(chatRequest{Model: c.Model, Messages: msgs, Temperature: c.Temperature})
	if err != nil {
		return "", Usage{}, fmt.Errorf("llm: 组请求失败: %w", err)
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", Usage{}, fmt.Errorf("llm: 建请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: timeout}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", Usage{}, fmt.Errorf("llm: 调用失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", Usage{}, fmt.Errorf("llm: 读响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", Usage{}, fmt.Errorf("llm: API 返回 %d: %s", resp.StatusCode, truncateRunes(string(raw), 600))
	}
	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return "", Usage{}, fmt.Errorf("llm: 响应解析失败: %w", err)
	}
	if len(cr.Choices) == 0 {
		return "", Usage{}, errors.New("llm: 响应无 choices")
	}
	u := Usage{
		PromptTokens:     cr.Usage.PromptTokens,
		CacheHitTokens:   cr.Usage.PromptCacheHit,
		CacheMissTokens:  cr.Usage.PromptCacheMiss,
		CompletionTokens: cr.Usage.CompletionTokens,
	}
	c.recordExpense(u)
	return cr.Choices[0].Message.Content, u, nil
}

// recordExpense 把一次调用的费用记进 expense_ledger。
// 失败只记日志，不阻塞、不吞回复（账本坏了不该让一次好回答作废）。
func (c *Client) recordExpense(u Usage) {
	if c.Ledger == nil {
		return
	}
	cost := CostMilli(u.CacheHitTokens, u.CacheMissTokens, u.CompletionTokens, c.Peak)
	_, err := c.Ledger.RecordExpense(ledger.ExpenseRecord{
		SessionID: c.SessionID, TaskID: c.TaskID, Model: c.Model,
		PromptTokens: u.PromptTokens, CacheHitTokens: u.CacheHitTokens,
		CacheMissTokens: u.CacheMissTokens, CompletionTokens: u.CompletionTokens,
		CostCNYMilli: cost,
	})
	if err != nil {
		log.Printf("llm: record expense failed: %v", err)
	}
}

// TriageCall 是给 triage.Triage 注入的 ModelCallFunc：
// 把 BuildTriagePrompt 的输出当一条 user 消息发给模型，
// 返回模型原文 + 输入/输出 token 数。非法 JSON 由 triage.ParseTriageReply 校验。
func (c *Client) TriageCall(prompt string) (string, int64, int64, error) {
	reply, u, err := c.Chat(context.Background(), []Message{{Role: "user", Content: prompt}})
	if err != nil {
		return "", 0, 0, err
	}
	return reply, u.PromptTokens, u.CompletionTokens, nil
}

// TriageCallZones 是带缓存分区的分诊调用：A 区放冻结前缀（分诊提示词头
// 由 BuildTriagePrompt 自带，这里只补宪法摘要 + L1 索引），C 区放用户原话。
func (c *Client) TriageCallZones(frozen, carryover string, history []Message, tail string) (string, int64, int64, error) {
	msgs := BuildMessages(Zones{Frozen: frozen, Carryover: carryover, History: history, Tail: tail})
	reply, u, err := c.Chat(context.Background(), msgs)
	if err != nil {
		return "", 0, 0, err
	}
	return reply, u.PromptTokens, u.CompletionTokens, nil
}

var (
	// ErrBadArg 是参数含 shell 元字符。
	ErrBadArg = errors.New("llm: 参数含 shell 元字符，拒绝执行")
	// ErrEmptyEntryPoint 是总账 entry_point 为空。
	ErrEmptyEntryPoint = errors.New("llm: 工具 entry_point 为空")
)

// shellMeta 是参数里不许出现的字符：分号、与、管道、反引号、
// 美元、括号、换行、回车。含了就拒绝，不拼、不执行。
const shellMeta = "; & | ` $ ( ) \n\r"

// ValidateArgs 校验参数白名单：逐个参数查 shell 元字符，
// 含了就报错；空参数串也拒绝（防空串拼出歧义命令）。
func ValidateArgs(args []string) error {
	for _, a := range args {
		if a == "" {
			return fmt.Errorf("%w: 空参数", ErrBadArg)
		}
		if strings.ContainsAny(a, shellMeta) {
			return fmt.Errorf("%w: %q", ErrBadArg, a)
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
// L1 文本为空时直接回空数组，不调模型。
func (c *Client) RefineL1(ctx context.Context, l1Text string) ([]memory.WikiEntry, error) {
	if strings.TrimSpace(l1Text) == "" {
		return nil, nil
	}
	if utf8.RuneCountInString(l1Text) > MaxRefineRunes {
		l1Text = string([]rune(l1Text)[utf8.RuneCountInString(l1Text)-MaxRefineRunes:])
	}
	msgs := BuildMessages(Zones{Frozen: refineSystemPrompt, Tail: l1Text})
	reply, _, err := c.Chat(ctx, msgs)
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

// truncateRunes 按字符截断到 n 个字（先转 []rune，防中文乱码）。
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
