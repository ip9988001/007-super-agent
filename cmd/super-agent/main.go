// Command super-agent 是 007 超级 Agent 的主程序：按顺序接线
// config → ledger → registry → memory → executor → watchdog → gateway。
// 启动失败（配置读不到、总账读不到、建库失败）明确报错退出，不带病跑。
//
// --shadow（或 SHADOW_MODE=true）为影子模式：网关收信、分诊照常走，
// 执行只记“影子会跑什么”进 execution_ledger（状态 shadow），不真跑命令。
// 监工（internal/advisor）已于 2026-11-26 彻底下线（用户令）：
// 二号三号从来没有监工，007 也不要监工；前置判回执 + 网关硬底线
// 就是全部验收，任何路径都不再调监工、不再为它花一分钱。
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"007-super-agent/internal/agent"
	"007-super-agent/internal/backup"
	"007-super-agent/internal/config"
	"007-super-agent/internal/dispatch"
	"007-super-agent/internal/executor"
	"007-super-agent/internal/forge"
	"007-super-agent/internal/front"
	"007-super-agent/internal/gateway"
	"007-super-agent/internal/ledger"
	"007-super-agent/internal/llm"
	"007-super-agent/internal/memory"
	"007-super-agent/internal/registry"
	"007-super-agent/internal/retrieval"
	"007-super-agent/internal/sanitize"
	"007-super-agent/internal/selfevolve"
	"007-super-agent/internal/session"
	"007-super-agent/internal/triage"
	"007-super-agent/internal/watchdog"
)

const Version = "v0.1.0"

// daemon 是接线后的整机：网关的 handler 就是它的方法。
type daemon struct {
	cfg       *config.Config
	dataDir   string // 已解析的绝对数据目录（cfg.ResolveDataDir() 的结果）
	led       *ledger.Ledger
	reg       *registry.Registry
	mem       *memory.Store
	ex        *executor.Executor
	wd        *watchdog.Watchdog
	ag        *agent.Agent         // 执行会话主循环；nil 表示未接线（无模型调用）
	fr        *front.Front         // 前置会话（判断/分诊/派单/回话）；nil 表示未接线
	ch        *dispatch.Channel    // 前置→执行的内部派单通道
	modelCall triage.ModelCallFunc // Tier-1 模型调用（llm 客户端）；nil 时 Tier-1 报错
	session   string
	shadow    bool
	// sess 是会话存盘入口（P2-a）：plan_state + 步历史落盘处，
	// 前置（派单占位）与执行（断点续跑）共用同一实例。
	sess *session.Store
}

func newSessionID() string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("sa-%s-%s", time.Now().UTC().Format("20060102T150405Z"), hex.EncodeToString(b[:]))
}

// handle 是网关 worker 串行调的唯一入口：分诊 → 定位 → 影子记账或真执行。
// 返回网关终态 Outcome（线 A：四终态收口）。处理完由框架写一行 L1 日志
// （task_id、route、tool_id、结果状态、耗时），不经模型选择——模型漏记也不丢。
// L1 写失败只记日志，不阻塞回执。
// try/finally 兜底必写回信：终态为 FAILED/REJECTED 且产物目录里没有
// reply.md 时，补写一封失败回信（不覆盖已有的真实回信）；执行仍在
// 后台继续（空终态）时不写，等执行自己交活。
func (d *daemon) handle(env gateway.Envelope) gateway.Outcome {
	start := time.Now()
	route, toolID, status, outcome := d.handleInner(env)
	defer func() {
		if outcome.State == gateway.OutcomeFailed || outcome.State == gateway.OutcomeRejected {
			d.ensureFailureReply(env.TaskID, outcome.Reason)
		}
	}()
	line := fmt.Sprintf("task %s route=%s tool=%s status=%s duration_ms=%d",
		env.TaskID, route, toolID, status, time.Since(start).Milliseconds())
	if err := d.mem.AppendL1(line); err != nil {
		log.Printf("memory l1 task=%s: %v", env.TaskID, err)
	}
	return outcome
}

// handleInner 做真正的分诊与执行，返回（route、tool_id、结果状态、网关终态）
// 供 handle 记 L1 与网关标终态。
// 双会话设计（第 1 项）：入口一律走前置会话（front.Run）——判断、分诊、
// 派单、回话都在前置；执行会话只接派单干活，不直接见用户原话。
// 前置派单后等回执，回执状态即本单状态。
func (d *daemon) handleInner(env gateway.Envelope) (route, toolID, status string, outcome gateway.Outcome) {
	if d.fr == nil {
		// 无模型调用（影子模式且无密钥）：前置跑不起来，只走 Tier-0
		// 前缀直出（0 token），Tier-1 不可用；TOOL 去向只做影子记账。
		dec, err := triage.Triage(d.reg, d.led, triage.Input{Text: env.Text, TaskID: env.TaskID, SessionID: d.session}, nil)
		if err != nil {
			log.Printf("task=%s triage (no model): %v", env.TaskID, err)
			return "ERROR", "", "triage_error", gateway.Outcome{State: gateway.OutcomeFailed, Reason: "分诊失败：" + err.Error()}
		}
		if dec.Route != triage.RouteTool {
			log.Printf("task=%s route=%s tier=%d (no tool to run)", env.TaskID, dec.Route, dec.Tier)
			return string(dec.Route), dec.ToolID, "no_tool", gateway.Outcome{State: gateway.OutcomeFailed, Reason: "无模型调用且非 TOOL 去向，无产物可交付"}
		}
		e, _, rerr := triage.ResolveTool(d.reg, dec.ToolID)
		if rerr != nil {
			log.Printf("task=%s resolve %s: %v", env.TaskID, dec.ToolID, rerr)
			return string(dec.Route), dec.ToolID, "resolve_error", gateway.Outcome{State: gateway.OutcomeFailed, Reason: "定位工具失败：" + rerr.Error()}
		}
		if d.shadow {
			d.shadowRecord(env, e)
			return string(dec.Route), dec.ToolID, "shadow", gateway.Outcome{State: gateway.OutcomeDone, Reason: "影子模式：已记账，未真跑"}
		}
		return string(dec.Route), dec.ToolID, "no_tool", gateway.Outcome{State: gateway.OutcomeFailed, Reason: "无模型调用，无法执行"}
	}
	// P2-a：会话号解析——信封没给 session_id 时默认等于 task_id；
	// 显式给的 session_id 必须合规，否则拒绝该 wake（不静默改写）。
	// task_id 本身可能含非法字符，默认映射走 DefaultSessionID
	// （非法字符换 -、超长截断 + 内容哈希，同一任务号每次结果相同，
	// 崩溃重放仍能找回同一会话）。
	sessID := env.SessionID
	if sessID == "" {
		sessID = session.DefaultSessionID(env.TaskID)
	} else if err := session.ValidateSessionID(sessID); err != nil {
		log.Printf("task=%s session_id 非法，拒绝: %v", env.TaskID, err)
		return "ERROR", "", "bad_session", gateway.Outcome{State: gateway.OutcomeRejected, Reason: "session_id 非法：" + err.Error()}
	}
	// index_metrics 记账打上当前任务号（前置与执行共用一个 Registry，
	// 网关串行 + 派单 worker 单线程，同一时刻只有一个任务在跑）。
	d.reg.SetSession(d.session, env.TaskID)
	res, err := d.fr.RunWithSession(context.Background(), env.TaskID, env.Text, sessID)
	if err != nil {
		log.Printf("task=%s front run: %v", env.TaskID, err)
		if errors.Is(err, front.ErrTokenOverflow) {
			// 前置校验拒绝：派单核心指令超限，不是执行失败。
			return "ERROR", "", "front_overflow", gateway.Outcome{State: gateway.OutcomeRejected, Reason: err.Error()}
		}
		return "ERROR", "", "front_error", gateway.Outcome{State: gateway.OutcomeFailed, Reason: "前置失败：" + err.Error()}
	}
	toolID = strings.Join(res.ToolIDs, ",")
	return res.Route, toolID, res.Status, d.outcomeFromResult(env.TaskID, res)
}

// outcomeFromResult 把前置结果映射为网关终态（线 A）：
//   - done（直接回话或回执 done）：须验收产物（reply.md 存在且非空）才标 DONE，否则 FAILED；
//   - stalled 且无回执（等回执超时，执行仍在后台继续）：空终态，不标终态，
//     行保持 RUNNING，由看门狗 deadline 兜底；
//   - stalled 且有回执 / 其余一切（failed、max_iterations、agent_error
//     等）：FAILED；
//   - shadow：影子模式的产物就是账本记录，标 DONE。
func (d *daemon) outcomeFromResult(taskID string, res *front.FrontResult) gateway.Outcome {
	if res == nil {
		return gateway.Outcome{State: gateway.OutcomeFailed, Reason: "前置无结果"}
	}
	switch res.Status {
	case "done":
		if d.artifactExists(taskID) {
			return gateway.Outcome{State: gateway.OutcomeDone, Reason: "完成且有产物"}
		}
		return gateway.Outcome{State: gateway.OutcomeFailed, Reason: "回执称 done 但产物目录里没有 reply.md"}
	case "stalled":
		if res.Receipt == nil {
			// 等回执超时：执行仍在后台继续，不标终态。
			return gateway.Outcome{Reason: "等回执超时，执行仍在后台继续"}
		}
		return gateway.Outcome{State: gateway.OutcomeFailed, Reason: "执行停滞（回执 stalled）"}
	case "shadow", "shadow_agent":
		return gateway.Outcome{State: gateway.OutcomeDone, Reason: "影子模式：已记账，未真跑"}
	default:
		return gateway.Outcome{State: gateway.OutcomeFailed, Reason: "执行状态：" + res.Status}
	}
}

// writeFileAtomic 写文件：先写同目录临时文件再 os.Rename 原子替换，
// 避免看门狗或验收逻辑读到写了一半的 reply.md（P0 修复）。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".reply-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// artifactExistsStep 是步级版的回信存在检查（H-5）：计划步查
// step-<n>-reply.md，其余查顶层 reply.md；存在且去空白后非空才算有。
func (d *daemon) artifactExistsStep(taskID string, stepNo int, planStep bool) bool {
	name := "reply.md"
	if planStep && stepNo > 0 {
		name = fmt.Sprintf("step-%d-reply.md", stepNo)
	}
	if d.dataDir == "" {
		return false
	}
	b, err := os.ReadFile(filepath.Join(d.dataDir, "artifacts", taskID, name))
	return err == nil && len(strings.TrimSpace(string(b))) > 0
}

// artifactExists 检查某任务的产物是否已落袋（reply.md 存在且去空白后非空）。
// 只含空白字符的文件不算产物（P0 修复：TrimSpace 验收）。
// dataDir 为空（测试整机未设）时一律判为无产物，不碰相对路径。
func (d *daemon) artifactExists(taskID string) bool {
	if d.dataDir == "" {
		return false
	}
	b, err := os.ReadFile(filepath.Join(d.dataDir, "artifacts", taskID, "reply.md"))
	return err == nil && len(strings.TrimSpace(string(b))) > 0
}

// ensureFailureReply 兜底写失败回信：reply.md 已存在（真实回信）时不动；
// 不存在时写一封人话失败回信。写失败只记日志。dataDir 为空时不写
// （测试整机未设数据目录，不往仓库里写文件）。
// 用 O_CREATE|O_EXCL 排他创建（P0 修复）：先验与创建之间的竞态里
// 若真实回信刚落盘，创建失败即放弃，绝不覆盖。
// 已有文件但只是空白字符时（不算真实回信），删掉重建一次。
func (d *daemon) ensureFailureReply(taskID, reason string) {
	if d.dataDir == "" || d.artifactExists(taskID) {
		return
	}
	artDir := filepath.Join(d.dataDir, "artifacts", taskID)
	if err := os.MkdirAll(artDir, 0o700); err != nil {
		log.Printf("task=%s 兜底回信建目录失败: %v", taskID, err)
		return
	}
	text := fmt.Sprintf("任务未能完成。\n\n原因：%s\n时间：%s\n", reason, time.Now().UTC().Format(time.RFC3339))
	// 出站硬扫描：兜底回信也走同一道门（reason 可能带回错误里的用户原文）。
	text, n, blocked := sanitize.ScrubOutbound(text)
	if n > 0 || blocked {
		log.Printf("task=%s 出站脱敏：掩码 %d 处，整封拦截=%v", taskID, n, blocked)
	}
	p := filepath.Join(artDir, "reply.md")
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			if _, werr := f.Write([]byte(text)); werr != nil {
				f.Close()
				os.Remove(p)
				log.Printf("task=%s 兜底回信写失败: %v", taskID, werr)
			} else if cerr := f.Close(); cerr != nil {
				os.Remove(p)
				log.Printf("task=%s 兜底回信写失败: %v", taskID, cerr)
			}
			return
		}
		if !os.IsExist(err) {
			log.Printf("task=%s 兜底回信写失败: %v", taskID, err)
			return
		}
		// 文件在先验之后出现了：再验一次——是真实回信就放弃；
		// 仍只是空白字符就删掉重建一次。
		if d.artifactExists(taskID) {
			return
		}
		os.Remove(p)
	}
}

