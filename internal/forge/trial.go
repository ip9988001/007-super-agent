package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"007-super-agent/internal/registry"
)

// TrialState 是试用期状态（data/forge/trials/<tool-id>.json）。
type TrialState struct {
	ToolID          string         `json:"tool_id"`
	GapID           string         `json:"gap_id"`
	BlueprintID     string         `json:"blueprint_id,omitempty"`
	Risk            string         `json:"risk"`
	Signature       string         `json:"signature"` // task_text 前 200 字（30 天禁令用）
	RegisteredAt    string         `json:"registered_at"`
	Status          string         `json:"status"` // trial / active / quarantined / deprecated / review
	Revives         int            `json:"revives"`
	LastCalls       int            `json:"last_calls"`
	LastFailures    int            `json:"last_failures"`
	LastConsecutive int            `json:"last_consecutive"`
	DeprecatedUntil string         `json:"deprecated_until,omitempty"`
	Entry           registry.Entry `json:"entry"` // 注册时的条目快照（复活重注册用）
}

// writeTrial 写一条试用期状态（0600）。
func (s *Store) writeTrial(tr TrialState) error {
	if err := s.EnsureDirs(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(tr, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.TrialsDir(), tr.ToolID+".json"), append(b, '\n'), 0o600)
}

// ReadTrial 读一条试用期状态。
func (s *Store) ReadTrial(toolID string) (TrialState, error) {
	var tr TrialState
	b, err := os.ReadFile(filepath.Join(s.TrialsDir(), toolID+".json"))
	if err != nil {
		return tr, err
	}
	if err := json.Unmarshal(b, &tr); err != nil {
		return tr, err
	}
	return tr, nil
}

// ListTrials 列全部试用期状态（按注册时间先后）。
func (p *Pipeline) ListTrials() ([]TrialState, error) {
	entries, err := os.ReadDir(p.Store.TrialsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []TrialState
	for _, de := range entries {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".json") {
			continue
		}
		tr, err := p.Store.ReadTrial(strings.TrimSuffix(de.Name(), ".json"))
		if err != nil {
			continue
		}
		out = append(out, tr)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RegisteredAt < out[j].RegisteredAt })
	return out, nil
}

// register 是注册的统一入口（风险分级门禁，规格 §7）：
// - low/medium：先做版本快照，再 AddEntry，进试用期；
// - high：不注册，写 data/forge/pending/<gap-id>.json 挂起，等人工确认。
// 注册成功后缺口状态置 done。
func (p *Pipeline) register(ctx context.Context, taskID string, gap Gap, bp *Blueprint, entry registry.Entry) error {
	blueprintID := ""
	if bp != nil {
		blueprintID = bp.ID
	}
	if entry.Risk == "high" {
		// 高风险：挂起，不注册，等鑫鑫或 Muse 人工确认。
		pending := struct {
			GapID       string         `json:"gap_id"`
			BlueprintID string         `json:"blueprint_id,omitempty"`
			Entry       registry.Entry `json:"entry"`
			Reason      string         `json:"reason"`
		}{GapID: gap.GapID, BlueprintID: blueprintID, Entry: entry,
			Reason: "high 风险：删文件/发消息/花钱/改系统配置/Go 编译，需人工确认后才能注册"}
		b, err := json.MarshalIndent(pending, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(p.Store.PendingDir(), gap.GapID+".json"), append(b, '\n'), 0o600); err != nil {
			return err
		}
		_ = p.Store.AppendEvent(Event{
			Event: EvRegistered, GapID: gap.GapID, BlueprintID: blueprintID,
			Output: fmt.Sprintf("工具 %s 为 high 风险，挂起待人工确认（pending/%s.json）", entry.ToolID, gap.GapID),
			Result: "halt",
		})
		_ = p.Store.UpdateGapStatus(gap.GapID, "done")
		return nil
	}
	// 版本快照（注册前必做，隔离回滚的依据）。
	snapPath := filepath.Join(p.Store.SnapshotsDir(), gap.GapID+"-registry-before.json")
	if err := p.Reg.Snapshot(snapPath); err != nil {
		return fmt.Errorf("forge register %s: 快照失败: %w", gap.GapID, err)
	}
	if err := p.Reg.AddEntry(entry); err != nil {
		return fmt.Errorf("forge register %s: %w", gap.GapID, err)
	}
	tr := TrialState{
		ToolID: entry.ToolID, GapID: gap.GapID, BlueprintID: blueprintID,
		Risk: entry.Risk, Signature: firstRunes(gap.TaskText, 200),
		RegisteredAt: p.now().UTC().Format(time.RFC3339),
		Status:       "trial", Entry: entry,
	}
	if err := p.Store.writeTrial(tr); err != nil {
		return err
	}
	_ = p.Store.AppendEvent(Event{
		Event: EvRegistered, GapID: gap.GapID, BlueprintID: blueprintID,
		Output: fmt.Sprintf("工具 %s 已注册（risk=%s，试用期 10 次/14 天）", entry.ToolID, entry.Risk),
		Result: "ok",
	})
	_ = p.Store.UpdateGapStatus(gap.GapID, "done")
	return nil
}

