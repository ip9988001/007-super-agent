// Command super-agent 是 007 超级 Agent 的主程序：按顺序接线
// config → ledger → registry → memory → executor → watchdog → gateway。
// 启动失败（配置读不到、总账读不到、建库失败）明确报错退出，不带病跑。
//
// --shadow（或 SHADOW_MODE=true）为影子模式：网关收信、分诊照常走，
// 执行只记“影子会跑什么”进 execution_ledger（状态 shadow），不真跑命令。
// 影子模式的监工用只记 verdict、不拦截的桩；非影子模式接真实监工
// internal/advisor（Gemini 第 9 轮交付，HALT 最多打回 2 轮），
// 无密钥时拒绝启动，绝不跳过不可逆动作前审。
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"007-super-agent/internal/advisor"
	"007-super-agent/internal/agent"
	"007-super-agent/internal/backup"
	"007-super-agent/internal/config"
	"007-super-agent/internal/executor"
	"007-super-agent/internal/forge"
	"007-super-agent/internal/gateway"
	"007-super-agent/internal/ledger"
	"007-super-agent/internal/llm"
	"007-super-agent/internal/memory"
	"007-super-agent/internal/registry"
	"007-super-agent/internal/retrieval"
	"007-super-agent/internal/selfevolve"
	"007-super-agent/internal/triage"
	"007-super-agent/internal/watchdog"
)

const Version = "v0.1.0"

// shadowAdvisor 是影子模式的监工桩：只收证据、只记 verdict，不拦截，
// 不调任何模型。非影子模式用 internal/advisor 真实监工（第 11 步已接线）。
type shadowAdvisor struct{}

func (shadowAdvisor) CollectEvidenceBundle(t executor.Task, _ executor.Trigger) (executor.Evidence, error) {
	return executor.Evidence{TaskText: t.Text, Draft: t.Draft}, nil
}

func (shadowAdvisor) Check(e executor.Evidence, tr executor.Trigger) (executor.CheckResult, error) {
	return executor.CheckResult{Verdict: executor.VerdictNote, Note: "影子模式：只记 verdict，不拦截"}, nil
}

// daemon 是接线后的整机：网关的 handler 就是它的方法。
type daemon struct {
	cfg       *config.Config
	led       *ledger.Ledger
	reg       *registry.Registry
	mem       *memory.Store
	ex        *executor.Executor
	wd        *watchdog.Watchdog
	ag        *agent.Agent         // 主循环（对齐二号 Step 1+Step 4）；nil 表示未接线（无模型调用）
	modelCall triage.ModelCallFunc // Tier-1 模型调用（llm 客户端）；nil 时 Tier-1 报错
	session   string
	shadow    bool
}

func newSessionID() string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("sa-%s-%s", time.Now().UTC().Format("20060102T150405Z"), hex.EncodeToString(b[:]))
}

// handle 是网关 worker 串行调的唯一入口：分诊 → 定位 → 影子记账或真执行。
// 处理完由框架写一行 L1 日志（task_id、route、tool_id、结果状态、耗时），
// 不经模型选择——模型漏记也不丢。L1 写失败只记日志，不阻塞回执。
func (d *daemon) handle(env gateway.Envelope) {
	start := time.Now()
	route, toolID, status := d.handleInner(env)
	line := fmt.Sprintf("task %s route=%s tool=%s status=%s duration_ms=%d",
		env.TaskID, route, toolID, status, time.Since(start).Milliseconds())
	if err := d.mem.AppendL1(line); err != nil {
		log.Printf("memory l1 task=%s: %v", env.TaskID, err)
	}
}