// shadowRecord 只记“影子会跑什么”：写一行 execution_ledger，状态标
// shadow。不跑任何命令，不调监工（监工已下线）。
func (d *daemon) shadowRecord(env gateway.Envelope, e *registry.Entry) {
	verdict, note := string(executor.VerdictNote), "影子模式：只记，不执行"
	_, err := d.led.RecordExecution(ledger.ExecutionRecord{
		SessionID: d.session, TaskID: env.TaskID, ToolID: e.ToolID,
		Command: "shadow: " + e.EntryPoint, ExitCode: 0, Status: "shadow",
		AdvisorVerdict: verdict, AdvisorNote: note,
	})
	if err != nil {
		log.Printf("shadow record task=%s: %v", env.TaskID, err)
		return
	}
	log.Printf("task=%s SHADOW would run %s (tool=%s verdict=%s)", env.TaskID, e.EntryPoint, e.ToolID, verdict)
}

// runTool 是非影子模式的真执行：按总账的 entry_point 原样跑一条命令。
// 参数化调用走 llm.RunToolWithArgs（entry_point + 模型按 L3 拼出的参数，
// 参数先过白名单）；模型拼参数的第二步调用等后续接线再补。
// 返回执行结果状态（供 handle 记 L1）。
func (d *daemon) runTool(env gateway.Envelope, e *registry.Entry) string {
	argv := strings.Fields(e.EntryPoint)
	if len(argv) == 0 {
		log.Printf("task=%s tool %s entry_point 为空，不执行", env.TaskID, e.ToolID)
		return "entry_empty"
	}
	res, h, err := d.ex.Run(executor.RunRequest{
		TaskID: env.TaskID, SessionID: d.session, ToolID: e.ToolID,
		Argv: argv, Timeout: 30 * time.Second, Text: env.Text,
	})
	if err != nil {
		log.Printf("task=%s run %s: %v", env.TaskID, e.ToolID, err)
		return "run_error"
	}
	if h != nil {
		log.Printf("task=%s tool=%s 后台执行，句柄已返回（30 秒派单即返回）", env.TaskID, e.ToolID)
		return "background"
	}
	log.Printf("task=%s tool=%s exit=%d status=%s", env.TaskID, e.ToolID, res.ExitCode, res.Status)
	if res.ExitCode != 0 && (strings.Contains(res.ErrHead, "Usage") || strings.Contains(res.ErrHead, "用法")) {
		log.Printf("task=%s WARN tool=%s 快捷路径非零退出且 stderr 含用法提示，疑似误配 fast_path（该工具可能需要参数），请检查注册表", env.TaskID, e.ToolID)
	}
	// 快捷路径没有 agent 写 reply.md：成功时把工具完整输出落盘为 reply.md，
	// 回执才能带上原始结论，否则只剩一句“执行结束”。
	// 原子落盘（P0 修复）：防看门狗读到半截文件。
	if res.ExitCode == 0 && res.StdoutFull != "" {
		artDir := filepath.Join(d.dataDir, "artifacts", env.TaskID)
		if err := os.MkdirAll(artDir, 0o700); err == nil {
			out := res.StdoutFull
			if rs := []rune(out); len(rs) > 20000 {
				out = string(rs[:20000]) + "\n……（输出过长，已截断）"
			}
			// 出站硬扫描（线 B 修复轮第 3 棒）：快捷路径把工具完整输出
			// 落盘为 reply.md，工具输出可能含密钥，先过 ScrubOutbound。
			clean, n, blocked := sanitize.ScrubOutbound(out)
			if n > 0 || blocked {
				log.Printf("task=%s 出站脱敏：掩码 %d 处，整封拦截=%v", env.TaskID, n, blocked)
			}
			_ = writeFileAtomic(filepath.Join(artDir, "reply.md"), []byte(clean), 0o600)
		}
	}
	return res.Status
}

// executeDispatch 是派单 worker 的执行回调（第 1 项）：TOOL 走 runTool（仅 fast_path
// 工具）或 runAgent（其余），AGENT/META 走 runAgent（带派单）；影子模式只记账不真跑。
// 返回短回执：状态、结论（reply.md 前 500 字）、产物指针、本单费用。
func (d *daemon) executeDispatch(ctx context.Context, disp *dispatch.Dispatch) (*dispatch.Receipt, error) {
	env := gateway.Envelope{TaskID: disp.TaskID, Text: disp.UserText, Source: "dispatch", TS: disp.CreatedAt, SessionID: disp.SessionID}
	var status string
	if d.shadow {
		if disp.Route == "TOOL" && len(disp.ToolIDs) > 0 {
			e, _, err := triage.ResolveTool(d.reg, disp.ToolIDs[0])
			if err != nil {
				return nil, err
			}
			d.shadowRecord(env, e)
			status = "shadow"
		} else {
			status = d.runAgent(env, disp)
		}
	} else if disp.Route == "TOOL" && len(disp.ToolIDs) == 1 {
		e, _, err := triage.ResolveTool(d.reg, disp.ToolIDs[0])
		if err != nil {
			return nil, err
		}
		if fastPathOK(disp, e) {
			status = d.runTool(env, e)
		} else {
			status = d.runAgent(env, disp)
		}
	} else {
		status = d.runAgent(env, disp)
	}
	r := d.buildReceipt(disp, status)
	// 规划 v2：步完成时发 plan_step_callback 事件（异步回调驱动的
	// durable 记录）：session_id、step_no、回执摘要、事实清单摘要。
	// 串行调度器（前置规划循环）凭它叫醒前置、推进下一步。
	if d.led != nil && disp.SessionID != "" && disp.StepNo > 0 {
		detail := fmt.Sprintf("步 %d 完成回调：回执 %s（%s）", disp.StepNo, status, r.Conclusion)
		if r.FactList != nil {
			detail += "；事实：" + r.FactList.Summary()
		}
		if _, err := d.led.RecordDispatchEvent(ledger.DispatchEvent{
			SessionID: disp.SessionID, TaskID: disp.TaskID, DispatchID: disp.ID,
			Kind: ledger.DispatchEventPlanStepCallback, Detail: detail,
		}); err != nil {
			log.Printf("task=%s 记 plan_step_callback 失败: %v", disp.TaskID, err)
		}
	}
	// P2-a：步状态落盘——执行完把本步标 DONE/FAILED，产物与结论写进计划。
	// 影子模式（没真跑）不写；计划 Status 不自动流转（由前置/规划 v2 判终态）。
	if d.sess != nil && disp.SessionID != "" && disp.StepNo > 0 && !strings.HasPrefix(status, "shadow") {
		st := session.StepFailed
		if status == "done" {
			st = session.StepDone
		}
		if err := d.sess.CompleteStep(disp.SessionID, disp.StepNo, st, r.Conclusion, r.Artifacts, status); err != nil {
			log.Printf("task=%s 会话步状态更新失败: %v", disp.TaskID, err)
		}
	}
	return r, nil
}

// fastPathOK：TOOL 派单是否走 runTool 快捷路径。
// 只有同时满足单工具、注册表显式 fast_path: true，才放行；其余一律走 runAgent
// （安全默认），多工具也强制走 runAgent（防静默丢弃 ToolIDs[1:]）。
func fastPathOK(disp *dispatch.Dispatch, e *registry.Entry) bool {
	return disp != nil && disp.Route == "TOOL" && len(disp.ToolIDs) == 1 && e != nil && e.FastPath
}

// isPerCallOutputDump 是每次调用的输出底稿文件名吗（step-<N>.txt）。
// 底稿是执行层每次调工具的全文存底（供复查），不是交付物；
// 回执产物列表里不列它（B2，CF-015 交接被 42 个文件名淹没的教训）。
func isPerCallOutputDump(name string) bool {
	return perCallDumpRe.MatchString(name)
}

var perCallDumpRe = regexp.MustCompile(`^step-\d+\.txt$`)

// absPathRe 匹配绝对路径（/ 开头、多段）。Go 正则不支持后顾，
// URL 里的一部分用 FindAllStringIndex 再看前一字符排除。
var absPathRe = regexp.MustCompile(`/[A-Za-z0-9_.\-]+(?:/[A-Za-z0-9_.\-]+)*`)

// parseClaimedReceiptFiles 从回执正文里取“成果在哪里”那一栏列出的
// 绝对路径（去重）。回执是四栏格式（干了什么/结果是什么/成果在哪里/
// 用了哪些工具）；找不到“成果在哪里”这一栏，或栏里没列路径，
// 返回空（claimed=0，置信度里工具那点占 100%）。
func parseClaimedReceiptFiles(reply string) []string {
	lines := strings.Split(reply, "\n")
	start := -1
	for i, ln := range lines {
		if strings.Contains(ln, "成果在哪里") {
			start = i
			break
		}
	}
	if start < 0 {
		// 退一步：标题行（# 开头）含“成果”或“产出”也认。
		for i, ln := range lines {
			t := strings.TrimSpace(ln)
			if strings.HasPrefix(t, "#") && (strings.Contains(t, "成果") || strings.Contains(t, "产出")) {
				start = i
				break
			}
		}
	}
	if start < 0 {
		return nil
	}
	// 栏尾：下一栏标题（四栏其余三栏或 # 标题行）或文件尾。
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, "#") ||
			strings.Contains(t, "干了什么") ||
			strings.Contains(t, "结果是什么") ||
			strings.Contains(t, "用了哪些工具") {
			end = i
			break
		}
	}
	section := strings.Join(lines[start:end], "\n")
	locs := absPathRe.FindAllStringIndex(section, -1)
	seen := map[string]bool{}
	var out []string
	for _, loc := range locs {
		if loc[0] > 0 {
			// 前一字符是字母/数字/连字符/下划线/点/冒号/斜杠时，
			// 是 URL 或域名的一部分（如 https://…/path），不是文件路径。
			prev := section[loc[0]-1]
			if prev == ':' || prev == '/' ||
				(prev >= '0' && prev <= '9') ||
				(prev >= 'a' && prev <= 'z') ||
				(prev >= 'A' && prev <= 'Z') ||
				prev == '-' || prev == '_' || prev == '.' {
				continue
			}
		}
		p := strings.TrimRight(section[loc[0]:loc[1]], "./")
		if p == "" || p == "/" {
			continue
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// fileNonEmpty 判文件去空白后非空（与 verifyReceiptFiles 的“空”口径
// 一致：只读前 1MB，空白字符以外有一字节即非空）。读失败按空算。
func fileNonEmpty(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 1024*1024)
	n, _ := io.ReadFull(f, buf)
	for _, c := range buf[:n] {
		if c != ' ' && c != '\t' && c != '\r' && c != '\n' {
			return true
		}
	}
	return false
}

// verifyReceiptFiles 逐个核回执列出的文件：三关——在盘上存在、
// 去空白后非空、新建/修改时间在本步接单时间之后（2 秒容差，
// 与 artifactFacts 的 Fresh 口径一致）。返回核对有的个数与缺失路径。
// 只核“文件是不是真的在”，不核“文件对不对”（用户 2026-11-26 定）。
func verifyReceiptFiles(paths []string, start time.Time) (verified int, missing []string) {
	for _, p := range paths {
		ok := false
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			if info.ModTime().After(start.Add(-2 * time.Second)) {
				ok = fileNonEmpty(p)
			}
		}
		if ok {
			verified++
		} else {
			missing = append(missing, p)
		}
	}
	return verified, missing
}

// classifyMissingFile 判一个“缺失”文件的缺失原因（Summary 第 4 行
// 聚合用，返回 dispatch.MissingReason* 常量）：
// 不存在 → 不存在；存在但空（去空白后无内容，口径与 verifyReceiptFiles
// 的“空”一致）→ 是空的；存在、非空、mtime 在本轮派单之前：
// mtime 在任务开始之后 → 这单里早一步做的，否则 → 以前的旧文件。
// 只对已判缺失的文件调（非空的缺失文件一定是 mtime 过早，
// 见 verifyReceiptFiles 三关）；任务开始时间取不到（零值）时
// 按 fail-closed 判旧文件，绝不把来历不明的文件说成“这单里早一步做的”。
func classifyMissingFile(path string, taskStart time.Time) int {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return dispatch.MissingReasonNonexistent
	}
	if !fileNonEmpty(path) {
		return dispatch.MissingReasonEmpty
	}
	if !taskStart.IsZero() && info.ModTime().After(taskStart) {
		return dispatch.MissingReasonEarlyTask
	}
	return dispatch.MissingReasonOld
}

// taskStartTime 取任务开始时间（任务首次派单的时间，UTC）：
// 取 data/dispatch 里该任务最早一份派单文件的 CreatedAt。
// 不用 plan_runs.created_ts——2026-11-26 实测：plan_runs 只有
// 2026-11-18 之前的 11 行测试数据，RecordPlanRun 在现版代码里
// 已无调用方，真任务查不到；派单文件是派单事件本身的落盘，
// 最早一份的 CreatedAt 就是任务首次派单的时间（Tavily 任务实测
// 2026-11-26T07:57:32Z，即北京时间 15:57:32）。取不到返回零值。
func (d *daemon) taskStartTime(taskID string) time.Time {
	return d.earliestDispatchTime(taskID)
}

