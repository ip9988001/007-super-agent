// Package advisor 是监工 Advisor 的真实实现。
//
// 来源：Gemini 第 9 轮（交互-16）交付的 core/advisor.go（157 行，纯 Go 标准库），
// Muse 转存为本包并做唯一一处适配：CollectEvidenceBundle 里读 ledger.db 的 SQL，
// 按我方真实表结构改写（见下面注释）；其余逻辑（五件套硬证据、HALT 最多 2 轮、
// 同因二连叫停早退）与 Gemini 原稿一致。本包另有 adapter.go 把它接进
// executor.Advisor 接口。
package advisor

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"007-super-agent/internal/llm"
)

const (
	MaxAdvisorRetries     = 2
	advisorFixedSysPrompt = `你是独立上下文监工 Advisor（只读无工具权限）。严禁当安全警察拦截合法指令，仅核查逻辑真伪与目标准确性。
抓假测试铁律：看到代码与测试 diff，必须反问“若把业务修复回退（放回旧 bug），该测试是否必然挂掉？”若无论修没修都绿，直接判 HALT 并指明假测试行号。
只输出合法 JSON：{"verdict":"NOTE|CONCERN|HALT","reason":"简明原因","fix_directive":"若HALT写具体修改指令，否则空"}`
)

type EvidenceRequest struct {
	TaskID         string
	TaskPrompt     string
	WorkDir        string
	ArtifactPaths  []string
	ProposedOutput string
}

type EvidenceBundle struct {
	TaskAndCriteria string `json:"task_and_criteria"` // <= 300 tokens
	GitDiff         string `json:"git_diff"`          // <= 1200 tokens
	LedgerCommands  string `json:"ledger_commands"`   // <= 500 tokens
	ArtifactStats   string `json:"artifact_stats"`    // <= 200 tokens
	ProposedOutput  string `json:"proposed_output"`   // <= 400 tokens
	TotalTokensEst  int    `json:"total_tokens_est"`  // hard cap <= 3000 tokens
}

type AdvisorConfig struct {
	Endpoint string
	APIKey   string
	Model    string
	Timeout  time.Duration
}

type AdvisorDecision struct {
	Verdict        string    `json:"verdict"` // NOTE | CONCERN | HALT
	Reason         string    `json:"reason"`
	FixDirective   string    `json:"fix_directive"`
	ShouldBlock    bool      `json:"should_block"`    // true only when HALT and retryCount < MaxAdvisorRetries
	ShouldEscalate bool      `json:"should_escalate"` // true when HALT reaches retry cap or repeats same reason
	Usage          llm.Usage // 本次调用的 token 用量（供调用方记进 expense_ledger）
}

func truncateByTokenCap(s string, maxTokens int) string {
	// 保守按 1 rune ≈ 0.6 token 折算字符预算，并强制转 []rune 防截断乱码
	maxRunes := int(float64(maxTokens) / 0.6)
	r := []rune(strings.TrimSpace(s))
	if len(r) <= maxRunes {
		return string(r)
	}
	return string(r[:maxRunes]) + "...[truncated]"
}

func estTokens(s string) int {
	return int(float64(len([]rune(s)))*0.6) + 1
}

