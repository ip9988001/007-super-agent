package selfevolve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"007-super-agent/internal/forge"
	"007-super-agent/internal/ledger"
	"007-super-agent/internal/memlock"
	"007-super-agent/internal/memory"
	"007-super-agent/internal/registry"
)

// 自查与重建的硬口径（第 20 步任务书）。
const (
	// ReviewThreshold：同一问题签名 7 天内出现这么多次才产生提议；
	// 不足只记教训、不提议。
	ReviewThreshold = 3
	// ReviewWindowDays：信号聚合窗口（天，含今天）。
	ReviewWindowDays = 7
	// ReviewWindowHour/Min：每日自查窗口（北京时间 04:30–04:45，
	// 精炼 04:00 之后跑）。
	ReviewWindowHour     = 4
	ReviewWindowStartMin = 30
	ReviewWindowEndMin   = 45
	// RebuildWeekday：每周重建是周日 04:30–04:45（与自查同一窗口）。
	RebuildWeekday = time.Sunday
	// CheckInterval：常驻循环的轮询间隔（照抄 RefineLoop/BackupLoop）。
	CheckInterval = 60 * time.Second
	// SelfEvolveLongDuration 是自查/锻造/重建耗时告警线（20 分钟）：
	// 与 04:00 精炼只隔 30 分钟，超 20 分钟记一条告警。
	SelfEvolveLongDuration = 20 * time.Minute
	// SummaryModel：当日汇总费用行的 model 字段标记——汇总行的
	// token 与费用与调用行重复，汇总时必须排除该标记（见
	// ledger.SummarizeExpenseByTask），否则当日费用会 double count。
	SummaryModel = "selfevolve-summary"
)

// ReviewFunc 是一次自查点评的模型调用（调用方接 llm.Client.ReviewSelfEvolve）。
// 返回模型原文（JSON 对象），费用由调用方按 taskID 记账；
// 测试里用桩，不真调 API。
type ReviewFunc func(ctx context.Context, taskID, signalsText string) (string, error)

// Loop 是自进化闭环的常驻循环：每天自查 + 每周重建。
// shadow 为真时只记“本应自查”，不执行、不写账本、不写 Wiki。
type Loop struct {
	led       *ledger.Ledger
	reg       *registry.Registry
	mem       *memory.Store
	dataDir   string
	sessionID string
	review    ReviewFunc
	Forge     *forge.Pipeline // 第 23 步锻造流水线：nil 时不跑缺口处理
	shadow    bool

	mu        sync.Mutex
	shadowLog map[string]bool // 影子模式下已记过“本应自查”的日期（防每 60 秒刷屏）
}

// New 建一个自进化循环。review 为 nil 时非影子模式的自查会跳过并记日志
// （主程序里非影子模式一定有模型客户端，不会走到这一步）。
func New(led *ledger.Ledger, reg *registry.Registry, mem *memory.Store, dataDir, sessionID string, review ReviewFunc, shadow bool) *Loop {
	return &Loop{
		led: led, reg: reg, mem: mem, dataDir: dataDir,
		sessionID: sessionID, review: review, shadow: shadow,
		shadowLog: map[string]bool{},
	}
}

// beijingNow 取北京时间；系统无 Asia/Shanghai 时区数据时用固定 +8 区兜底
// （与 watchdog 包同口径）。
func beijingNow() time.Time {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		loc = time.FixedZone("CST", 8*3600)
	}
	return time.Now().In(loc)
}

// selfEvolveTaskID 是自查在账本里的任务号。
func selfEvolveTaskID(date string) string { return "selfevolve-" + date }

// rebuildTaskID 是重建在账本里的任务号。
func rebuildTaskID(date string) string { return "selfevolve-rebuild-" + date }

// reviewMarkerFile 是“当天已跑过自查”的标记文件（放数据目录，内容为日期）。
func reviewMarkerFile(dataDir string) string {
	return filepath.Join(dataDir, "selfevolve_last")
}

// rebuildMarkerFile 是“当天已跑过重建”的标记文件（放数据目录，内容为日期）。
// forgeMarkerFile 是"当天已跑过缺口处理"的标记文件（放数据目录，内容为日期）。
func forgeMarkerFile(dataDir string) string {
	return filepath.Join(dataDir, "forge_last")
}

func rebuildMarkerFile(dataDir string) string {
	return filepath.Join(dataDir, "selfevolve_rebuild_last")
}