// earliestDispatchTime 扫 data/dispatch 目录，取该任务最早一份
// 派单文件（<taskID>-<纳秒时间戳>-<序号>.json，跳过 .receipt.json）
// 的 CreatedAt；扫不到返回零值。文件名只当预筛，以文件里
// task_id 字段为准（防 taskID 是另一任务前缀的同名误撞）。
func (d *daemon) earliestDispatchTime(taskID string) time.Time {
	dir := filepath.Join(d.dataDir, "dispatch")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return time.Time{}
	}
	prefix := taskID + "-"
	var earliest time.Time
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, prefix) ||
			!strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".receipt.json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var dd struct {
			TaskID    string `json:"task_id"`
			CreatedAt string `json:"created_at"`
		}
		if err := json.Unmarshal(b, &dd); err != nil || dd.TaskID != taskID {
			continue
		}
		if ts, err := time.Parse(time.RFC3339, dd.CreatedAt); err == nil {
			if earliest.IsZero() || ts.Before(earliest) {
				earliest = ts
			}
		}
	}
	return earliest
}

// formatToolCallBreakdown 把按工具计的成功调用数拼成人话，
// 如 cap-ops-python×41、test-echo×2（按次数降序、同次数按名字）。
func formatToolCallBreakdown(counts map[string]int) string {
	if len(counts) == 0 {
		return ""
	}
	type kv struct {
		tool string
		n    int
	}
	list := make([]kv, 0, len(counts))
	for tool, n := range counts {
		list = append(list, kv{tool, n})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].n != list[j].n {
			return list[i].n > list[j].n
		}
		return list[i].tool < list[j].tool
	})
	parts := make([]string, 0, len(list))
	for _, e := range list {
		parts = append(parts, fmt.Sprintf("%s×%d", e.tool, e.n))
	}
	return strings.Join(parts, "、")
}

// buildReceipt 按执行结果拼短回执：结论取 reply.md 前 2000 字（没有就
// 按状态写一句话），产物取 artifacts/<taskID>/ 下的文件（每次调用的
// 输出底稿 step-<N>.txt 不列——底稿留盘备查，只是不进回执，免得
// 42 个文件名淹没交接，CF-015 教训），费用按账本
// 查本单，步数按 execution_ledger 行数计。
func (d *daemon) buildReceipt(disp *dispatch.Dispatch, status string) *dispatch.Receipt {
	r := &dispatch.Receipt{
		DispatchID: disp.ID, TaskID: disp.TaskID, Status: status,
		ShardNo:   disp.ShardNo,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		// 规划 v2：事实清单随回执附上（runAgent 里 verifyDelivery 算好的，
		// 原样抄，不重新算）；前置凭它判回执。
		FactList: disp.FactList,
	}
	artDir := filepath.Join(d.dataDir, "artifacts", disp.TaskID)
	// 分片任务未做完时，回执带续跑指针（本片产物目录，断点已落盘），
	// 下一片从这里接续（F-10 刹车交接）。
	if disp.ShardNo > 0 && status != "done" {
		r.ResumePointer = artDir
	}
	// H-5：结论读步级回信文件——计划步读 step-<n>-reply.md（执行层
	// 的草稿只写在那，不写顶层），其余读顶层 reply.md。
	conclusionName := "reply.md"
	if disp.PlanStep && disp.StepNo > 0 {
		conclusionName = fmt.Sprintf("step-%d-reply.md", disp.StepNo)
	}
	if b, err := os.ReadFile(filepath.Join(artDir, conclusionName)); err == nil {
		text := strings.TrimSpace(string(b))
		if rs := []rune(text); len(rs) > 2000 {
			text = string(rs[:2000]) + "……"
		}
		// 出站纵深防御：结论取自回信文件（正常已在落盘前脱敏），回传
		// 网关前再过一次 ScrubOutbound，防任何绕过落盘点的旧产物或
		// 外部写入把密钥带进回执。
		if clean, n, blocked := sanitize.ScrubOutbound(text); n > 0 || blocked {
			log.Printf("task=%s 回执结论出站脱敏：掩码 %d 处，整封拦截=%v", disp.TaskID, n, blocked)
			text = clean
		}
		r.Conclusion = text
	}
	if r.Conclusion == "" {
		r.Conclusion = "执行结束，状态：" + status
	}
	if entries, err := os.ReadDir(artDir); err == nil {
		for _, e := range entries {
			// B2：每次调用的输出底稿（step-<N>.txt）不进回执产物列表——
			// 底稿文件本身留盘备查，只是回执里不列；step-<N>-reply.md、
			// reply.md、声明产物和其他真文件照常列。
			if !e.IsDir() && !isPerCallOutputDump(e.Name()) {
				r.Artifacts = append(r.Artifacts, filepath.Join(artDir, e.Name()))
			}
		}
	}
	if calls, cost, err := d.led.TaskExpense(disp.TaskID); err != nil {
		log.Printf("task=%s 查本单费用失败: %v", disp.TaskID, err)
	} else {
		r.CostCNYMilli = cost
		_ = calls
	}
	if execs, err := d.led.ListExecutions(disp.TaskID, 1000); err != nil {
		log.Printf("task=%s 查本单步数失败: %v", disp.TaskID, err)
	} else {
		r.Steps = len(execs)
	}
	return r
}

// memorySummary 给规划器用的相关记忆摘要：Recall 前 3 条（每条 ≤240 字），
// 无关或查不到时返回空串，规划器不靠它编造。失败只记日志，不阻塞任务。
//
// 线 B 第一阶段：
//   - 取前 40 名候选记召回账本（ledger.RecordRecall，节点 3），交付仍
//     只取前 3 条，交付语义与截断口径不变；
//   - 账本写入前 RecordRecall 内脱敏一次；记账失败只记日志，不阻塞任务。
//
// 线 B 第二阶段第 3 棒：
//   - 接了简介主库且 TwoStageRecallEnabled 时走新两阶段召回：
//     第一段 40 篇候选记账，交付 0～5 张卡片（0 张即拒推，返回空串）；
//   - 旧 Recall 路径保留（开关关闭、未接主库或新路径出错时回落）；
//   - 脱敏口径（用户 2026-11-24 09:12 纠正）：交付给模型前绝不掩码
//     （旧节点 4 已取消），模型照常看、照常用密钥。
func (d *daemon) memorySummary(text string) string {
	if d.mem == nil {
		return ""
	}
	start := time.Now()
	if memory.TwoStageRecallEnabled && d.mem.HasSummaryStore() {
		res, err := d.mem.RecallTwoStage(text)
		if err == nil {
			if len(res.Cards) == 0 {
				return ""
			}
			var b strings.Builder
			b.WriteString("相关记忆摘要（供规划参考，不在其中不要编造）：\n")
			delivered := make([]string, 0, len(res.Cards))
			candStrs := make([]string, 0, len(res.Pool))
			for _, c := range res.Pool {
				candStrs = append(candStrs, fmt.Sprintf("%s=%d", c.SourcePath, c.PoolRank))
			}
			tokensEst := 0
			for _, c := range res.Cards {
				fmt.Fprintf(&b, "- %s\n", c.Display())
				delivered = append(delivered, c.SourcePath)
				tokensEst += c.Tokens()
			}
			if d.led != nil {
				if _, rerr := d.led.RecordRecall(ledger.RecallRecord{
					SessionID: d.session, TaskID: "",
					Query: text, Candidates: candStrs,
					Delivered: delivered, TokensEst: int64(tokensEst),
					LatencyMs: time.Since(start).Milliseconds(),
				}); rerr != nil {
					log.Printf("memorySummary recall_log 记账失败（不阻塞）: %v", rerr)
				}
			}
			return b.String()
		}
		log.Printf("memorySummary 两阶段召回失败，回落旧 Recall: %v", err)
	}
	cands, err := d.mem.RecallCandidates(text, 40)
	if err != nil || len(cands) == 0 {
		return ""
	}
	top := cands
	if len(top) > memory.TopK {
		top = top[:memory.TopK]
	}
	var b strings.Builder
	b.WriteString("相关记忆摘要（供规划参考，不在其中不要编造）：\n")
	delivered := make([]string, 0, len(top))
	candStrs := make([]string, 0, len(cands))
	for _, r := range cands {
		candStrs = append(candStrs, fmt.Sprintf("%s=%d", r.Source, r.Score))
	}
	tokensEst := 0
	for _, r := range top {
		fmt.Fprintf(&b, "- 【%s】%s\n", r.Source, r.Text)
		delivered = append(delivered, r.Source)
		tokensEst += registry.EstimateTokens(r.Source + r.Text)
	}
	if d.led != nil {
		if _, rerr := d.led.RecordRecall(ledger.RecallRecord{
			SessionID: d.session, TaskID: "",
			Query: text, Candidates: candStrs,
			Delivered: delivered, TokensEst: int64(tokensEst),
			LatencyMs: time.Since(start).Milliseconds(),
		}); rerr != nil {
			log.Printf("memorySummary recall_log 记账失败（不阻塞）: %v", rerr)
		}
	}
	return b.String()
}

// runAgent 是 AGENT/META 去向的真执行：主循环逐轮调工具、看到结果再决定下一步，
// 执行完直接走网关硬验货（verifyDelivery），不再交活前审（监工已下线）。
// 影子模式只做一次决定、记一行 shadow 回执，不执行。
// 返回执行结果状态（供 handle 记 L1）。
// disp 为空时是旧调用口径（直接跑用户原话，现查记忆）；disp 非空时是
// 派单口径：任务文本按派单拼（用户原话 + 目标 + 验收 + 工具 + 骨架 +
// 约束 + 指针），记忆用派单带的碎片（不重查），步数上限按派单预估×2
// （10–50 步，900 秒 / 200 步最后外墙不动）。
func (d *daemon) runAgent(env gateway.Envelope, disp *dispatch.Dispatch) string {
	if d.ag == nil {
		log.Printf("task=%s AGENT but agent not wired (no llm client)", env.TaskID)
		return "agent_unavailable"
	}
	taskText := env.Text
	memSummary := d.memorySummary(env.Text)
	runStart := time.Now() // 验货 mtime 的起点（产物须在此之后生成/改动，2 秒容差）
	if disp != nil {
		taskText = dispatchTaskText(disp, d.reg)
		memSummary = dispatchMemorySummary(disp)
		// 派单步数上限（预估×2，夹在 10–50 步之间）；分片号（第 15 项）；
		// 执行是串行的，跑完恢复 0，不影响下一单。
		d.ag.StepCap = clampStepCap(disp.EstSteps)
		d.ag.ShardNo = disp.ShardNo
		d.ag.TaskType = disp.TaskType                 // P0-2/P0-3：空值按 execute
		d.ag.ToolIDs = disp.ToolIDs                   // P1：派单建议的工具编号，声明给模型
		d.ag.MustReadMemories = disp.MustReadMemories // 2026-11-26 记忆召回整改：必读记忆编号，执行开工时代码自动读全文
		defer func() {
			d.ag.StepCap = 0
			d.ag.ShardNo = 0
			d.ag.TaskType = ""
			d.ag.ToolIDs = nil
			d.ag.MustReadMemories = nil
		}()
	}
	if d.shadow {
		if _, rerr := d.led.RecordExecution(ledger.ExecutionRecord{
			SessionID: d.session, TaskID: env.TaskID, ToolID: "agent",
			Command:  "shadow agent loop (no execution)",
			ExitCode: 0, Status: "shadow", AdvisorVerdict: string(executor.VerdictNote),
			AdvisorNote: "影子模式：只做决定、不执行",
		}); rerr != nil {
			log.Printf("shadow agent record task=%s: %v", env.TaskID, rerr)
		}
		return "shadow_agent"
	}
	// P2-a：会话断点续跑——本步的对话历史按 (session, step) 存盘；
	// 先把本会话已完成步的产物接续进记忆摘要，让模型接着上一步干
	// （同一会话内第二任务能引用第一任务的产物，靠的就是这行接续）。
	sessID, stepNo := "", 0
	if disp != nil {
		sessID, stepNo = disp.SessionID, disp.StepNo
	}
	if sessID != "" {
		if err := session.ValidateSessionID(sessID); err != nil {
			log.Printf("task=%s session_id 非法，关闭本单会话存盘: %v", env.TaskID, err)
			sessID, stepNo = "", 0
		} else {
			if stepNo <= 0 {
				stepNo = 1
			}
			if d.sess != nil {
				if carry, cerr := d.sess.PlanSummary(sessID); cerr == nil && carry != "" {
					memSummary += "\n" + carry
				}
			}
		}
	}
	d.ag.RunSessionID = sessID
	d.ag.RunStepNo = stepNo
	// H-5：计划步的草稿写 step-<n>-reply.md（见 Agent.replyFileName）；
	// 单步派单仍写顶层 reply.md。
	d.ag.RunPlanStep = disp != nil && disp.PlanStep
	// H-1：重干原因透传给执行层——续跑首轮靠它把“上次错在哪”追加进
	// 对话历史；不是重干时为空。
	if disp != nil {
		d.ag.RunRedoReason = disp.LastRedoReason
	}
	defer func() { d.ag.RunSessionID = ""; d.ag.RunStepNo = 0; d.ag.RunPlanStep = false; d.ag.RunRedoReason = "" }()
	// H-2：记下本轮执行开始前的账本最大行号——本次 runAgent 窗口的
	// 起点；computeFactList 只数 id 大于它的行，不把之前步骤的
	// 账本行算进当前步。账本未接线时为 0（旧口径）。
	var execIDBefore int64
	if d.led != nil {
		if maxID, err := d.led.MaxExecutionID(); err == nil {
			execIDBefore = maxID
		} else {
			log.Printf("task=%s 取账本最大行号失败，本步按全单口径统计: %v", env.TaskID, err)
		}
	}
	// 监工已下线：执行完直接走网关硬验货（verifyDelivery），不再交活前审。
	// 完成与否由前置判回执 + 网关硬底线决定，与二号三号同一套路。
	res, err := d.ag.Run(context.Background(), env.TaskID, taskText, memSummary, "")
	if err != nil {
		log.Printf("task=%s agent run: %v", env.TaskID, err)
		return "agent_error"
	}
	log.Printf("task=%s agent status=%s steps=%d", env.TaskID, res.Status, res.CompletedSteps)
	return d.verifyDelivery(disp, env.TaskID, res, runStart, execIDBefore)
}

