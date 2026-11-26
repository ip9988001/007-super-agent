package summary

import (
	"context"
	"fmt"
	"strings"

	"007-super-agent/internal/llm"
	"007-super-agent/internal/sanitize"
)

// PilotDoc 是一篇试点文档的输入。
type PilotDoc struct {
	DocID      string
	SourcePath string // 相对路径，如 wiki/entities/xxx.md
	Title      string
	Content    []byte // 原文（只读，绝不改写）
}

// RunReport 是一篇试点文档的生成实录（进 manifest，不含任何密钥原文）。
type RunReport struct {
	DocID            string          `json:"doc_id"`
	Attempts         int             `json:"attempts"`
	ModelsUsed       []string        `json:"models_used"`
	StrongUsed       bool            `json:"strong_model_used"`
	PromptTokens     int64           `json:"prompt_tokens"`
	CompletionTokens int64           `json:"completion_tokens"`
	CostMilli        int64           `json:"cost_milli"` // 毫分，1 毫分 = 0.001 元
	CostCNY          float64         `json:"cost_cny"`
	RetryCount       int             `json:"retry_count"`
	Status           Status          `json:"status"`
	AttemptChecks    [][]CheckResult `json:"attempt_checks"`
	FinalChecks      []CheckResult   `json:"final_checks"`
	StrongAvailable  bool            `json:"strong_model_available"`
	StrongProbeNote  string          `json:"strong_probe_note,omitempty"`
}

// chatClient 是生成器依赖的最小模型调用面：llm.Client 直接满足，
// 单测用假实现。费用不走生产账本（调用方不给 Client 配 Ledger），
// 只按返回的 Usage 在本地算成本。
type chatClient interface {
	ChatTask(ctx context.Context, msgs []llm.Message, taskID string) (string, llm.Usage, error)
}

// Generator 是简介生成器：小模型跑首轮，被打回才升强模型重写，
// 连续失败 3 次熔断降级为规则兜底（v2.1 §5/§6）。
type Generator struct {
	Flash           chatClient
	FlashModel      string
	Strong          chatClient // 为 nil 表示无强模型
	StrongModel     string
	StrongAvailable bool
	StrongProbeNote string
	Collision       *CollisionChecker
	// MaxInputRunes 是送模型的正文上限（字符，先转 []rune 切）。
	MaxInputRunes int
}

// CostMilliForUsage 按模型名与 API 返回用量算一次调用费用（毫分）。
// API 未回缓存命中/未命中分项时（两者皆 0），全部 prompt 按未命中价算，
// 不低估成本。
func CostMilliForUsage(model string, u llm.Usage) int64 {
	hit, miss := u.CacheHitTokens, u.CacheMissTokens
	if hit+miss == 0 && u.PromptTokens > 0 {
		miss = u.PromptTokens
	}
	return llm.CostMilliForModel(model, hit, miss, u.CompletionTokens, false)
}

// summaryPrompt 是写简介的冻结提示词（放 system，内容固定）。
const summaryPrompt = `你是记忆简介生成器。只输出两行，不要任何解释、不要 markdown 代码围栏。
第一行以 summary: 开头，写一句话简介；第二行以 triggers: 开头，写触发词，词与词之间用中文逗号分隔。
硬规矩，一个都不能违反：
- summary 30 到 100 单位：1 个汉字算 1 单位，1 个连续英文或数字词算 1 单位；等效宽度不超过 120（汉字数乘 1 加 ASCII 字符数乘 0.5）。
- 触发词 3 到 8 个；触发词与 summary 的词重合率小于 30%，要换说法，不要复述简介里的原词。
- 触发词必须同时覆盖三类：日常口语的换一种说法、中英文与缩写的对照说法、症状与报错类词。
- 触发词禁用泛词：系统、配置、记录、问题、方案、教程，一个都不能出现。
- 简介要写清这篇适用于什么场景、解决什么问题；不要模板空话（本文介绍了、总的来说、综上所述等），不要复读标题。
- 不要在简介里写任何密钥、token、密码原文；只写用途与方法。
`

const playbookRule = `- 这是剧本类文档：summary 或 triggers 里至少含 1 个动作词，如注册、部署、排查、迁移、备份、安装、验证、抓取。`

const antiPatternRule = `- 这是反套路文档：summary 里必须含否定或警示词，如禁止、避免、不要、失效、勿用、踩坑，绝不能写成正面教程。`