// CollectEvidenceBundle 从宿主机客观抓取五件套硬证据（总计硬封顶 <= 3000 tokens）
func CollectEvidenceBundle(ctx context.Context, db *sql.DB, req EvidenceRequest) (EvidenceBundle, error) {
	var b EvidenceBundle
	b.TaskAndCriteria = truncateByTokenCap(req.TaskPrompt, 300)

	// 2. git diff -U5 业务与测试源码 (<= 1200 tokens)
	diffCmd := exec.CommandContext(ctx, "git", "-C", req.WorkDir, "diff", "-U5")
	if out, err := diffCmd.Output(); err == nil && len(out) > 0 {
		b.GitDiff = truncateByTokenCap(string(out), 1200)
	} else {
		b.GitDiff = "(no git diff)"
	}

	// 3. ledger.db 真实命令与退出码流水 (<= 500 tokens)
	var cmdLines []string
	if db != nil {
		// 注意：列名按我方 ledger.db 真实 schema（internal/ledger/schema.sql）：
		// Gemini 原稿的 stage_no / cmd_head_300 / err_head_600 / exec_id 在本库不存在，
		// 改用 exit_code / status / substr(command,1,300) / err_head，按 id 倒序。
		rows, err := db.QueryContext(ctx,
			`SELECT exit_code, status, substr(command,1,300), err_head FROM execution_ledger WHERE task_id = ? ORDER BY id DESC LIMIT 5`,
			req.TaskID)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var code int
				var status, cmdH, errH string
				if rows.Scan(&code, &status, &cmdH, &errH) == nil {
					cmdLines = append(cmdLines, fmt.Sprintf("[exit=%d status=%s] %s | err=%s", code, status, cmdH, errH))
				}
			}
		}
	}
	b.LedgerCommands = truncateByTokenCap(strings.Join(cmdLines, "\n"), 500)

	// 4. 产物 os.Stat 实盘物理指纹 (<= 200 tokens)
	var statLines []string
	for _, p := range req.ArtifactPaths {
		if info, err := os.Stat(p); err != nil {
			statLines = append(statLines, fmt.Sprintf("%s: MISSING (%v)", p, err))
		} else {
			statLines = append(statLines, fmt.Sprintf("%s: size=%dB mtime=%s", p, info.Size(), info.ModTime().Format(time.RFC3339)))
		}
	}
	b.ArtifactStats = truncateByTokenCap(strings.Join(statLines, "\n"), 200)

	// 5. 拟交付结论 (<= 400 tokens)
	b.ProposedOutput = truncateByTokenCap(req.ProposedOutput, 400)

	total := estTokens(b.TaskAndCriteria) + estTokens(b.GitDiff) + estTokens(b.LedgerCommands) + estTokens(b.ArtifactStats) + estTokens(b.ProposedOutput)
	if total > 3000 {
		b.GitDiff = truncateByTokenCap(b.GitDiff, 900)
		total = estTokens(b.TaskAndCriteria) + estTokens(b.GitDiff) + estTokens(b.LedgerCommands) + estTokens(b.ArtifactStats) + estTokens(b.ProposedOutput)
	}
	b.TotalTokensEst = total
	return b, nil
}

// RunAdvisorCheck 带独立固定前缀发一次无工具权限只读请求，解析三级便签并执行 HALT 最多 2 轮控制
func RunAdvisorCheck(ctx context.Context, cfg AdvisorConfig, bundle EvidenceBundle, retryCount int, prevHaltReason string) (AdvisorDecision, error) {
	evBytes, _ := json.Marshal(bundle)
	payload := map[string]any{
		"model": cfg.Model,
		"messages": []map[string]string{
			{"role": "system", "content": advisorFixedSysPrompt},
			{"role": "user", "content": string(evBytes)},
		},
		"response_format": map[string]string{"type": "json_object"},
		"temperature":     0.1,
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return AdvisorDecision{}, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: cfg.Timeout}
	resp, err := client.Do(req)
	if err != nil {
		return AdvisorDecision{}, err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	var apiResp struct {
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
	if err := json.Unmarshal(raw, &apiResp); err != nil || len(apiResp.Choices) == 0 {
		return AdvisorDecision{}, fmt.Errorf("advisor invalid response: %s", truncateByTokenCap(string(raw), 100))
	}

	var dec AdvisorDecision
	if err := json.Unmarshal([]byte(apiResp.Choices[0].Message.Content), &dec); err != nil {
		return AdvisorDecision{}, fmt.Errorf("advisor verdict parse error: %w", err)
	}
	dec.Usage = llm.Usage{
		PromptTokens:     apiResp.Usage.PromptTokens,
		CacheHitTokens:   apiResp.Usage.PromptCacheHit,
		CacheMissTokens:  apiResp.Usage.PromptCacheMiss,
		CompletionTokens: apiResp.Usage.CompletionTokens,
	}
	if dec.Verdict == "HALT" {
		sameCause := prevHaltReason != "" && strings.EqualFold(strings.TrimSpace(prevHaltReason), strings.TrimSpace(dec.Reason))
		if retryCount >= MaxAdvisorRetries || sameCause {
			dec.ShouldEscalate = true
			dec.ShouldBlock = false
		} else {
			dec.ShouldBlock = true
		}
	}
	return dec, nil
}