// artifactRelatedRedo 判重干原因是否关于产物（H-2，启发式）：
// 产物不存在/缺失/过期/为空/内容不对/不符、mtime 过早、不是本单新生成、
// 没产出——这类重干必须逼执行层真正重写产物，产物新鲜度基线强制用
// 本轮开始时间。原因里只提产物但不说它坏了（比如“产物正常，但结论
// 不对”），不在此列，仍按续跑口径回溯。
func artifactRelatedRedo(reason string) bool {
	for _, kw := range []string{
		"不存在", "缺失", "过期", "为空", "内容不对", "内容错误", "内容错",
		"不符", "mtime", "不是本单新生成", "不是新生成", "没产出", "未产出",
		"无产物", "写错", "旧的", "旧文件", "上一步的",
	} {
		if strings.Contains(reason, kw) {
			return true
		}
	}
	return false
}

// factBaseline 取产物新鲜度与置信度时间关的基线时间：
//   - 重干原因明确关于产物 → 强制用本轮 start，逼执行层真正重写产物；
//   - 断点续跑且首轮时间已记 → 回溯到 FirstStartTime，首轮产物按首轮
//     时间判定新鲜，免得硬底线③误杀“首轮已写好、二轮未重写”的合法续跑；
//   - 其余一律用本步开始时间 start（接单时间）——用户 2026-11-26 定：
//     成果文件的修改时间必须在接单时间之后。以前计划步用 plan
//     created_ts，比接单早约 3 分钟，上一步的文件也能混进来，太松。
func (d *daemon) factBaseline(disp *dispatch.Dispatch, start time.Time, resumed bool) time.Time {
	if disp != nil && artifactRelatedRedo(disp.LastRedoReason) {
		return start
	}
	if resumed && disp != nil && !disp.FirstStartTime.IsZero() {
		return disp.FirstStartTime
	}
	return start
}

// computeFactList 算一步的免费事实清单（规划 v2）：代码查，不花模型调用。
// 只读事实，不判通过；硬底线（HardFloorRed）只当一条记录供前置看，红了
// 也不由代码否决（2026-11-26 用户定：一票否决已去掉，事实清单只当参考）。
// 本函数每派单只调一次（verifyDelivery 入口），verify_cmd 只跑一次，
// 产物只查一次；结果挂在 disp.FactList 上，buildReceipt 原样抄进回执。
// execIDBefore 是本次 runAgent 开始前的 execution_ledger 最大 id
// （H-2）：只统计 id 大于它的行（本次执行窗口内的调用），不把之前
// 步骤的账本行算进当前步；传 0 表示不设窗口（旧口径，全单统计）。
// gatewaySubstantiveCall 判一次成功调用算不算实质执行（网关账本口径）：
// 名字只读的工具（查记忆/读文件/展开工具单/网关自己的验货）不算；
// cap-ops-python 虽然名字上算只读（写没写文件名字证明不了），但它
// 是通用执行工具（跑代码、写文件、调命令都靠它），一次成功调用就是
// 一次真实执行，算实质——否则靠 python 干活的步在没声明产物/校验
// 命令时会被账本兜底误判为“没动手”（对齐 E2E 任务 A 第 3 步：
// 复制文件被算成 0 次实质调用，网关直接判失败、前置重干烧光预算）。
// 执行会话里 python 是否真写了文件，另由置信度的成果文件核对与
// 前置判回执把关；这里只认“跑没跑”。注意：这里用 agent.IsReadOnlyToolByName
// （原始名字口径）——只读刹车与白名单已于 2026-11-26 删掉；网关若
// 跟着白名单走，“只读 3 次也算实质调用”，硬底线①的“≥1 次实质调用”
// 就形同虚设，会放过假 DONE。
func gatewaySubstantiveCall(toolID string) bool {
	if toolID == "cap-ops-python" {
		return true
	}
	return !agent.IsReadOnlyToolByName(toolID)
}

// truncateRunes 截字符串到 n 个字符（按 rune 数，不切半个汉字），
// 超了尾巴加“……”。本包自带一份（front/llm 各有一份同名函数，
// 包不同名不冲突）。
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "……"
}

func (d *daemon) computeFactList(disp *dispatch.Dispatch, taskID string, baseline time.Time, execIDBefore int64, resumed bool) *dispatch.FactList {
	fl := &dispatch.FactList{
		HasMachineCheck: len(disp.ExpectedArtifacts) > 0 || len(disp.VerifyCmd) > 0,
		Resumed:         resumed,
	}
	// 任务开始时间（Summary 第 5 行 + 缺失原因分类用）：任务首次派单的时间。
	fl.TaskStartTime = d.taskStartTime(taskID)
	// 调用次数：成功调用（不含 verify-cmd）与实质调用（成功且非只读，
	// cap-ops-python 按网关口径算实质，见 gatewaySubstantiveCall）。
	// H-2：只数本次 runAgent 窗口内的行（e.ID > execIDBefore）——
	// 多步共用同一 taskID，第 2 步 0 调用也不能继承第 1 步的账本。
	// 方向 b 严谨版：本步之前尝试的调用另数进 PriorStepCalls——
	// 只数 FirstExecIDBefore < e.ID <= execIDBefore 的行（同一 步的
	// 历史尝试）。“记没记过”以 FirstStartTime 是否已设置（非零）为准，
	// 不拿 FirstExecIDBefore>0 当信号——账本为空时首次记录就是 0，
	// 拿它当信号会漏掉空账本上第一步的历史调用；老计划表两个字段
	// 都缺（FirstStartTime 为零），仍安全退化为严格当前窗口口径。
	// PriorStepCalls 只用于硬底线①的续跑豁免与事实展示，绝不算进
	// 本轮 SuccessfulCalls。
	// M-3：顺手记最后一次 cap-ops-python 成功调用的命令摘要（账本
	// 按 id 倒序，第一个命中的就是最近一次），供前置模型一眼识别
	// 是不是真干了活（光数“跑没跑”分不出空跑只读探测）。
	if d.led != nil {
		if execs, err := d.led.ListExecutions(taskID, 1000); err == nil {
			toolCounts := map[string]int{}
			for _, e := range execs {
				if e.ID <= execIDBefore {
					if !disp.FirstStartTime.IsZero() && e.ID > disp.FirstExecIDBefore &&
						e.Status == "success" && e.ToolID != "verify-cmd" {
						fl.PriorStepCalls++
						if gatewaySubstantiveCall(e.ToolID) {
							fl.PriorStepSubstantiveCalls++
						}
					}
					continue
				}
				if e.Status == "success" && e.ToolID != "verify-cmd" {
					fl.SuccessfulCalls++
					toolCounts[e.ToolID]++
					if gatewaySubstantiveCall(e.ToolID) {
						fl.SubstantiveCalls++
					}
					if e.ToolID == "cap-ops-python" && fl.LastPythonCmd == "" {
						fl.LastPythonCmd = truncateRunes(strings.TrimSpace(e.Command), 500)
					}
				}
			}
			fl.ToolCallBreakdown = formatToolCallBreakdown(toolCounts)
		}
	}
	// 产物事实。
	fl.Artifacts = artifactFacts(disp.ExpectedArtifacts, baseline)
	// 校验命令事实（只跑一次）。
	if len(disp.VerifyCmd) > 0 {
		fl.VerifyCmd = d.runVerifyCmdFact(disp, taskID)
	}
	// 硬底线（只当记录供前置看，不再一票否决——2026-11-26 用户定）：
	// ① 0 次成功调用：所有步一律红灯——research 步也不能凭空编造，
	// execute 步更不用说（原来只在有机器检查时才拦，research 步 0 调用
	// 能直接过关）。方向 b 严谨版：断点续跑且本步历史已有成功调用时
	// 豁免——执行层读回历史、确认活已干完、0 新调用是合法的；
	// 但豁免只关这条红灯，不算全绿（见下 AllGreen 的 SuccessfulCalls>=1）。
	// ② execute 步：没有产物全合格、校验命令也没真实通过，且 0 次
	// 实质调用 → 红灯（账本兜底口径：只有只读查询、没有真正动手）。
	// H-2：声明了 verify_cmd 不能豁免实质调用检查——以前 verify_cmd
	// 失败会进硬底线④，所以 HasMachineCheck=true 跳过②没问题；④
	// 删掉后，写了 verify_cmd 反而把 0 次实质调用的底线关掉了。
	// 只有“有产物全合格”或“校验命令真实通过”时，才允许豁免。
	// 方向 b：实质调用也算本步历史尝试（PriorStepSubstantiveCalls）。
	// ③ 声明产物缺失或空或过期 → 红灯。
	// verify_cmd 不再进硬底线（对齐二号三号：校验命令是可选的，
	// 前置想加就加、不想加就不加；声明了且跑了的结果照常记在
	// 事实清单里当参考证据，只供前置模型看，不由代码一票否决）。
	resumedWithPrior := resumed && fl.PriorStepCalls > 0
	if fl.SuccessfulCalls == 0 && !resumedWithPrior {
		fl.HardFloorRed = true
		fl.HardFloorReason = "0 次工具调用：本单模型一次工具都没调"
	}
	hasSubstantive := fl.SubstantiveCalls > 0 || (resumed && fl.PriorStepSubstantiveCalls > 0)
	if disp.TaskType == "execute" && len(disp.ExpectedArtifacts) == 0 &&
		(fl.VerifyCmd == nil || !fl.VerifyCmd.Passed) &&
		!hasSubstantive && !fl.HardFloorRed {
		fl.HardFloorRed = true
		fl.HardFloorReason = "账本里无实质执行记录：本单只有只读查询，没有真正动手"
	}
	for _, a := range fl.Artifacts {
		if a.Problem != "" {
			fl.HardFloorRed = true
			fl.HardFloorReason = "产物 " + a.Path + "：" + a.Problem
			break
		}
	}
	// 全绿：有机器检查，且成功调用 ≥1、全部产物合格、校验命令通过。
	// （verify_cmd 失败不进硬底线，但也不算“全绿”——快路径不直接放行，
	// 交前置模型看事实清单里的失败记录再判；模型仍可判通过。）
	// 方向 b：本轮 0 次新调用即使豁免了硬底线①，也绝不走快车道免判——
	// 必须交前置模型语义把关（防假 DONE 关键）。
	fl.AllGreen = fl.HasMachineCheck && !fl.HardFloorRed && fl.SuccessfulCalls >= 1 &&
		(fl.VerifyCmd == nil || fl.VerifyCmd.Passed)
	// 置信度（2026-11-26 用户定，两点各 50%）：
	// ①工具调用：本步成功调用 ≥1 次得 50 分，没调 0 分；回执没声明
	// 成果文件（claimed==0）时这点占 100 分（调了 100，没调 0）。
	// ②成果文件核对：解析回执“成果在哪里”那一栏列出的绝对路径，
	// 逐个核三关（存在、去空白后非空、mtime 在本步接单时间之后），
	// 核对有 M 个 / 回执说 N 个，这点 = M/N×50 分；N==0 时这点 0 分。
	// 时间那关堵死“没调工具但文件都在”的口子：没调工具就不可能在
	// 接单后新建/改文件，时间一核全算缺失，总分 0%。
	claimed := parseClaimedReceiptFiles(d.replyText(disp, taskID))
	fl.ClaimedFiles = len(claimed)
	if len(claimed) > 0 {
		verified, missing := verifyReceiptFiles(claimed, baseline)
		fl.VerifiedFiles = verified
		fl.MissingFiles = missing
		// 缺失原因分类（Summary 第 4 行聚合用）：逐个缺失文件按 mtime
		// 与任务开始时间比，分四类计数；置信度算法本身一字不动。
		for _, p := range missing {
			switch classifyMissingFile(p, fl.TaskStartTime) {
			case dispatch.MissingReasonEarlyTask:
				fl.MissingEarlyTask++
			case dispatch.MissingReasonOld:
				fl.MissingOld++
			case dispatch.MissingReasonNonexistent:
				fl.MissingNonexistent++
			case dispatch.MissingReasonEmpty:
				fl.MissingEmpty++
			}
		}
		fl.ConfidenceFilePoints = verified * 50 / len(claimed)
		if fl.SuccessfulCalls >= 1 {
			fl.ConfidenceToolPoints = 50
		}
	} else if fl.SuccessfulCalls >= 1 {
		fl.ConfidenceToolPoints = 100
	}
	fl.Confidence = fl.ConfidenceToolPoints + fl.ConfidenceFilePoints
	return fl
}