// shouldReview 判断现在是否该跑自查：北京时间 04:30–04:45，
// 且标记文件里的日期不是今天（当天未跑过）。
func shouldReview(now time.Time, marker string) bool {
	if now.Hour() != ReviewWindowHour || now.Minute() < ReviewWindowStartMin || now.Minute() >= ReviewWindowEndMin {
		return false
	}
	return marker != now.Format("2006-01-02")
}

// shouldRebuild 判断现在是否该跑重建：周日北京时间 04:30–04:45，
// 且标记文件里的日期不是今天（当天未跑过）。
func shouldRebuild(now time.Time, marker string) bool {
	if now.Weekday() != RebuildWeekday {
		return false
	}
	return shouldReview(now, marker)
}

// Run 常驻轻量循环：每 60 秒检查一次，自查窗口到了跑 ReviewDaily，
// 周日同一窗口再跑 RebuildWeekly；跑完才写当天标记；ctx 取消时退出。
// 影子模式下窗口到了只记一行“本应自查”日志，不执行、不写账本。
func (l *Loop) Run(ctx context.Context) {
	tick := time.NewTicker(CheckInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		l.maybeRun(beijingNow())
	}
}

// maybeRun 是 Run 的一次窗口检查（抽出来供测试直接调，
// 用构造的时间测窗口逻辑，不用等真实时钟）。
// 自查/锻造/重建任一窗口到了，先抢记忆写锁（与 04:00 精炼共用，
// 见 internal/memlock）：抢不到说明精炼正在写，本次跳过、标记不写，
// 下个 60 秒 tick 再试；影子模式不写记忆，不抢锁。耗时超 20 分钟
// 记一条告警（与精炼只隔 30 分钟）。
func (l *Loop) maybeRun(now time.Time) {
	date := now.Format("2006-01-02")
	reviewDue := shouldReview(now, readMarker(reviewMarkerFile(l.dataDir)))
	forgeDue := shouldReview(now, readMarker(forgeMarkerFile(l.dataDir)))
	rebuildDue := shouldRebuild(now, readMarker(rebuildMarkerFile(l.dataDir)))
	if !reviewDue && !forgeDue && !rebuildDue {
		return
	}
	if l.shadow {
		if reviewDue {
			l.logShadowOnce("review", date)
		}
		if forgeDue {
			l.logShadowOnce("forge", date)
		}
		if rebuildDue {
			l.logShadowOnce("rebuild", date)
		}
		return
	}
	start := time.Now()
	lk, err := memlock.TryLock(l.dataDir)
	if err != nil {
		if errors.Is(err, memlock.ErrLocked) {
			log.Printf("self-evolve loop: 记忆写锁被占用（精炼正在写），本次自查/锻造/重建跳过，下个 60 秒再试")
		} else {
			log.Printf("self-evolve loop: 取记忆写锁失败: %v（本次跳过）", err)
		}
		return
	}
	defer lk.Unlock()
	if reviewDue {
		if _, err := l.ReviewDaily(context.Background(), date); err != nil {
			log.Printf("self-evolve loop: 自查失败: %v", err)
		} else if err := writeMarker(reviewMarkerFile(l.dataDir), date); err != nil {
			log.Printf("self-evolve loop: 写自查标记失败: %v（下个 60 秒会重跑一次）", err)
		}
	}
	if forgeDue {
		if err := l.runForge(now, date); err != nil {
			log.Printf("self-evolve loop: 锻造处理失败: %v", err)
		} else if err := writeMarker(forgeMarkerFile(l.dataDir), date); err != nil {
			log.Printf("self-evolve loop: 写锻造标记失败: %v（下个 60 秒会重跑一次）", err)
		}
	}
	if rebuildDue {
		if _, err := l.RebuildWeekly(context.Background(), date); err != nil {
			log.Printf("self-evolve loop: 重建失败: %v", err)
		} else if err := writeMarker(rebuildMarkerFile(l.dataDir), date); err != nil {
			log.Printf("self-evolve loop: 写重建标记失败: %v（下个 60 秒会重跑一次）", err)
		}
	}
	if dur := time.Since(start); dur > SelfEvolveLongDuration {
		log.Printf("self-evolve loop: 本次自查/锻造/重建耗时 %s，超 %s 告警", dur, SelfEvolveLongDuration)
	}
}

