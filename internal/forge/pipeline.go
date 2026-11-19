package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"007-super-agent/internal/executor"
	"007-super-agent/internal/ledger"
	"007-super-agent/internal/registry"
	"007-super-agent/internal/retrieval"
)

// ModelCall 是锻造的模型调用（由主程序接 llm.Client.CallTask）。
// frozen 是冻结提示词，tail 是本轮证据；返回模型回复正文。
type ModelCall func(ctx context.Context, taskID, frozen, tail string) (string, error)

// ReviewFunc 是监工审阅（由主程序接 executor.ReviewDelivery 的适配器）。
// 返回 verdict（"NOTE" / "CONCERN" / "HALT"）与意见 note。
type ReviewFunc func(ctx context.Context, taskID, title, body, workDir string) (verdict string, note string, err error)

// Runner 是命令执行器（executor.Executor 满足；测试用桩）。
type Runner interface {
	Run(req executor.RunRequest) (*executor.Result, *executor.Handle, error)
}

// ErrBudgetPaused 是预算耗尽（PAUSED_BUDGET，次日续跑）。
var ErrBudgetPaused = errors.New("forge: 当日进化预算已用完，暂停至次日")

// 固定第 1 检查项：监工两道门都先核查 reuse_search 分流是否合理。
const fixedCheckItem1 = `先看检索留痕：检索是否按 §2A 走了（第零层强制全查、外部层按语言生态排序、≤3 次搜索、核心 API 只查元数据 ≤15 次/缺口、raw 直读不计额度）；红线/硬门槛判得对不对；有没有“该采用却去自造”（成熟工具在手却重写），或“该自造却硬采用”（覆盖 <80%、胶水 >100 行、魔改非公开接口、许可被禁却硬上）。分流错了就 HALT，不管后面写得再好。`

// Pipeline 是锻造流水线（规格 §4–§8）。
type Pipeline struct {
	Store  *Store
	Reg    *registry.Registry
	Led    *ledger.Ledger
	Ret    *retrieval.Client
	Model  ModelCall
	Review ReviewFunc
	Run    Runner

	SessionID string

	Now func() time.Time // 默认 time.Now；测试可注入固定时钟

	// 预算与配额（规格 §10；默认 0.50 元/天、每天最多 3 个缺口）。
	DailyBudgetMilli int64
	MaxGapsPerDay    int
}

func (p *Pipeline) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Pipeline) budgetMilli() int64 {
	if p.DailyBudgetMilli > 0 {
		return p.DailyBudgetMilli
	}
	return 500
}

func (p *Pipeline) maxGaps() int {
	if p.MaxGapsPerDay > 0 {
		return p.MaxGapsPerDay
	}
	return 3
}

// beijingDate 返回北京时间的日期（锻造任务的 task_id 用它）。
func beijingDate(t time.Time) string {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		loc = time.FixedZone("CST", 8*3600)
	}
	return t.In(loc).Format("2006-01-02")
}

// ForgeTaskID 返回当天的锻造任务号（费用归属用）。
func (p *Pipeline) ForgeTaskID() string {
	return "forge-" + beijingDate(p.now())
}

// callModel 是带预算检查的模型调用：每次调用前先查当天锻造任务的
// 累计费用，超支就记 PAUSED_BUDGET 并停在步骤边界（规格 §10）。
func (p *Pipeline) callModel(ctx context.Context, taskID, frozen, tail string) (string, error) {
	spent, err := p.Led.SummarizeExpenseByTask(taskID)
	if err != nil {
		return "", fmt.Errorf("forge budget: %w", err)
	}
	if spent.CostCNYMilli >= p.budgetMilli() {
		_ = p.Store.AppendEvent(Event{
			Event: EvPausedBudget, GapID: "", Input: taskID,
			Output: fmt.Sprintf("当日锻造费用 %d 毫分已达上限 %d", spent.CostCNYMilli, p.budgetMilli()),
			Result: "halt",
		})
		return "", ErrBudgetPaused
	}
	if p.Model == nil {
		return "", fmt.Errorf("forge: 未接模型")
	}
	reply, err := p.Model(ctx, taskID, frozen, tail)
	if err != nil && isTransientModelErr(err) {
		// 瞬时失败（超时/读响应失败）：等 5 秒重试 1 次，仍失败才上报。
		// 2026-11-19 缺口 1 第二轮：修复调用死于 context deadline exceeded。
		time.Sleep(5 * time.Second)
		return p.Model(ctx, taskID, frozen, tail)
	}
	return reply, err
}