// replyText 读执行层回信文件正文（计划步读 step-<n>-reply.md，
// 其余读顶层 reply.md）；读不到返回 ""（回执没声明成果文件）。
func (d *daemon) replyText(disp *dispatch.Dispatch, taskID string) string {
	name := "reply.md"
	if disp.PlanStep && disp.StepNo > 0 {
		name = fmt.Sprintf("step-%d-reply.md", disp.StepNo)
	}
	b, err := os.ReadFile(filepath.Join(d.dataDir, "artifacts", taskID, name))
	if err != nil {
		return ""
	}
	return string(b)
}

// verifyDelivery 是网关硬验货（P0-2）：执行会话只返回 stopped（模型停下），
// 完成与否由这里按派单里的机器可检标准决定，验过才返回 done。
// 只有 res.Status == "stopped" 才验；stalled/max_iterations/agent_error 等
// 原样返回（下游标 FAILED）。验不过返回 failed，并把原因写进 reply.md
// （覆盖执行方草稿，但保留草稿前 500 字供参考），让回执结论说真话。
// 零额外 token，全是确定性代码检查。
// 规划 v2：事实清单在这里算一次（computeFactList），挂在 disp.FactList
// 上供 buildReceipt 抄进回执；验货用同一份事实，verify_cmd 不重复跑。
// 部分事实清单（中优）：stalled/max_iterations 回执也算一份事实清单，
// 但状态保持原样非 done，绝不触发 verifyResearch/verifyExecute 改标 done
// （H-4 红线：非 done 永不判通过）。
func (d *daemon) verifyDelivery(disp *dispatch.Dispatch, taskID string, res *agent.RunResult, start time.Time, execIDBefore int64) string {
	if res == nil {
		return "agent_error"
	}
	// 部分事实清单：不管什么状态都先算一份（stalled/max_iterations 也带），
	// 供前置判回执参考；只有 stopped 才继续走下面的硬验货。
	if disp != nil {
		// 事实清单算一次（只读代码查 + verify_cmd 跑一次）。
		resumed := res.Resumed
		baseline := d.factBaseline(disp, start, resumed)
		disp.FactList = d.computeFactList(disp, taskID, baseline, execIDBefore, resumed)
		// H-2：续跑 + 首轮时间已记 + 有重干原因 + 基线早于本轮开始（用了
		// CreatedTS 或 FirstStartTime）+ 声明了产物时，提示前置模型：
		// 产物可能上次修改于本步首轮，不必因“本轮没重写”而误判。
		if resumed && !disp.FirstStartTime.IsZero() && strings.TrimSpace(disp.LastRedoReason) != "" &&
			baseline.Before(start) && len(disp.ExpectedArtifacts) > 0 {
			disp.FactList.ArtifactNote = "产物上次修改于本步首轮（断点续跑，首轮产物按首轮时间判定新鲜）"
		}
	}
	if res.Status != "stopped" {
		return res.Status
	}
	if disp == nil {
		// 无派单（旧口径直接跑用户原话）：没有机器可检标准，原样返回。
		return res.Status
	}
	taskType := disp.TaskType
	if taskType == "" {
		taskType = "execute"
	}
	var ok bool
	var reason string
	if taskType == "research" {
		ok, reason = d.verifyResearch(taskID, disp, disp.FactList)
	} else {
		ok, reason = d.verifyExecute(disp, taskID, disp.FactList)
	}
	if ok {
		log.Printf("task=%s 网关验货通过（%s）", taskID, taskType)
		return "done"
	}
	log.Printf("task=%s 网关验货未通过（%s）：%s", taskID, taskType, reason)
	d.writeVerificationFailureReply(disp, taskID, reason, res.ReplyDraft)
	return "failed"
}

// verifyResearch 验调研任务（P0-3）：只验回信文件存在非空 + 事实清单在。
// 硬底线（HardFloorRed，含 0 次调用）不再由网关一票否决（2026-11-26
// 用户定：一票否决已去掉，事实清单只当参考）：0 次调用就交报告时，
// 网关照常放行，前置看事实清单（置信度 0%、0 次调用）自己判失败。
// 账本未接线时 fail-closed（与 verifySubstantiveExecution 一致，
// 不再 fail-open——接线缺失本身就是验货失败，不能当通过）。
// 事实清单（fl）由 verifyDelivery 算好传进来，不重复查账本；
// fl 为空时 fail-closed（事实清单缺失本身就是验货失败，退路已收口）。
// 回信文件看步级文件（H-5）：计划步查 step-<n>-reply.md，其余查 reply.md。
func (d *daemon) verifyResearch(taskID string, disp *dispatch.Dispatch, fl *dispatch.FactList) (bool, string) {
	stepNo, planStep := 0, false
	if disp != nil {
		stepNo, planStep = disp.StepNo, disp.PlanStep
	}
	if !d.artifactExistsStep(taskID, stepNo, planStep) {
		return false, "调研任务无交付：回信文件不存在或为空"
	}
	if fl == nil {
		return false, "事实清单缺失，无法验货"
	}
	return true, ""
}

// verifyExecute 验执行任务（P0-2）：只做两道网关检查——
//   - 事实清单在（fl 为空时 fail-closed，事实清单缺失本身就是验货失败）；
//   - 派单既没声明 expected_artifacts 也没声明 verify_cmd 时，退到账本
//     检查：至少 1 行非只读工具调用（并记账本事件提醒派单未声明机器可检标准）。
//
// 硬底线（HardFloorRed：0 次调用、0 实质调用、声明产物有问题）不再由网关
// 一票否决（2026-11-26 用户定：一票否决已去掉，事实清单只当参考）：
// 声明的产物缺失/过期、执行方 0 次调用时，网关照常放行，前置看事实清单
// （置信度、调用次数、产物核对结果）自己判。verify_cmd 的结果只记在
// 事实清单里当参考证据，不由网关判。
// 事实清单（fl）由 verifyDelivery 算好传进来（verify_cmd 已跑过，
// 产物已查过），这里只读事实，不重复跑命令、不重复查账本。
func (d *daemon) verifyExecute(disp *dispatch.Dispatch, taskID string, fl *dispatch.FactList) (bool, string) {
	if fl == nil {
		return false, "事实清单缺失，无法验货"
	}
	if len(disp.ExpectedArtifacts) == 0 && len(disp.VerifyCmd) == 0 {
		// 方向 b：续跑时本步历史尝试的实质调用也算（PriorStepSubstantiveCalls）。
		if fl.SubstantiveCalls < 1 && !(fl.Resumed && fl.PriorStepSubstantiveCalls >= 1) {
			return false, "账本里无实质执行记录：本单只有只读查询，没有真正动手"
		}
		if d.led != nil {
			// 派单未声明机器可检标准：记账本事件，提醒前置下次写明。
			_, _ = d.led.RecordDispatchEvent(ledger.DispatchEvent{
				SessionID: d.session, TaskID: taskID, DispatchID: disp.ID,
				Kind: ledger.DispatchEventSoftAlert, DispatchEstTokens: 0,
				Detail: "派单未声明 expected_artifacts/verify_cmd，网关只做了账本兜底检查",
			})
		}
	}
	return true, ""
}

// artifactFacts 逐项查产物清单，返回每项的事实（存在/大小/新鲜/
// 非空/合法/问题原因）。问题原因（Problem）与 verifyArtifacts 的
// 旧文案逐字一致（“不是绝对路径”等），供事实清单与验货共用——
// 事实清单与验货是同一份检查，不各写一套（规划 v2）。
func artifactFacts(paths []string, start time.Time) []dispatch.ArtifactFact {
	earliest := start.Add(-2 * time.Second)
	out := make([]dispatch.ArtifactFact, 0, len(paths))
	for _, p := range paths {
		f := dispatch.ArtifactFact{Path: p}
		if !filepath.IsAbs(p) {
			f.Problem = "不是绝对路径"
			out = append(out, f)
			continue
		}
		escaped := false
		for _, seg := range strings.Split(p, string(os.PathSeparator)) {
			if seg == ".." {
				escaped = true
				break
			}
		}
		if escaped {
			f.Problem = "路径含 ..，不许逃逸"
			out = append(out, f)
			continue
		}
		fi, err := os.Stat(p)
		if err != nil {
			f.Problem = "不存在或不可读"
			out = append(out, f)
			continue
		}
		f.Exists = true
		if fi.IsDir() {
			f.Problem = "是目录，不是文件"
			out = append(out, f)
			continue
		}
		f.Size = fi.Size()
		if fi.Size() == 0 {
			f.Problem = "空文件"
			out = append(out, f)
			continue
		}
		// 全空白文件（只有空格/换行/空字节）也不算产物：只读前 4KB 验。
		if blank, rerr := isBlankFile(p); rerr == nil && blank {
			f.Problem = "全空白文件"
			out = append(out, f)
			continue
		}
		f.NonEmpty = true
		if fi.ModTime().Before(earliest) {
			f.Problem = "不是本单新生成的，mtime 过早"
			out = append(out, f)
			continue
		}
		f.Fresh = true
		if strings.HasSuffix(strings.ToLower(p), ".json") {
			b, err := os.ReadFile(p)
			if err != nil {
				f.Problem = "读失败"
				out = append(out, f)
				continue
			}
			if !json.Valid(b) {
				f.Problem = "JSON 不合法"
				out = append(out, f)
				continue
			}
		}
		f.Valid = true
		out = append(out, f)
	}
	return out
}

// verifyArtifacts 逐项核对产物清单（P0-2/Q7）：只认绝对路径；含 ".." 段
// 的直接判失败；只认文件（目录不算）；文件须存在、非空、非全空白；
// mtime 须 >= 任务开始时间 - 2 秒（容差）；.json 文件附加合法性校验。
// 返回未通过项的原因清单（为空即全部通过）。
// 由 artifactFacts 的事实导出，与原来逐字一致（规划 v2 重构）。
func verifyArtifacts(paths []string, start time.Time) []string {
	var bad []string
	for _, f := range artifactFacts(paths, start) {
		if f.Problem != "" {
			bad = append(bad, f.Path+"（"+f.Problem+"）")
		}
	}
	return bad
}

// isBlankFile 读文件前 4KB，全是空白字符（空格/换行/制表/空字节）即判空白。
// 启发式：4KB 全空白的文件几乎不可能是真实交付；前 4KB 有内容即放过。
func isBlankFile(p string) (bool, error) {
	f, err := os.Open(p)
	if err != nil {
		return false, err
	}
	defer f.Close()
	buf := make([]byte, 4096)
	n, err := f.Read(buf)
	if err != nil && err != io.EOF {
		return false, err
	}
	return len(bytes.Trim(buf[:n], " \t\r\n\x00")) == 0, nil
}

// runVerifyCmdFact 执行派单声明的校验命令（P0-2/Q2），返回执行事实
// （是否跑了、exit 码、是否通过、原因）。argv 已拆分，走 executor，
// 60 秒超时；exit 0 才算过。命令本身记进 execution_ledger（ToolID 为
// verify-cmd，是验货动作，不计入任务的实质执行检查）。
// 事实清单与验货共用这一个入口——命令只跑一次，不重复执行（规划 v2）。
func (d *daemon) runVerifyCmdFact(disp *dispatch.Dispatch, taskID string) *dispatch.VerifyCmdFact {
	f := &dispatch.VerifyCmdFact{Cmd: disp.VerifyCmd, ExitCode: -1}
	if d.ex == nil {
		f.Detail = "执行器未接线，无法跑校验命令"
		return f
	}
	res, h, err := d.ex.Run(executor.RunRequest{
		TaskID: taskID, SessionID: d.session, ToolID: "verify-cmd",
		Argv: disp.VerifyCmd, Timeout: 60 * time.Second, Text: disp.UserText,
	})
	if err != nil {
		f.Detail = "执行失败：" + err.Error()
		return f
	}
	if h != nil {
		r, werr := h.Wait()
		if werr != nil {
			f.Detail = "等后台失败：" + werr.Error()
			return f
		}
		res = r
	}
	if res == nil {
		f.Detail = "执行无返回"
		return f
	}
	f.Ran = true
	f.ExitCode = res.ExitCode
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(res.ErrHead)
		if msg == "" {
			msg = strings.TrimSpace(res.StdoutTail)
		}
		if rs := []rune(msg); len(rs) > 200 {
			msg = string(rs[:200])
		}
		f.Detail = fmt.Sprintf("exit=%d：%s", res.ExitCode, msg)
		return f
	}
	f.Passed = true
	return f
}

// runVerifyCmd 执行派单声明的校验命令（P0-2/Q2）：argv 已拆分，走 executor，
// 60 秒超时；exit 0 才算过。由 runVerifyCmdFact 导出（规划 v2 重构，
// 行为与原来一致：命令只跑一次，事实由 computeFactList 复用）。
func (d *daemon) runVerifyCmd(disp *dispatch.Dispatch, taskID string) (bool, string) {
	f := d.runVerifyCmdFact(disp, taskID)
	if f == nil || !f.Ran {
		if f != nil && f.Detail != "" {
			return false, f.Detail
		}
		return false, "执行器未接线，无法跑校验命令"
	}
	if !f.Passed {
		return false, f.Detail
	}
	return true, ""
}

