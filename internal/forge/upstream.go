package forge

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// CheckUpstream 是已采用工具的上游复查（规格 §2A.6）：每周自查
// （周日 04:30 窗口）时，对每个采用/包装分支进来的工具查一次上游
// 状态——仓库已归档，或最近提交已超 18 个月 → 隔离（熔断器跳闸时
// 也可单独调用）。自造工具没有上游，跳过。
// 返回复查的工具数。
func (p *Pipeline) CheckUpstream(ctx context.Context) (int, error) {
	trials, err := p.ListTrials()
	if err != nil {
		return 0, err
	}
	checked := 0
	for _, tr := range trials {
		if tr.Status != "trial" && tr.Status != "active" {
			continue
		}
		fullName := upstreamFromSource(tr.Entry.SourceFile)
		if fullName == "" {
			continue // 自造工具，无上游
		}
		owner, repo, ok := strings.Cut(fullName, "/")
		if !ok {
			continue
		}
		checked++
		d, err := p.Ret.RepoDetailsCall(ctx, owner, repo)
		if err != nil {
			// 查不到上游（删库/断网）：只记账，不隔离，避免误杀。
			_ = p.Store.AppendEvent(Event{
				Event: EvQuarantined, GapID: tr.GapID, BlueprintID: tr.BlueprintID,
				Output: fmt.Sprintf("工具 %s 上游复查失败（%v），未隔离", tr.ToolID, err),
				Result: "halt",
			})
			continue
		}
		reason := ""
		if d.Archived {
			reason = fmt.Sprintf("上游 %s 已归档", fullName)
		} else if pushed, err := time.Parse(time.RFC3339, d.PushedAt); err == nil {
			if p.now().Sub(pushed) > 18*30*24*time.Hour {
				reason = fmt.Sprintf("上游 %s 最近提交 %s，已超 18 个月", fullName, d.PushedAt)
			}
		}
		if reason != "" {
			if err := p.quarantine(ctx, tr, reason+"（每周上游复查）"); err != nil {
				return checked, err
			}
		}
	}
	return checked, nil
}

// upstreamFromSource 从条目的 SourceFile 里取上游 full_name：
// 格式为 "forge:<gap-id> (upstream <owner>/<repo>)"，自造工具没有括号部分。
func upstreamFromSource(source string) string {
	i := strings.Index(source, "(upstream ")
	if i < 0 {
		return ""
	}
	rest := source[i+len("(upstream "):]
	j := strings.Index(rest, ")")
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:j])
}