// isTransientModelErr 判断模型调用错误是否值得重试（瞬时网络/超时）。
func isTransientModelErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "deadline exceeded") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "context deadline") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "eof")
}

// localCheck 是第零层（规格 §2A.1）：本机注册表 + 已隔离记录强制全查，
// 零成本。命中返回 (true, 说明)；未命中返回 (false, 检查摘要)——
// 摘要进检索留痕，供监工核验“确实查过”（2026-11-19 监工 HALT 指出）。
func (p *Pipeline) localCheck(taskText string) (bool, string) {
	kws := retrieval.ExtractKeywords(taskText)
	if len(kws) == 0 {
		return false, "关键词为空，未做本地比对"
	}
	// 注册表：关键词与 one_liner/tool_id 重叠 ≥2 个词即命中。
	bestHit, bestTool := 0, ""
	for _, e := range p.Reg.AllEntries() {
		hay := strings.ToLower(e.ToolID + " " + e.OneLiner + " " + e.Category + " " + e.Subcategory)
		hit := 0
		for _, kw := range kws {
			if strings.Contains(hay, strings.ToLower(kw)) {
				hit++
			}
		}
		if hit > bestHit {
			bestHit, bestTool = hit, e.ToolID
		}
		if hit >= 2 {
			return true, fmt.Sprintf("注册表已有工具 %s（关键词命中 %d 个）", e.ToolID, hit)
		}
	}
	// 已隔离记录：同名/同关键词的隔离工具也算命中（不再重造）。
	trials, _ := p.ListTrials()
	nBad := 0
	for _, tr := range trials {
		if tr.Status != "quarantined" && tr.Status != "deprecated" {
			continue
		}
		nBad++
		hay := strings.ToLower(tr.ToolID + " " + tr.GapID)
		hit := 0
		for _, kw := range kws {
			if strings.Contains(hay, strings.ToLower(kw)) {
				hit++
			}
		}
		if hit >= 2 {
			return true, fmt.Sprintf("已隔离/已废弃工具 %s（状态 %s，关键词命中 %d 个）", tr.ToolID, tr.Status, hit)
		}
	}
	return false, fmt.Sprintf("注册表 %d 条：最高重叠 %d 个关键词（%s），无 ≥2 命中；隔离/弃用记录 %d 条：无 ≥2 命中", len(p.Reg.AllEntries()), bestHit, bestTool, nBad)
}

// ProcessGaps 处理当天的缺口队列（规格 §4）：pending 在先、deferred
// （顺延的）在后，按 created_ts 排序，最多 MaxGapsPerDay 个。
// 30 天禁令：同一缺口签名被废弃过（DEPRECATED）则跳过。
func (p *Pipeline) ProcessGaps(ctx context.Context) (int, error) {
	if err := p.Store.EnsureDirs(); err != nil {
		return 0, err
	}
	pending, err := p.Store.ListGaps("pending")
	if err != nil {
		return 0, err
	}
	deferred, err := p.Store.ListGaps("deferred")
	if err != nil {
		return 0, err
	}
	var queue []Gap
	queue = append(queue, pending...)
	queue = append(queue, deferred...) // 顺延的在队列最高位续跑
	sort.Slice(queue, func(i, j int) bool { return queue[i].CreatedTS < queue[j].CreatedTS })
	// 30 天禁令：同一签名（task_text 前 200 字）被废弃过则跳过。
	banned := map[string]bool{}
	trials, _ := p.ListTrials()
	for _, tr := range trials {
		if tr.Status != "deprecated" || tr.DeprecatedUntil == "" {
			continue
		}
		if until, err := time.Parse(time.RFC3339, tr.DeprecatedUntil); err == nil && p.now().Before(until) {
			banned[firstRunes(tr.Signature, 200)] = true
		}
	}
	processed := 0
	for _, g := range queue {
		if processed >= p.maxGaps() {
			break
		}
		if banned[firstRunes(g.TaskText, 200)] {
			_ = p.Store.UpdateGapStatus(g.GapID, "rejected")
			_ = p.Store.AppendEvent(Event{
				Event: EvGapReported, GapID: g.GapID,
				Output: "同一缺口签名 30 天内被废弃，跳过重造",
				Result: "rejected",
			})
			continue
		}
		if err := p.ProcessGap(ctx, g); err != nil {
			if errors.Is(err, ErrBudgetPaused) {
				return processed, nil // 预算耗尽：停在步骤边界，次日续跑
			}
			return processed, err
		}
		processed++
	}
	return processed, nil
}