// writeVerificationFailureReply 写一封验货失败回信，覆盖执行方草稿：
// 说清失败原因，并保留草稿前 500 字供参考（草稿本身仍在步骤产物里，
// 不丢）。让回执结论说真话，不让用户看到一份“成功”的草稿。
// 原因里可能拼了 verify_cmd 的报错输出（环境变量/密钥），落盘前先过
// 出站脱敏（Gemini 审阅中危），不让密钥绕过脱敏进 reply.md。
// 计划步写 step-<n>-reply.md，其余写顶层 reply.md（H-5：计划步不写
// 顶层，看门狗才不会误标整单）。
func (d *daemon) writeVerificationFailureReply(disp *dispatch.Dispatch, taskID, reason, draft string) {
	if d.dataDir == "" {
		return
	}
	var b strings.Builder
	b.WriteString("任务未能完成（网关验货未通过）。\n\n原因：")
	b.WriteString(reason)
	b.WriteString("\n\n执行方原回复（供参考，未通过验货）：\n")
	draft = strings.TrimSpace(draft)
	if rs := []rune(draft); len(rs) > 500 {
		draft = string(rs[:500]) + "……"
	}
	b.WriteString(draft)
	b.WriteString("\n")
	body, n, blocked := sanitize.ScrubOutbound(b.String())
	if n > 0 || blocked {
		log.Printf("task=%s 验货失败回信脱敏：掩码 %d 处，整封拦截=%v", taskID, n, blocked)
	}
	name := "reply.md"
	if disp != nil && disp.PlanStep && disp.StepNo > 0 {
		name = fmt.Sprintf("step-%d-reply.md", disp.StepNo)
	}
	artDir := filepath.Join(d.dataDir, "artifacts", taskID)
	_ = os.MkdirAll(artDir, 0o700)
	_ = writeFileAtomic(filepath.Join(artDir, name), []byte(body), 0o600)
}

// clampStepCap 把派单预估步数换算成执行步数上限：预估×2，夹在 10–50 步。
// 预估 ≤0 时给默认 10 步。900 秒 / 200 步最后外墙不受影响。
func clampStepCap(estSteps int) int {
	cap := estSteps * 2
	if cap < 10 {
		cap = 10
	}
	if cap > 50 {
		cap = 50
	}
	return cap
}

// dispatchTaskText 把派单拼成执行会话的任务文本：用户原话打头，
// 再附目标、验收标准、建议工具、步骤骨架、约束、指针。执行按它干活，
// 不重分诊、不重查记忆。
// 建议工具后附 one_liner（调法）：执行模型不展开工具单就看不到 one_liner，
// 只给工具编号它不知道 inputs 格式，会传空 inputs 调失败（2026-11-24
// 验证任务 002/003/004 教训）；把 one_liner 贴进任务文本，模型照抄调法。
func dispatchTaskText(disp *dispatch.Dispatch, reg *registry.Registry) string {
	var b strings.Builder
	b.WriteString("用户原话：\n" + disp.UserText + "\n")
	if strings.TrimSpace(disp.Goal) != "" {
		b.WriteString("目标：" + disp.Goal + "\n")
	}
	if strings.TrimSpace(disp.Criteria) != "" {
		b.WriteString("验收标准：" + disp.Criteria + "\n")
	}
	if len(disp.ToolIDs) > 0 {
		b.WriteString("建议工具（已分诊，不重分诊）：\n")
		for _, id := range disp.ToolIDs {
			line := "- " + id
			if reg != nil {
				if e, ok := reg.Entry(id); ok && e.OneLiner != "" {
					line += "：" + e.OneLiner
				}
			}
			b.WriteString(line + "\n")
		}
	}
	if len(disp.PlanSkeleton) > 0 {
		b.WriteString("步骤骨架：\n")
		for i, s := range disp.PlanSkeleton {
			fmt.Fprintf(&b, "%d. %s\n", i+1, s)
		}
	}
	if len(disp.Constraints) > 0 {
		b.WriteString("约束：\n")
		for _, s := range disp.Constraints {
			b.WriteString("- " + s + "\n")
		}
	}
	if len(disp.Pointers) > 0 {
		b.WriteString("指针（路径/链接/记忆 ID，不贴全文）：\n")
		for _, s := range disp.Pointers {
			b.WriteString("- " + s + "\n")
		}
	}
	if disp.ShardNo > 0 {
		fmt.Fprintf(&b, "分片：本片是第 %d 片（每片独立 900 秒预算）。\n", disp.ShardNo)
	}
	if strings.TrimSpace(disp.ResumeFrom) != "" {
		b.WriteString("续跑起点（上一片的断点，已落盘，从这里接续，不要从头重做）：\n" + disp.ResumeFrom + "\n")
	}
	return b.String()
}

// dispatchMemorySummary 把派单带的记忆碎片拼成执行会话的记忆摘要
// （碎片为空返回空串）。
func dispatchMemorySummary(disp *dispatch.Dispatch) string {
	if len(disp.Memory) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("相关记忆摘要（前置已查）：\n")
	for _, m := range disp.Memory {
		b.WriteString("- " + m + "\n")
	}
	// 脱敏口径（用户 2026-11-24 09:12 纠正）：交付给模型前绝不掩码
	// （旧节点 4 已取消）；碎片在前置 doRecall 里同样不再掩码。
	return b.String()
}

