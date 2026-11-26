// Package front 的规划循环 v2（前置判回执）。
//
// 设计（用户 2026-11-25 定，对齐二号/三号）：不设核验器、不设监工、
// 不设验收——前置规划（plan_task）→ 前置一步步派单 → 执行层干活交
// 回执 → 前置自己看回执判做完没有、结果符不符合要求；好则下一步，
// 不好叫重干（每步最多 2 次），全做完大白话汇报用户。
//
// 判卷是大模型在判，不是机械在判：前置模型读执行层的回执正文
// （四栏：干了什么、结果是什么、成果在哪里、用了哪些工具，栏目
// 空着也没关系），用大白话定下一步是继续/重干/改计划/停下，
// 代码跟着它的决定走；不要求回执 JSON、不要求判卷 JSON。
// 事实清单是辅助参考：网关随回执附免费事实清单（dispatch.FactList，
// 代码查，不花模型调用：置信度——工具调用分 + 成果文件核对分，
// 产物在不在/多大/新不新、verify_cmd 过没过、实质调用几次），
// 缺了不算失败，前置看回执正文与产物本身来判。
// verify_cmd 是可选的：前置想给某步加个校验命令可以加，不想加
// 就不加；没有 verify_cmd 不算不过关，声明了且跑了的结果只当
// 参考证据，不由代码一票否决。
//
// 事实清单只当参考，不一票否决（2026-11-26 用户定）：硬底线
// （HardFloorRed）只当一条记录供前置模型看，红了也不直接判失败；
// 前置看事实 + 回执正文 + 状态自己判，置信度低时先读报告、核对文件
// 再判。回执状态不是 done 时也可以判通过，只要事实证明主要成果已拿到。
// 快路径免判：置信度 100% 且回执状态 done 的步，代码直接推进，
// 不叫前置模型（省 60–70% 判回执调用）。只在三种情况叫前置：
// ① 置信度未满，② 事实清单红/黄，③ 全部步做完
// （最终核验 + 大白话汇报）。
// “pass”子串模糊匹配已删（"did not pass"/"password" 误判通过）。
//
// 账本分 role 记账（第 3 点）：front_plan / front_judge / executor；
// 钱顶已取消（2026-11-26 用户定）：不再设任务钱顶、规划子顶、
// 预算熔断，任务结束时算账汇报（本单花了多少 + DeepSeek 账户余额）。
// 步数/轮数/时间那几道保险还在，花钱不是无限的。
//
// 异步回调驱动（第 5 点）：网关在步完成时发 plan_step_callback 事件
// （dispatch_events）到内部队列；串行调度器（本循环）凭它叫醒前置。
// 本实现是同步循环（前置等回执），事件是 durable 记录；WAITING_STEP /
// RESUMABLE 阶段进 plan_state.json；看门狗超时靠 RenewDeadline 续期；
// CANCELLED 终态 + 每步派发前检查是否被叫停。
//
// 统一失败状态机（第 7 点）：同步重干 ≤2 次 → 该步可选且无依赖则
// 跳过，否则局部重规划（全任务重规划上限 ≤2 次）→ 超限停下诚实汇报。
// 取代方案里“重干 ≤2 则停”与“重试→跳过→改计划→停”两套重叠机制。
package front

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"007-super-agent/internal/dispatch"
	"007-super-agent/internal/llm"
	"007-super-agent/internal/sanitize"
	"007-super-agent/internal/session"
)

// 规划循环 v2 的常量。
// （2026-11-26 用户定：钱顶全不要——任务钱顶、规划子顶、预算熔断
// 全部取消，改成算账：任务结束时报本单总花费 + DeepSeek 账户余额。
// 步数/轮数/时间那几道保险还在，花钱不是无限的。）
const (
	// maxStepRedo 是每步最多重干次数（同步重干）。
	maxStepRedo = 2
	// maxTaskReplan 是全任务重规划上限次数。
	maxTaskReplan = 2
	// maxJudgeSelfChecks 是判回执时前置只读自查上限（次/步）。
	maxJudgeSelfChecks = 2
	// maxPlanLoopRounds 是计划循环总轮次熔断（防死循环）。
	maxPlanLoopRounds = 60
)

// truncateRunes 按字截断（本包本地版；session/ledger/llm 各有一份同名
// 小函数，包内不共享）。n<=0 时返回空。
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n])
}

// planGoal 取计划的目标人话（M-2）：优先 ps.Goal（前置 plan_task 给的），
// 为空时退回 TaskID（老计划没 Goal 字段）。judge/replan/汇报打印目标
// 时都用它，不要直接拿 TaskID 当目标给模型看。
func planGoal(ps *session.PlanState) string {
	if ps == nil {
		return ""
	}
	if strings.TrimSpace(ps.Goal) != "" {
		return ps.Goal
	}
	return ps.TaskID
}

// carryoverNote 取计划的翻新接续摘要（L-3）：非空时截约 2000 字拼进
// judge/replan/汇报的提示词，让模型判回执、改计划、写汇报时知道
// 这单前面已经干过什么（翻新前翻新后的进度不断档）。
func carryoverNote(ps *session.PlanState) string {
	if ps == nil || strings.TrimSpace(ps.Carryover) == "" {
		return ""
	}
	return "前面会话的接续摘要（翻新带过来的，别当没发生过）：\n" + truncateRunes(ps.Carryover, 2000) + "\n"
}

// planVerdict 是前置判回执的结论。
type planVerdict string

const (
	verdictPass   planVerdict = "pass"   // 这步过了，推进下一步
	verdictRedo   planVerdict = "redo"   // 这步没做好，重干一次
	verdictReplan planVerdict = "replan" // 计划本身有问题，改计划
	verdictStop   planVerdict = "stop"   // 停下，诚实汇报（不再推进）
)

