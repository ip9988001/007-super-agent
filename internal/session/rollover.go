// Package session 的 P2-b 部分：Flash 摘要压缩续接（照三号那套）。
//
// 三号规则（二号调查证实，用户 2026-11-25 06:11 确认“就按照这个来”）：
// - 翻新阈值 200000 tokens。触发：上一轮结束水位 >200K → 下一轮
// 开工前翻，运行中途不翻；只在派单前检查（仿三号 maybeRollover）。
// - 摘要 800–5000 字（原话见 summaryPromptTemplate），硬顶 6000 字，
// 超长硬截断。摘要 5 段，禁寒暄/过程叙述/表格/复述已知。
// - 摘要模型 = 对话同一个 deepseek-flash，不换模型。
// - 摘要存进新会话 plan_state.json 的 carryover 字段（不落独立文件）；
// 新会话开头注入摘要（前缀“【上一段会话的接续摘要】”）；
// carryover 末尾附“【上一段会话完整原文】<旧会话文件绝对路径> 需要时可直接读回”。
// - 新会话换新编号（UUID），token 清零，同模型同配置。
// - 旧会话文件原地保留不删；prev 只留最近 8 条；7 天未活动的已终结
// 会话挪归档文件夹（还是留着，不删）。
// - 降级：模型两次写不出摘要 → 从旧会话最近 6 轮原文顶上。
//
// 与三号唯一不同的地方：文件分开存（plan_state.json + front.jsonl +
// steps/step-<n>.jsonl），因 007 有前置+执行两个角色、三号只有一个，
// 混一文件会串；数字与做法照三号。
//
// 水位口径：PlanState.TokensUsed 是本会话内单次模型调用最大的
// prompt_tokens（峰值上下文大小），不是累计和——每次调用的 prompt
// 已含全部历史（冻结 + 历史 + 尾巴），累加会重复计算；三号的 used
// 也是当前会话上下文大小，所以水位取峰值，与三号同口径。
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"007-super-agent/internal/ledger"
	"007-super-agent/internal/llm"
)

// 翻新阈值与摘要字数（照三号，二号调查证实）。
const (
	// DefaultRolloverThreshold 是翻新阈值 tokens（照三号 200000）。
	DefaultRolloverThreshold = 200000
	// SummaryMinRunes 是摘要最少 800 字（照三号原话）。
	SummaryMinRunes = 800
	// SummaryMaxRunes 是摘要最多 5000 字（软，照三号原话）。
	SummaryMaxRunes = 5000
	// CarryoverMaxRunes 是 carryover 硬顶 6000 字（照三号 CARRYOVER_MAX_CHARS，超长硬截断）。
	CarryoverMaxRunes = 6000
	// SummaryTimeout 是摘要调用超时（照三号 runPiRaw 420s）。
	SummaryTimeout = 420 * time.Second
	// MaxSummaryAttempts 是摘要最多试 2 次，失败走降级（照三号）。
	MaxSummaryAttempts = 2
	// TailDigestRounds 是降级抽的最近轮数（照三号：最近 6 轮原文）。
	TailDigestRounds = 6
	// MaxPrevSessions 是 prev 只留最近 8 条（照三号 session.json 的 prev）。
	MaxPrevSessions = 8
	// DefaultArchiveAfterDays 是归档阈值：7 天未活动挪归档文件夹。
	DefaultArchiveAfterDays = 7
	// MaxSummaryMaterialRunes 是拼给摘要模型的素材上限（防摘要调用费用失控）。
	MaxSummaryMaterialRunes = 20000
)

// RolloverThresholdEnv 是覆盖翻新阈值的环境变量（测试与调参用）：
// 设为正整数即生效；未设或非法时用默认 200000。沙箱用低阈值触发
// 真实翻新（不用真跑 20 万 token）。
const RolloverThresholdEnv = "SESSION_ROLLOVER_TOKENS"

// SummaryFailEnv 是强制摘要失败的环境变量（仅测试用）：
// 设为 "1" 时 MaybeRollover 跳过模型调用、直接走 tailDigest 降级，
// 用于验证降级路径。生产不要设。
const SummaryFailEnv = "SESSION_ROLLOVER_SUMMARY_FAIL"

// RolloverConfig 是翻新配置（阈值可调，默认照三号）。
type RolloverConfig struct {
	ThresholdTokens int64         // 翻新阈值；<=0 用默认 200000
	SummaryTimeout  time.Duration // 摘要超时；<=0 用默认 420s
	ArchiveAfter    time.Duration // 归档阈值；<=0 用默认 7 天
}