// handleInner 做真正的分诊与执行，返回（route、tool_id、结果状态）供 handle 记 L1。
func (d *daemon) handleInner(env gateway.Envelope) (route, toolID, status string) {
	dec, err := triage.Triage(d.reg, d.led, triage.Input{Text: env.Text, TaskID: env.TaskID, SessionID: d.session}, d.modelCall)
	if err != nil {
		// Tier-1 需要模型调用：modelCall 为 nil（影子模式且无密钥）或
		// 模型调用失败时只记日志，不执行。
		log.Printf("triage task=%s: %v", env.TaskID, err)
		return "ERROR", "", "triage_error"
	}
	// AGENT 去向走主循环（对齐二号：主模型逐轮调工具，看到结果再决定下一步），
	// 不再只挑一个工具；单步路径行为不变。
	if dec.Route == triage.RouteAgent {
		return string(dec.Route), "", d.runAgent(env)
	}
	if dec.Route != triage.RouteTool {
		log.Printf("task=%s route=%s tier=%d (no tool to run)", env.TaskID, dec.Route, dec.Tier)
		return string(dec.Route), dec.ToolID, "no_tool"
	}
	e, _, err := triage.ResolveTool(d.reg, dec.ToolID)
	if err != nil {
		log.Printf("task=%s resolve %s: %v", env.TaskID, dec.ToolID, err)
		// 第 14 步 B-03：预检探针失败要记账（失败行进 execution_ledger），
		// 不禁用、不静默跳过，任务本身失败返回，不执行。
		if errors.Is(err, triage.ErrProbeFailed) {
			if _, rerr := d.led.RecordExecution(ledger.ExecutionRecord{
				SessionID: d.session, TaskID: env.TaskID, ToolID: dec.ToolID,
				Command: "probe " + dec.ToolID, ExitCode: 1, Status: "failure",
				ErrHead: err.Error(),
			}); rerr != nil {
				log.Printf("probe record task=%s: %v", env.TaskID, rerr)
			}
		}
		return string(dec.Route), dec.ToolID, "resolve_error"
	}
	if d.shadow {
		d.shadowRecord(env, e)
		return string(dec.Route), dec.ToolID, "shadow"
	}
	return string(dec.Route), dec.ToolID, d.runTool(env, e)
}

// shadowRecord 只记“影子会跑什么”：先过一遍监工桩（只记 verdict，
// 不拦截），再写一行 execution_ledger，状态标 shadow。不跑任何命令。
func (d *daemon) shadowRecord(env gateway.Envelope, e *registry.Entry) {
	verdict, note := string(executor.VerdictNote), "影子模式：只记 verdict，不拦截"
	if d.ex.Advisor != nil {
		res, err := d.ex.ReviewDelivery(executor.Task{
			TaskID: env.TaskID, SessionID: d.session, ToolID: e.ToolID,
			Text: env.Text, Command: "shadow: " + e.EntryPoint,
		})
		if err != nil {
			log.Printf("shadow review task=%s: %v", env.TaskID, err)
		} else {
			verdict, note = string(res.Verdict), res.Note
		}
	}
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
	return res.Status
}