// isStalledRedoReason 判重干原因是否来自 stalled/max_iterations
// （执行层已读过材料但没及时交活）：原因里带“事实清单缺失”
// （judgeFallback 对 stalled/max_iterations 回执的兜底重干文案）、
// “stalled”或“max_iterations”字样。
func isStalledRedoReason(reason string) bool {
	if strings.TrimSpace(reason) == "" {
		return false
	}
	lower := strings.ToLower(reason)
	for _, kw := range []string{"事实清单缺失", "stalled", "max_iterations", "停滞"} {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// stepToAction 把计划里的一步转成单步 AGENT 派单 Action：
// 步骤表只有这一行（工具编号、inputs、预期产出、验收），工具编号
// 只有这一步的，验收与步类型都取自该步；PlanStepNo 让 dispatch()
// 直接用该步号，不再 AllocateStep 另占一位。
// prevSummaries 是前面已完成步的摘要（供本步执行参考，如上一步的产物路径）。
func stepToAction(p PlanArgs, s PlanStep, memFragments []string, prevSummaries []string) Action {
	verify := ""
	if len(s.ExpectedArtifacts) > 0 {
		verify = "验收：产物 " + strings.Join(s.ExpectedArtifacts, "、")
	} else if len(s.VerifyCmd) > 0 {
		verify = "验收：命令 " + strings.Join(s.VerifyCmd, " ")
	} else {
		verify = "验收：无机器检查（纯调研步），以回执结论与事实为准"
	}
	inputs := ""
	if len(s.Inputs) > 0 {
		inputs = "，inputs=[" + strings.Join(s.Inputs, " ") + "]"
	}
	skeleton := fmt.Sprintf("步骤%d：工具 %s%s；预期：%s；%s",
		s.StepNo, s.ToolID, inputs, s.ExpectedOutput, verify)
	// M-2：本单只执行这一步，严禁提前越界执行后续步骤——
	// 派单自动带入用户原话（含全单要求），执行层顺手把后几步的活
	// 全干完会直接触发“越界重干”（prod-real-002 第 1 步）。
	// 用户原话仅作背景参考。
	skeleton += "；本单只执行第 " + fmt.Sprintf("%d", s.StepNo) + " 步，严禁提前越界执行后续步骤（用户原话仅作背景参考）"
	// 回执四栏（对齐二号三号：回执内容系统性一点、好整理好总结）：
	// 交回执按四栏写——干了什么、结果是什么、成果在哪里、用了哪些工具；
	// 栏目空着也没关系，不编造；没产出文件就填“无”，严禁照抄派单里的
	// 预期路径充当成果，未成功执行的工具不要填入“用了哪些工具”。
	// 前置看实质内容判，不因空栏判不过关。
	skeleton += "；交回执按四栏写：干了什么、结果是什么、成果在哪里、用了哪些工具（栏目空着也没关系，不编造；没产出文件就填“无”，严禁照抄派单里的预期路径充当成果，未成功执行的工具不要填入“用了哪些工具”）"
	// M-2：重干时把上次失败原因带给执行层，免得它原样重犯同一个错。
	if strings.TrimSpace(s.LastRedoReason) != "" {
		skeleton += "；上次失败原因：" + s.LastRedoReason + "，这次注意避开这个坑，不要原样重犯"
	}
	// M-1：stalled/max_iterations 触发的重干——上次已读过相关材料但
	// 未及时交活，本次禁止重复全量读取旧文件，直接整合已有信息写出
	// 交付物（重干原因里带“事实清单缺失”/stalled/max_iterations
	// 字样时才加这句，别的不用）。
	if isStalledRedoReason(s.LastRedoReason) {
		skeleton += "；上次已读过相关材料但未及时交活，本次禁止重复全量读取旧文件，请直接整合已有信息写出交付物"
	}
	memory := append([]string{}, memFragments...)
	for _, ps := range prevSummaries {
		memory = append(memory, ps)
	}
	stepType := s.StepType
	if stepType == "" {
		stepType = p.TaskType
	}
	if stepType == "" {
		stepType = "execute"
	}
	// EstSteps 动态（CF 诊断高优）：规划给了就用规划值，否则按步类型
	// 给默认值——research=12（StepCap 24）、execute=10（StepCap 20）。
	// 原来写死 1 → StepCap 10，调研步读几轮文件就撞墙（cf-010/011/012）。
	estSteps := s.EstSteps
	if estSteps <= 0 {
		if stepType == "research" {
			estSteps = 12
		} else {
			estSteps = 10
		}
	}
	return Action{
		Action: "dispatch", Route: "AGENT",
		Goal:              fmt.Sprintf("%s（第 %d 步：%s）", p.Goal, s.StepNo, s.ExpectedOutput),
		Criteria:          p.Criteria,
		ToolIDs:           []string{s.ToolID},
		Plan:              []string{skeleton},
		Memory:            memory,
		MustReadMemories:  p.MustReadMemories,
		Pointers:          p.Pointers,
		Constraints:       p.Constraints,
		EstSteps:          estSteps,
		EstSeconds:        300,
		TaskType:          stepType,
		ExpectedArtifacts: s.ExpectedArtifacts,
		VerifyCmd:         s.VerifyCmd,
		PlanStepNo:        s.StepNo,
		// 方向 b / H-1 / H-2：本步的账本起点、首轮时间、重干原因，
		// 从计划步里原样带给派单（首次派发前已由 runPlan 记下，
		// 重干时原样透传，绝不更新）。
		FirstExecIDBefore: s.FirstExecIDBefore,
		FirstStartTime:    s.FirstStartTime,
		LastRedoReason:    s.LastRedoReason,
	}
}

// taskTotalCost 读本任务全部角色的费用（毫分）：规划 + 判卷 + 执行 +
// 翻新 + 终报全算，不漏无标签调用（SummarizeExpenseByTask 按任务汇总，
// 只排除 selfevolve-summary 这类后台自进化调用）。Led 为空（测试）
// 时返回 0，不报错。2026-11-26：钱顶取消后只用于算账汇报（本单花了多少），
// 不再用于任何上限判断。
func (f *Front) taskTotalCost(taskID string) int64 {
	if f.Led == nil {
		return 0
	}
	sum, err := f.Led.SummarizeExpenseByTask(taskID)
	if err != nil {
		log.Printf("front task=%s 读全任务费用失败（按 0 论）: %v", taskID, err)
		return 0
	}
	return sum.CostCNYMilli
}

// deepSeekBalanceURL 是查 DeepSeek 账户余额的接口（2026-11-26 核实可用：
// GET /user/balance，Bearer 密钥，返回 {"is_available":true,
// "balance_infos":[{"currency":"CNY","total_balance":"48.12",...}]}）。
const deepSeekBalanceURL = "https://api.deepseek.com/user/balance"

// queryDeepSeekBalance 查 DeepSeek 账户剩余余额（人民币，CNY）。
// 尽力而为：密钥找不到、接口失败、解析失败都返回 ("", false)，
// 调用方只报“余额未知”，绝不能因为查余额失败而让任务失败。
// 密钥只进 Authorization 头，绝不写日志。
func queryDeepSeekBalance() (string, bool) {
	key, ok := llm.ResolveAPIKey()
	if !ok || key == "" {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, deepSeekBalanceURL, nil)
	if err != nil {
		return "", false
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	var v struct {
		IsAvailable  bool `json:"is_available"`
		BalanceInfos []struct {
			Currency     string `json:"currency"`
			TotalBalance string `json:"total_balance"`
		} `json:"balance_infos"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", false
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return "", false
	}
	if !v.IsAvailable {
		return "", false
	}
	for _, b := range v.BalanceInfos {
		if b.Currency == "CNY" && b.TotalBalance != "" {
			if amt, err := strconv.ParseFloat(b.TotalBalance, 64); err == nil {
				return fmt.Sprintf("%.2f", amt), true
			}
			return b.TotalBalance, true
		}
	}
	return "", false
}

// accountingLine 算本单账：本单总花费（毫分→元）+ DeepSeek 账户余额。
// 余额查不到时写“余额未知”，不影响任务成败。
func (f *Front) accountingLine(taskID string) string {
	spent := float64(f.taskTotalCost(taskID)) / 1000.0
	if bal, ok := queryDeepSeekBalance(); ok {
		return fmt.Sprintf("本单花了 ¥%.2f，账户余额 ¥%s。", spent, bal)
	}
	return fmt.Sprintf("本单花了 ¥%.2f（账户余额未知）。", spent)
}

// savePlanState 存计划（f.sess 为空时不做，测试走内存计划）。
func (f *Front) savePlanState(ps *session.PlanState) error {
	if f.sess == nil {
		return nil
	}
	return f.sess.SavePlan(ps)
}

// completePlanStep 终结一步（f.sess 为空时只改内存计划）。
func (f *Front) completePlanStep(ps *session.PlanState, stepNo int, status session.StepStatus, summary string, artifacts []string, failure string) error {
	if f.sess != nil {
		if err := f.sess.CompleteStep(ps.SessionID, stepNo, status, summary, artifacts, failure); err != nil {
			return err
		}
	}
	// 内存计划同步（CompleteStep 已改过落盘计划；内存这份也要一致）。
	for i := range ps.Steps {
		if ps.Steps[i].StepNo == stepNo {
			ps.Steps[i].Status = status
			ps.Steps[i].Summary = summary
			ps.Steps[i].Artifacts = artifacts
			break
		}
	}
	// L-5：CurrentStep 只在这里推一次（原来循环里和循环后各推一次，
	// 重复赋值，删掉循环里的那行）。
	ps.CurrentStep = stepNo + 1
	return nil
}

// reopenPlanStep 重开一步（判回执后决定重干时用）。
// reason 是上一次判回执叫重干的原因（M-2）：写进该步的 LastRedoReason，
// 下次派单带给执行层，免得它原样重犯同一个错；reason 为空时不覆盖旧值。
func (f *Front) reopenPlanStep(ps *session.PlanState, stepNo int, reason string) error {
	if f.sess != nil {
		if err := f.sess.ReopenStep(ps.SessionID, stepNo, reason); err != nil {
			return err
		}
	}
	for i := range ps.Steps {
		if ps.Steps[i].StepNo == stepNo {
			ps.Steps[i].Status = session.StepRunning
			ps.Steps[i].Attempts++
			if strings.TrimSpace(reason) != "" {
				ps.Steps[i].LastRedoReason = truncateRunes(reason, 300)
			}
			ps.CurrentStep = stepNo
			break
		}
	}
	ps.CurrentStep = stepNo
	ps.Phase = session.PhaseRunning
	return nil
}

// setPlanPhase 改计划阶段（f.sess 为空时只改内存）。
func (f *Front) setPlanPhase(ps *session.PlanState, phase session.PlanPhase) {
	ps.Phase = phase
	if f.sess != nil {
		if err := f.sess.SetPhase(ps.SessionID, phase); err != nil {
			log.Printf("front plan %s 改阶段 %s 失败: %v", ps.PlanID, phase, err)
		}
	}
}

// renewWatchdog 续期看门狗（长计划跑过 1800 秒时，免得被当死任务）：
// 每步派发前调一次。RenewDeadline 为空（测试）时跳过。
func (f *Front) renewWatchdog(taskID string) {
	if f.RenewDeadline == nil {
		return
	}
	f.RenewDeadline(taskID)
}

// openPlanState 打开或新建计划状态：有未终结的旧计划时断点续跑
// （用旧计划的步骤表与进度，不丢已完成的步）；没有时按 PlanArgs 新建。
// 返回（计划状态，是否续跑，错误）。
func (f *Front) openPlanState(sessID, taskID string, p PlanArgs) (*session.PlanState, bool, error) {
	if f.sess == nil {
		// 无会话存盘（测试）：内存计划，不做断点续跑。
		ps := &session.PlanState{
			PlanID: psPlanID(sessID, taskID), SessionID: sessID, TaskID: taskID,
			Goal:   p.Goal, // M-2：judge/replan 打印目标时用 Goal，不用 TaskID。
			Status: session.PlanRunning, Phase: session.PhaseRunning,
			CreatedTS: time.Now().UTC().Format(time.RFC3339),
			UpdatedTS: time.Now().UTC().Format(time.RFC3339),
		}
		for _, s := range p.Steps {
			ps.Steps = append(ps.Steps, planStepFromArgs(s, taskID))
		}
		ps.TotalSteps = len(ps.Steps)
		ps.CurrentStep = 1
		return ps, false, nil
	}
	ps, isNew, err := f.sess.OpenSession(sessID, taskID)
	if err != nil {
		return nil, false, err
	}
	if !isNew {
		// 断点续跑：旧计划未终结，用它的步骤表与进度。
		// 新 PlanArgs 与旧计划不一致时以旧计划为准（它有进度），记日志。
		if len(ps.Steps) == 0 {
			return nil, false, fmt.Errorf("session: 会话 %q 旧计划无步骤，无法续跑", sessID)
		}
		// M-2：旧计划没 Goal 时（P2-b 之前建的计划）用新 PlanArgs 的补上。
		if ps.Goal == "" {
			ps.Goal = p.Goal
		}
		ps.Phase = session.PhaseResumable
		if err := f.sess.SavePlan(ps); err != nil {
			return nil, false, err
		}
		log.Printf("front plan %s 断点续跑：会话 %s，当前第 %d 步，共 %d 步", ps.PlanID, sessID, ps.CurrentStep, len(ps.Steps))
		return ps, true, nil
	}
	// 新计划：按 PlanArgs 填步骤表。
	ps.Goal = p.Goal // M-2：judge/replan 打印目标时用 Goal，不用 TaskID。
	for _, s := range p.Steps {
		ps.Steps = append(ps.Steps, planStepFromArgs(s, taskID))
	}
	ps.TotalSteps = len(ps.Steps)
	ps.CurrentStep = 1
	ps.Phase = session.PhaseRunning
	if err := f.sess.SavePlan(ps); err != nil {
		return nil, false, err
	}
	return ps, false, nil
}

// psPlanID 给内存计划（无会话存盘）造一个计划号。
func psPlanID(sessID, taskID string) string {
	return "mem-" + sessID + "-" + taskID
}

// planStepFromArgs 把前置的 PlanStep 转成会话的 PlanStep。
func planStepFromArgs(s PlanStep, taskID string) session.PlanStep {
	stepType := session.StepExecute
	st := s.StepType
	if st == "research" {
		stepType = session.StepResearch
	}
	return session.PlanStep{
		StepNo: s.StepNo, TaskID: taskID, ToolID: s.ToolID, Inputs: s.Inputs,
		DependsOn: s.DependsOn, StepType: stepType, Optional: s.Optional,
		ExpectedOutput:    s.ExpectedOutput,
		ExpectedArtifacts: s.ExpectedArtifacts, VerifyCmd: s.VerifyCmd,
		EstSteps: s.EstSteps,
		Status:   session.StepPending, Attempts: 0,
	}
}

// stepHasMachineCheck 判某步有没有机器检查（expected_artifacts 或 verify_cmd）。
func stepHasMachineCheck(s session.PlanStep) bool {
	return len(s.ExpectedArtifacts) > 0 || len(s.VerifyCmd) > 0
}

// stepIsOptionalAndUndepended 判某步是否可选且无依赖（可跳过）：
// Optional 为真（前置的 PlanStep.Optional 原样带过来），且没有其他步
// 的 depends_on 引用它。默认不可跳过（宁严勿松）。
func stepIsOptionalAndUndepended(ps *session.PlanState, stepNo int) bool {
	var target *session.PlanStep
	for i := range ps.Steps {
		if ps.Steps[i].StepNo == stepNo {
			target = &ps.Steps[i]
			break
		}
	}
	if target == nil || !target.Optional {
		return false
	}
	for i := range ps.Steps {
		for _, d := range ps.Steps[i].DependsOn {
			if d == stepNo {
				return false
			}
		}
	}
	return true
}

// prevStepSummaries 取前面已完成步的摘要（供下一步执行参考）。
func prevStepSummaries(ps *session.PlanState, uptoStep int) []string {
	var out []string
	lastDone := 0
	for _, s := range ps.Steps {
		if s.StepNo < uptoStep && (s.Status == session.StepDone || s.Status == session.StepSkipped) {
			line := fmt.Sprintf("第 %d 步已完成：%s", s.StepNo, s.Summary)
			if len(s.Artifacts) > 0 {
				line += "；产物：" + strings.Join(s.Artifacts, "、")
			}
			out = append(out, line)
			// 只记 DONE 步：跳过的步没执行过，没有详细报告文件。
			if s.Status == session.StepDone {
				lastDone = s.StepNo
			}
		}
	}
	// 详细报告指针（CF-015 教训：交接摘要被截到 200 字，上一步的
	// 关键路径全丢了，执行层只能重读文件）：指向上一步的详细报告，
	// 让执行层先读它再动手，不用靠摘要猜。
	if lastDone > 0 && ps != nil && ps.TaskID != "" {
		out = append(out, fmt.Sprintf("上一步的详细报告在 artifacts/%s/step-%d-reply.md，先读它再动手。", ps.TaskID, lastDone))
	}
	return out
}

// runPlan 是规划循环 v2 的主循环：前置一步步派单、每步看回执判。
// 终结时写好 reply.md，返回 FrontResult（Status done/failed/stalled）。
func (f *Front) runPlan(ctx context.Context, taskID, taskText, artDir string, memFragments []string, plan PlanArgs, sessionID string) *FrontResult {
	sessID := sessionID
	if sessID == "" {
		sessID = taskID
	}
	if err := session.ValidateSessionID(sessID); err != nil {
		log.Printf("front task=%s session_id 非法（%v），规划循环走内存计划", taskID, err)
		sessID = session.DefaultSessionID(taskID)
	}
	ps, resumed, err := f.openPlanState(sessID, taskID, plan)
	if err != nil {
		return f.failPlanReply(artDir, taskID, plan.Goal, fmt.Sprintf("打开计划失败：%v", err), ps)
	}
	if resumed {
		log.Printf("front task=%s 规划循环断点续跑（计划 %s）", taskID, ps.PlanID)
	}
	replans := ps.Replans
	rounds := 0
	for {
		rounds++
		if rounds > maxPlanLoopRounds {
			return f.failPlanReply(artDir, taskID, plan.Goal, fmt.Sprintf("计划循环超 %d 轮熔断，停下诚实汇报", maxPlanLoopRounds), ps)
		}
		// 找下一步：当前步起第一个 PENDING/RUNNING 步。
		idx := -1
		for i := range ps.Steps {
			if ps.Steps[i].Status == session.StepPending || ps.Steps[i].Status == session.StepRunning {
				idx = i
				break
			}
		}
		if idx < 0 {
			// 没有待跑的步了：全部 DONE/SKIPPED/FAILED 已终结 → 最终核验 + 汇报。
			return f.finishPlan(artDir, taskID, ps)
		}
		step := ps.Steps[idx]
		// P2-b：翻新检查（派单前）：水位 >200K 先翻新再开工。翻新后
		// 会话号变了、计划表已继承到新会话；重读计划、continue 重新找步。
		// 翻新失败记日志、不阻塞派单（翻新是安全阀）。新会话水位清零，
		// 下一轮检查必不翻，不会死循环。
		if f.sess != nil {
			if newID, rolled, rerr := f.sess.MaybeRollover(ctx, sessID, taskID, plan.Goal, session.DefaultRolloverConfig(), f.summarizer()); rerr != nil {
				log.Printf("front task=%s 第 %d 步翻新检查失败，继续用旧会话: %v", taskID, step.StepNo, rerr)
			} else if rolled {
				log.Printf("front task=%s 第 %d 步翻新：会话 %s → %s", taskID, step.StepNo, sessID, newID)
				sessID = newID
				nps, lerr := f.sess.LoadPlan(sessID)
				if lerr != nil || nps == nil {
					return f.failPlanReply(artDir, taskID, plan.Goal, fmt.Sprintf("翻新后读新计划失败: %v", lerr), ps)
				}
				ps = nps
				continue
			}
		}
		// 每步派发前检查是否被叫停（CANCELLED 终态）。
		if f.sess != nil {
			if cur, lerr := f.sess.LoadPlan(sessID); lerr == nil && cur != nil && cur.Status == session.PlanCancelled {
				return f.failPlanReply(artDir, taskID, plan.Goal, "任务已被叫停（CANCELLED），不再派新步", ps)
			}
		}
		// 看门狗续期 + 阶段置为等回执。
		f.renewWatchdog(taskID)
		f.setPlanPhase(ps, session.PhaseWaitingStep)
		// 方向 b 严谨版：本步首次派发前，记下账本起点与开始时间
		// （只记一次，重干不覆盖；记失败只记日志、不阻塞派单；
		// 内存里的步同步更新，stepToAction 才能带上）。
		// “记没记过”以 FirstStartTime 是否已设置（非零）为准——
		// 账本为空时 FirstExecIDBefore 就是 0，不能拿它当信号。
		if f.sess != nil && step.FirstStartTime.IsZero() {
			var execIDBefore int64
			if f.Led != nil {
				if maxID, merr := f.Led.MaxExecutionID(); merr == nil {
					execIDBefore = maxID
				} else {
					log.Printf("front task=%s 第 %d 步取账本最大行号失败（按 0 记）: %v", taskID, step.StepNo, merr)
				}
			}
			now := time.Now()
			if rerr := f.sess.RecordStepStart(sessID, step.StepNo, execIDBefore, now); rerr != nil {
				log.Printf("front task=%s 第 %d 步记账本起点失败（不阻塞派单）: %v", taskID, step.StepNo, rerr)
			} else {
				ps.Steps[idx].FirstExecIDBefore = execIDBefore
				ps.Steps[idx].FirstStartTime = now
				step = ps.Steps[idx]
			}
		}
		// 派一步。
		act := stepToAction(plan, frontStepFromSession(step), memFragments, prevStepSummaries(ps, step.StepNo))
		res, derr := f.dispatch(ctx, artDir, taskID, taskText, act, memFragments, sessID)
		if derr != nil {
			// 派单本身失败（校验/超限等）：按步失败处理，走失败状态机。
			log.Printf("front task=%s 第 %d 步派单失败: %v", taskID, step.StepNo, derr)
			outcome := f.handleStepFailure(ctx, taskID, artDir, ps, step, fmt.Sprintf("派单失败：%v", derr), &replans, plan, memFragments)
			if outcome != nil {
				return outcome
			}
			continue
		}
		if res == nil {
			return f.failPlanReply(artDir, taskID, plan.Goal, fmt.Sprintf("第 %d 步派单无结果", step.StepNo), ps)
		}
		if res.Status == "stalled" && res.Receipt == nil {
			// 等回执超时：执行仍在跑，网关不终结（watchdog 兜底，
			// 回执落盘后下次唤醒再汇报）；计划保持 RUNNING 可续跑。
			f.setPlanPhase(ps, session.PhaseResumable)
			log.Printf("front task=%s 第 %d 步等回执超时，计划保持 RUNNING 待续跑", taskID, step.StepNo)
			return &FrontResult{Status: "stalled", Route: res.Route, DispatchID: res.DispatchID, ToolIDs: res.ToolIDs}
		}
		r := res.Receipt
		if r == nil {
			// 有状态无回执（如 failed 且无回执体）：按步失败处理。
			outcome := f.handleStepFailure(ctx, taskID, artDir, ps, step, fmt.Sprintf("第 %d 步无回执（状态 %s）", step.StepNo, res.Status), &replans, plan, memFragments)
			if outcome != nil {
				return outcome
			}
			continue
		}
		// 收到回执（网关已在步完成时记了 plan_step_callback 事件）：
		// 判这步：事实清单作参考 → 快路径 → 前置判回执。
		verdict, reason := f.judgeReceipt(ctx, taskID, ps, step, r)
		f.setPlanPhase(ps, session.PhaseRunning)
		switch verdict {
		case verdictPass:
			summary := stepSummaryFromReceipt(step, r, reason)
			if cerr := f.completePlanStep(ps, step.StepNo, session.StepDone, summary, r.Artifacts, ""); cerr != nil {
				log.Printf("front task=%s 第 %d 步落盘失败: %v", taskID, step.StepNo, cerr)
			}
			log.Printf("front task=%s 第 %d 步通过（%s）", taskID, step.StepNo, reason)
		case verdictRedo:
			outcome := f.handleStepRedo(ctx, taskID, artDir, ps, step, reason, &replans, plan, memFragments)
			if outcome != nil {
				return outcome
			}
		case verdictReplan:
			outcome := f.handleStepReplan(ctx, taskID, artDir, ps, step, reason, &replans, plan, memFragments)
			if outcome != nil {
				return outcome
			}
		case verdictStop:
			return f.failPlanReply(artDir, taskID, plan.Goal, fmt.Sprintf("第 %d 步停下：%s", step.StepNo, reason), ps)
		}
	}
}

// frontStepFromSession 把会话的 PlanStep 转回前置的 PlanStep
// （stepToAction 用；step_type、optional 与 expected_output 从会话步还原）。
func frontStepFromSession(s session.PlanStep) PlanStep {
	st := "execute"
	if s.StepType == session.StepResearch {
		st = "research"
	}
	return PlanStep{
		StepNo: s.StepNo, ToolID: s.ToolID, Inputs: s.Inputs,
		DependsOn: s.DependsOn, StepType: st, Optional: s.Optional,
		ExpectedOutput: s.ExpectedOutput, LastRedoReason: s.LastRedoReason,
		ExpectedArtifacts: s.ExpectedArtifacts, VerifyCmd: s.VerifyCmd,
		EstSteps:          s.EstSteps,
		FirstExecIDBefore: s.FirstExecIDBefore, FirstStartTime: s.FirstStartTime,
	}
}

// stepSummaryFromReceipt 给一步写步间摘要（≤1000 字，供下一步参考与最终汇报用）。
func stepSummaryFromReceipt(step session.PlanStep, r *dispatch.Receipt, reason string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "工具 %s，结论：%s；判定：%s", step.ToolID, r.Conclusion, reason)
	if len(r.Artifacts) > 0 {
		fmt.Fprintf(&b, "；产物：%s", strings.Join(r.Artifacts, "、"))
	}
	if r.FactList != nil {
		fmt.Fprintf(&b, "；事实：%s", r.FactList.Summary())
	}
	s := b.String()
	if len([]rune(s)) > 1000 {
		s = string([]rune(s)[:1000])
	}
	return s
}

// judgeReceipt 判一步的回执：事实清单只当参考 → 快路径免判 → 前置判回执。
// 事实清单（代码判断）只出事实当参考，不一票否决；前置看事实 + 回执
// 正文 + 状态自己判。返回（结论，原因人话）。
func (f *Front) judgeReceipt(ctx context.Context, taskID string, ps *session.PlanState, step session.PlanStep, r *dispatch.Receipt) (planVerdict, string) {
	// 回执本身不是 done（stalled/max_iterations/agent_error/failed）：
	// 不锁死，由前置模型看事实来判。状态不是 done 时模型要多看一眼，
	// 事实证明主要成果已拿到（置信度高、主要文件核对通过）时可以判通过。
	if r.Status != "done" {
		return f.judgeByModel(ctx, taskID, ps, step, r, "回执状态是 "+r.Status+"（不是 done），请多看一眼：核对事实清单与回执正文，如果事实证明主要成果已经拿到（置信度高、主要文件核对通过），可以判通过；否则按实际情况判重干/改计划/停下")
	}
	fl := r.FactList
	if fl == nil {
		// 事实清单缺失：只是辅助参考没了，不算失败——走前置判回执
		// （模型看回执正文 + 只读自查），按回执与产物本身来判。
		log.Printf("front task=%s 第 %d 步事实清单缺失，走前置判回执", taskID, step.StepNo)
		return f.judgeByModel(ctx, taskID, ps, step, r, "事实清单缺失（辅助参考没了，不算失败），请结合回执文字与只读自查判断")
	}
	// 事实清单只当参考：硬底线（HardFloorRed）不再一票否决，只当一条
	// 记录供前置模型看——红了也不直接判失败，由模型结合事实来判。
	// 快路径（2026-11-26 用户定）：置信度 100% 且回执状态 done 的步，
	// 代码直接推进，不叫前置模型。
	if r.Status == "done" && fl.Confidence == 100 {
		return verdictPass, "快路径：置信度 100%（" + fl.Summary() + "）"
	}
	// 其他情况叫前置模型判（置信度未满、事实红/黄、无机器检查的纯调研步）。
	v, reason := f.judgeByModel(ctx, taskID, ps, step, r, "")
	// CF-014：账本压过报告——模型判了重干/改计划/停下，但账本证明这步
	// 确实干了实活、声明产物都在盘上，报告那句“没产出/失败”不算数，
	// 以账本为准判通过（最多是报告写错了，不是活没干完）。
	if v != verdictPass && ledgerOverrulesReport(r) {
		return verdictPass, fmt.Sprintf("账本为证：回执说没产出/失败，但账本显示成功调用 %d 次、实质 %d 次，声明产物都在盘上——以账本为准，这步过了（模型原判 %s：%s）", r.FactList.SuccessfulCalls, r.FactList.SubstantiveCalls, v, reason)
	}
	return v, reason
}

// judgeByModel 叫前置模型判一步的回执（front_judge 角色）：
// 输入是回执 + 事实清单 + 该步的预期；模型用大白话定结论
// （通过 / 重干 / 改计划 / 停下），代码跟着它的决定走。
// 模型调用失败 / 结论写不明白时，按事实与回执兜底（judgeFallback），
// 绝不因判不了就标通过。
func (f *Front) judgeByModel(ctx context.Context, taskID string, ps *session.PlanState, step session.PlanStep, r *dispatch.Receipt, note string) (planVerdict, string) {
	call := f.CallWithToolsJudge
	if call == nil {
		call = f.CallWithTools
	}
	if call == nil {
		// 无模型调用（测试桩）：按事实清单兜底。
		return judgeFallback(r)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "你是 007 的前置，正在判执行层交回的一步回执。只看事实，不信漂亮话。\n")
	fmt.Fprintf(&b, "任务目标：%s\n", planGoal(ps))
	fmt.Fprintf(&b, "这一步：第 %d 步，工具 %s，预期：%s\n", step.StepNo, step.ToolID, stepSummaryExpected(step))
	if note := carryoverNote(ps); note != "" {
		b.WriteString(note)
	}
	if len(step.ExpectedArtifacts) > 0 {
		fmt.Fprintf(&b, "声明产物：%s\n", strings.Join(step.ExpectedArtifacts, "、"))
	}
	if len(step.VerifyCmd) > 0 {
		fmt.Fprintf(&b, "校验命令：%s\n", strings.Join(step.VerifyCmd, " "))
	}
	fmt.Fprintf(&b, "回执状态：%s\n回执结论：%s\n", r.Status, r.Conclusion)
	if len(r.Artifacts) > 0 {
		fmt.Fprintf(&b, "回执产物：%s\n", strings.Join(r.Artifacts, "、"))
	}
	if r.FactList != nil {
		fmt.Fprintf(&b, "事实清单（代码查的，不花模型调用）：%s\n", r.FactList.Summary())
	} else {
		fmt.Fprintf(&b, "事实清单：缺失\n")
	}
	// 详细报告指针（CF-015 教训：回执结论被截，判卷只看结论会误判）：
	// 这步的详细报告全文在 step-<N>-reply.md，拿不准时先读它再判。
	if step.StepNo > 0 {
		fmt.Fprintf(&b, "这步的详细报告在 artifacts/%s/step-%d-reply.md（回执结论只是它的前 2000 字），拿不准时先读它再判。\n", taskID, step.StepNo)
	}
	if note != "" {
		fmt.Fprintf(&b, "注意：%s\n", note)
	}
	// 对齐二号三号：判卷用大白话，不回 JSON——先写结论（四个词之一
	// 写在第一行：通过 / 重干 / 改计划 / 停下），再写一句话原因。
	// 结论词要写清楚，代码就靠它认你的决定；写不明白就走事实兜底。
	b.WriteString("请用大白话判这步，不要回 JSON。第一行只写结论四个词之一：通过 / 重干 / 改计划 / 停下；第二行写一句话原因。\n")
	b.WriteString("通过=这步做完了、结果符合预期；重干=这步没做好、值得重干一次；改计划=计划本身有问题（如环境与假设矛盾）、要改后面的步；停下=做不下去了、停下诚实汇报。\n")
	b.WriteString("账本为准：事实清单里成功调用、实质调用的数和产物在不在，是代码查的账本说了算；回执结论里写“没产出”“失败了”，但账本显示确实干了实活、产物也在盘上时，以账本为准，判通过，不许改计划。\n")
	b.WriteString("事实清单只当参考：置信度、调用次数、文件核对是代码查的参考信息，不是一票否决；你看事实 + 回执正文 + 状态自己判。置信度低时先读详细报告、核对文件再判。回执状态不是 done 时也可以判通过，只要事实证明主要成果已经拿到。\n")
	b.WriteString("核对结果里“判为 成果文件缺失”只是代码按时间戳算的，你要看缺失的原因再判：如果缺失的文件“都是这单里早一步做的”，并且文件内容验过是对的（校验命令通过，或者报告里核对过内容无误），就判通过，不要重干。如果缺失的文件里“有以前的旧文件”、“有不存在的”、“有是空的”，或者内容不对，置信度低于 100%，那就直接根据回执内容核实之后，再根据你核实的实际结果决定是否重干。\n")
	b.WriteString("判的时候可以结合你手里的只读检查工具（read、bash）自己查文件、查目录、查账本再定，但最多查 " + fmt.Sprint(maxJudgeSelfChecks) + " 次。")
	frozen := "你是 007 超级 Agent 的前置，负责判执行层交回的回执。只看事实，不信漂亮话；拿不准就叫重干，不要硬标通过。"
	// M-1：真把 read/bash 传给模型判回执（提示词早说了能自查，代码
	// 却传 tools=nil、还丢弃 ToolCalls——言行不一）。模型最多自查
	// maxJudgeSelfChecks 次，每次结果回填进历史再判；自查完还不给
	// 结论时按事实兜底，不再追问。
	tools := []llm.ToolDefinition{
		llm.NewToolDefinition(frontToolRead, "读文件：读一个绝对路径的文件（小文件全文，超 8KB 给结构/签名）。只读，什么都不改。", readSchema),
		llm.NewToolDefinition(frontToolBash, "只读 bash：只用来看和查（ls/cat/grep/状态），跑完机器上什么都不许变；要改东西一律不许用。", bashSchema),
	}
	var history []llm.Message
	tail := b.String()
	for selfCheck := 0; selfCheck <= maxJudgeSelfChecks; selfCheck++ {
		m, fr, promptTokens, _, err := call(taskID, frozen, tail, tools, history)
		if err != nil {
			log.Printf("front task=%s 第 %d 步判回执模型调用失败，按事实兜底: %v", taskID, step.StepNo, err)
			return judgeFallback(r)
		}
		if fr == "length" {
			// 截断：本轮作废，按事实兜底（P0-1 截断拦截，前置原生也要拦）。
			log.Printf("front task=%s 第 %d 步判回执被截断，按事实兜底", taskID, step.StepNo)
			return judgeFallback(r)
		}
		// L-4：判回执的 token 也记水位（翻新阈值用），不然水位低估。
		if f.sess != nil && ps.SessionID != "" && promptTokens > 0 {
			if rerr := f.sess.RecordTokens(ps.SessionID, promptTokens); rerr != nil {
				log.Printf("front task=%s 判回执记水位失败: %v", taskID, rerr)
			}
		}
		if len(m.ToolCalls) == 0 {
			return parseJudgeVerdict(m.Content, r)
		}
		if selfCheck >= maxJudgeSelfChecks {
			// H-1：自查次数用完还不给结论——追加 1 次 tools=nil 的
			// 最终收口调用，强制模型只输出“第一行结论词 + 第二行原因”；
			// 这次调用也报错或无法解析才落入 judgeFallback。
			log.Printf("front task=%s 第 %d 步判回执自查 %d 次仍无结论，追加收口调用", taskID, step.StepNo, maxJudgeSelfChecks)
			return f.judgeFinalCloseOut(ctx, taskID, ps, step, r, frozen, history)
		}
		// 回填工具结果，继续问结论。
		history = append(history, llm.Message{Role: "user", Content: tail})
		history = append(history, m)
		for _, tc := range m.ToolCalls {
			var out string
			switch tc.Function.Name {
			case frontToolRead:
				var args struct {
					Path string `json:"path"`
				}
				if jerr := json.Unmarshal([]byte(tc.Function.Arguments), &args); jerr != nil {
					out = "读文件失败：参数解析失败：" + jerr.Error()
				} else if res, rerr := f.doRead(args.Path); rerr != nil {
					out = "读文件失败：" + rerr.Error()
				} else {
					out = res
				}
			case frontToolBash:
				var args struct {
					Command string `json:"command"`
				}
				if jerr := json.Unmarshal([]byte(tc.Function.Arguments), &args); jerr != nil {
					out = "只读查看失败：参数解析失败：" + jerr.Error()
				} else if res, rerr := f.doBash(ctx, args.Command); rerr != nil {
					out = "只读查看失败：" + rerr.Error()
				} else {
					out = res
				}
			default:
				out = "不支持的自查工具：" + tc.Function.Name + "（只许 read、bash）"
			}
			history = append(history, llm.ToolMessage(tc.ID, out))
		}
		tail = ""
	}
	return judgeFallback(r)
}

// judgeFinalCloseOut 是判回执的最终收口（H-1）：自查次数用完、模型
// 仍不给结论时，追加 1 次 tools=nil 的调用，强制模型只输出
// “第一行结论词 + 第二行原因”；这次调用也报错、被截断或无法解析
// 才落入 judgeFallback。
// parseJudgeVerdict 认不出结论时内部已落入 judgeFallback。
func (f *Front) judgeFinalCloseOut(ctx context.Context, taskID string, ps *session.PlanState, step session.PlanStep, r *dispatch.Receipt, frozen string, history []llm.Message) (planVerdict, string) {
	call := f.CallWithToolsJudge
	if call == nil {
		call = f.CallWithTools
	}
	if call == nil {
		return judgeFallback(r)
	}
	tail := "你已经用完自查次数，现在必须直接给结论，不要再调任何工具。第一行只写结论四个词之一：通过 / 重干 / 改计划 / 停下；第二行写一句话原因。\n"
	m, fr, promptTokens, _, err := call(taskID, frozen, tail, nil, history)
	if err != nil {
		log.Printf("front task=%s 第 %d 步判回执收口调用失败，按事实兜底: %v", taskID, step.StepNo, err)
		return judgeFallback(r)
	}
	if fr == "length" {
		log.Printf("front task=%s 第 %d 步判回执收口被截断，按事实兜底", taskID, step.StepNo)
		return judgeFallback(r)
	}
	// L-4：收口调用的 token 也记水位（翻新阈值用），不然水位低估。
	if f.sess != nil && ps.SessionID != "" && promptTokens > 0 {
		if rerr := f.sess.RecordTokens(ps.SessionID, promptTokens); rerr != nil {
			log.Printf("front task=%s 判回执收口记水位失败: %v", taskID, rerr)
		}
	}
	if len(m.ToolCalls) == 0 {
		return parseJudgeVerdict(m.Content, r)
	}
	// tools=nil 不应再有工具调用；万一有，按无法解析论处。
	log.Printf("front task=%s 第 %d 步判回执收口仍带工具调用，按事实兜底", taskID, step.StepNo)
	return judgeFallback(r)
}

// stepSummaryExpected 取一步的预期（人话短句）。
func stepSummaryExpected(step session.PlanStep) string {
	if len(step.ExpectedArtifacts) > 0 {
		return "产出 " + strings.Join(step.ExpectedArtifacts, "、")
	}
	if len(step.VerifyCmd) > 0 {
		return "通过校验命令 " + strings.Join(step.VerifyCmd, " ")
	}
	return "完成调研并给出结论"
}

// reportNoOutputWords 是回执结论里“没产出”的说法（CF-014 的报告写
// “未产出、邮箱断链”，不在认输词表里，得另列）。
var reportNoOutputWords = []string{
	"未产出", "没产出", "没有产出", "无产出",
	"未生成", "没生成", "没有生成",
	"断链",
}

// reportAdmitsFailure 判回执结论是不是在说“这步没做完/没产出”
// （自认失败）：含认输词，或明确说没产出/断链。先剔除无害上下文，
// “之前的失败后来成功了”这类事后总结不算认输（同 substantiveConclusion）。
// 空结论也算没交代（账本说了算）。
func reportAdmitsFailure(r *dispatch.Receipt) bool {
	c := strings.TrimSpace(r.Conclusion)
	if c == "" {
		return true
	}
	lower := strings.ToLower(c)
	for _, h := range substantiveHarmlessContexts {
		lower = strings.ReplaceAll(lower, h, "")
	}
	if isOvercomeFailure(lower) {
		for _, w := range substantiveDefeatPhrases {
			if strings.Contains(lower, w) {
				return true
			}
		}
		return false
	}
	for _, w := range substantiveDefeatWords {
		if strings.Contains(lower, w) {
			return true
		}
	}
	for _, w := range reportNoOutputWords {
		if strings.Contains(lower, w) {
			return true
		}
	}
	return false
}

// ledgerOverrulesReport 判账本是不是压过报告（CF-014）：
// 回执结论写着“没产出/失败”，但事实清单显示这步确实干了实活
// （成功调用与实质调用都 ≥1）、声明产物都在盘上且合格、校验命令
// 也没失败——这时以账本为准，报告那句话不算数。
// 声明产物是必要条件：没声明产物的步，账本上没有产物证据，
// 报告说失败就按失败论（守住“注册失败必须重干”的老回归）；
// 有声明产物且全合格，才压过报告。校验命令失败时绝不压过
// （机器检查是实打实的失败证据，不是报告的空话）。
func ledgerOverrulesReport(r *dispatch.Receipt) bool {
	if !reportAdmitsFailure(r) {
		return false
	}
	if r.Status != "done" || r.FactList == nil {
		return false
	}
	fl := r.FactList
	// 硬底线不再当关卡：0 次调用、产物缺失、产物有问题这三条，
	// 下面成功调用 / 产物列表 / 产物 Problem 的检查已经各自拦了，
	// 不用再单独看 HardFloorRed。
	if fl.SuccessfulCalls < 1 || fl.SubstantiveCalls < 1 {
		return false
	}
	if fl.VerifyCmd != nil && !fl.VerifyCmd.Passed {
		return false
	}
	if len(fl.Artifacts) == 0 {
		return false
	}
	for _, a := range fl.Artifacts {
		if a.Problem != "" {
			return false
		}
	}
	return true
}

// judgeFallback 按事实与回执兜底判一步（模型判不了时才走这里）。
// 事实清单只当参考：硬底线不再一票否决，只当一条记录；全绿→通过；
// 中间态看回执本身：done + 结论实在 + 成功调用 ≥1 就通过，否则重干；
// 校验命令明确失败时中间态也不得放行（H-3，fail-closed：重干）。
// 事实清单缺失：不再直接标重干——看回执正文与产物本身：
// 回执状态 done、结论是实话（非空、非“执行结束”套话）、且回执里
// 有产物指针（buildReceipt 只列磁盘上真实存在的文件）→ 判通过；
// 否则重干（由状态机管次数，超限会停下汇报）。
// 注意：FactList==nil 分支只验“产物在列”，不验产物非空与新鲜度——
// 实况中清单缺失只出现在非 done 路径（网关只在 agent_error /
// delivery_halt 时不算清单），此分支是防御兜底，不是主路径。
func judgeFallback(r *dispatch.Receipt) (planVerdict, string) {
	if r.FactList != nil {
		if r.FactList.AllGreen {
			return verdictPass, "事实全绿（判回执兜底）"
		}
		// H-3：校验命令明确失败时，中间态不得直接放行——
		// “校验命令失败交模型看证据再定”，绝不是“模型挂了就无视
		// 校验失败”。模型判不了落入兜底时，fail-closed：重干。
		if r.FactList.VerifyCmd != nil && !r.FactList.VerifyCmd.Passed {
			return verdictRedo, "校验命令未通过且模型未能判定（判回执兜底重干）"
		}
		// CF-014：账本压过报告——报告写着没产出/失败，但账本显示确实干了
		// 实活、声明产物都在盘上，以账本为准判通过；不许只凭报告那句话重干。
		if ledgerOverrulesReport(r) {
			return verdictPass, fmt.Sprintf("账本为证：回执说没产出/失败，但账本显示成功调用 %d 次、实质 %d 次，声明产物都在盘上——以账本为准，这步过了（判回执兜底）", r.FactList.SuccessfulCalls, r.FactList.SubstantiveCalls)
		}
		// 中间态（不红也不绿）：research 步天生无机器检查、全绿不了，
		// 没声明产物/校验命令的 execute 步也是——不能因为“没全绿”
		// 就重干（对齐 E2E 任务 A：调研步回执实在、网关已验过，
		// 兜底重干烧光了 ¥0.05 预算）。看回执本身：done + 结论实在
		// + 成功调用 ≥1 就通过，否则重干。0 次调用仍被成功调用数拦住；
		// 产物问题不再由硬底线一票否决，账本压过报告那支已单独拦了
		// 产物有问题的自动放行，这里只按回执本身判。
		if r.Status == "done" && substantiveConclusion(r) && r.FactList.SuccessfulCalls >= 1 {
			return verdictPass, "回执自认完成、结论实在且有成功调用（判回执兜底）"
		}
		return verdictRedo, "事实未全绿（判回执兜底）：" + r.FactList.Summary()
	}
	// 事实清单缺失：看回执正文与产物本身，不直接标重干。
	if r.Status == "done" {
		if substantiveConclusion(r) && len(r.Artifacts) > 0 {
			return verdictPass, "回执自认完成、结论与产物齐全（事实清单缺失，兜底判通过）"
		}
	}
	return verdictRedo, "事实清单缺失且回执不完整，无法核实（兜底重干）"
}

// substantiveConclusion 判回执结论是不是实在话（不是空话/套话/空壳）。
// M-2：四栏模板启用后，光“非空”不够——四栏全填“无”或自述失败的
// 回执也得拦。做法：剥掉四个栏目名与标点，只数剩下的实际内容
// （至少 8 个有效字符），且不含自我认输词；套话原文直接判不实在。
func substantiveConclusion(r *dispatch.Receipt) bool {
	c := strings.TrimSpace(r.Conclusion)
	if c == "" || c == "执行结束，状态：done" {
		return false
	}
	lower := strings.ToLower(c)
	// 先剔除无害上下文（cf-011 教训：诚实提到“报错文件”被“报错”
	// 一词误杀），再匹配败词。
	for _, h := range substantiveHarmlessContexts {
		lower = strings.ReplaceAll(lower, h, "")
	}
	// “失败”一词：先排除“之前的失败后来成功了”这类事后总结
	// （如“之前失败的重试也过了”），再匹配——“注册失败”这类
	// 当前步自认失败仍拦（M-2 回归测试要求）。
	if isOvercomeFailure(lower) {
		for _, w := range substantiveDefeatPhrases {
			if strings.Contains(lower, w) {
				return false
			}
		}
		return len([]rune(stripReceiptTemplate(c))) >= 8
	}
	for _, w := range substantiveDefeatWords {
		if strings.Contains(lower, w) {
			return false
		}
	}
	return len([]rune(stripReceiptTemplate(c))) >= 8
}

// isOvercomeFailure 判结论是否在说“之前的失败后来成功了”
// （如“之前失败的重试也过了”）：这类事后总结不是自认失败，
// 不应被“失败”一词拦住。须同时有“过去失败”与“后来成功”两半。
func isOvercomeFailure(lower string) bool {
	if !strings.Contains(lower, "失败") {
		return false
	}
	hasPast := strings.Contains(lower, "之前") || strings.Contains(lower, "上次") || strings.Contains(lower, "重试")
	hasSuccess := strings.Contains(lower, "也过了") || strings.Contains(lower, "已通过") || strings.Contains(lower, "已成功") || strings.Contains(lower, "最终成功") || strings.Contains(lower, "重试成功")
	return hasPast && hasSuccess
}

// substantiveDefeatWords 是结论里的自我认输词：带上这些词，
// 结论再长也不是“做完了”的实在话。
// “报错”已剔除（cf-011 教训：回执里诚实提到“报错文件”被“报错”
// 一词误杀；无害上下文见下，先剔除）。“失败”保留——“注册失败”
// 这类当前步自认失败必须拦（M-2 回归测试）；“之前失败的重试
// 也过了”这类事后总结由 isOvercomeFailure 先排除。
// 与 Gemini 处方的偏离：处方要求把“失败”也换成明确短语，但那样
// “注册失败”/“登录失败”这类动词+失败的自认失败就拦不住了，
// 现有回归测试 TestJudgeFallbackEmptyTemplateOrFailureConclusion
// 明确要求拦住它；故保留“失败”，用无害上下文 + 已克服排除
// 实现处方目标（不误杀诚实提及），而不丢正确拦截。
var substantiveDefeatWords = []string{
	"失败", "没做成", "无法完成", "未能完成", "没有做成",
	"未完成", "没完成", "没有完成",
}

// substantiveDefeatPhrases 是“已克服的失败”总结里仍要拦的明确
// 自认失败短语（不含泛词“失败”，免得把事后总结误杀）。
var substantiveDefeatPhrases = []string{
	"任务失败", "执行失败", "未能完成", "无法完成", "没有做成", "没做成",
	"未完成", "没完成", "没有完成",
}

// substantiveHarmlessContexts 是“报错”字样出现但不是自认失败的
// 无害上下文：匹配败词前先从结论里剔除，别让诚实提及误杀。
var substantiveHarmlessContexts = []string{
	"报错文件", "无报错", "未报错", "没有报错",
}

// stripReceiptTemplate 剥掉回执四栏的栏目名与标点，只剩实际内容
// （路径、文件名、工具名里的字符照常计数，它们是实在的成果指针）。
func stripReceiptTemplate(c string) string {
	s := c
	for _, label := range []string{"干了什么", "结果是什么", "成果在哪里", "用了哪些工具"} {
		s = strings.ReplaceAll(s, label, "")
	}
	var b []rune
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
			continue
		}
		b = append(b, r)
	}
	return string(b)
}

// parseJudgeVerdict 解析模型判回执的大白话结论（对齐二号三号：
// 前置模型自己判，不要求 JSON 格式）。
// 第一行优先：提示词要求模型第一行只写结论词（通过 / 重干 / 改计划 /
// 停下），第二行写原因。第一行合规范就直接采纳，绝不被第二行的解释
// 文字反向劫持（H-1：第二行“主动停下”“无需重干”不能推翻第一行
// 的“通过”）；第一行不合规范才退化到全文扫描，拿不准走 judgeFallback。
// H-4 红线保留：不做 "pass" 子串模糊匹配——"did not pass"、
// "password" 都不能被误判通过；英文单词只认词边界后的完整词。
// M-1：原因原样保留（截 300 字内）传给重干避坑机制，不丢。
func parseJudgeVerdict(content string, r *dispatch.Receipt) (planVerdict, string) {
	first, rest := splitJudgeFirstLine(content)
	if v, extra, ok := matchJudgeFirstLine(first); ok {
		reason := strings.TrimSpace(extra)
		if rest != "" {
			if reason != "" {
				reason += "\n" + rest
			} else {
				reason = rest
			}
		}
		return v, judgeReason(reason)
	}
	// 第一行不合规范：退化到全文扫描。否定优先于肯定（重干是安全
	// 方向），肯定只认明确结论短语，拿不准走 judgeFallback。
	lower := strings.ToLower(strings.TrimSpace(content))
	if containsAny(lower, judgeRedoWords) {
		return verdictRedo, judgeReason(content)
	}
	if containsAny(lower, judgeStopWords) {
		return verdictStop, judgeReason(content)
	}
	if containsAny(lower, judgeReplanWords) {
		return verdictReplan, judgeReason(content)
	}
	if containsAny(lower, judgePassPhrases) {
		return verdictPass, judgeReason(content)
	}
	return judgeFallback(r)
}

// judgeReason 把模型写的原话当 reason 返回（M-1：重干原因要传给
// 执行层避坑，丢了就等于盲目重跑）：去首尾空白，截 300 字以内；
// 空了才用默认文案。
func judgeReason(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "模型判：结论未附原因（大白话结论）"
	}
	return truncateRunes(s, 300)
}

// splitJudgeFirstLine 按行拆出第一个非空行（结论行）与剩余行（原因）。
func splitJudgeFirstLine(content string) (first, rest string) {
	lines := strings.Split(content, "\n")
	for i, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		first = ln
		rest = strings.TrimSpace(strings.Join(lines[i+1:], "\n"))
		return first, rest
	}
	return "", ""
}

// judgeFirstLineOpener 是第一行可采纳的结论开头（结论词或常见结论短语）。
type judgeFirstLineOpener struct {
	word string
	v    planVerdict
}

var judgeFirstLineOpeners = []judgeFirstLineOpener{
	{"可以通过", verdictPass}, {"通过", verdictPass}, {"做完了", verdictPass}, {"已完成", verdictPass},
	{"完成了", verdictPass}, {"内容正确", verdictPass}, {"没问题", verdictPass},
	{"没有问题", verdictPass}, {"确认无误", verdictPass}, {"符合预期", verdictPass},
	{"做对了", verdictPass}, {"写对了", verdictPass},
	{"pass", verdictPass},
	{"重干", verdictRedo}, {"没做好", verdictRedo}, {"没做完", verdictRedo},
	{"没有做完", verdictRedo}, {"未完成", verdictRedo}, {"没通过", verdictRedo},
	{"没有通过", verdictRedo}, {"未通过", verdictRedo}, {"不通过", verdictRedo},
	{"未做完", verdictRedo}, {"没完成", verdictRedo}, {"没有完成", verdictRedo},
	{"redo", verdictRedo},
	{"改计划", verdictReplan}, {"计划有问题", verdictReplan}, {"replan", verdictReplan},
	{"停下", verdictStop}, {"做不下去", verdictStop}, {"stop", verdictStop},
}

// matchJudgeFirstLine 认第一行结论：精确匹配或以结论开头就采纳，
// 并把开头词之后的那半行当原因的一部分返回；同一行里带否定词时
// 不采纳（防“通过检查但失败了”这类行首介词误判），退化全文扫描。
// 英文词只认词边界后的完整词（"password" 不能命中 "pass"）。
func matchJudgeFirstLine(first string) (planVerdict, string, bool) {
	f := normalizeJudgeLine(first)
	if f == "" {
		return "", "", false
	}
	lower := strings.ToLower(f)
	for _, o := range judgeFirstLineOpeners {
		if lower == o.word {
			return o.v, "", true
		}
		if !strings.HasPrefix(lower, o.word) {
			continue
		}
		// 开头词之后必须是词边界（分隔符或中文字符，不能是英文字母），
		// 否则 "password" 会误命中 "pass"（H-4）。
		rest := lower[len(o.word):]
		if rest != "" {
			if r := []rune(rest)[0]; r >= 'a' && r <= 'z' {
				continue
			}
		}
		// 同行否定词直接 contradict 开头结论 → 不采纳，退化全文扫描。
		if containsAny(rest, judgeRedoWords) {
			return "", "", false
		}
		// 开头词之后的半行是原因的一部分：去前导分隔符再返回。
		extra := strings.TrimLeft(strings.TrimSpace(f[len(o.word):]), "，,、；;：: ")
		return o.v, extra, true
	}
	return "", "", false
}

// normalizeJudgeLine 清洗一行结论：去空白、Markdown 加粗、
// “结论：”类前缀与行尾标点，剩下的才拿去认结论词。
func normalizeJudgeLine(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "*")
	s = strings.TrimSpace(s)
	for _, p := range []string{"结论：", "结论:", "结论", "verdict:", "verdict：", "Verdict:"} {
		if strings.HasPrefix(s, p) {
			s = strings.TrimSpace(strings.TrimPrefix(s, p))
			break
		}
	}
	s = strings.TrimRight(s, "。！!；;，,、")
	return strings.TrimSpace(s)
}

// judgeStopWords：全文扫描里认“停下”的词。
var judgeStopWords = []string{"停下", "做不下去", "stop"}

// judgeReplanWords：全文扫描里认“改计划”的词。
var judgeReplanWords = []string{"改计划", "计划有问题", "replan"}

// judgeRedoWords：全文扫描里认“重干”的否定词（重干是安全方向）。
// 含复合否定（“不符合预期”不能被“符合预期”反向命中；“未能通过”
// 不能被“通过”反向命中）与英文 verdict（模型惯性回 JSON 时用）。
var judgeRedoWords = []string{
	"重干", "没做好", "没做完", "没有做完", "未完成", "没通过", "没有通过",
	"未通过", "不通过", "未做完", "没完成", "没有完成",
	"不符合预期", "未符合预期", "未能通过", "无法通过", "不能通过",
	"不予通过", "尚未完成", "只完成了一半", "并未完成",
	"失败", "为空", "是空的", "重新执行", "redo",
	"did not pass", "didn't pass", "not pass", "not passed",
	"failed", "failure",
}

// judgePassPhrases：全文扫描里认“通过”的明确结论短语。
// 孤立的“通过”已移除（中文里“通过”常作介词，如“试图通过 python
// 写入”）；只认带结论语境的短语，拿不准走 judgeFallback。
var judgePassPhrases = []string{
	"结论：通过", "结论:通过", "判为通过", "验证通过", "审核通过", "可以通过",
}

// containsAny 判 s 里是否含词表中的任一词。
func containsAny(s string, words []string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

// handleStepRedo 处理“重干”：同步重干 ≤2 次 → 该步可选且无依赖则
// 跳过，否则局部重规划（上限 ≤2 次）→ 超限停下诚实汇报。
// 返回非空表示本单已终结（停下/跳过后无步可跑也会在主循环终结，
// 这里只在“停下”时返回终结结果）。
func (f *Front) handleStepRedo(ctx context.Context, taskID, artDir string, ps *session.PlanState, step session.PlanStep, reason string, replans *int, plan PlanArgs, memFragments []string) *FrontResult {
	if step.Attempts < maxStepRedo {
		// 还能重干：重开这步（Attempts+1），主循环继续派它。
		// M-2：把失败原因带进去（写进该步的 LastRedoReason），
		// 下次派单告诉执行层上次错在哪，免得原样重犯。
		if err := f.reopenPlanStep(ps, step.StepNo, reason); err != nil {
			log.Printf("front task=%s 第 %d 步重开失败: %v", taskID, step.StepNo, err)
		}
		log.Printf("front task=%s 第 %d 步重干（第 %d 次）：%s", taskID, step.StepNo, step.Attempts+1, reason)
		return nil
	}
	// 重干 2 次还不过：可选且无依赖 → 跳过；否则局部重规划。
	if stepIsOptionalAndUndepended(ps, step.StepNo) {
		summary := fmt.Sprintf("跳过：重干 %d 次仍不过，且为可选无依赖步；原因：%s", maxStepRedo, reason)
		if cerr := f.completePlanStep(ps, step.StepNo, session.StepSkipped, summary, nil, ""); cerr != nil {
			log.Printf("front task=%s 第 %d 步跳过落盘失败: %v", taskID, step.StepNo, cerr)
		}
		log.Printf("front task=%s 第 %d 步跳过（可选无依赖）", taskID, step.StepNo)
		return nil
	}
	// 局部重规划。
	return f.handleStepReplan(ctx, taskID, artDir, ps, step, reason, replans, plan, memFragments)
}

// handleStepFailure 处理派单失败/无回执：与 handleStepRedo 同一套状态机
// （重干 ≤2 → 可选跳过 → 局部重规划 → 停下）。
func (f *Front) handleStepFailure(ctx context.Context, taskID, artDir string, ps *session.PlanState, step session.PlanStep, reason string, replans *int, plan PlanArgs, memFragments []string) *FrontResult {
	return f.handleStepRedo(ctx, taskID, artDir, ps, step, reason, replans, plan, memFragments)
}

// handleStepReplan 处理“改计划”：叫前置模型重排剩余步骤
// （front_plan 角色），校验通过后替换计划表（未跑的步），重规划
// 次数 +1；上限 2 次，超了停下诚实汇报。
func (f *Front) handleStepReplan(ctx context.Context, taskID, artDir string, ps *session.PlanState, step session.PlanStep, reason string, replans *int, plan PlanArgs, memFragments []string) *FrontResult {
	if *replans >= maxTaskReplan {
		return f.failPlanReply(artDir, taskID, plan.Goal,
			fmt.Sprintf("第 %d 步重规划已达 %d 次上限，停下：%s", step.StepNo, maxTaskReplan, reason), ps)
	}
	newSteps, err := f.replanSteps(ctx, taskID, ps, step, reason)
	if err != nil {
		return f.failPlanReply(artDir, taskID, plan.Goal,
			fmt.Sprintf("第 %d 步要改计划，但重规划失败（%v），停下：%s", step.StepNo, err, reason), ps)
	}
	// 替换未跑的步（已 DONE/SKIPPED 的不动；当前步标 FAILED 后替换）。
	kept := make([]session.PlanStep, 0, len(ps.Steps))
	for _, s := range ps.Steps {
		if s.StepNo < step.StepNo && (s.Status == session.StepDone || s.Status == session.StepSkipped) {
			kept = append(kept, s)
		}
	}
	failedStep := step
	failedStep.Status = session.StepFailed
	failedStep.Summary = "改计划前最后一次尝试失败：" + reason
	kept = append(kept, failedStep)
	nextNo := step.StepNo + 1
	// M-7：模型给的新步 step_no 是它自己编的号（从 1 开始），重编
	// 后 DependsOn 要按 oldNo→newNo 映射表重算——原来直接把模型
	// 的号原样留下，依赖会指错步（或指到已终结的步）。
	oldToNew := make(map[int]int, len(newSteps))
	for i, ns := range newSteps {
		oldToNew[ns.StepNo] = nextNo + i
	}
	for _, ns := range newSteps {
		ns.StepNo = nextNo
		ns.TaskID = taskID
		ns.Status = session.StepPending
		ns.Attempts = 0
		// DependsOn：能映射的按表换号；映射不到的（指向已保留的步
		// 或失败步本身）原样保留——依赖的步还在计划里。
		for j, dep := range ns.DependsOn {
			if mapped, ok := oldToNew[dep]; ok {
				ns.DependsOn[j] = mapped
			}
		}
		kept = append(kept, ns)
		nextNo++
	}
	ps.Steps = kept
	ps.TotalSteps = len(kept)
	ps.CurrentStep = step.StepNo + 1
	ps.Replans++
	*replans = ps.Replans
	ps.Phase = session.PhaseRunning
	if err := f.savePlanState(ps); err != nil {
		log.Printf("front task=%s 重规划后存盘失败: %v", taskID, err)
	}
	log.Printf("front task=%s 第 %d 步改计划（第 %d 次重规划），新步数 %d", taskID, step.StepNo, ps.Replans, len(newSteps))
	return nil
}

// replanSteps 叫前置模型重排剩余步骤（front_plan 角色）：
// 输入是已完成步的摘要 + 卡住的这步 + 原因；输出 JSON 的 steps 数组
// （与 plan_task 的 steps 同格式）。解析/校验失败返回错误。
func (f *Front) replanSteps(ctx context.Context, taskID string, ps *session.PlanState, step session.PlanStep, reason string) ([]session.PlanStep, error) {
	call := f.CallWithTools
	if call == nil {
		return nil, fmt.Errorf("无模型调用，无法重规划")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "你是 007 的前置，正在改一个多步任务的计划。原计划在第 %d 步卡住了。\n", step.StepNo)
	fmt.Fprintf(&b, "任务目标：%s\n", planGoal(ps))
	if note := carryoverNote(ps); note != "" {
		b.WriteString(note)
	}
	fmt.Fprintf(&b, "已完成步：\n")
	for _, s := range ps.Steps {
		if s.Status == session.StepDone || s.Status == session.StepSkipped {
			fmt.Fprintf(&b, "- 第 %d 步（%s）：%s\n", s.StepNo, s.ToolID, s.Summary)
		}
	}
	fmt.Fprintf(&b, "卡住的步：第 %d 步，工具 %s，原因：%s\n", step.StepNo, step.ToolID, reason)
	b.WriteString("请给出修订后的剩余步骤（从下一步开始，粗粒度 3–5 步），只回一个 JSON 对象，不要任何解释：\n")
	b.WriteString("{\"steps\":[{\"step_no\":1,\"tool_id\":\"真实工具编号\",\"inputs\":[\"参数\"],\"expected_output\":\"这步做完应得到什么\",\"expected_artifacts\":[\"/绝对路径/产物\"],\"verify_cmd\":[\"命令\"],\"step_type\":\"execute|research\",\"optional\":false}]}\n")
	b.WriteString("注意：工具编号必须是注册表里真实存在的；inputs 不许 shell 语法与相对路径；execute 步必须有 expected_artifacts 或 verify_cmd；step_type 是 research 的步 verify 可空。")
	frozen := "你是 007 超级 Agent 的前置，负责把多步任务拆成粗粒度步骤表。只用注册表里真实存在的工具编号。"
	// CF-014：校验打回重改——模型给的结果解析失败或校验失败（如编了个
	// 不存在的工具编号）时，把错处原样喂回模型重改，最多
	// maxConsecutiveValidationErrors（2）次；2 次都改不对才认失败。
	// 模型调用失败、截断不是模型答案的错，不重改，直接返回。
	prompt := b.String()
	var lastErr error
	for attempt := 0; attempt < maxConsecutiveValidationErrors; attempt++ {
		m, fr, promptTokens, _, err := call(taskID, frozen, prompt, nil, nil)
		if err != nil {
			return nil, fmt.Errorf("重规划模型调用失败：%w", err)
		}
		// L-4：重规划的 token 也记水位（翻新阈值用），不然水位低估。
		if f.sess != nil && ps.SessionID != "" && promptTokens > 0 {
			if rerr := f.sess.RecordTokens(ps.SessionID, promptTokens); rerr != nil {
				log.Printf("front task=%s 重规划记水位失败: %v", taskID, rerr)
			}
		}
		if fr == "length" || len(m.ToolCalls) > 0 {
			return nil, fmt.Errorf("重规划模型返回不可用（截断或带工具调用）")
		}
		content := strings.TrimSpace(m.Content)
		content = strings.TrimPrefix(content, "```json")
		content = strings.TrimPrefix(content, "```")
		content = strings.TrimSuffix(content, "```")
		content = strings.TrimSpace(content)
		var v struct {
			Steps []PlanStep `json:"steps"`
		}
		if err := json.Unmarshal([]byte(content), &v); err != nil {
			lastErr = fmt.Errorf("重规划结果解析失败：%w", err)
		} else if len(v.Steps) == 0 {
			lastErr = fmt.Errorf("重规划结果 steps 为空")
		} else {
			// 校验：把新步拼成一份 PlanArgs 走 validatePlan（目标用原目标）。
			np := PlanArgs{Goal: "重规划后的剩余步骤", Steps: v.Steps, TaskType: "execute"}
			// task_type 按步各自的 step_type 论，整单默认 execute 即可。
			if err := f.validatePlan(np); err != nil {
				lastErr = fmt.Errorf("重规划结果校验失败：%w", err)
			} else {
				var out []session.PlanStep
				for _, s := range v.Steps {
					out = append(out, planStepFromArgs(s, taskID))
				}
				return out, nil
			}
		}
		// 本轮结果不可用：把错处原样喂回下一轮（最后一轮不用喂，直接认失败）。
		if attempt+1 < maxConsecutiveValidationErrors {
			log.Printf("front task=%s 重规划第 %d 次被打回：%v，重改一次", taskID, attempt+1, lastErr)
			prompt = b.String() + "\n上次改计划的结果被校验打回：" + lastErr.Error() + "。请按这个错处修正，只回一个 JSON 对象，不要任何解释。\n"
		}
	}
	return nil, lastErr
}

// finishPlan 全部步做完：最终核验 + 大白话汇报（叫前置模型写汇报，
// front_judge 角色；失败时用模板汇报兜底），计划标 DONE。
func (f *Front) finishPlan(artDir, taskID string, ps *session.PlanState) *FrontResult {
	// M-6：标 DONE 前先校验——至少 1 步 StepDone，且最后一步（最大
	// StepNo）不是未替换的 StepFailed；否则走失败汇报，不标 DONE
	//（防“一步没做完、最后一步还挂着失败”却标完成的假 DONE）。
	done := 0
	lastNo, lastStatus := 0, session.StepPending
	for _, s := range ps.Steps {
		if s.Status == session.StepDone {
			done++
		}
		if s.StepNo > lastNo {
			lastNo, lastStatus = s.StepNo, s.Status
		}
	}
	if done < 1 || lastStatus == session.StepFailed {
		return f.failPlanReply(artDir, taskID, planGoal(ps),
			fmt.Sprintf("终结校验未通过：已做完 %d 步，最后一步（第 %d 步）状态是 %s，不能标完成", done, lastNo, lastStatus), ps)
	}
	// L-5：计划已被叫停（CANCELLED）时不许再标 DONE——终结状态不动
	//（不覆写 CANCELLED）；只写一封说明回信，状态 failed。
	if ps.Status == session.PlanCancelled {
		report := "这个任务已被叫停，没能做完。\n\n已做完 " + fmt.Sprint(done) + "/" + fmt.Sprint(len(ps.Steps)) + " 步。\n"
		report += "\n" + f.accountingLine(taskID) + "\n"
		report, _, _ = sanitize.ScrubOutbound(report)
		if err := writeFileAtomic(filepath.Join(artDir, "reply.md"), []byte(report), 0o600); err != nil {
			log.Printf("front task=%s 写叫停回信失败: %v", taskID, err)
		}
		log.Printf("front task=%s 计划已被叫停，不标 DONE", taskID)
		return &FrontResult{Status: "failed", Route: "AGENT", ToolIDs: planToolIDs(ps)}
	}
	report := f.finalReport(taskID, ps)
	// 算账（2026-11-26 用户定：钱顶取消，任务结束时报本单花费 + 账户余额）。
	report += "\n" + f.accountingLine(taskID) + "\n"
	// M-6：最终汇报落盘前先过出站脱敏（模型写的汇报可能带回
	// 执行层产物里的密钥/原文）。
	report, _, _ = sanitize.ScrubOutbound(report)
	if err := writeFileAtomic(filepath.Join(artDir, "reply.md"), []byte(report), 0o600); err != nil {
		log.Printf("front task=%s 写最终回信失败: %v", taskID, err)
		return &FrontResult{Status: "failed", Route: "AGENT"}
	}
	ps.Status = session.PlanDone
	ps.Phase = session.PhaseRunning
	if err := f.savePlanState(ps); err != nil {
		log.Printf("front task=%s 计划标 DONE 存盘失败: %v", taskID, err)
	}
	log.Printf("front task=%s 计划 %s 全部做完，已汇报", taskID, ps.PlanID)
	return &FrontResult{Status: "done", Route: "AGENT", ToolIDs: planToolIDs(ps)}
}

// planToolIDs 取计划里用过的工具编号（去重，供 FrontResult）。
// ps 为空时返回空（openPlanState 失败时 failPlanReply 也要能调）。
func planToolIDs(ps *session.PlanState) []string {
	if ps == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range ps.Steps {
		if s.ToolID != "" && !seen[s.ToolID] {
			seen[s.ToolID] = true
			out = append(out, s.ToolID)
		}
	}
	return out
}

// finalReport 写最终汇报：先叫前置模型（front_summary 角色）用大白话
// 总结；模型不可用时用模板汇报兜底（按步列结果，不编造）。
func (f *Front) finalReport(taskID string, ps *session.PlanState) string {
	// H-3：终报走 front_summary 角色（不再混进 front_judge）；
	// 未接线时退回 CallWithToolsJudge，再退回 CallWithTools。
	call := f.CallWithToolsSummary
	if call == nil {
		call = f.CallWithToolsJudge
	}
	if call == nil {
		call = f.CallWithTools
	}
	if call != nil {
		var b strings.Builder
		b.WriteString("你是 007 的前置，任务全部做完了。请用大白话给用户写一封汇报（不要术语、不要堆概念，像跟朋友说话一样），只回汇报正文。\n")
		fmt.Fprintf(&b, "任务：%s\n", planGoal(ps))
		if note := carryoverNote(ps); note != "" {
			b.WriteString(note)
		}
		b.WriteString("各步结果：\n")
		for _, s := range ps.Steps {
			st := "做完"
			if s.Status == session.StepSkipped {
				st = "跳过"
			} else if s.Status == session.StepFailed {
				st = "没做完"
			}
			fmt.Fprintf(&b, "- 第 %d 步（%s）：%s；%s\n", s.StepNo, s.ToolID, st, s.Summary)
			if len(s.Artifacts) > 0 {
				fmt.Fprintf(&b, "  产物：%s\n", strings.Join(s.Artifacts, "、"))
			}
		}
		m, fr, promptTokens, _, err := call(taskID, "你是 007 超级 Agent 的前置，负责给用户写大白话汇报。", b.String(), nil, nil)
		// L-4：汇报的 token 也记水位（翻新阈值用），不然水位低估。
		if f.sess != nil && ps.SessionID != "" && promptTokens > 0 {
			if rerr := f.sess.RecordTokens(ps.SessionID, promptTokens); rerr != nil {
				log.Printf("front task=%s 最终汇报记水位失败: %v", taskID, rerr)
			}
		}
		if err == nil && fr != "length" && len(m.ToolCalls) == 0 && strings.TrimSpace(m.Content) != "" {
			return strings.TrimSpace(m.Content) + "\n"
		}
		log.Printf("front task=%s 最终汇报模型调用不可用，用模板兜底: err=%v fr=%s", taskID, err, fr)
	}
	return templateReport(ps)
}

// templateReport 模板汇报（最终汇报的兜底）：按步列结果，不编造。
func templateReport(ps *session.PlanState) string {
	var b strings.Builder
	b.WriteString("任务做完了，各步结果如下：\n\n")
	for _, s := range ps.Steps {
		st := "做完"
		if s.Status == session.StepSkipped {
			st = "跳过（可选步）"
		} else if s.Status == session.StepFailed {
			st = "没做完"
		}
		fmt.Fprintf(&b, "第 %d 步（%s）：%s。%s\n", s.StepNo, s.ToolID, st, s.Summary)
		if len(s.Artifacts) > 0 {
			fmt.Fprintf(&b, "产物：%s\n", strings.Join(s.Artifacts, "、"))
		}
	}
	return b.String()
}

// failPlanReply 计划失败：写诚实失败回信（大白话，不编造），计划标 FAILED。
func (f *Front) failPlanReply(artDir, taskID, goal, reason string, ps *session.PlanState) *FrontResult {
	var b strings.Builder
	fmt.Fprintf(&b, "这个任务没能做完，原因如下：\n\n%s\n", reason)
	if ps != nil {
		b.WriteString("\n各步情况：\n")
		for _, s := range ps.Steps {
			st := "待做"
			if s.Status == session.StepDone {
				st = "做完"
			} else if s.Status == session.StepSkipped {
				st = "跳过"
			} else if s.Status == session.StepFailed {
				st = "没做完"
			}
			fmt.Fprintf(&b, "第 %d 步（%s）：%s。%s\n", s.StepNo, s.ToolID, st, s.Summary)
		}
		ps.Status = session.PlanFailed
		ps.Phase = session.PhaseRunning
		if err := f.savePlanState(ps); err != nil {
			log.Printf("front task=%s 计划标 FAILED 存盘失败: %v", taskID, err)
		}
	}
	// 算账：任务结束时报本单花费 + 账户余额（余额查不到写“余额未知”）。
	fmt.Fprintf(&b, "\n%s\n", f.accountingLine(taskID))
	// M-6：失败回信落盘前先过出站脱敏（原因里可能带回验货报错
	// 或执行层产物里的密钥/原文）。
	body, _, _ := sanitize.ScrubOutbound(b.String())
	if err := writeFileAtomic(filepath.Join(artDir, "reply.md"), []byte(body), 0o600); err != nil {
		log.Printf("front task=%s 写失败回信失败: %v", taskID, err)
	}
	log.Printf("front task=%s 计划失败：%s", taskID, reason)
	return &FrontResult{Status: "failed", Route: "AGENT", ToolIDs: planToolIDs(ps)}
}