// DefaultRolloverConfig 返回默认翻新配置（阈值 200000，照三号；
// 环境变量 SESSION_ROLLOVER_TOKENS 可覆盖阈值，测试用低阈值触发翻新）。
func DefaultRolloverConfig() RolloverConfig {
	cfg := RolloverConfig{
		ThresholdTokens: DefaultRolloverThreshold,
		SummaryTimeout:  SummaryTimeout,
		ArchiveAfter:    DefaultArchiveAfterDays * 24 * time.Hour,
	}
	if v := strings.TrimSpace(os.Getenv(RolloverThresholdEnv)); v != "" {
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			cfg.ThresholdTokens = n
		}
	}
	return cfg
}

// PrevSession 是一条翻新链记录（旧会话）：只记最近 8 条。
type PrevSession struct {
	SessionID string `json:"session_id"`
	PlanID    string `json:"plan_id"`
	RolledAt  string `json:"rolled_at"` // RFC3339
	PlanPath  string `json:"plan_path"` // 旧会话 plan_state.json 绝对路径
}

// Summarizer 是翻新摘要的模型调用（P2-b）：调用方注入一个调
// deepseek-flash 的实现（会话同一个模型，不换模型）。
// prompt 是已拼好的摘要提示词（含三号原话与 5 段结构要求）。
// 返回模型写的摘要正文；失败返回错误（调用方按降级策略处理）。
// taskID 用于费用归属记账。
type Summarizer func(ctx context.Context, taskID, prompt string) (string, error)

// summaryPromptTemplate 是翻新摘要的提示词（照三号原话）：
// 800–5000 字、5 段结构、禁寒暄/过程/表格。%s 是会话素材。
const summaryPromptTemplate = `你是会话压缩器。把下面这段会话/计划内容压缩成一份接续摘要，供下一个新会话继续干活（新会话看不到旧会话全文，只能看到这份摘要，所以关键信息一个都不能丢）。

按实际信息量决定长度：不得少于 800 字，不得多于 5000 字。
关键细节多就写到 5000 字为止；信息不足时宁可只写 800 字也不许凑字数。

摘要分 5 段写：
① 当前目标及为什么
② 做到哪步 + 证据（路径/tag/commit）
③ 关键决定与技术细节（路径/端口/命令/配置值/数字/阈值）
④ 被推翻的结论与踩过的坑
⑤ 下一步待办
禁止：寒暄、过程叙述、表格、复述已知。

下面是本会话的内容：
%s`

// buildSummaryPrompt 把素材拼进摘要提示词。
func buildSummaryPrompt(material string) string {
	return fmt.Sprintf(summaryPromptTemplate, material)
}

// RecordTokens 更新会话水位（P2-b）：把本次模型调用的 prompt_tokens
// 记进计划的 TokensUsed，取最大值（峰值上下文大小）。
// 注意：不是累计和——每次调用的 prompt 已含全部历史，累加会重复计算；
// 三号的 used 也是当前会话上下文大小，所以水位取峰值，与三号同口径。
// 计划不存在时跳过（前置首轮模型调用发生在首个派单建计划之前，
// 那几轮 token 小，峰值以后轮为准）。失败返回错误，调用方只记日志、
// 不阻塞任务。
func (s *Store) RecordTokens(sessionID string, promptTokens int64) error {
	if promptTokens <= 0 {
		return nil
	}
	if err := ValidateSessionID(sessionID); err != nil {
		return err
	}
	l := s.lockFor(sessionID)
	l.Lock()
	defer l.Unlock()
	ps, err := s.LoadPlan(sessionID)
	if err != nil {
		return err
	}
	if ps == nil {
		return nil
	}
	if promptTokens <= ps.TokensUsed {
		return nil
	}
	ps.TokensUsed = promptTokens
	return s.savePlanLocked(ps)
}