// ConfirmPending 是 high 风险工具的人工确认入口（由鑫鑫或 Muse 触发，
// 未来接 forge-run 子命令）：把 pending 里的条目正式注册进试用期。
func (p *Pipeline) ConfirmPending(ctx context.Context, gapID string) (string, error) {
	b, err := os.ReadFile(filepath.Join(p.Store.PendingDir(), gapID+".json"))
	if err != nil {
		return "", fmt.Errorf("forge confirm %s: 没有挂起记录: %w", gapID, err)
	}
	var pending struct {
		GapID       string         `json:"gap_id"`
		BlueprintID string         `json:"blueprint_id,omitempty"`
		Entry       registry.Entry `json:"entry"`
	}
	if err := json.Unmarshal(b, &pending); err != nil {
		return "", err
	}
	gap, err := p.Store.ReadGap(gapID)
	if err != nil {
		return "", err
	}
	var bp *Blueprint
	if pending.BlueprintID != "" {
		bp = &Blueprint{ID: pending.BlueprintID}
	}
	snapPath := filepath.Join(p.Store.SnapshotsDir(), gapID+"-registry-before.json")
	if err := p.Reg.Snapshot(snapPath); err != nil {
		return "", err
	}
	if err := p.Reg.AddEntry(pending.Entry); err != nil {
		return "", err
	}
	tr := TrialState{
		ToolID: pending.Entry.ToolID, GapID: gapID, BlueprintID: pending.BlueprintID,
		Risk: pending.Entry.Risk, Signature: firstRunes(gap.TaskText, 200),
		RegisteredAt: p.now().UTC().Format(time.RFC3339),
		Status:       "trial", Entry: pending.Entry,
	}
	if err := p.Store.writeTrial(tr); err != nil {
		return "", err
	}
	_ = os.Remove(filepath.Join(p.Store.PendingDir(), gapID+".json"))
	_ = p.Store.AppendEvent(Event{
		Event: EvRegistered, GapID: gapID, BlueprintID: pending.BlueprintID,
		Output: fmt.Sprintf("人工确认后注册 %s（risk=%s）", pending.Entry.ToolID, pending.Entry.Risk),
		Result: "ok",
	})
	_ = bp
	return pending.Entry.ToolID, nil
}

// CheckTrials 是试用期检查（规格 §8，04:30 循环里跑）：
// - 转正：前 10 次真实调用 <3 次失败，或 14 天内 ≥3 次成功；
// - 熔断（任一即隔离）：连续 2 次失败；试用期内累计 ≥3 次失败；
// - 14 天不足 3 次调用但 ≥2 次失败 → 待人工复核（不自动隔离）；
// - 隔离后可修复复活 1 次（重跑 staging 测试；通过重注册，不通过废弃）。
func (p *Pipeline) CheckTrials(ctx context.Context) (int, error) {
	trials, err := p.ListTrials()
	if err != nil {
		return 0, err
	}
	checked := 0
	for _, tr := range trials {
		switch tr.Status {
		case "trial", "review":
			checked++
			if err := p.checkOneTrial(ctx, tr); err != nil {
				return checked, err
			}
		case "quarantined":
			// 隔离后可修复复活 1 次。
			if tr.Revives < 1 {
				checked++
				if err := p.tryRevive(ctx, tr); err != nil {
					return checked, err
				}
			}
		}
	}
	return checked, nil
}

func (p *Pipeline) checkOneTrial(ctx context.Context, tr TrialState) error {
	registeredAt, err := time.Parse(time.RFC3339, tr.RegisteredAt)
	if err != nil {
		return err
	}
	calls, failures, consecutive, err := p.Led.ToolStats(tr.ToolID, registeredAt)
	if err != nil {
		return err
	}
	tr.LastCalls = calls
	tr.LastFailures = failures
	tr.LastConsecutive = consecutive
	age := p.now().Sub(registeredAt)
	successes := calls - failures
	// 熔断（任一即隔离）。
	if consecutive >= 2 || failures >= 3 {
		reason := fmt.Sprintf("熔断：连续失败 %d 次，累计失败 %d 次（%d 次调用）",
			consecutive, failures, calls)
		return p.quarantine(ctx, tr, reason)
	}
	// 转正。
	if (calls >= 10 && failures < 3) || (age >= 14*24*time.Hour && successes >= 3) {
		tr.Status = "active"
		if err := p.Reg.SetExperimental(tr.ToolID, false); err != nil {
			// 注册表里已无此工具（被手动删了）：只改状态，不报错。
			_ = err
		}
		if err := p.Store.writeTrial(tr); err != nil {
			return err
		}
		_ = p.Store.AppendEvent(Event{
			Event: EvRegistered, GapID: tr.GapID, BlueprintID: tr.BlueprintID,
			Output: fmt.Sprintf("工具 %s 试用期转正（%d 次调用，%d 次失败）", tr.ToolID, calls, failures),
			Result: "ok",
		})
		return nil
	}
	// 14 天不足 3 次调用但 ≥2 次失败 → 待人工复核。
	if age >= 14*24*time.Hour && calls < 3 && failures >= 2 {
		tr.Status = "review"
		if err := p.Store.writeTrial(tr); err != nil {
			return err
		}
		_ = p.Store.AppendEvent(Event{
			Event: EvQuarantined, GapID: tr.GapID, BlueprintID: tr.BlueprintID,
			Output: fmt.Sprintf("工具 %s 14 天仅 %d 次调用、%d 次失败，待人工复核（未自动隔离）", tr.ToolID, calls, failures),
			Result: "halt",
		})
		return nil
	}
	return p.Store.writeTrial(tr)
}