// ProcessGap 跑通一个缺口的全流程：检索 → 分流 → 采用/包装或自造 →
// 监工两道门 → staging 试造 → 注册（风险分级门禁）。
func (p *Pipeline) ProcessGap(ctx context.Context, gap Gap) error {
	taskID := p.ForgeTaskID()
	if err := p.Store.UpdateGapStatus(gap.GapID, "processing"); err != nil {
		return err
	}
	// ---- 检索（复用优先） ----
	tr, err := p.Ret.Retrieve(ctx, retrieval.GapInput{
		GapID: gap.GapID, TaskText: gap.TaskText,
		Keywords:   gap.Keywords,
		LocalCheck: p.localCheck,
	})
	if err != nil {
		// 限流顺延：记 SEARCH_DEFERRED_RATE_LIMIT，缺口留队列最高位，
		// 次日续跑，不转自造（交互-23 Q5）。
		var rlErr *retrieval.ErrRateLimited
		if errors.As(err, &rlErr) {
			_ = p.saveTrace(gap.GapID, tr)
			_ = p.Store.AppendEvent(Event{
				Event: EvSearchDeferredRateLimit, GapID: gap.GapID,
				Output:        fmt.Sprintf("检索到限顺延：%v", err),
				Result:        "halt",
				SelfCoreCalls: tr.CoreUsed,
			})
			_ = p.Store.UpdateGapStatus(gap.GapID, "deferred")
			return nil
		}
		_ = p.Store.UpdateGapStatus(gap.GapID, "pending")
		return fmt.Errorf("forge retrieve %s: %w", gap.GapID, err)
	}
	_ = p.saveTrace(gap.GapID, tr)
	_ = p.Store.AppendEvent(Event{
		Event: EvReuseSearchDone, GapID: gap.GapID,
		Input:  gap.TaskText,
		Output: fmt.Sprintf("outcome=%s searches=%d core=%d", tr.Outcome, tr.SearchesUsed, tr.CoreUsed),
		Result: "ok",
	})
	switch tr.Outcome {
	case "layer0":
		// 第零层命中：已有工具/已隔离记录，不做任何外部搜索，缺口关闭。
		_ = p.Store.UpdateGapStatus(gap.GapID, "done")
		return nil
	case "exhausted":
		_ = p.Store.AppendEvent(Event{
			Event: EvSearchExhaustedNoMatch, GapID: gap.GapID,
			Output: "3 次搜索无合格候选，转自造",
			Result: "ok",
		})
		return p.selfBuild(ctx, taskID, gap, tr, nil)
	case "recommend":
		// 模型分流：采用/包装 vs 自造。
		decision, err := p.triage(ctx, taskID, gap, tr)
		if err != nil {
			_ = p.Store.UpdateGapStatus(gap.GapID, "pending")
			return err
		}
		switch decision.Branch {
		case "adopt", "wrap":
			_ = p.Store.AppendEvent(Event{
				Event: map[string]string{"adopt": EvAdopted, "wrap": EvWrapped}[decision.Branch],
				GapID: gap.GapID,
				Output: fmt.Sprintf("candidate=%s coverage=%d%% wrapper_lines=%d license=%s reason=%s",
					decision.Candidate, decision.Coverage, decision.WrapperLines, decision.LicenseTier, decision.Reason),
				Result: "ok",
			})
			return p.adoptOrWrap(ctx, taskID, gap, tr, decision)
		default:
			_ = p.Store.AppendEvent(Event{
				Event: EvSelfBuild, GapID: gap.GapID,
				Output: fmt.Sprintf("分流结论：自造（%s）", decision.Reason),
				Result: "ok",
			})
			return p.selfBuild(ctx, taskID, gap, tr, decision)
		}
	default:
		return fmt.Errorf("forge: 未知检索结果 %q", tr.Outcome)
	}
}

// saveTrace 把检索留痕存到 data/forge/retrieval/<gap-id>.json（不进 wiki）。
func (p *Pipeline) saveTrace(gapID string, tr *retrieval.Trace) error {
	if tr == nil {
		return nil
	}
	b, err := json.MarshalIndent(tr, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(p.Store.RetrievalDir(), gapID+".json"), append(b, '\n'), 0o600)
}