// MaybeRollover 在派单前检查会话水位（仿三号 maybeRollover）：
// - 无计划 / 计划已终结 / 水位 <= 阈值 → 不翻，返回 ("", false, nil)。
// - 水位 > 阈值 → 先翻新再开工：调 Summarizer 写摘要（800–5000 字，
// 硬顶 6000），把摘要存进新会话的 carryover，旧会话原地保留，
// 新会话（新 UUID）继承计划表继续跑，token 清零。
// 返回 (newSessionID, true, nil)。
// 摘要失败两次 → 降级：用旧会话最近 6 轮原文当 carryover（不调模型）。
// sum 为 nil 时直接走降级（无模型可用时仍能翻新）。
// 翻新失败返回错误，调用方记日志、继续用旧会话派单（翻新是安全阀，
// 失败绝不能杀任务）。
func (s *Store) MaybeRollover(ctx context.Context, sessionID, taskID, goal string, cfg RolloverConfig, sum Summarizer) (string, bool, error) {
	if err := ValidateSessionID(sessionID); err != nil {
		return "", false, err
	}
	// H-6：先沿翻新链追到最新活跃会话（拿旧号进来时翻的是新会话，
	// 不能对已终结的旧会话重复翻新）。
	resolved, rerr := s.ResolveActiveSession(sessionID)
	if rerr != nil || resolved == "" {
		resolved = sessionID
	}
	sessionID = resolved
	threshold := cfg.ThresholdTokens
	if threshold <= 0 {
		threshold = DefaultRolloverThreshold
	}
	// 读计划、判水位（持锁）；素材在解锁后拼（M-4：素材要读步历史
	// JSONL，读盘不在锁里做）。
	l := s.lockFor(sessionID)
	l.Lock()
	ps, err := s.LoadPlan(sessionID)
	if err != nil {
		l.Unlock()
		return "", false, err
	}
	if ps == nil || ps.Status.IsTerminal() || ps.TokensUsed <= threshold {
		l.Unlock()
		return "", false, nil
	}
	oldPlanID := ps.PlanID
	oldTokens := ps.TokensUsed
	oldPath, _ := s.PlanPath(sessionID)
	oldDir, _ := s.SessionDir(sessionID)
	l.Unlock()
	material := s.buildSummaryMaterial(ps, goal)

	// 生成摘要（不持锁）：最多试 2 次；两次失败或无模型 → tailDigest 降级。
	summary := ""
	var sumErr error
	if sum != nil && os.Getenv(SummaryFailEnv) != "1" {
		timeout := cfg.SummaryTimeout
		if timeout <= 0 {
			timeout = SummaryTimeout
		}
		for attempt := 1; attempt <= MaxSummaryAttempts; attempt++ {
			sctx, cancel := context.WithTimeout(ctx, timeout)
			out, err := sum(sctx, taskID, buildSummaryPrompt(material))
			cancel()
			if err == nil && strings.TrimSpace(out) != "" {
				summary = out
				break
			}
			sumErr = err
			log.Printf("session %s 翻新摘要第 %d 次失败: %v", sessionID, attempt, err)
		}
	}
	fallback := false
	if summary == "" {
		// 降级：最近 6 轮原文（照三号 tailDigest）。
		summary = s.tailDigest(sessionID, taskID)
		fallback = true
		log.Printf("session %s 翻新摘要失败（%v），走 tailDigest 降级", sessionID, sumErr)
	}
	// 硬顶 6000 字（照三号 CARRYOVER_MAX_CHARS）。
	summary = truncateRunes(summary, CarryoverMaxRunes)
	// L-3：末尾除了 plan_state.json 路径，再附旧会话目录（含 steps/
	// 原始对话），模型真要读回全文时不用猜目录在哪。
	carryover := "【上一段会话的接续摘要】\n" + summary + "\n【上一段会话完整原文】" + oldPath + "（会话目录 " + oldDir + "，内含 steps/ 原始对话）需要时可直接读回"

	// 建新会话（持旧锁）：新计划继承步表，carryover 存入，token 清零，
	// prev 记旧会话（只留最近 8 条）；先存新计划、再把旧计划标
	// ROLLED_OVER（新计划先落盘，崩溃时旧计划仍可续跑，不丢进度）。
	newID := newSessionID()
	l.Lock()
	defer l.Unlock()
	cur, err := s.LoadPlan(sessionID)
	if err != nil {
		return "", false, err
	}
	if cur == nil || cur.Status.IsTerminal() || cur.TokensUsed <= threshold {
		// 锁外等待期间计划变了（已终结/水位回落），放弃翻新。
		return "", false, nil
	}
	steps := make([]PlanStep, len(cur.Steps))
	copy(steps, cur.Steps)
	nps := &PlanState{
		PlanID: newPlanID(), SessionID: newID, TaskID: cur.TaskID,
		Goal:   cur.Goal,
		Status: PlanRunning, Phase: PhaseRunning,
		// H-6：翻新保留原计划的 CreatedTS，不重置——产物新鲜度的基线
		// （factBaseline）用它，翻新前已完成步骤生成的产物在翻新后
		// 复检或引用时仍算新鲜，不会被误判过期。
		CreatedTS: cur.CreatedTS,
		UpdatedTS: time.Now().UTC().Format(time.RFC3339),
		Steps:     steps, CurrentStep: cur.CurrentStep, TotalSteps: cur.TotalSteps,
		Failures: cur.Failures, Replans: cur.Replans,
		Carryover: carryover, TokensUsed: 0,
		RolloverCount: cur.RolloverCount + 1,
		Prev: appendPrev(cur.Prev, PrevSession{
			SessionID: sessionID, PlanID: oldPlanID,
			RolledAt: time.Now().UTC().Format(time.RFC3339), PlanPath: oldPath,
		}),
		PrevPlanID: cur.PrevPlanID, PrevSummaries: cur.PrevSummaries,
	}
	// 新会话 ID 是刚生成的、无人知晓，不存在锁竞争；直接用持锁版写盘。
	if err := s.savePlanLocked(nps); err != nil {
		return "", false, err
	}
	// M-1：把旧会话的步历史文件（steps/step-*.jsonl）拷到新会话——
	// 翻新后执行层按新会话号读历史，拷过来才能读回、resumed 仍为 true，
	// 否则翻新后同一 步续跑会从头开始（方向 b 的 PriorStepCalls 靠
	// 账本不受影响，但 resumed=false 会丢掉硬底线①豁免）。
	// 拷失败只记日志、不阻塞翻新（翻新本身已完成）。
	if copyErr := s.copyStepHistories(sessionID, newID); copyErr != nil {
		log.Printf("session %s 翻新：步历史复制到新会话 %s 失败（不阻塞）: %v", sessionID, newID, copyErr)
	}
	// H-6：旧计划标 ROLLED_OVER 时写前进指针 RolledTo=新会话号——
	// 崩溃重启拿旧号恢复时，OpenSession/LoadPlan 沿这条链追到新会话。
	cur.Status = PlanRolledOver
	cur.RolledTo = newID
	if err := s.savePlanLocked(cur); err != nil {
		// 新计划已落盘，旧计划标终态失败：记事件、不回滚（新会话已可用，
		// 旧会话下次打开时 OpenSession 会按未终结处理，可续跑，不丢单）。
		log.Printf("session %s 旧计划标 ROLLED_OVER 失败: %v（新会话 %s 已可用）", sessionID, err, newID)
	}
	suffix := ""
	if fallback {
		suffix = "（tailDigest 降级）"
	}
	s.event(sessionID, taskID, ledger.DispatchEventSessionRollover,
		fmt.Sprintf("会话翻新：水位 %d 超阈值 %d，%s → %s，摘要 %d 字%s，旧会话原地保留",
			oldTokens, threshold, sessionID, newID, utf8.RuneCountInString(summary), suffix))
	log.Printf("session %s 翻新：水位 %d 超阈值 %d → 新会话 %s（摘要 %d 字）", sessionID, oldTokens, threshold, newID, utf8.RuneCountInString(summary))
	return newID, true, nil
}