// memorySummary 给规划器用的相关记忆摘要：Recall 前 3 条（每条 ≤240 字），
// 无关或查不到时返回空串，规划器不靠它编造。失败只记日志，不阻塞任务。
func (d *daemon) memorySummary(text string) string {
	if d.mem == nil {
		return ""
	}
	rs, err := d.mem.Recall(text)
	if err != nil || len(rs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("相关记忆摘要（供规划参考，不在其中不要编造）：\n")
	for _, r := range rs {
		fmt.Fprintf(&b, "- 【%s】%s\n", r.Source, r.Text)
	}
	return b.String()
}

// runAgent 是 AGENT 去向的真执行：主循环逐轮调工具、看到结果再决定下一步，
// 最后交活前审。监工 HALT 时把打回意见喂回主循环再跑一轮（最多 2 次重跑，
// 与监工的 HALT 上限对齐），让它照着改；影子模式只做一次决定、记一行 shadow
// 回执，不执行。返回执行结果状态（供 handle 记 L1）。
func (d *daemon) runAgent(env gateway.Envelope) string {
	if d.ag == nil {
		log.Printf("task=%s AGENT but agent not wired (no llm client)", env.TaskID)
		return "agent_unavailable"
	}
	memSummary := d.memorySummary(env.Text)
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
	feedback := ""
	for attempt := 0; attempt <= 2; attempt++ {
		res, err := d.ag.Run(context.Background(), env.TaskID, env.Text, memSummary, feedback)
		if err != nil {
			log.Printf("task=%s agent run: %v", env.TaskID, err)
			return "agent_error"
		}
		// 交活前审（触发点 ①）：Draft 为最终回复。HALT 时不交付，原因已记账；
		// 还有重跑次数就把打回意见喂回去再跑一轮，没次数了才停下上报。
		if d.ex.Advisor == nil {
			log.Printf("task=%s agent status=%s steps=%d (no advisor)", env.TaskID, res.Status, res.CompletedSteps)
			return res.Status
		}
		rres, rerr := d.ex.ReviewDelivery(executor.Task{
			TaskID: env.TaskID, SessionID: d.session, ToolID: "agent",
			Text: env.Text, Command: "agent " + env.TaskID, Draft: res.ReplyDraft,
			WorkDir: d.ag.WorkDir(env.TaskID),
		})
		if rerr != nil && !errors.Is(rerr, executor.ErrAdvisorHalt) && !errors.Is(rerr, executor.ErrAdvisorHaltFinal) {
			log.Printf("task=%s delivery review: %v", env.TaskID, rerr)
			return res.Status
		}
		if rres.Verdict != executor.VerdictHalt {
			log.Printf("task=%s agent status=%s steps=%d attempts=%d", env.TaskID, res.Status, res.CompletedSteps, attempt+1)
			return res.Status
		}
		log.Printf("task=%s delivery HALT (retry=%d): %s", env.TaskID, rres.RetryCount, rres.Note)
		if attempt >= 2 || errors.Is(rerr, executor.ErrAdvisorHaltFinal) {
			return "delivery_halt"
		}
		feedback = rres.Note
	}
	return "delivery_halt"
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

	reg, err := registry.Load(dataDir)
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

	ex := &executor.Executor{Ledger: led}
	if shadow {
		ex.Advisor = shadowAdvisor{}
		log.Printf("executor ready: shadow mode, advisor=stub (record only, never blocks)")
	} else {
		log.Printf("executor ready: live mode, advisor wired below (fatal if key missing)")
	}

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

	d := &daemon{cfg: cfg, led: led, reg: reg, mem: mem, ex: ex, wd: wd, session: newSessionID(), shadow: shadow}
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
		// 监工接线（第 11 步）：非影子模式必须有真实 Advisor 才能启动。
		// 影子模式沿用 shadowAdvisor 桩（只记 verdict，不拦截）。
		if !shadow {
			workDir, _ := os.Getwd()
			adv, err := advisor.New(dbPath, advisor.AdvisorConfig{
				Endpoint: strings.TrimRight(llm.DefaultBaseURL, "/") + "/v1/chat/completions",
				APIKey:   apiKey, Model: cfg.AdvisorModel, Timeout: 120 * time.Second,
			}, workDir)
			if err != nil {
				log.Fatalf("advisor: %v", err)
			}
			ex.Advisor = adv
			adv.Ledger = led // 第 13 步：监工调用的费用进 expense_ledger
			log.Printf("advisor ready: model=%s (key found, value never logged)", cfg.AdvisorModel)
		}
		// 主循环接线（对齐二号 Step 1+Step 4）：只用 worker 模型（flash）逐轮
		// 决定调哪个工具，不再另配核验模型（reasoner 已摘）；影子模式也接线，
		// 只做决定、不执行。
		d.ag = agent.New(lc.CallTask, reg, led, ex, dataDir, d.session)
		// 第 23 步：缺口检测只写报告（本地写盘），影子模式也开。
		d.ag.Forge = &forge.Store{DataDir: dataDir}
		log.Printf("agent ready: worker=%s shadow=%v", cfg.WorkerModel, shadow)
	}
	gw := gateway.New(cfg, led, d.handle)
	gw.SetSession(d.session)
	// 第 14 步 B-02：启动时重放崩溃前没确认的 inbox 行（先于 Start）。
	if n, err := gw.ReplayInbox(); err != nil {
		log.Printf("gateway replay: %v", err)
	} else if n > 0 {
		log.Printf("gateway replay: %d inbox rows requeued", n)
	}
	gw.Start()

	// 每日精炼（第 12 步）：影子模式不启动（精炼会真写 Wiki）；
	// 非影子模式下模型已接线则起常驻循环，每天北京时间 04:00–04:15
	// 精炼前一天的 L1。模型不可用时循环内每天只记一条日志，不报错。
	refineCtx, refineCancel := context.WithCancel(context.Background())
	defer refineCancel()
	if !shadow && lc != nil {
		go wd.RefineLoop(refineCtx, mem, dataDir, lc.RefineL1)
		log.Printf("refine loop started: daily 04:00 Beijing, model=%s", cfg.WorkerModel)
	} else if !shadow {
		log.Printf("refine loop disabled: no llm client")
	}

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
		seLoop.Forge = newForgePipeline(dataDir, reg, led, ex, lc, d.session)
		log.Printf("forge ready: daily budget 500 milli, max 3 gaps/day")
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
// 模型走 lc.CallTask（flash），监工走 ex.ReviewDelivery（固定第 1 检查项
// 由 forge 包自带），执行走 ex，检索走匿名 GitHub 客户端（绝不带 Token）。
func newForgePipeline(dataDir string, reg *registry.Registry, led *ledger.Ledger, ex *executor.Executor, lc *llm.Client, sessionID string) *forge.Pipeline {
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
	review := func(ctx context.Context, taskID, title, body, workDir string) (string, string, error) {
		res, err := ex.ReviewDelivery(executor.Task{
			TaskID: taskID, SessionID: sessionID, ToolID: "forge",
			Text: title + "\n" + body, Command: "forge review", Draft: body, WorkDir: workDir,
		})
		if err != nil && (errors.Is(err, executor.ErrAdvisorHalt) || errors.Is(err, executor.ErrAdvisorHaltFinal)) {
			// 监工 HALT 是 verdict，不是调用失败。
			return string(res.Verdict), res.Note, nil
		}
		if err != nil {
			return "", "", err
		}
		return string(res.Verdict), res.Note, nil
	}
	return &forge.Pipeline{
		Store: store, Reg: reg, Led: led, Ret: ret,
		Model: model, Review: review, Run: ex,
		SessionID:        sessionID,
		DailyBudgetMilli: 500, MaxGapsPerDay: 3,
	}
}

// runForgeCmd 是 `super-agent forge-run` 子命令：手动跑一次锻造后退出。
func runForgeCmd(args []string, dataDirFlag string) {
	fs := flag.NewFlagSet("forge-run", flag.ExitOnError)
	dataDir := fs.String("data-dir", dataDirFlag, "数据目录（覆盖配置）")
	gapID := fs.String("gap", "", "只处理指定缺口")
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
	workDir, _ := os.Getwd()
	adv, err := advisor.New(dbPath, advisor.AdvisorConfig{
		Endpoint: strings.TrimRight(llm.DefaultBaseURL, "/") + "/v1/chat/completions",
		APIKey:   apiKey, Model: cfg.AdvisorModel, Timeout: 120 * time.Second,
	}, workDir)
	if err != nil {
		log.Fatalf("advisor: %v", err)
	}
	ex.Advisor = adv
	adv.Ledger = led
	fp := newForgePipeline(dDir, reg, led, ex, lc, sessionID)
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
