// Package selfevolve 是 007 超级 Agent 的自进化闭环（第 20 步）：
// 每天自己读账本、找重复问题、提修补方案、测过之后自己应用，
// 注册表每周重建一次。
//
// 闭环四步（每天 04:30，精炼 04:00 之后跑）：
//  1. 聚合：最近 7 天账本信号（执行失败 / 探针失败 / 监工 HALT /
//     CONCERN / 分诊错选代理 / 费用突增），见 ledger.AggregateSignals。
//  2. 点评：调一次 worker 模型（提示词冻结在 llm 包），输出教训与提议。
//  3. 分档：同一签名 7 天内 ≥3 次才产生提议；能自动应用的只有
//     注册表元数据修正（entry_point 修路 + 探针验证），其余
//     （宪法 / 分诊提示词 / 执行层与网关代码）只提议、等人批。
//     宪法绝不自动改。
//  4. 落盘：教训进 Wiki、提议进 selfevolve_proposals 表、
//     每次模型调用与当日汇总都进 expense_ledger。
//
// 铁律：只读账本、不改宪法、不重写 registry.json（修正只活在内存
// 覆盖层里）；写失败返回错误，不许静默吞掉；不写任何密钥、Token。
package selfevolve

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"007-super-agent/internal/ledger"
)

// ProposalKind 是提议的种类；只有前两种可自动应用，其余只提议不应用。
const (
	KindRegistryEntryPointFix = "registry_entrypoint_fix" // 修注册表 entry_point 路径（自动档）
	KindRegistryVerified      = "registry_verified"       // 工具已修好、重新探针验证（自动档）
	KindConstitution          = "constitution"            // 改宪法（只提议，绝不自动改）
	KindTriagePrompt          = "triage_prompt"           // 改分诊提示词（只提议）
	KindCode                  = "code"                    // 改执行层/网关代码（只提议）
	KindOther                 = "other"                   // 其他（只提议）
)

// autoApplicableKind 判断一种提议是否可自动应用：只有注册表元数据
// 修正两档可以；宪法、分诊提示词、代码、其他一律只提议不应用。
func autoApplicableKind(kind string) bool {
	return kind == KindRegistryEntryPointFix || kind == KindRegistryVerified
}

// validProposalKind 校验提议种类在六档之内。
func validProposalKind(kind string) bool {
	switch kind {
	case KindRegistryEntryPointFix, KindRegistryVerified,
		KindConstitution, KindTriagePrompt, KindCode, KindOther:
		return true
	}
	return false
}

// ProposalDraft 是模型点评输出的一条提议草案（未入库、未校验）。
type ProposalDraft struct {
	Signature     string // 问题签名，如 exec_fail:cap-code-run-audit
	Kind          string // 六档之一
	ToolID        string // 注册表两档必填
	NewEntryPoint string // entrypoint_fix 档必填
	Text          string // 提议内容一句话
}

// ReviewOutput 是一次模型点评的解析结果。
type ReviewOutput struct {
	Lessons   []string        // 教训，每条一句话
	Proposals []ProposalDraft // 提议草案（调用方再按阈值与分档校验）
}

// reviewReplyJSON 是模型回复的 JSON 结构（解析用，与提示词里的格式一致）。
type reviewReplyJSON struct {
	Lessons   []string `json:"lessons"`
	Proposals []struct {
		Signature     string `json:"signature"`
		Kind          string `json:"kind"`
		ToolID        string `json:"tool_id"`
		NewEntryPoint string `json:"new_entry_point"`
		Text          string `json:"text"`
	} `json:"proposals"`
}

// parseReviewReply 解析模型点评回复：去围栏、解 JSON、逐条校验。
// 非法 JSON 返回错误（调用方记账、不写 Wiki）；单条草案非法
// （签名空、种类不在六档内、注册表档缺 tool_id、修路档缺新入口）
// 只丢弃该条、不让一条坏草案毁掉整轮点评。
func parseReviewReply(reply string) (ReviewOutput, error) {
	var out ReviewOutput
	reply = strings.TrimSpace(reply)
	reply = strings.TrimPrefix(reply, "```json")
	reply = strings.TrimPrefix(reply, "```")
	reply = strings.TrimSuffix(reply, "```")
	reply = strings.TrimSpace(reply)
	var raw reviewReplyJSON
	if err := json.Unmarshal([]byte(reply), &raw); err != nil {
		return out, fmt.Errorf("selfevolve: 点评回复不是合法 JSON 对象: %w", err)
	}
	for _, l := range raw.Lessons {
		if strings.TrimSpace(l) != "" {
			out.Lessons = append(out.Lessons, strings.TrimSpace(l))
		}
	}
	for _, p := range raw.Proposals {
		p.Signature = strings.TrimSpace(p.Signature)
		p.Kind = strings.TrimSpace(p.Kind)
		p.ToolID = strings.TrimSpace(p.ToolID)
		p.NewEntryPoint = strings.TrimSpace(p.NewEntryPoint)
		p.Text = strings.TrimSpace(p.Text)
		if p.Signature == "" || !validProposalKind(p.Kind) || p.Text == "" {
			continue
		}
		if (p.Kind == KindRegistryEntryPointFix || p.Kind == KindRegistryVerified) && p.ToolID == "" {
			continue
		}
		if p.Kind == KindRegistryEntryPointFix && p.NewEntryPoint == "" {
			continue
		}
		out.Proposals = append(out.Proposals, ProposalDraft{
			Signature: p.Signature, Kind: p.Kind, ToolID: p.ToolID,
			NewEntryPoint: p.NewEntryPoint, Text: p.Text,
		})
	}
	return out, nil
}

// formatSignals 把信号聚合成发给模型的紧凑文本：计数行在前（一定保留）、
// 错误样本在后（超上限时先丢样本）。行数很少，一次点评的输入
// 通常只有几百 token。
func formatSignals(s ledger.SignalCounts) string {
	var sb strings.Builder
	sb.WriteString("近 7 天账本信号（签名: 次数）：\n")
	writeCounts := func(kind string, m map[string]int) {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&sb, "%s:%s: %d\n", kind, k, m[k])
		}
	}
	writeCounts(ledger.SigExecFail, s.ExecFailures)
	writeCounts(ledger.SigProbeFail, s.ProbeFailures)
	writeCounts(ledger.SigAdvisorHalt, s.AdvisorHalts)
	writeCounts(ledger.SigAdvisorConcern, s.AdvisorConcerns)
	writeCounts(ledger.SigTriageMisfire, s.TriageMisfires)
	if s.ExpenseSpike {
		fmt.Fprintf(&sb, "%s: 今日费用 %d 毫分，前 7 日均 %d 毫分，超 2 倍；7 天内突增 %d 天\n",
			ledger.SigExpenseSpike, s.ExpenseTodayMilli, s.ExpensePrior7DayMeanMilli, s.ExpenseSpikeDays)
	} else {
		fmt.Fprintf(&sb, "费用：今日 %d 毫分，前 7 日均 %d 毫分，无突增\n",
			s.ExpenseTodayMilli, s.ExpensePrior7DayMeanMilli)
	}
	if len(s.Samples) > 0 {
		sb.WriteString("最近失败样本（错误头，截断 200 字）：\n")
		for _, sm := range s.Samples {
			head := sm.ErrHead
			if r := []rune(head); len(r) > 200 {
				head = string(r[:200])
			}
			fmt.Fprintf(&sb, "- %s | %s | %s\n", sm.ToolID, sm.Command, head)
		}
	}
	return sb.String()
}