// appendPrev 把旧会话记进 prev，只留最近 8 条（照三号）。
func appendPrev(prev []PrevSession, p PrevSession) []PrevSession {
	out := append(prev, p)
	if len(out) > MaxPrevSessions {
		out = out[len(out)-MaxPrevSessions:]
	}
	return out
}

// copyStepHistories 把旧会话 steps/ 下的 step-<n>.jsonl 拷到新会话
// （M-1：翻新后 resumed 仍为 true 的保障）。新会话 steps/ 目录不
// 存在时先建；单个文件拷失败记日志、继续拷其余的；旧会话无 steps/
// 目录时直接返回 nil。归档文件（.attempt-1.jsonl 这类旧尝试快照）
// 不拷——新会话不需要。
func (s *Store) copyStepHistories(oldID, newID string) error {
	oldDir, err := s.SessionDir(oldID)
	if err != nil {
		return err
	}
	newDir, err := s.SessionDir(newID)
	if err != nil {
		return err
	}
	oldSteps := filepath.Join(oldDir, "steps")
	entries, err := os.ReadDir(oldSteps)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 旧会话无步历史，无事可拷
		}
		return err
	}
	newSteps := filepath.Join(newDir, "steps")
	if err := os.MkdirAll(newSteps, 0o755); err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "step-") || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		if strings.Contains(e.Name(), ".attempt-") {
			continue
		}
		src := filepath.Join(oldSteps, e.Name())
		dst := filepath.Join(newSteps, e.Name())
		b, rerr := os.ReadFile(src)
		if rerr != nil {
			log.Printf("session 翻新：步历史 %s 读失败: %v", e.Name(), rerr)
			continue
		}
		if werr := os.WriteFile(dst, b, 0o600); werr != nil {
			log.Printf("session 翻新：步历史 %s 写新会话失败: %v", e.Name(), werr)
		}
	}
	return nil
}