// logShadowOnce 在影子模式下记一行“本应自查”日志；同一日期同一事项只记一次。
func (l *Loop) logShadowOnce(kind, date string) {
	key := kind + ":" + date
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.shadowLog[key] {
		return
	}
	l.shadowLog[key] = true
	log.Printf("self-evolve shadow: 本应跑%s（%s），影子模式不执行、不写账本", kind, date)
}

func readMarker(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func writeMarker(path, date string) error {
	return os.WriteFile(path, []byte(date+"\n"), 0o600)
}

// ReviewResult 是一次自查的结果。
type ReviewResult struct {
	Date      string // 自查日期（北京时间 YYYY-MM-DD）
	Signals   int    // 信号总数
	Lessons   int    // 写进 Wiki 的教训条数
	Proposals int    // 新建提议数
	Applied   int    // 自动应用数（注册表元数据修正）
	Pending   int    // 待人批数（宪法/分诊提示词/代码/其他）
	Rejected  int    // 应用失败数（探针未通过等）
	NoSignals bool   // 近 7 天无信号（未调模型）
	CostMilli int64  // 当日自进化费用合计（毫分，含汇总行本身不计）
}

// runForge 跑一次锻造处理（第 23 步，04:30 窗口）：先处理缺口队列
// （检索 → 分流 → 蓝图/采用 → 监工 → staging → 注册），再检查试用期
// （转正/熔断/复活）；周日顺带复查已采用工具的上游状态。
// 预算耗尽时 ProcessGaps 内部记 PAUSED_BUDGET 并停在步骤边界。
func (l *Loop) runForge(now time.Time, date string) error {
	if l.Forge == nil {
		return nil
	}
	ctx := context.Background()
	n, err := l.Forge.ProcessGaps(ctx)
	if err != nil {
		return fmt.Errorf("forge process gaps: %w", err)
	}
	log.Printf("self-evolve forge: 处理 %d 个缺口（%s）", n, date)
	if _, err := l.Forge.CheckTrials(ctx); err != nil {
		return fmt.Errorf("forge check trials: %w", err)
	}
	if now.Weekday() == time.Sunday {
		if _, err := l.Forge.CheckUpstream(ctx); err != nil {
			log.Printf("self-evolve forge: 上游复查失败: %v", err)
		}
	}
	// §2A.7 升级触发线：14 天内 ≥3 次检索器自身导致的顺延
	// （self_core_calls ≥40 的 SEARCH_DEFERRED_RATE_LIMIT）——
	// 触发时只记日志，不自动改策略，留待人工定。
	if d, err := l.Forge.Store.CountDeferred14d(now); err == nil && d >= 3 {
		log.Printf("self-evolve forge: 升级触发线——近 14 天检索器自身导致顺延 %d 次（≥3），请人工复核检索策略", d)
	}
	return nil
}

// ReviewDaily 跑一次自查：聚合信号 → 调模型点评 → 教训进 Wiki →
// 提议入库并按分档应用 → 写当日汇总费用行与执行汇总行。
// 无信号时不调模型，只写一条“无重复问题”的 Wiki 记录。
// 模型调用失败或回复非法时记一行 failure 回执并返回错误，不写 Wiki。
func (l *Loop) ReviewDaily(ctx context.Context, date string) (ReviewResult, error) {
	res := ReviewResult{Date: date}
	taskID := selfEvolveTaskID(date)
	if l.led == nil {
		return res, fmt.Errorf("selfevolve review %s: 账本为空", date)
	}
	signals, err := l.led.AggregateSignals(ReviewWindowDays)
	if err != nil {
		return res, fmt.Errorf("selfevolve review %s: 聚合信号失败: %w", date, err)
	}
	res.Signals = signals.TotalSignals()

	// 无信号：不调模型，只记一条“无重复问题”的 Wiki 教训。
	if res.Signals == 0 {
		res.NoSignals = true
		if l.mem != nil {
			if err := l.mem.WriteWiki(memory.WikiEntry{
				Kind:    memory.KindSynthesis,
				Title:   "自进化自查 " + date,
				Summary: fmt.Sprintf("%s 自查：近 7 天无重复问题（执行失败 0、探针失败 0、监工 HALT 0、费用无突增）。", date),
				Body:    fmt.Sprintf("%s 自查：近 7 天账本无重复问题信号，本次未调模型、未花钱。\n\n费用：当日 %d 毫分，前 7 日均 %d 毫分。", date, signals.ExpenseTodayMilli, signals.ExpensePrior7DayMeanMilli),
			}); err != nil {
				return res, fmt.Errorf("selfevolve review %s: 写 Wiki 失败: %w", date, err)
			}
			res.Lessons = 1
		}
		if err := l.writeDailySummary(taskID); err != nil {
			return res, err
		}
		l.recordReviewSummary(taskID, res, signals)
		log.Printf("selfevolve review %s: 无信号，未调模型", date)
		return res, nil
	}

	// 有信号：调模型点评。
	if l.review == nil {
		return res, fmt.Errorf("selfevolve review %s: 未接模型调用", date)
	}
	reply, err := l.review(ctx, taskID, formatSignals(signals))
	if err != nil {
		l.recordReviewFailure(taskID, err)
		return res, fmt.Errorf("selfevolve review %s: 模型调用失败: %w", date, err)
	}
	out, err := parseReviewReply(reply)
	if err != nil {
		l.recordReviewFailure(taskID, err)
		return res, fmt.Errorf("selfevolve review %s: %w", date, err)
	}

	// 教训进 Wiki：单一条目，标题含日期。
	if l.mem != nil && len(out.Lessons) > 0 {
		var body strings.Builder
		for _, ls := range out.Lessons {
			fmt.Fprintf(&body, "- %s\n", ls)
		}
		fmt.Fprintf(&body, "\n信号汇总（近 7 天）：执行失败 %d、探针失败 %d、监工 HALT %d、监工 CONCERN %d、分诊错选 %d、费用突增 %v。",
			sumCounts(signals.ExecFailures), sumCounts(signals.ProbeFailures),
			sumCounts(signals.AdvisorHalts), sumCounts(signals.AdvisorConcerns),
			sumCounts(signals.TriageMisfires), signals.ExpenseSpike)
		if err := l.mem.WriteWiki(memory.WikiEntry{
			Kind:    memory.KindSynthesis,
			Title:   "自进化自查 " + date,
			Summary: fmt.Sprintf("%s 自查：%d 条教训，%d 条提议。", date, len(out.Lessons), len(out.Proposals)),
			Body:    body.String(),
		}); err != nil {
			return res, fmt.Errorf("selfevolve review %s: 写 Wiki 失败: %w", date, err)
		}
		res.Lessons = len(out.Lessons)
	}

	// 提议：阈值校验 → 去重 → 入库 → 按分档应用。
	for _, d := range out.Proposals {
		count, ok := signals.CountFor(d.Signature)
		if !ok || count < ReviewThreshold {
			log.Printf("selfevolve review %s: 提议 %q 丢弃（信号不足阈值：%d 次，阈值 %d）", date, d.Signature, count, ReviewThreshold)
			continue
		}
		if _, found, err := l.led.FindProposalBySignature(d.Signature, ReviewWindowDays); err != nil {
			return res, fmt.Errorf("selfevolve review %s: 提议去重失败: %w", date, err)
		} else if found {
			log.Printf("selfevolve review %s: 提议 %q 跳过（7 天内已有提议）", date, d.Signature)
			continue
		}
		n, err := l.led.CountProposalsByDate(date)
		if err != nil {
			return res, fmt.Errorf("selfevolve review %s: 提议编号失败: %w", date, err)
		}
		proposalID := fmt.Sprintf("se-%s-%03d", date, n+1)
		if _, err := l.led.RecordProposal(ledger.Proposal{
			ProposalID: proposalID, Date: date, Signature: d.Signature,
			EvidenceCount: count, Text: d.Text, Status: ledger.ProposalPending,
		}); err != nil {
			return res, fmt.Errorf("selfevolve review %s: 提议入库失败: %w", date, err)
		}
		res.Proposals++
		if !autoApplicableKind(d.Kind) {
			res.Pending++
			continue
		}
		if err := l.applyProposal(d); err != nil {
			// 自动应用失败：状态改为 rejected（不是 pending），
			// 原因记日志与执行汇总，不阻塞后续提议。
			_ = l.led.UpdateProposalStatus(proposalID, ledger.ProposalRejected)
			res.Rejected++
			log.Printf("selfevolve review %s: 提议 %s 自动应用失败（已标 rejected）: %v", date, proposalID, err)
			continue
		}
		if err := l.led.UpdateProposalStatus(proposalID, ledger.ProposalApplied); err != nil {
			return res, fmt.Errorf("selfevolve review %s: 提议 %s 标 applied 失败: %w", date, proposalID, err)
		}
		res.Applied++
	}

	// 当日汇总费用行 + 执行汇总行。
	if err := l.writeDailySummary(taskID); err != nil {
		return res, err
	}
	sum, _ := l.led.SummarizeExpenseByTask(taskID)
	res.CostMilli = sum.CostCNYMilli
	l.recordReviewSummary(taskID, res, signals)
	log.Printf("selfevolve review %s: signals=%d lessons=%d proposals=%d applied=%d pending=%d rejected=%d cost=%d 毫分",
		date, res.Signals, res.Lessons, res.Proposals, res.Applied, res.Pending, res.Rejected, res.CostMilli)
	return res, nil
}

// applyProposal 执行一条自动档提议（调用前已校验种类与阈值）：
//   - registry_entrypoint_fix：先过探针，通过才存入内存覆盖层；
//   - registry_verified：跑一次探针，通过即标本进程内已验证。
//
// 其他种类不进本函数（调用方已分流为 pending）。
func (l *Loop) applyProposal(d ProposalDraft) error {
	if l.reg == nil {
		return fmt.Errorf("selfevolve apply %s: 注册表为空", d.Signature)
	}
	switch d.Kind {
	case KindRegistryEntryPointFix:
		return l.reg.ApplyEntryPointFix(d.ToolID, d.NewEntryPoint)
	case KindRegistryVerified:
		return l.reg.Probe(d.ToolID)
	default:
		return fmt.Errorf("selfevolve apply %s: 种类 %q 不可自动应用", d.Signature, d.Kind)
	}
}

// writeDailySummary 写当日汇总费用行：汇总 task_id=selfevolve-<日期>
// 的全部调用行（不含汇总行本身），model 标 selfevolve-summary。
// 汇总行与调用行的 token/费用重复，汇总时必须排除该标记——
// 见 ledger.SummarizeExpenseByTask 与本文件顶部 SummaryModel 注释。
func (l *Loop) writeDailySummary(taskID string) error {
	sum, err := l.led.SummarizeExpenseByTask(taskID)
	if err != nil {
		return fmt.Errorf("selfevolve summary %s: 汇总费用失败: %w", taskID, err)
	}
	if _, err := l.led.RecordExpense(ledger.ExpenseRecord{
		SessionID: l.sessionID, TaskID: taskID, Model: SummaryModel,
		PromptTokens: sum.PromptTokens, CacheHitTokens: sum.CacheHitTokens,
		CacheMissTokens: sum.CacheMissTokens, CompletionTokens: sum.CompletionTokens,
		CostCNYMilli: sum.CostCNYMilli,
	}); err != nil {
		return fmt.Errorf("selfevolve summary %s: 写汇总行失败: %w", taskID, err)
	}
	return nil
}

// recordReviewSummary 写一次自查的执行汇总行（ToolID selfevolve-daily）。
func (l *Loop) recordReviewSummary(taskID string, res ReviewResult, signals ledger.SignalCounts) {
	if l.led == nil {
		return
	}
	_, err := l.led.RecordExecution(ledger.ExecutionRecord{
		SessionID: l.sessionID, TaskID: taskID, ToolID: "selfevolve-daily",
		Command: "selfevolve review " + res.Date, ExitCode: 0, Status: "success",
		StdoutTail: fmt.Sprintf("signals=%d lessons=%d proposals=%d applied=%d pending=%d rejected=%d cost=%d 毫分 expense_today=%d 毫分 expense_7d_mean=%d 毫分",
			res.Signals, res.Lessons, res.Proposals, res.Applied, res.Pending, res.Rejected,
			res.CostMilli, signals.ExpenseTodayMilli, signals.ExpensePrior7DayMeanMilli),
	})
	if err != nil {
		log.Printf("selfevolve review summary record failed: %v", err)
	}
}

// recordReviewFailure 写一次自查失败的回执行（模型调用失败或回复非法）。
func (l *Loop) recordReviewFailure(taskID string, err error) {
	if l.led == nil {
		return
	}
	_, rerr := l.led.RecordExecution(ledger.ExecutionRecord{
		SessionID: l.sessionID, TaskID: taskID, ToolID: "selfevolve-daily",
		Command: "selfevolve review", ExitCode: 1, Status: "failure",
		ErrHead: err.Error(),
	})
	if rerr != nil {
		log.Printf("selfevolve review failure record failed: %v", rerr)
	}
}

func sumCounts(m map[string]int) int {
	n := 0
	for _, c := range m {
		n += c
	}
	return n
}

// ---- 每周注册表重建 ----

// discoveryCandidates 是重建时“新发现的本机工具”的候选清单：
// 不在注册表里、但装机后可能用得上的常见命令。只做 LookPath 存在性
// 检查，不执行、不装机；命中即写进重建报告，供人决定是否入库。
// 注意：这只是候选清单，不是全盘 PATH 扫描（全盘扫噪音大、也慢）。
var discoveryCandidates = []string{
	"docker", "kubectl", "terraform", "ansible", "sqlite3",
	"htop", "tmux", "rsync", "make", "go", "cargo", "rustc",
	"pip", "pipx", "nmap", "tcpdump",
}

// rebuildSnapshot 是上次重建的快照（放数据目录 selfevolve/rebuild_last.json，
// 0600）：每条工具的上次探针结果，供下次算差异。
type rebuildSnapshot struct {
	Date       string                      `json:"date"`
	Tools      map[string]rebuildToolState `json:"tools"`
	Discovered []string                    `json:"discovered"`
}

// rebuildToolState 是一条工具在快照里的状态。
type rebuildToolState struct {
	OK       bool `json:"ok"`
	Verified bool `json:"verified"`
}

// RebuildResult 是一次重建的结果。
type RebuildResult struct {
	Date         string   // 重建日期（北京时间 YYYY-MM-DD）
	Checked      int      // 探针的条目数（全部 84 条，含不可用）
	OK           int      // 探针通过数
	Broken       int      // 探针失败数
	NewlyBroken  []string // 上次好、这次坏
	NewlyOK      []string // 上次坏、这次好
	StillBroken  []string // 两次都坏（只列前 5，余数见 Wiki）
	StillBrokenN int      // 两次都坏的总数
	Discovered   []string // 新发现的本机工具（候选清单命中、且不在注册表里）
}

// RebuildWeekly 跑一次注册表重建：重探全部条目（复用 registry 的 Probe），
// 输出差异报告进 Wiki（新坏的 / 新好的 / 新发现的本机工具），
// 只更新 verified 内存覆盖层（通过标已验证、失败撤标记），
// 绝不改写 registry.json 文件本身。
func (l *Loop) RebuildWeekly(ctx context.Context, date string) (RebuildResult, error) {
	var res RebuildResult
	res.Date = date
	taskID := rebuildTaskID(date)
	if l.led == nil || l.reg == nil {
		return res, fmt.Errorf("selfevolve rebuild %s: 账本或注册表为空", date)
	}
	snap := loadRebuildSnapshot(filepath.Join(l.dataDir, "selfevolve", "rebuild_last.json"))
	next := rebuildSnapshot{Date: date, Tools: map[string]rebuildToolState{}}
	knownTools := map[string]bool{}

	for _, cat := range registry.Categories() {
		for _, e := range l.reg.EntriesByCategory(cat) {
			res.Checked++
			knownTools[e.ToolID] = true
			var err error
			if e.Available {
				err = l.reg.Probe(e.ToolID)
				if err != nil {
					l.reg.UnmarkVerified(e.ToolID)
				}
			} else {
				// 不可用条目不进调用链，但重建仍探一次存在性（只记录，不算故障）。
				err = registry.ProbeEntry(*e)
			}
			ok := err == nil
			if ok {
				res.OK++
			} else {
				res.Broken++
			}
			next.Tools[e.ToolID] = rebuildToolState{OK: ok, Verified: ok && e.Available}
			if prev, had := snap.Tools[e.ToolID]; had && e.Available {
				switch {
				case prev.OK && !ok:
					res.NewlyBroken = append(res.NewlyBroken, e.ToolID)
				case !prev.OK && ok:
					res.NewlyOK = append(res.NewlyOK, e.ToolID)
				case !prev.OK && !ok:
					res.StillBroken = append(res.StillBroken, e.ToolID)
				}
			}
		}
	}
	res.StillBrokenN = len(res.StillBroken)
	if len(res.StillBroken) > 5 {
		res.StillBroken = res.StillBroken[:5]
	}
	sort.Strings(res.NewlyBroken)
	sort.Strings(res.NewlyOK)
	sort.Strings(res.StillBroken)

	// 新发现的本机工具：候选清单里在 PATH 上、且不在注册表里的命令。
	for _, cand := range discoveryCandidates {
		if knownTools[cand] {
			continue
		}
		if _, err := exec.LookPath(cand); err == nil {
			res.Discovered = append(res.Discovered, cand)
		}
	}
	next.Discovered = res.Discovered

	// 差异报告进 Wiki（单一条目，标题含日期）。
	if l.mem != nil {
		var body strings.Builder
		fmt.Fprintf(&body, "检查 %d 条，通过 %d 条，失败 %d 条。\n\n", res.Checked, res.OK, res.Broken)
		writeList := func(title string, items []string, total int) {
			fmt.Fprintf(&body, "%s（%d）：", title, total)
			if len(items) == 0 {
				body.WriteString("无\n\n")
				return
			}
			body.WriteString("\n")
			for _, it := range items {
				fmt.Fprintf(&body, "- %s\n", it)
			}
			if total > len(items) {
				fmt.Fprintf(&body, "（另有 %d 条未列出）\n", total-len(items))
			}
			body.WriteString("\n")
		}
		writeList("新坏的", res.NewlyBroken, len(res.NewlyBroken))
		writeList("新好的", res.NewlyOK, len(res.NewlyOK))
		writeList("仍坏的", res.StillBroken, res.StillBrokenN)
		writeList("新发现的本机工具", res.Discovered, len(res.Discovered))
		body.WriteString("说明：重建只更新 verified 内存覆盖层，不改写 registry.json；新发现的工具须由人决定是否入库。")
		if err := l.mem.WriteWiki(memory.WikiEntry{
			Kind:    memory.KindSynthesis,
			Title:   "自进化注册表重建 " + date,
			Summary: fmt.Sprintf("%s 重建：检查 %d 条，新坏 %d 条、新好 %d 条、新发现 %d 个工具。", date, res.Checked, len(res.NewlyBroken), len(res.NewlyOK), len(res.Discovered)),
			Body:    body.String(),
		}); err != nil {
			return res, fmt.Errorf("selfevolve rebuild %s: 写 Wiki 失败: %w", date, err)
		}
	}

	// 存快照（0600），供下次算差异。
	if err := saveRebuildSnapshot(filepath.Join(l.dataDir, "selfevolve", "rebuild_last.json"), next); err != nil {
		return res, fmt.Errorf("selfevolve rebuild %s: 存快照失败: %w", date, err)
	}

	// 执行汇总行。
	if _, err := l.led.RecordExecution(ledger.ExecutionRecord{
		SessionID: l.sessionID, TaskID: taskID, ToolID: "selfevolve-rebuild",
		Command: "selfevolve rebuild " + date, ExitCode: 0, Status: "success",
		StdoutTail: fmt.Sprintf("checked=%d ok=%d broken=%d newly_broken=%d newly_ok=%d still_broken=%d discovered=%d",
			res.Checked, res.OK, res.Broken, len(res.NewlyBroken), len(res.NewlyOK), res.StillBrokenN, len(res.Discovered)),
	}); err != nil {
		return res, fmt.Errorf("selfevolve rebuild %s: 记账失败: %w", date, err)
	}
	log.Printf("selfevolve rebuild %s: checked=%d ok=%d broken=%d newly_broken=%d newly_ok=%d discovered=%d",
		date, res.Checked, res.OK, res.Broken, len(res.NewlyBroken), len(res.NewlyOK), len(res.Discovered))
	return res, nil
}

// loadRebuildSnapshot 读重建快照；文件不存在或非法时返回空快照
// （首次重建即基线，差异为空），不报错。
func loadRebuildSnapshot(path string) rebuildSnapshot {
	snap := rebuildSnapshot{Tools: map[string]rebuildToolState{}}
	b, err := os.ReadFile(path)
	if err != nil {
		return snap
	}
	if err := json.Unmarshal(b, &snap); err != nil {
		return rebuildSnapshot{Tools: map[string]rebuildToolState{}}
	}
	if snap.Tools == nil {
		snap.Tools = map[string]rebuildToolState{}
	}
	return snap
}

// saveRebuildSnapshot 写重建快照（0600，目录不存在时创建）。
func saveRebuildSnapshot(path string, snap rebuildSnapshot) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}