// triage 调模型做分流判定。
func (p *Pipeline) triage(ctx context.Context, taskID string, gap Gap, tr *retrieval.Trace) (*TriageDecision, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "缺口 %s：%s\n触发：%s\n", gap.GapID, gap.TaskText, gap.Trigger)
	fmt.Fprintf(&b, "检索留痕：%s\n", tr.Summary())
	for i, c := range tr.Candidates {
		fmt.Fprintf(&b, "候选 %d：%s（运行需求 %s）stars=%d 许可=%s 最后提交=%s 安全标记=%s 红线=%v\n",
			i+1, c.FullName, c.RuntimeNeed, c.Stars, c.License, c.LastCommit, c.SecurityFlag, c.RedLines)
	}
	reply, err := p.callModel(ctx, taskID, triagePromptHead, b.String())
	if err != nil {
		return nil, err
	}
	return ParseTriageDecision(reply)
}

// ---- 自造分支 ----

// selfBuild 自造分支：蓝图 → 监工审蓝图（≤2 轮打回重做）→ staging
// 试造（测试不过时模型修脚本，最多 2 轮）→ 监工审测试结果（≤2 轮
// 打回重做）→ 注册。
func (p *Pipeline) selfBuild(ctx context.Context, taskID string, gap Gap, tr *retrieval.Trace, decision *TriageDecision) error {
	var tail strings.Builder
	fmt.Fprintf(&tail, "缺口 %s：%s\n触发：%s\n", gap.GapID, gap.TaskText, gap.Trigger)
	fmt.Fprintf(&tail, "检索留痕：%s\n", tr.Summary())
	if decision != nil {
		fmt.Fprintf(&tail, "分流结论：自造（%s）\n", decision.Reason)
	}
	if tr.Recommended != "" {
		// 有推荐候选却判自造：监工必查覆盖矩阵（2026-11-19 缺口 1
		// 第二次 HALT）。缺口分析里逐候选列：候选、覆盖率%、缺失能力
		// 清单、预估胶水行数；没有这张表，蓝图一定被打回。
		fmt.Fprintf(&tail, "注意：检索给出了推荐候选 %s，但分流判自造。缺口分析里必须逐候选列覆盖矩阵（候选、覆盖率%%、缺失能力清单、预估胶水行数、为何不采用），否则监工会按误分流打回。\n", tr.Recommended)
	}
	fmt.Fprintf(&tail, "补充要求：JSON 里再加两个字段——category（9 大类之一：fetch/code/seo/design/security/runtime/media/ops/memory）与 subcategory（该大类的子类，见注册表 v1.7）；name 用英文 kebab-case。\n")
	reply, err := p.callModel(ctx, taskID, blueprintPromptHead, tail.String())
	if err != nil {
		_ = p.Store.UpdateGapStatus(gap.GapID, "pending")
		return err
	}
	bp, extra, err := p.parseAndWriteBlueprint(reply, gap, tr, decision)
	if err != nil {
		_ = p.Store.UpdateGapStatus(gap.GapID, "pending")
		return fmt.Errorf("forge blueprint %s: %w", gap.GapID, err)
	}
	// 监工审蓝图（固定第 1 检查项 + ≤2 轮打回重做）。
	// 审阅体是精简版：检索留痕前置（监工证据把拟交付结论截到 400 token，
	// 蓝图全文放后面监工看不到留痕），全文仍在蓝图文件里。
	stageDir := filepath.Join(p.Store.StagingDir(), gap.GapID)
	if err := os.MkdirAll(stageDir, 0o700); err != nil {
		return err
	}
	md := p.blueprintReviewBody(bp, tr)
	err = p.advisorGate(ctx, taskID, "审蓝图 "+bp.ID, md, p.Store.BlueprintsDir(),
		EvAdvisorBlueprintHalt, EvAdvisorBlueprintPass, gap.GapID, bp.ID,
		func(note string) (string, error) {
			// 打回重做：把监工意见喂回模型重画蓝图。
			r, err := p.callModel(ctx, taskID, blueprintPromptHead,
				tail.String()+"\n监工打回意见（按此修改重画）："+note)
			if err != nil {
				return "", err
			}
			nbp, _, err := p.parseAndWriteBlueprint(r, gap, tr, decision)
			if err != nil {
				return "", err
			}
			*bp = *nbp
			md, _ := p.Store.ReadBlueprint(gap.GapID)
			return md, nil
		})
	if err != nil {
		if errors.Is(err, errAdvisorRejected) {
			_ = p.Store.UpdateGapStatus(gap.GapID, "rejected")
			return nil
		}
		_ = p.Store.UpdateGapStatus(gap.GapID, "pending")
		return err
	}
	// staging 试造：先清空上轮残留（重跑/采用失败转自造时），再写脚本 + 静态检查。
	if ents, rerr := os.ReadDir(stageDir); rerr == nil {
		for _, de := range ents {
			_ = os.RemoveAll(filepath.Join(stageDir, de.Name()))
		}
	}
	scriptPath := filepath.Join(stageDir, bp.ScriptName)
	if err := os.WriteFile(scriptPath, []byte(bp.Script), 0o700); err != nil {
		return err
	}
	findings, err := ScanStagingFiles(stageDir)
	if err != nil {
		return err
	}
	if len(findings) > 0 {
		_ = p.Store.AppendEvent(Event{
			Event: EvTestFail, GapID: gap.GapID, BlueprintID: bp.ID,
			Output: "静态检查命中：" + strings.Join(findings, "; "),
			Result: "fail",
		})
		_ = p.Store.UpdateGapStatus(gap.GapID, "rejected")
		return nil
	}
	// 测试用例（缺口类必须回放 ≥1 条真实历史失败输入）。
	replay := p.replayInput(gap)
	tests, err := p.genTests(ctx, taskID, gap, bp.Name, bp.Description, bp.ScriptName, bp.Script, stageDir, replay)
	if err != nil {
		_ = p.Store.UpdateGapStatus(gap.GapID, "pending")
		return err
	}
	// 跑测试；不过时模型诊断修复（脚本/测试/输入文件三选一），最多 2 轮。
	// 失败的测试结果也落盘（排障无据是 2026-11-19 的教训）。
	resultMD, err := p.runTestsWithFix(ctx, taskID, gap, bp, stageDir, tests)
	_ = os.WriteFile(filepath.Join(stageDir, "test-result.md"), []byte(resultMD), 0o600)
	if err != nil {
		_ = p.Store.UpdateGapStatus(gap.GapID, "pending")
		return err
	}
	// 监工审测试结果（固定第 1 检查项 + ≤2 轮打回重做；留痕前置）。
	err = p.advisorGate(ctx, taskID, "审测试结果 "+bp.ID, "【复用检索留痕】\n"+tr.Summary()+"\n\n"+resultMD, stageDir,
		EvAdvisorTestHalt, EvAdvisorTestPass, gap.GapID, bp.ID,
		func(note string) (string, error) {
			// 打回重做：重新生成测试用例并重跑。
			t2, err := p.genTests(ctx, taskID, gap, bp.Name, bp.Description, bp.ScriptName, bp.Script, stageDir, replay)
			if err != nil {
				return "", err
			}
			md2, err := p.runTestsWithFix(ctx, taskID, gap, bp, stageDir, t2)
			if err != nil {
				return "", err
			}
			_ = os.WriteFile(filepath.Join(stageDir, "test-result.md"), []byte(md2), 0o600)
			return md2, nil
		})
	if err != nil {
		if errors.Is(err, errAdvisorRejected) {
			_ = p.Store.UpdateGapStatus(gap.GapID, "rejected")
			return nil
		}
		_ = p.Store.UpdateGapStatus(gap.GapID, "pending")
		return err
	}
	// 注册（风险分级门禁）。
	entry := registry.Entry{
		ToolID:         "forge-" + bp.Name,
		OneLiner:       bp.Description,
		EntryPoint:     scriptPath,
		CallType:       "cli",
		OutputContract: "stdout 输出处理结果，exit 0 为成功，非 0 为失败",
		SourceFile:     "forge:" + gap.GapID,
		Category:       p.validCategory(extra.Category),
		Subcategory:    p.validSubcategory(extra.Category, extra.Subcategory),
		Forged:         true,
		Experimental:   true,
		BlueprintID:    bp.ID,
		Risk:           bp.RiskLevel,
	}
	return p.register(ctx, taskID, gap, bp, entry)
}