func main() {
	shadowFlag := flag.Bool("shadow", false, "影子模式：只记不跑")
	dataDirFlag := flag.String("data-dir", "", "数据目录（覆盖配置）")
	listenFlag := flag.String("listen", "", "监听地址（覆盖配置）")
	flag.Parse()

	// 子命令：index-memory —— 重建语义向量索引后退出，不启动守护进程。
	// 用法：super-agent index-memory [--data-dir DIR]
	if flag.NArg() > 0 && flag.Arg(0) == "index-memory" {
		runIndexMemory(*dataDirFlag)
		return
	}

	// 子命令：forge-run —— 手动跑一次锻造（验收用），跑完退出，不启动守护进程。
	// 用法：super-agent forge-run [--data-dir DIR] [--gap GAPID] [--check-trials] [--confirm-pending GAPID]
	// 默认：处理缺口队列 + 检查试用期；--gap 只处理指定缺口；--check-trials 只检查试用期；
	// --confirm-pending GAPID 人工确认一个 high 风险挂起工具并注册。
	if flag.NArg() > 0 && flag.Arg(0) == "forge-run" {
		runForgeCmd(flag.Args()[1:], *dataDirFlag)
		return
	}

	// 子命令：backup-run —— 手动跑一次记忆自动备份（第 15 步），跑完退出，不启动守护进程。
	// 用法：super-agent backup-run [--data-dir DIR] [--date YYYY-MM-DD]
	// 默认日期为昨天（与 04:00 精炼后备份的口径一致）。备份开关关闭时直接报错退出。
	if flag.NArg() > 0 && flag.Arg(0) == "backup-run" {
		runBackupOnce(flag.Args()[1:], *dataDirFlag)
		return
	}

	// 子命令：patrol-run —— 手动跑一次简介巡检（线 B 第二阶段第 4 棒），跑完退出，不启动守护进程。
	// 用法：super-agent patrol-run [--data-dir DIR] [--full]
	// 默认增量（mtime+size 预过滤）；--full 全量 SHA-256 校准。
	if flag.NArg() > 0 && flag.Arg(0) == "patrol-run" {
		runPatrolOnce(flag.Args()[1:], *dataDirFlag)
		return
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if *dataDirFlag != "" {
		cfg.DataDir = *dataDirFlag
	}
	if *listenFlag != "" {
		cfg.ListenAddr = *listenFlag
	}
	shadow := cfg.Shadow || *shadowFlag

	dataDir := cfg.ResolveDataDir()
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		log.Fatalf("mkdir data dir %s: %v", dataDir, err)
	}
	log.Printf("config ready: data_dir=%s listen=%s shadow=%v env_file=%s", dataDir, cfg.ListenAddr, shadow, cfg.EnvFile)
	if len(cfg.Missing) > 0 {
		log.Printf("config missing keys (defaults used): %v", cfg.Missing)
	}
	for _, w := range cfg.Warnings {
		log.Printf("config warning: %s", w)
	}

	dbPath := filepath.Join(dataDir, "ledger.db")
	led, err := ledger.Open(dbPath)
	if err != nil {
		log.Fatalf("ledger open %s: %v", dbPath, err)
	}
	defer ledger.Close(led)
	log.Printf("ledger ready: %s", dbPath)

	// index_metrics 记录器（第 6 项）：懒加载每次读索引都记一行
	// （注入 token 与相对全量省下的 token），供每日 09:00 核算。
	// 打不开时只记日志、不致命——核算只是少了索引这一项，服务照常跑。
	// 注意：记录器为 nil 时绝不传 WithMetricsRecorder（nil 指针装进
	// 接口后非空，调用会 panic）；只有成功打开才接线。
	var loadOpts []registry.Option
	if rec, err := registry.NewLedgerFileRecorder(dbPath); err != nil {
		log.Printf("registry metrics: 打不开 index_metrics 记录器：%v（索引记账关闭）", err)
	} else {
		defer rec.Close()
		loadOpts = append(loadOpts, registry.WithMetricsRecorder(rec))
	}
	reg, err := registry.Load(dataDir, loadOpts...)
	if err != nil {
		log.Fatalf("registry load: %v", err)
	}
	log.Printf("registry ready: %d entries", reg.Count())

	mem, err := memory.Open(filepath.Join(dataDir, "memory"))
	if err != nil {
		log.Fatalf("memory open: %v", err)
	}
	if n, err := mem.PruneL1(); err != nil {
		log.Printf("memory prune L1: %v", err)
	} else {
		log.Printf("memory ready: pruned %d expired L1 files", n)
	}
	// 语义检索：本地 Ollama embedding（qwen3-embedding:0.6b），向量库在
	// data/memory/vectors.db。Ollama 或向量库不可用时只记日志、继续跑，
	// Recall 降级只用词法。第 22 步：索引自动增量重建——每天 04:00
	// 精炼成功后后台重建（只嵌新增/改动过的文档）；启动时索引为空
	// 也后台补建一次，不再需要手动跑 index-memory。
	var sem *memory.Service
	if s, err := memory.OpenSemantic(
		filepath.Join(dataDir, "memory", "vectors.db"),
		memory.SemanticDefaultURL, memory.SemanticDefaultModel); err != nil {
		log.Printf("semantic disabled: %v", err)
	} else {
		sem = s
		mem.WithSemantic(sem.Search)
		log.Printf("semantic ready: model=%s docs=%d dim=%d", sem.Model(), sem.Count(), sem.Dim())
		defer sem.Close()
	}

	// 简介主库（线 B 第二阶段第 3 棒）：data/memory/summaries.db
	// 存在时才接线（不存在则走旧 Recall 路径，不报错、不建空库——
	// 空库会让两阶段召回误以为简介已建完）。问句向量走本地
	// Ollama qwen3-embedding:0.6b，不走 API。主库文件由
	// summary-backfill 从接入 manifest 回填（见
	// docs/line-b-phase2-stage3-deploy-notes.md）。
	if _, err := os.Stat(filepath.Join(dataDir, "memory", "summaries.db")); err == nil {
		if sumStore, err := memory.OpenSummaryStore(filepath.Join(dataDir, "memory", "summaries.db")); err != nil {
			log.Printf("summary store disabled: %v", err)
		} else {
			mem.WithSummaryStore(sumStore, memory.OllamaEmbedFunc("", ""))
			n, _ := sumStore.CountDocs()
			nv, _ := sumStore.CountVectors()
			log.Printf("summary store ready: docs=%d vectors=%d", n, nv)
			defer sumStore.Close()
		}
	} else {
		log.Printf("summary store: data/memory/summaries.db 不存在，走旧 Recall 路径")
	}

	ex := &executor.Executor{Ledger: led}
	// 监工已下线：Executor.Advisor 保持 nil（不审），不接任何监工。
	log.Printf("executor ready: live mode, advisor removed (no review)")

	wd := watchdog.New(led, dbPath, nil)
	// 第 22 步：语义索引自动增量重建。精炼成功后由 RefineLoop 经
	// afterRefineSemantic 后台触发；启动时索引为空也在后台补建一次。
	// 重建是增量的（只嵌新增/改动过的文档），日常只有几篇，几秒完成。
	if sem != nil {
		memRoot := filepath.Join(dataDir, "memory")
		rebuild := func() {
			n, err := sem.Rebuild(memRoot)
			if err != nil {
				log.Printf("semantic auto-rebuild: %v", err)
				return
			}
			log.Printf("semantic auto-rebuild: 本次向量化 %d 篇，库内共 %d 篇", n, sem.Count())
		}
		wd.SetSemanticHook(func(ctx context.Context) error {
			rebuild()
			return nil
		})
		if sem.Count() == 0 {
			log.Printf("semantic auto-rebuild: 索引为空，启动后后台补建")
			go rebuild()
		}
	}
	if res, err := wd.CheckRegistry(dataDir); err != nil {
		log.Printf("watchdog check: %v", err)
	} else {
		log.Printf("watchdog check: %d checked, %d ok, %d broken, %d skipped", res.Checked, res.OK, len(res.Broken), res.Skipped)
	}
	// 启动探针：对主总账的全部可用可执行条目跑一遍存在性探针，
	// 结果记进本进程覆盖层——失败的条目直接不进主循环清单，
	// 模型没机会挑一个本机不存在的工具（陌生机器拉取部署时，
	// 缺种子的 11 条入口会在这里被剔掉，而不是等模型挑中了才失败）。
	checked, ok, failed := reg.ProbeAll()
	log.Printf("registry probe: %d checked, %d ok, %d failed (failed excluded from agent list)", checked, ok, failed)
	if sv, err := wd.DailySavings(); err != nil {
		log.Printf("watchdog savings: %v", err)
	} else {
		log.Printf("watchdog savings: date=%s triage=%d tier0=%.0f%% net_saved=%d warn=%v", sv.Date, sv.TriageCount, sv.Tier0Ratio*100, sv.NetSavedTokens, sv.Warn)
	}

	d := &daemon{cfg: cfg, dataDir: dataDir, led: led, reg: reg, mem: mem, ex: ex, wd: wd, session: newSessionID(), shadow: shadow}
	// P2-a：会话存盘入口（plan_state + 步历史），前置与执行共用。
	d.sess = session.New(dataDir, led)
	// P2-b：启动时归档一次 7 天未活动的已终结会话（挪归档文件夹，不删）。
	// best-effort：失败只记日志、不阻塞启动。
	if archived, aerr := d.sess.ArchiveIdleSessions(0); aerr != nil {
		log.Printf("启动归档旧会话失败: %v", aerr)
	} else if len(archived) > 0 {
		log.Printf("启动归档旧会话 %d 个: %v", len(archived), archived)
	}
	// DeepSeek 客户端接线（第 10 步）：模型名从配置读，密钥只读键名
	// DEEPSEEK_API_KEY。非影子模式下无密钥明确报错退出，不悄悄降级
	// 成 Tier-0-only；影子模式下无密钥只记日志（Tier-1 会报 ErrNoModel）。
	var lc *llm.Client
	if apiKey, hasKey := llm.ResolveAPIKey(); !hasKey {
		if !shadow {
			log.Fatalf("llm: 未找到 DEEPSEEK_API_KEY（环境变量或 .env），非影子模式拒绝启动")
		}
		log.Printf("llm: 未找到 DEEPSEEK_API_KEY，影子模式下 Tier-1 分诊不可用（Tier-0 前缀直出不受影响）")
	} else {
		lc = llm.New(cfg.WorkerModel, apiKey)
		lc.Ledger = led
		lc.SessionID = d.session
		d.modelCall = lc.TriageCall
		log.Printf("llm ready: model=%s (key found, value never logged)", cfg.WorkerModel)
		// 监工已下线：此处不再接线 Advisor（2026-11-26 用户令）。
		// 主循环接线（对齐二号 Step 1+Step 4）：只用 worker 模型（flash）逐轮
		// 决定调哪个工具，不再另配核验模型（reasoner 已摘）；影子模式也接线，
		// 只做决定、不执行。
		// 规划 v2 分角色记账：执行层按 executor 记账。withRole 浅拷贝
		// 客户端（无互斥锁，安全），并发任务各用各的拷贝，不会串角色。
		executorCall := func(taskID, frozen, tail string) (string, int64, int64, error) {
			return lc.CallTaskRole(taskID, frozen, tail, ledger.ExpenseRoleExecutor)
		}
		executorCallMeta := func(taskID, frozen, tail string) (string, string, int64, int64, error) {
			return lc.CallTaskWithMetaRole(taskID, frozen, tail, ledger.ExpenseRoleExecutor)
		}
		executorCallTools := func(taskID, frozen, tail string, tools []llm.ToolDefinition, history []llm.Message) (llm.Message, string, int64, int64, error) {
			return lc.CallTaskFullRole(taskID, frozen, tail, tools, history, ledger.ExpenseRoleExecutor)
		}
		d.ag = agent.New(executorCall, reg, led, ex, dataDir, d.session)
		// P0-1：执行会话用 CallTaskWithMeta（带 finish_reason），截断时整轮作废。
		d.ag.CallWithMeta = executorCallMeta
		d.ag.CallWithTools = executorCallTools // P1：原生工具调用（生产唯一路径）
		d.ag.Sess = d.sess                     // P2-a：执行会话的步历史存盘入口
		// 线 B 第二阶段：只读补搜工具（search_memory/read_memory）接记忆
		// Store，模型可在任务中途自己补查记忆（每任务各限 3 次）。
		d.ag.WithMemoryStore(mem)
		// 第 23 步：缺口检测只写报告（本地写盘），影子模式也开。
		d.ag.Forge = &forge.Store{DataDir: dataDir}
		log.Printf("agent ready: worker=%s shadow=%v", cfg.WorkerModel, shadow)
		// 前置会话接线（第 1 项）：判断/分诊/派单/回话，只用 worker 模型
		// （flash）；派单通道是前置→执行的内部通道。
		// 规划 v2 分角色记账：前置规划按 front_plan、判回执按 front_judge。
		frontPlanCall := func(taskID, frozen, tail string) (string, int64, int64, error) {
			return lc.CallTaskRole(taskID, frozen, tail, ledger.ExpenseRoleFrontPlan)
		}
		frontPlanCallTools := func(taskID, frozen, tail string, tools []llm.ToolDefinition, history []llm.Message) (llm.Message, string, int64, int64, error) {
			return lc.CallTaskFullRole(taskID, frozen, tail, tools, history, ledger.ExpenseRoleFrontPlan)
		}
		frontJudgeCallTools := func(taskID, frozen, tail string, tools []llm.ToolDefinition, history []llm.Message) (llm.Message, string, int64, int64, error) {
			return lc.CallTaskFullRole(taskID, frozen, tail, tools, history, ledger.ExpenseRoleFrontJudge)
		}
		// H-3：最终汇报按 front_summary 角色记账（不再混进 front_judge，
		// 全任务总费用分账完整）。
		frontSummaryCallTools := func(taskID, frozen, tail string, tools []llm.ToolDefinition, history []llm.Message) (llm.Message, string, int64, int64, error) {
			return lc.CallTaskFullRole(taskID, frozen, tail, tools, history, ledger.ExpenseRoleFrontSummary)
		}
		d.ch = dispatch.New(dataDir)
		d.fr = front.New(frontPlanCall, reg, mem, d.ch, dataDir, d.session, led)
		d.fr.CallWithTools = frontPlanCallTools           // 2 号：前置原生工具调用（生产唯一路径）
		d.fr.CallWithToolsJudge = frontJudgeCallTools     // 规划 v2：判回执按 front_judge 记账
		d.fr.CallWithToolsSummary = frontSummaryCallTools // H-3：最终汇报按 front_summary 记账
		// M-5：翻新摘要按 rollover 角色记账（不计入规划 ¥0.05 熔断，
		// 但仍记总账本），且透传 ctx（420 秒超时生效）。
		d.fr.CallRollover = func(ctx context.Context, taskID, prompt string) (string, error) {
			reply, _, _, err := lc.CallTaskContextRole(ctx, taskID, "", prompt, ledger.ExpenseRoleRollover)
			return reply, err
		}
		// 规划 v2 看门狗续期：前置在每步派发前把 inbox RUNNING 行的
		// deadline 与 updated_ts 推后 1800 秒，免得长计划被当死任务。
		d.fr.RenewDeadline = func(taskID string) {
			if err := led.TouchInboxRunning(taskID, time.Now().Add(gateway.WatchdogDeadline)); err != nil {
				log.Printf("front task=%s 看门狗续期失败: %v", taskID, err)
			}
		}
		log.Printf("front ready: worker=%s", cfg.WorkerModel)
	}
	gw := gateway.New(cfg, led, d.handle)
	gw.SetSession(d.session)
	gw.SetDataDir(d.dataDir) // 水位扫荡给重排耗尽的任务写失败回信用（P0 修复）
	// 第 14 步 B-02：启动时重放崩溃前没确认的 inbox 行（先于 Start）。
	if n, err := gw.ReplayInbox(); err != nil {
		log.Printf("gateway replay: %v", err)
	} else if n > 0 {
		log.Printf("gateway replay: %d inbox rows requeued", n)
	}
	gw.Start()

	// L-1：启动时列出未终结的计划（断点续跑的观测入口）：只记日志，
	// 告诉值班的人“这几个计划还活着、停在第几步”；真正把计划拉起来
	// 跑靠上面 ReplayInbox 重放未确认的 inbox 行（外部重投递），
	// 没有 inbox 行的计划不会被自动唤醒——这是设计如此，不是漏扫。
	if d.sess != nil {
		if active, aerr := d.sess.ListActivePlans(); aerr != nil {
			log.Printf("启动列未终结计划失败: %v", aerr)
		} else if len(active) > 0 {
			for _, ps := range active {
				log.Printf("未终结计划：会话 %s 计划 %s 停在第 %d/%d 步（状态 %s）", ps.SessionID, ps.PlanID, ps.CurrentStep, ps.TotalSteps, ps.Status)
			}
		}
	}

	// 派单 worker（第 1 项）：前置→执行的内部通道常驻执行循环。
	// 只在前置已接线时起（无模型调用时没有派单，worker 不起）。
	// 启动时先把落盘未回执的派单重入队（不丢单）。
	refineCtx, refineCancel := context.WithCancel(context.Background())
	defer refineCancel()
	if d.ch != nil {
		go dispatch.Worker(refineCtx, d.ch, d.executeDispatch)
		log.Printf("dispatch worker started")
	}

	// L-2：会话归档巡查（每 24 小时一次）：启动时只归档过一次，
	// 长期跑的进程里 7 天未活动的已终结会话靠这条巡查挪归档，
	// 不然 sessions/ 里会一直攒旧会话。best-effort，失败只记日志。
	if d.sess != nil {
		go func() {
			ticker := time.NewTicker(24 * time.Hour)
			defer ticker.Stop()
			for {
				select {
				case <-refineCtx.Done():
					return
				case <-ticker.C:
					if archived, aerr := d.sess.ArchiveIdleSessions(7 * 24 * time.Hour); aerr != nil {
						log.Printf("会话归档巡查失败: %v", aerr)
					} else if len(archived) > 0 {
						log.Printf("会话归档巡查归档 %d 个: %v", len(archived), archived)
					}
				}
			}
		}()
	}

	// 每日精炼（第 12 步）：影子模式不启动（精炼会真写 Wiki）；
	// 非影子模式下模型已接线则起常驻循环，每天北京时间 04:00–04:15
	// 精炼前一天的 L1。模型不可用时循环内每天只记一条日志，不报错。
	// （refineCtx 在派单 worker 启动处已建好，这里复用。）
	if !shadow && lc != nil {
		go wd.RefineLoop(refineCtx, mem, dataDir, lc.RefineL1)
		log.Printf("refine loop started: daily 04:00 Beijing, model=%s", cfg.WorkerModel)
	} else if !shadow {
		log.Printf("refine loop disabled: no llm client")
	}

	// 每日 09:00 省 token 核算（第 6 项）：把 R-31 的假规则变成真定时。
	// 常驻循环每天北京时间 09:00–09:15 核算前一天的完整账，落盘到
	// data/daily-savings/<日期>.json，净省为负记告警日志。纯算术，
	// 不调模型。影子模式也起（只读账本，不写库）。
	go wd.SavingsLoop(refineCtx, dataDir)
	log.Printf("savings loop started: daily 09:00 Beijing")

	// 自进化闭环（第 20 步）：非影子模式起常驻循环，每天北京时间 04:30
	// 自查、每周日 04:30 重建注册表；影子模式也起循环，但只记
	// “本应自查”，不执行、不写账本、不写 Wiki。非影子模式下 lc 一定
	// 非空（无密钥时上面已 fatal）；影子模式下 lc 可能为空，循环内
	// 的影子分支不调模型，安全。
	var reviewFn selfevolve.ReviewFunc
	if lc != nil {
		reviewFn = lc.ReviewSelfEvolve
	}
	seLoop := selfevolve.New(led, reg, mem, dataDir, d.session, reviewFn, shadow)
	go seLoop.Run(refineCtx)

	// 第 23 步锻造：非影子模式且有模型时接线进 04:30 自查循环
	// （处理缺口队列 + 试用期检查，周日复查上游）；影子模式不跑
	// 锻造（会真调模型、真改注册表）。
	if !shadow && lc != nil {
		seLoop.Forge = newForgePipeline(dataDir, reg, led, ex, lc, d.session, cfg.ForgeDailyBudgetMilli)
		log.Printf("forge ready: daily budget %d milli, max 3 gaps/day", cfg.ForgeDailyBudgetMilli)
	}
	log.Printf("self-evolve loop started: daily 04:30 Beijing, rebuild Sunday 04:30")

	// 记忆自动备份（第 15 步）：仅非影子模式且 BACKUP_ENABLED=1 时接线，
	// 默认关闭。备份在 RefineLoop 精炼成功后触发，失败只记账+告警，
	// 不阻塞精炼与主程序。Token 只认键名（BACKUP_TOKEN_KEY），值永不进日志。
	if !shadow && cfg.BackupEnabled {
		br := backup.NewRunner(backup.Config{
			Enabled:     true,
			Repo:        cfg.BackupRepo,
			ServerID:    cfg.BackupServerID,
			TokenEnvKey: cfg.BackupTokenKey,
			EnvFile:     cfg.EnvFile,
			DataDir:     dataDir,
			RepoDir:     cfg.BackupRepoDir,
		}, led)
		wd.SetBackupHook(func(ctx context.Context, date string) error {
			_, err := br.Run(ctx, date)
			return err
		})
		log.Printf("memory backup enabled: repo=%s server=%s token_key=%s", br.RepoName(), br.ServerID(), cfg.BackupTokenKey)
	} else if !shadow {
		log.Printf("memory backup disabled (default off)")
	}

	// 每日账本备份（第 14 步 B-07）：影子模式不启动；
	// 非影子模式下每天北京时间 03:30 热备 ledger.db，留 7 天。
	if !shadow {
		go wd.BackupLoop(refineCtx, dataDir)
		log.Printf("backup loop started: daily 03:30 Beijing")
	}

	// 简介巡检（线 B 第二阶段第 4 棒）：影子模式不启动（巡检写主库与账本）；
	// 非影子模式下简介主库存在时起常驻循环：增量每小时 :17/:47，全量每天
	// 北京时间 02:00–02:15。主库不存在时不启动（走旧 Recall 路径）。
	if !shadow {
		if _, err := os.Stat(filepath.Join(dataDir, "memory", "summaries.db")); err == nil {
			go wd.PatrolLoop(refineCtx, filepath.Join(dataDir, "memory"), filepath.Join(dataDir, "memory", "summaries.db"), dataDir, d.session)
			log.Printf("patrol loop started: incremental :17/:47 hourly, full daily 02:00 Beijing")
		} else {
			log.Printf("patrol loop disabled: summary store not found")
		}
	}

	srv := &http.Server{Addr: cfg.ListenAddr, Handler: gw}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("gateway listen %s: %v", cfg.ListenAddr, err)
		}
	}()
	log.Printf("gateway ready: listening %s (POST /wake), session=%s", cfg.ListenAddr, d.session)
	log.Printf("007-super-agent %s starting... (native direct mode, no sandbox) shadow=%v", Version, shadow)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Printf("shutting down...")
	_ = srv.Close()
	gw.Stop()
	log.Printf("stopped")
}