// quarantine 隔离：从注册表移除（快照留作回滚依据），状态置
// QUARANTINED，主循环不再选它（注册表里已无此条目）。
func (p *Pipeline) quarantine(ctx context.Context, tr TrialState, reason string) error {
	if err := p.Reg.RemoveEntry(tr.ToolID); err != nil {
		// 已不在注册表里也算隔离成功（幂等）。
		_ = err
	}
	tr.Status = "quarantined"
	if err := p.Store.writeTrial(tr); err != nil {
		return err
	}
	_ = p.Store.AppendEvent(Event{
		Event: EvQuarantined, GapID: tr.GapID, BlueprintID: tr.BlueprintID,
		Output: fmt.Sprintf("工具 %s 已隔离：%s（快照 %s-registry-before.json 留作回滚依据）", tr.ToolID, reason, tr.GapID),
		Result: "fail",
	})
	return nil
}

// tryRevive 修复复活 1 次：重跑 staging 测试（tests.json）；
// 通过 → 重注册进新一轮试用期；不通过 → 永久 DEPRECATED + 30 天禁令。
func (p *Pipeline) tryRevive(ctx context.Context, tr TrialState) error {
	stageDir := filepath.Join(p.Store.StagingDir(), tr.GapID)
	testsB, err := os.ReadFile(filepath.Join(stageDir, "tests.json"))
	if err != nil {
		// 没有测试留痕可重跑：直接废弃。
		return p.deprecate(ctx, tr, "隔离后无 staging 测试留痕可重跑")
	}
	var plan TestPlan
	if err := json.Unmarshal(testsB, &plan); err != nil {
		return p.deprecate(ctx, tr, "staging 测试留痕损坏")
	}
	gap, err := p.Store.ReadGap(tr.GapID)
	if err != nil {
		return p.deprecate(ctx, tr, "缺口报告已丢失")
	}
	taskID := p.ForgeTaskID()
	entryName := filepath.Base(tr.Entry.EntryPoint)
	resultMD, _, rerr := p.runTests(ctx, taskID, gap, stageDir, entryName, &plan, true)
	if rerr != nil {
		return p.deprecate(ctx, tr, fmt.Sprintf("复活重测未通过：%v\n%s", rerr, resultMD))
	}
	// 重注册（新一轮试用期）。
	snapPath := filepath.Join(p.Store.SnapshotsDir(), tr.GapID+"-revive-registry-before.json")
	if err := p.Reg.Snapshot(snapPath); err != nil {
		return err
	}
	tr.Entry.Experimental = true
	if err := p.Reg.AddEntry(tr.Entry); err != nil {
		return err
	}
	tr.Status = "trial"
	tr.Revives++
	tr.RegisteredAt = p.now().UTC().Format(time.RFC3339)
	if err := p.Store.writeTrial(tr); err != nil {
		return err
	}
	_ = p.Store.AppendEvent(Event{
		Event: EvRevived, GapID: tr.GapID, BlueprintID: tr.BlueprintID,
		Output: fmt.Sprintf("工具 %s 修复复活（重测通过，第 %d 次复活）", tr.ToolID, tr.Revives),
		Result: "ok",
	})
	return nil
}

// deprecate 永久废弃 + 30 天内同一缺口签名禁造。
func (p *Pipeline) deprecate(ctx context.Context, tr TrialState, reason string) error {
	tr.Status = "deprecated"
	tr.DeprecatedUntil = p.now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	if err := p.Store.writeTrial(tr); err != nil {
		return err
	}
	_ = p.Store.AppendEvent(Event{
		Event: EvDeprecated, GapID: tr.GapID, BlueprintID: tr.BlueprintID,
		Output: fmt.Sprintf("工具 %s 已废弃：%s（30 天内同一缺口签名禁造）", tr.ToolID, reason),
		Result: "fail",
	})
	return nil
}