// parseAndWriteBlueprint 解析模型输出的蓝图 JSON，校验后落盘并记账。
func (p *Pipeline) parseAndWriteBlueprint(reply string, gap Gap, tr *retrieval.Trace, decision *TriageDecision) (*Blueprint, blueprintExtra, error) {
	bp, err := ParseBlueprint(reply, gap.GapID)
	if err != nil {
		return nil, blueprintExtra{}, err
	}
	var extra blueprintExtra
	_ = json.Unmarshal([]byte(reply[strings.Index(reply, "{"):strings.LastIndex(reply, "}")+1]), &extra)
	bp.ReuseSearch = tr.Summary()
	if decision != nil {
		bp.ReuseSearch += fmt.Sprintf("\n分流结论：自造——%s", decision.Reason)
	}
	if err := p.Store.WriteBlueprint(bp); err != nil {
		return nil, blueprintExtra{}, err
	}
	_ = p.Store.AppendEvent(Event{
		Event: EvBlueprintWritten, GapID: gap.GapID, BlueprintID: bp.ID,
		Output: fmt.Sprintf("name=%s risk=%s", bp.Name, bp.RiskLevel),
		Result: "ok",
	})
	return bp, extra, nil
}

type blueprintExtra struct {
	Category    string `json:"category"`
	Subcategory string `json:"subcategory"`
}