// newSessionID 生成一个新会话号（UUID v4，36 字，符合 session_id 白名单）。
func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// buildSummaryMaterial 拼翻新摘要的素材（计划表 + 步摘要 + 失败 +
// 旧摘要 + 已有的接续摘要）。素材总量截到 20000 字，防摘要调用费用失控。
// 只读内存里的计划，不碰历史文件（不持锁、不读盘，模型调用前拼）。
func (s *Store) buildSummaryMaterial(ps *PlanState, goal string) string {
	var b strings.Builder
	if strings.TrimSpace(goal) != "" {
		fmt.Fprintf(&b, "当前目标：%s\n", goal)
	}
	fmt.Fprintf(&b, "任务：%s；计划：%s；进度：第 %d 步 / 共 %d 步\n", ps.TaskID, ps.PlanID, ps.CurrentStep, ps.TotalSteps)
	for _, st := range ps.Steps {
		line := fmt.Sprintf("第 %d 步（%s，%s）：%s", st.StepNo, st.ToolID, st.Status, st.Summary)
		if len(st.Artifacts) > 0 {
			line += "；产物：" + strings.Join(st.Artifacts, "、")
		}
		if len(st.Inputs) > 0 {
			line += "；参数：" + strings.Join(st.Inputs, " ")
		}
		b.WriteString(line + "\n")
	}
	if len(ps.Failures) > 0 {
		b.WriteString("失败记录：\n")
		for _, f := range ps.Failures {
			b.WriteString("- " + f + "\n")
		}
	}
	// M-4：最近活跃步的 JSONL 尾部也拼进来（只读步摘要时，单步内大量
	// 调研/踩坑的细节会丢，模型写不出有血有肉的摘要）。
	if tails := s.recentStepTails(ps); tails != "" {
		b.WriteString(tails)
	}
	if len(ps.PrevSummaries) > 0 {
		b.WriteString("之前会话的摘要：\n")
		for _, p := range ps.PrevSummaries {
			b.WriteString("- " + p + "\n")
		}
	}
	if strings.TrimSpace(ps.Carryover) != "" {
		b.WriteString("上一段接续摘要：\n" + ps.Carryover + "\n")
	}
	out := b.String()
	if utf8.RuneCountInString(out) > MaxSummaryMaterialRunes {
		out = truncateRunes(out, MaxSummaryMaterialRunes)
	}
	return out
}

// recentStepTails 取最近 2 个有历史的步的末尾消息（M-4，翻新素材用）：
// 从当前步往前找，每步取末尾 8 条，assistant 截 500 字、tool 截 200 字。
// 读盘失败或无历史时返回 ""（不阻塞翻新）。
func (s *Store) recentStepTails(ps *PlanState) string {
	var b strings.Builder
	found := 0
	for i := len(ps.Steps) - 1; i >= 0 && found < 2; i-- {
		stepNo := ps.Steps[i].StepNo
		msgs, _, err := s.LoadMessages(ps.SessionID, stepNo, ps.TaskID)
		if err != nil || len(msgs) == 0 {
			continue
		}
		found++
		if len(msgs) > 8 {
			msgs = msgs[len(msgs)-8:]
		}
		fmt.Fprintf(&b, "第 %d 步最近对话（末尾 %d 条）：\n", stepNo, len(msgs))
		for _, m := range msgs {
			b.WriteString(formatHistoryLine(m) + "\n")
		}
	}
	return b.String()
}

// formatHistoryLine 把一条历史消息写成一行摘要素材（M-4）：
// assistant 的工具调用只留函数名与截断参数，不把整段 arguments 贴出来。
func formatHistoryLine(m llm.Message) string {
	c := truncateRunes(m.Content, 500)
	if m.Role == "tool" {
		c = truncateRunes(m.Content, 200)
	}
	line := fmt.Sprintf("[%s] %s", m.Role, c)
	if len(m.ToolCalls) > 0 {
		var calls []string
		for _, tc := range m.ToolCalls {
			args := truncateRunes(tc.Function.Arguments, 200)
			calls = append(calls, tc.Function.Name+"("+args+")")
		}
		line += "；调用：" + strings.Join(calls, "、")
	}
	return line
}