// newForgePipeline 建锻造流水线（守护进程与 forge-run 子命令共用）：
// 模型走 lc.CallTask（flash），执行走 ex，检索走匿名 GitHub 客户端（绝不带 Token）。
// 监工已下线（2026-11-26 用户令）：审阅门直接放行（NOTE），不调任何模型。
// forgeBudgetMilli 是当日锻造预算上限（毫分），来自 config.ForgeDailyBudgetMilli
// （规格 §10 默认 500；.env FORGE_DAILY_BUDGET_MILLI 或同名环境变量可覆盖）。
func newForgePipeline(dataDir string, reg *registry.Registry, led *ledger.Ledger, ex *executor.Executor, lc *llm.Client, sessionID string, forgeBudgetMilli int64) *forge.Pipeline {
	store := &forge.Store{DataDir: dataDir}
	ret := retrieval.New(http.DefaultClient, filepath.Join(dataDir, "forge", "retrieval", "cache"))
	// 锻造的模型调用（蓝图/分流/测试生成）提示词与输出都比主循环长，
	// 120 秒默认超时在测试机上两次打爆（2026-11-19 缺口 1 两轮）；
	// 锻造单独用 300 秒超时的克隆客户端，主循环不动。
	forgeLC := *lc
	forgeLC.Timeout = 300 * time.Second
	model := func(ctx context.Context, taskID, frozen, tail string) (string, error) {
		reply, _, _, err := forgeLC.CallTask(taskID, frozen, tail)
		return reply, err
	}
	// 监工已下线（2026-11-26 用户令）：审阅门直接放行，不调任何模型、
	// 不花一分钱。forge 的 advisorGate 收到 NOTE 即通过。
	review := func(ctx context.Context, taskID, title, body, workDir string) (string, string, error) {
		return string(executor.VerdictNote), "监工已下线：不再审，直接放行", nil
	}
	return &forge.Pipeline{
		Store: store, Reg: reg, Led: led, Ret: ret,
		Model: model, Review: review, Run: ex,
		SessionID:        sessionID,
		DailyBudgetMilli: forgeBudgetMilli, MaxGapsPerDay: 3,
	}
}

// runForgeCmd 是 `super-agent forge-run` 子命令：手动跑一次锻造后退出。
func runForgeCmd(args []string, dataDirFlag string) {
	fs := flag.NewFlagSet("forge-run", flag.ExitOnError)
	dataDir := fs.String("data-dir", dataDirFlag, "数据目录（覆盖配置）")
	gapID := fs.String("gap", "", "只处理指定缺口")
	retrievalOnly := fs.Bool("retrieval-only", false, "只跑检索（第零层+外部层），不分流/不注册（验收回归用，需配合 --gap）")
	checkTrials := fs.Bool("check-trials", false, "只检查试用期")
	confirmPending := fs.String("confirm-pending", "", "人工确认一个 high 风险挂起工具并注册")
	if err := fs.Parse(args); err != nil {
		log.Fatalf("forge-run: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if *dataDir != "" {
		cfg.DataDir = *dataDir
	}
	dDir := cfg.ResolveDataDir()
	if err := os.MkdirAll(dDir, 0700); err != nil {
		log.Fatalf("mkdir data dir %s: %v", dDir, err)
	}
	dbPath := filepath.Join(dDir, "ledger.db")
	led, err := ledger.Open(dbPath)
	if err != nil {
		log.Fatalf("ledger open %s: %v", dbPath, err)
	}
	defer ledger.Close(led)
	reg, err := registry.Load(dDir)
	if err != nil {
		log.Fatalf("registry load: %v", err)
	}
	apiKey, hasKey := llm.ResolveAPIKey()
	if !hasKey {
		log.Fatalf("llm: 未找到 DEEPSEEK_API_KEY（环境变量或 .env）")
	}
	lc := llm.New(cfg.WorkerModel, apiKey)
	lc.Ledger = led
	sessionID := newSessionID()
	lc.SessionID = sessionID
	ex := &executor.Executor{Ledger: led}
	// 监工已下线：不接 Advisor。
	fp := newForgePipeline(dDir, reg, led, ex, lc, sessionID, cfg.ForgeDailyBudgetMilli)
	ctx := context.Background()
	taskID := fp.ForgeTaskID()
	spentBefore, _ := led.SummarizeExpenseByTask(taskID)
	switch {
	case *confirmPending != "":
		toolID, err := fp.ConfirmPending(ctx, *confirmPending)
		if err != nil {
			log.Fatalf("forge-run confirm-pending: %v", err)
		}
		log.Printf("forge-run: 已确认并注册 %s", toolID)
	case *checkTrials:
		n, err := fp.CheckTrials(ctx)
		if err != nil {
			log.Fatalf("forge-run check-trials: %v", err)
		}
		log.Printf("forge-run: 检查 %d 个试用期", n)
	case *gapID != "":
		g, err := fp.Store.ReadGap(*gapID)
		if err != nil {
			log.Fatalf("forge-run: 读缺口 %s: %v", *gapID, err)
		}
		if *retrievalOnly {
			tr, err := fp.RetrieveOnly(ctx, g)
			if err != nil {
				log.Fatalf("forge-run: 检索缺口 %s: %v", *gapID, err)
			}
			log.Printf("forge-run: 缺口 %s 仅检索完毕 outcome=%s searches=%d core=%d recommended=%s layer0=%v",
				*gapID, tr.Outcome, tr.SearchesUsed, tr.CoreUsed, tr.Recommended, tr.Layer0Hit)
			log.Printf("forge-run: 检索摘要 %s", tr.Summary())
			break
		}
		if err := fp.ProcessGap(ctx, g); err != nil {
			log.Fatalf("forge-run: 处理缺口 %s: %v", *gapID, err)
		}
		log.Printf("forge-run: 缺口 %s 处理完毕", *gapID)
	default:
		n, err := fp.ProcessGaps(ctx)
		if err != nil {
			log.Fatalf("forge-run: 处理缺口队列: %v", err)
		}
		log.Printf("forge-run: 处理 %d 个缺口", n)
		if _, err := fp.CheckTrials(ctx); err != nil {
			log.Fatalf("forge-run: 检查试用期: %v", err)
		}
	}
	spentAfter, _ := led.SummarizeExpenseByTask(taskID)
	log.Printf("forge-run: 本次费用 %d 毫分（当日锻造任务 %s 累计 %d 毫分）",
		spentAfter.CostCNYMilli-spentBefore.CostCNYMilli, taskID, spentAfter.CostCNYMilli)
}

// runBackupOnce 是 `super-agent backup-run` 子命令：手动跑一次记忆备份。
// 与守护进程的接线完全一致（同一份 config、同一个 backup.Runner），
// 区别只是跑完即退出。Token 按 BACKUP_TOKEN_KEY 的键名取：先进程环境变量，
// 空了再读 .env 文件（Runner 内部兜底），启动时不必导出 .env。
func runBackupOnce(args []string, dataDirFlag string) {
	fs := flag.NewFlagSet("backup-run", flag.ExitOnError)
	dateFlag := fs.String("date", "", "备份的 L1 日期 YYYY-MM-DD（默认昨天）")
	_ = fs.Parse(args)
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if dataDirFlag != "" {
		cfg.DataDir = dataDirFlag
	}
	if !cfg.BackupEnabled {
		log.Fatalf("backup-run: 记忆自动备份未开启（BACKUP_ENABLED=1 才跑）")
	}
	dataDir := cfg.ResolveDataDir()
	dbPath := filepath.Join(dataDir, "ledger.db")
	led, err := ledger.Open(dbPath)
	if err != nil {
		log.Fatalf("ledger open %s: %v", dbPath, err)
	}
	defer ledger.Close(led)
	date := *dateFlag
	if date == "" {
		// 与精炼钩子同口径：昨天（北京时间）。
		loc, lerr := time.LoadLocation("Asia/Shanghai")
		if lerr != nil {
			loc = time.FixedZone("CST", 8*3600)
		}
		date = time.Now().In(loc).AddDate(0, 0, -1).Format("2006-01-02")
	}
	br := backup.NewRunner(backup.Config{
		Enabled:     true,
		Repo:        cfg.BackupRepo,
		ServerID:    cfg.BackupServerID,
		TokenEnvKey: cfg.BackupTokenKey,
		EnvFile:     cfg.EnvFile,
		DataDir:     dataDir,
		RepoDir:     cfg.BackupRepoDir,
	}, led)
	res, err := br.Run(context.Background(), date)
	if err != nil {
		log.Fatalf("backup-run: %v", err)
	}
	log.Printf("backup-run done: committed=%v no_change=%v files=%d branch=%s date=%s",
		res.Committed, res.NoChange, res.Files, res.Branch, date)
}

// runPatrolOnce 是 `super-agent patrol-run` 子命令：手动跑一次简介巡检。
// 与守护进程的巡检循环共用 PatrolOnce（同一份主库、同一个账本），
// 区别只是跑完即退出。主库不存在时报错退出（不建空库）。
func runPatrolOnce(args []string, dataDirFlag string) {
	fs := flag.NewFlagSet("patrol-run", flag.ExitOnError)
	fullFlag := fs.Bool("full", false, "全量 SHA-256 校准（默认增量）")
	_ = fs.Parse(args)
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if dataDirFlag != "" {
		cfg.DataDir = dataDirFlag
	}
	dataDir := cfg.ResolveDataDir()
	dbPath := filepath.Join(dataDir, "ledger.db")
	led, err := ledger.Open(dbPath)
	if err != nil {
		log.Fatalf("ledger open %s: %v", dbPath, err)
	}
	defer ledger.Close(led)
	memRoot := filepath.Join(dataDir, "memory")
	storePath := filepath.Join(memRoot, "summaries.db")
	res, err := watchdog.PatrolOnce(memRoot, storePath, *fullFlag, led, "", "patrol-run")
	if err != nil {
		log.Fatalf("patrol-run: %v", err)
	}
	kind := "增量"
	if *fullFlag {
		kind = "全量"
	}
	log.Printf("patrol-run done（%s）：%s", kind, res.Summary())
	for _, d := range res.Details {
		log.Printf("patrol-run: %s", d)
	}
}

// runIndexMemory 是 `super-agent index-memory` 子命令：重建 data/memory/
// 下的语义向量索引（vectors.db），只对新增/改动过的文档重新向量化。
// dataDir 为空时用配置的默认数据目录。
func runIndexMemory(dataDirFlag string) {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if dataDirFlag != "" {
		cfg.DataDir = dataDirFlag
	}
	dataDir := cfg.ResolveDataDir()
	memRoot := filepath.Join(dataDir, "memory")
	sem, err := memory.OpenSemantic(
		filepath.Join(memRoot, "vectors.db"),
		memory.SemanticDefaultURL, memory.SemanticDefaultModel)
	if err != nil {
		log.Fatalf("semantic open: %v", err)
	}
	defer sem.Close()
	start := time.Now()
	n, err := sem.Rebuild(memRoot)
	if err != nil {
		log.Fatalf("index-memory: %v", err)
	}
	log.Printf("index-memory done: 本次向量化 %d 篇，库内共 %d 篇，模型 %s，用时 %v",
		n, sem.Count(), sem.Model(), time.Since(start).Round(time.Second))
}