// runTestsWithFix 跑测试；不过时把失败结果喂给模型诊断修复
// （脚本错 / 测试错 / 输入文件缺失三选一，错哪边改哪边），最多 2 轮。
// 2 轮仍不过返回错误（缺口回 pending，次日人工看）。
func (p *Pipeline) runTestsWithFix(ctx context.Context, taskID string, gap Gap, bp *Blueprint, stageDir string, plan *TestPlan) (string, error) {
	resultMD, err := p.runTests(ctx, taskID, gap, stageDir, bp.ScriptName, plan, true)
	for round := 0; round < 2 && err != nil; round++ {
		var b strings.Builder
		fmt.Fprintf(&b, "工具：%s（%s）\n缺口：%s\n", bp.Name, bp.Description, gap.TaskText)
		fmt.Fprintf(&b, "当前脚本（%s）：\n```\n%s\n```\n", bp.ScriptName, bp.Script)
		planB, _ := json.MarshalIndent(plan, "", "  ")
		fmt.Fprintf(&b, "当前测试计划：\n```json\n%s\n```\n", string(planB))
		if ents, rerr := os.ReadDir(stageDir); rerr == nil {
			var names []string
			for _, de := range ents {
				names = append(names, de.Name())
			}
			fmt.Fprintf(&b, "staging 现有文件：%s\n", strings.Join(names, ", "))
		}
		fmt.Fprintf(&b, "测试失败结果：\n%s\n", resultMD)
		reply, merr := p.callModel(ctx, taskID, fixPromptHead, b.String())
		if merr != nil {
			return resultMD, merr
		}
		fixed, ferr := ParseFixPlan(reply, bp.ScriptName)
		if ferr != nil {
			return resultMD, ferr
		}
		bp.Script = fixed.Script
		if fixed.ScriptName != "" {
			bp.ScriptName = fixed.ScriptName
		}
		scriptPath := filepath.Join(stageDir, bp.ScriptName)
		if werr := os.WriteFile(scriptPath, []byte(bp.Script), 0o700); werr != nil {
			return resultMD, werr
		}
		// 蓝图里的脚本同步更新（留痕一致）。
		_ = p.Store.WriteBlueprint(bp)
		plan = &TestPlan{Tests: fixed.Tests, Fixtures: fixed.Fixtures}
		resultMD, err = p.runTests(ctx, taskID, gap, stageDir, bp.ScriptName, plan, true)
	}
	return resultMD, err
}

// replayInput 取该缺口对应的真实历史失败输入（执行账本）。
// 有则返回原命令文本，供 genTests 强制回放；没有返回空串。
func (p *Pipeline) replayInput(gap Gap) string {
	toolID := ""
	if gap.Evidence != nil {
		if v, ok := gap.Evidence["tool_id"].(string); ok {
			toolID = v
		}
	}
	if toolID == "" {
		return ""
	}
	failed, err := p.Led.FailedInputs(toolID, 1)
	if err != nil || len(failed) == 0 {
		return ""
	}
	return failed[0].Command
}

// genTests 调模型生成 staging 测试计划（≥3 条用例 + 输入文件）。
// 把真实脚本文件名与全文、staging 现有文件清单都给模型，测试必须
// 按真实接口写、引用的输入文件必须在 fixtures 里声明。
func (p *Pipeline) genTests(ctx context.Context, taskID string, gap Gap, toolName, toolDesc, scriptName, script, stageDir, replay string) (*TestPlan, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "工具：%s\n说明：%s\n缺口：%s\n", toolName, toolDesc, gap.TaskText)
	fmt.Fprintf(&b, "staging 目录：%s\n", stageDir)
	fmt.Fprintf(&b, "真实脚本文件名：%s（测试的 argv 里必须出现它）\n", scriptName)
	fmt.Fprintf(&b, "真实脚本全文：\n```\n%s\n```\n", script)
	if ents, err := os.ReadDir(stageDir); err == nil {
		var names []string
		for _, de := range ents {
			names = append(names, de.Name())
		}
		fmt.Fprintf(&b, "staging 现有文件：%s\n", strings.Join(names, ", "))
	}
	if replay != "" {
		fmt.Fprintf(&b, "真实历史失败输入（必须回放其中至少 1 条）：%s\n", replay)
	}
	reply, err := p.callModel(ctx, taskID, testsPromptHead, b.String())
	if err != nil {
		return nil, err
	}
	return ParseTestCases(reply, scriptName)
}