// tailDigest 是翻新摘要的降级（照三号）：模型两次写不出摘要时，
// 从旧会话最近一步历史里抽最近 6 轮原文顶上（不调模型）。
// 无历史时退回计划步摘要。返回的文本已截到硬顶 6000 字以内。
func (s *Store) tailDigest(sessionID, taskID string) string {
	ps, err := s.LoadPlan(sessionID)
	if err != nil || ps == nil {
		return "（无会话记录可抽）"
	}
	var msgs []llm.Message
	for i := len(ps.Steps) - 1; i >= 0; i-- {
		m, _, err := s.LoadMessages(sessionID, ps.Steps[i].StepNo, taskID)
		if err != nil || len(m) == 0 {
			continue
		}
		msgs = m
		break
	}
	if len(msgs) == 0 {
		// 无历史：退回计划步摘要。
		var lines []string
		for _, st := range ps.Steps {
			lines = append(lines, stepSummaryLine(st))
		}
		if len(lines) == 0 {
			return "（会话无内容）"
		}
		return truncateRunes("最近 6 轮原文（无历史，退回步摘要）：\n- "+strings.Join(lines, "\n- "), CarryoverMaxRunes)
	}
	// M-4：按 Role=="assistant" 计数倒推 6 轮（原来按 6 条消息截，带多
	// 工具调用时只有 1–2 轮）；一轮 = 一条 assistant 消息及其后的 tool 结果。
	start := 0
	rounds := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			rounds++
			if rounds >= TailDigestRounds {
				start = i
				break
			}
		}
	}
	msgs = msgs[start:]
	var b strings.Builder
	b.WriteString("最近 6 轮原文（摘要模型两次失败，降级顶上）：\n")
	for _, m := range msgs {
		b.WriteString(formatHistoryLine(m) + "\n")
	}
	return truncateRunes(b.String(), CarryoverMaxRunes)
}

// ArchiveIdleSessions 把超过阈值未活动的已终结会话挪到归档文件夹
// （dataDir/sessions-archive/<session_id>/）：文件原地保留、不删，
// 只是换个地方放。只归档已终结（DONE/FAILED/CANCELLED/ROLLED_OVER）
// 且 UpdatedTS 超过阈值的会话；未终结的（还在跑）绝不动。
// 返回归档的会话号。单会话出错记日志、继续归档下一个，不中断。
// 调用方在启动时调一次（best-effort，失败不阻塞启动）。
func (s *Store) ArchiveIdleSessions(olderThan time.Duration) ([]string, error) {
	if olderThan <= 0 {
		olderThan = DefaultArchiveAfterDays * 24 * time.Hour
	}
	root := filepath.Join(s.dataDir, "sessions")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("session: 读 sessions 目录失败: %w", err)
	}
	cutoff := time.Now().Add(-olderThan)
	var archived []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		if err := ValidateSessionID(id); err != nil {
			continue
		}
		// L-2：移动目录前先取该会话的锁（防归档与新请求并发冲突：
		// 锁住期间新请求进不来，不会读到一半被挪走的目录）。
		l := s.lockFor(id)
		l.Lock()
		ps, err := s.loadPlanRaw(id)
		if err != nil || ps == nil {
			l.Unlock()
			continue
		}
		if !ps.Status.IsTerminal() {
			l.Unlock()
			continue
		}
		ts, err := time.Parse(time.RFC3339, ps.UpdatedTS)
		if err != nil || !ts.Before(cutoff) {
			l.Unlock()
			continue
		}
		src := filepath.Join(root, id)
		dst := filepath.Join(s.dataDir, "sessions-archive", id)
		if _, err := os.Stat(dst); err == nil {
			log.Printf("session %s 归档目标已存在，跳过", id)
			l.Unlock()
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			log.Printf("session %s 建归档目录失败: %v", id, err)
			l.Unlock()
			continue
		}
		if err := os.Rename(src, dst); err != nil {
			log.Printf("session %s 归档移动失败: %v", id, err)
			l.Unlock()
			continue
		}
		l.Unlock()
		archived = append(archived, id)
		s.event(id, "", ledger.DispatchEventSessionArchive,
			fmt.Sprintf("会话 %s 已终结（%s）且 %s 未活动，挪归档文件夹", id, ps.Status, olderThan))
		log.Printf("session %s 已归档（终结 %s，%s 未活动）", id, ps.Status, olderThan)
	}
	return archived, nil
}