// Generate 为一篇试点文档生成简介，跑满质检闭环，返回简介与实录。
func (g *Generator) Generate(ctx context.Context, doc PilotDoc) (Summary, *RunReport, error) {
	rep := &RunReport{DocID: doc.DocID, StrongAvailable: g.StrongAvailable, StrongProbeNote: g.StrongProbeNote}
	maxInput := g.MaxInputRunes
	if maxInput <= 0 {
		maxInput = 6000
	}
	// 脱敏新规矩①：送模型之前先用 sanitize 把输入掩码。
	// 绝不改源文件：只对内存里的副本脱敏、截断。
	raw := TruncateRunes(string(doc.Content), maxInput)
	masked := sanitize.Sanitize(raw)
	isPlaybook := IsPlaybook(doc.SourcePath, doc.Title)
	isAnti := IsAntiPattern(doc.SourcePath)
	kind := "知识文档"
	if isPlaybook {
		kind = "剧本类文档"
	} else if isAnti {
		kind = "反套路文档"
	}
	frozen := summaryPrompt
	if isPlaybook {
		frozen += playbookRule + "\n"
	}
	if isAnti {
		frozen += antiPatternRule + "\n"
	}
	tail := fmt.Sprintf("文档标题：%s\n文档类型：%s\n正文（已脱敏，[REDACTED] 为已遮蔽的凭证串）：\n%s", doc.Title, kind, masked)

	var lastChecks []CheckResult
	var extraNote string
	for attempt := 0; attempt < MaxRetries; attempt++ {
		model, client := g.FlashModel, g.Flash
		prompt := frozen
		if attempt == 0 {
			// 首轮：小模型 + 基础提示词。
		} else if attempt == MaxRetries-1 && g.StrongAvailable && g.Strong != nil {
			// 最后一轮：升强模型重写。
			model, client = g.StrongModel, g.Strong
			rep.StrongUsed = true
			prompt += "\n上一次被打回，必须逐条改正（强模型重写）：\n" + strings.Join(FailedReasons(lastChecks), "\n") + "\n"
			if extraNote != "" {
				prompt += extraNote + "\n"
			}
		} else {
			// 中间轮：小模型 + 更严格的重写提示词。
			prompt += "\n上一次被打回，必须逐条改正（严格重写，不要只换几个词）：\n" + strings.Join(FailedReasons(lastChecks), "\n") + "\n"
			if extraNote != "" {
				prompt += extraNote + "\n"
			}
		}
		rep.ModelsUsed = append(rep.ModelsUsed, model)
		msgs := llm.BuildMessages(llm.Zones{Frozen: prompt, Tail: tail})
		taskID := fmt.Sprintf("summary-pilot-%s-a%d", doc.DocID, attempt+1)
		reply, usage, err := client.ChatTask(ctx, msgs, taskID)
		if err != nil {
			// 模型调用失败记一次失败，不伪造简介，继续下一轮。
			rep.Attempts++
			rep.RetryCount++
			rep.PromptTokens += usage.PromptTokens
			rep.CompletionTokens += usage.CompletionTokens
			rep.CostMilli += CostMilliForUsage(model, usage)
			lastChecks = []CheckResult{{Rule: 0, Name: "模型调用", Pass: false, Reason: fmt.Sprintf("模型调用失败：%v", err)}}
			rep.AttemptChecks = append(rep.AttemptChecks, lastChecks)
			continue
		}
		rep.Attempts++
		rep.PromptTokens += usage.PromptTokens
		rep.CompletionTokens += usage.CompletionTokens
		rep.CostMilli += CostMilliForUsage(model, usage)
		cand, perr := Parse(reply)
		if perr != nil {
			lastChecks = []CheckResult{{Rule: 0, Name: "简介格式解析", Pass: false, Reason: perr.Error()}}
			rep.AttemptChecks = append(rep.AttemptChecks, lastChecks)
			rep.RetryCount++
			continue
		}
		cand.DocID = doc.DocID
		cand.SourcePath = doc.SourcePath
		cand.SourceHash = SHA256Hex(doc.Content)
		NormalizeSlots(&cand)
		checks := CheckAll(cand, string(doc.Content), doc.Title)
		extraNote = ""
		if AllPass(checks) && g.Collision != nil {
			// 规则⑦：只在前 6 条 + 脱敏都通过后才查重（省向量调用）。
			dir := dirOf(doc.SourcePath)
			cc := g.Collision.CheckCollision(ctx, dir, doc.DocID, cand.Summary)
			checks = append(checks, cc)
			if !cc.Pass && !cc.Skipped {
				extraNote = "同目录查重撞车，撞车简介见打回原因：重写时必须写出差异化场景或报错特征，不要只换说法。"
			}
		}
		rep.AttemptChecks = append(rep.AttemptChecks, checks)
		if AllPass(checks) {
			cand.Status = StatusOK
			cand.SummaryHash = SHA256Hex([]byte(Format(cand)))
			cand.SummaryPath = summaryRelPath(doc.SourcePath)
			rep.Status = StatusOK
			rep.RetryCount = attempt // 成功前的失败轮数
			rep.FinalChecks = checks
			if g.Collision != nil {
				_ = g.Collision.Accept(ctx, dirOf(doc.SourcePath), doc.DocID, cand.Summary)
			}
			rep.CostCNY = float64(rep.CostMilli) / 1000
			return cand, rep, nil
		}
		lastChecks = checks
		rep.RetryCount = attempt + 1
	}
	// 规则⑧：连续失败 3 次，熔断降级为规则提取兜底入库。
	fb := FallbackSummary(doc.SourcePath, doc.Title, string(doc.Content))
	fb.DocID = doc.DocID
	fb.SourcePath = doc.SourcePath
	fb.SourceHash = SHA256Hex(doc.Content)
	fb.SummaryPath = summaryRelPath(doc.SourcePath)
	NormalizeSlots(&fb)
	fb.SummaryHash = SHA256Hex([]byte(Format(fb)))
	rep.Status = StatusDegradedOK
	rep.RetryCount = MaxRetries
	rep.FinalChecks = CheckAll(fb, string(doc.Content), doc.Title)
	rep.CostCNY = float64(rep.CostMilli) / 1000
	if g.Collision != nil {
		_ = g.Collision.Accept(ctx, dirOf(doc.SourcePath), doc.DocID, fb.Summary)
	}
	return fb, rep, nil
}

// dirOf 返回源文件所在子目录（如 wiki/entities）。
func dirOf(sourcePath string) string {
	if i := strings.LastIndex(sourcePath, "/"); i >= 0 {
		return sourcePath[:i]
	}
	return "."
}

// summaryRelPath 返回简介的镜像相对路径（分类子目录与源文件一致，
// 文件名与源文件完全一致），如 pilot-summaries/wiki/entities/xxx.md。
func summaryRelPath(sourcePath string) string {
	return "pilot-summaries/" + sourcePath
}