// runTests 在 staging 里跑测试用例，返回 test-result.md 全文。
// 先写 fixtures（测试用的输入文件），再跑；argv 里与 staging 文件
// 同名的相对路径解析成绝对路径（执行器的工作目录不是 staging，
// 相对路径不解析就找不到文件）；offlineCapable 时用 unshare -n 断网跑
// （离线类工具强制断网试跑）。
func (p *Pipeline) runTests(ctx context.Context, taskID string, gap Gap, stageDir, entryName string, plan *TestPlan, offlineCapable bool) (string, error) {
	// 测试留痕（复活重跑用）：把用例与输入文件原样存进 staging。
	if tb, err := json.MarshalIndent(plan, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(stageDir, "tests.json"), append(tb, '\n'), 0o600)
	}
	// 写 fixtures。
	for _, f := range plan.Fixtures {
		dst := filepath.Join(stageDir, filepath.Base(f.Name))
		if err := os.WriteFile(dst, []byte(f.Content), 0o600); err != nil {
			return "", fmt.Errorf("forge tests %s: 写 fixture %s 失败: %w", gap.GapID, f.Name, err)
		}
	}
	tests := plan.Tests
	var b strings.Builder
	fmt.Fprintf(&b, "# 测试结果 %s\n\n", gap.GapID)
	fmt.Fprintf(&b, "- 工具：%s\n- 时间：%s\n\n", entryName, p.now().UTC().Format(time.RFC3339))
	passed := 0
	for i, tc := range tests {
		argv := append([]string{}, tc.Argv...)
		if offlineCapable {
			argv = append([]string{"unshare", "-n"}, argv...)
		}
		// 工作目录 = staging：argv 原样传（入口脚本名与输入文件
		// 都是 staging 内的相对路径）。2026-11-19 缺口 1 第五轮教训：
		// 把相对路径解析成绝对路径后，要求相对路径的脚本直接报错
		// exit 2（3/4 个用例因此失败），改走工作目录才是正解。
		res, h, err := p.Run.Run(executor.RunRequest{
			TaskID: taskID, SessionID: p.SessionID,
			ToolID: "forge-staging", Argv: argv, Dir: stageDir,
			Timeout: 25 * time.Second, Text: gap.TaskText,
		})
		if err != nil {
			fmt.Fprintf(&b, "## %d. %s — FAIL\n- 命令：%v\n- 错误：%v\n\n", i+1, tc.Name, argv, err)
			continue
		}
		if h != nil {
			res, err = h.Wait()
			if err != nil {
				fmt.Fprintf(&b, "## %d. %s — FAIL\n- 命令：%v\n- 错误：%v\n\n", i+1, tc.Name, argv, err)
				continue
			}
		}
		var ok bool
		if tc.ExpectFail {
			// 失败用例：工具应正确报错（退出码非 0）。
			ok = res.ExitCode != 0
		} else {
			ok = res.ExitCode == 0
		}
		if ok && tc.ExpectContains != "" && !strings.Contains(res.StdoutTail+res.ErrHead, tc.ExpectContains) {
			ok = false
		}
		status := "PASS"
		if !ok {
			status = "FAIL"
		} else {
			passed++
		}
		fmt.Fprintf(&b, "## %d. %s — %s\n- 命令：%v\n- 退出码：%d\n- 期望 stdout 含：%q\n- stdout 尾部：\n```\n%s\n```\n- stderr 头部：%s\n\n",
			i+1, tc.Name, status, argv, res.ExitCode, tc.ExpectContains, res.StdoutTail, res.ErrHead)
	}
	fmt.Fprintf(&b, "## 汇总\n通过 %d/%d\n", passed, len(tests))
	if passed < len(tests) {
		_ = p.Store.AppendEvent(Event{
			Event: EvTestFail, GapID: gap.GapID,
			Output: fmt.Sprintf("测试通过 %d/%d", passed, len(tests)),
			Result: "fail",
		})
		return b.String(), fmt.Errorf("forge tests %s: 只通过 %d/%d", gap.GapID, passed, len(tests))
	}
	_ = p.Store.AppendEvent(Event{
		Event: EvTestPass, GapID: gap.GapID,
		Output: fmt.Sprintf("测试通过 %d/%d", passed, len(tests)),
		Result: "ok",
	})
	return b.String(), nil
}

var errAdvisorRejected = errors.New("forge: 监工打回 2 轮仍未通过，驳回")

// advisorGate 是监工的一道门：审 body（固定第 1 检查项打头）。
// NOTE/CONCERN → 通过；HALT → 记 halt 事件，调 rework(note) 重做
// 一轮再审，最多 2 轮重做，第 3 次仍 HALT 则驳回。
// passEvent/haltEvent 是账本事件名。
func (p *Pipeline) advisorGate(ctx context.Context, taskID, title, body, workDir, haltEvent, passEvent, gapID, blueprintID string, rework func(note string) (string, error)) error {
	if p.Review == nil {
		return fmt.Errorf("forge: 未接监工")
	}
	for round := 0; round <= 2; round++ {
		verdict, note, err := p.Review(ctx, taskID, title, fixedCheckItem1+"\n\n"+body, workDir)
		if err != nil {
			return err
		}
		switch verdict {
		case "HALT":
			_ = p.Store.AppendEvent(Event{
				Event: haltEvent, GapID: gapID, BlueprintID: blueprintID,
				Input: title, Output: note, Result: "halt",
			})
			if round >= 2 {
				_ = p.Store.AppendEvent(Event{
					Event: haltEvent, GapID: gapID, BlueprintID: blueprintID,
					Output: "打回 2 轮仍未通过，驳回",
					Result: "rejected",
				})
				return errAdvisorRejected
			}
			if rework == nil {
				return errAdvisorRejected
			}
			body, err = rework(note)
			if err != nil {
				return err
			}
		case "CONCERN", "NOTE":
			_ = p.Store.AppendEvent(Event{
				Event: passEvent, GapID: gapID, BlueprintID: blueprintID,
				Input: title, Output: note, Result: "ok",
			})
			return nil
		default:
			return fmt.Errorf("forge: 监工 verdict %q 非法", verdict)
		}
	}
	return errAdvisorRejected
}

// blueprintReviewBody 是监工审蓝图的精简审阅体：检索留痕与蓝图要点
// 前置（监工证据截断后仍可见），蓝图全文与脚本全文在磁盘文件里。
func (p *Pipeline) blueprintReviewBody(bp *Blueprint, tr *retrieval.Trace) string {
	var b strings.Builder
	fmt.Fprintf(&b, "【复用检索留痕】\n%s\n\n", tr.Summary())
	fmt.Fprintf(&b, "【蓝图】工具名：%s\n风险等级：%s\n凭据声明：%s\n试造脚本：%s（%d 行）\n\n",
		bp.Name, bp.RiskLevel, bp.Credentials, bp.ScriptName, len(strings.Split(bp.Script, "\n")))
	fmt.Fprintf(&b, "描述：%s\n\n缺口分析：%s\n\n方案设计：%s\n\n实施计划：%s\n\n风险缓解：%s\n\n安全检查：%s\n\n",
		bp.Description, bp.GapAnalysis, bp.SolutionDesign, bp.ImplementationPlan, bp.RiskMitigation, bp.SafetyChecks)
	fmt.Fprintf(&b, "蓝图全文：data/forge/blueprints/%s.md；脚本全文：data/forge/staging/%s/%s\n",
		bp.SourceGap, bp.SourceGap, bp.ScriptName)
	return b.String()
}

// validCategory 校验分类（9 大类）；非法回退到 ops。
func (p *Pipeline) validCategory(c string) string {
	if c == "" {
		return "ops"
	}
	for _, cat := range p.Reg.CategoryMetas() {
		if cat.ID == c {
			return c
		}
	}
	return "ops"
}

// validSubcategory 校验子类；非法回退到该大类的第一个子类。
func (p *Pipeline) validSubcategory(category, sub string) string {
	if sub == "" {
		return ""
	}
	subs := p.Reg.SubcategoriesOf(category)
	for _, s := range subs {
		if s.ID == sub {
			return sub
		}
	}
	if len(subs) > 0 {
		return subs[0].ID
	}
	return ""
}
