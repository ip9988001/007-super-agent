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
	"007-super-agent/internal/config"
	"007-super-agent/internal/executor"
	"007-super-agent/internal/gateway"
	"007-super-agent/internal/ledger"
	"007-super-agent/internal/llm"
	"007-super-agent/internal/memory"
	"007-super-agent/internal/registry"
	"007-super-agent/internal/triage"
	"007-super-agent/internal/watchdog"
)

const Version = "v0.1.0"

// shadowAdvisor 是影子模式的监工桩：只收证据、只记 verdict，不拦截，
// 不调任何模型。非影子模式用 internal/advisor 真实监工（第 11 步已接线）。
type shadowAdvisor struct{}

func (shadowAdvisor) CollectEvidenceBundle(t executor.Task) (executor.Evidence, error) {
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
	if dec.Route != triage.RouteTool {
		log.Printf("task=%s route=%s tier=%d (no tool to run)", env.TaskID, dec.Route, dec.Tier)
		return string(dec.Route), dec.ToolID, "no_tool"
	}
	e, _, err := triage.ResolveTool(d.reg, dec.ToolID)
	if err != nil {
		log.Printf("task=%s resolve %s: %v", env.TaskID, dec.ToolID, err)
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

func main() {
	shadowFlag := flag.Bool("shadow", false, "影子模式：只记不跑")
	dataDirFlag := flag.String("data-dir", "", "数据目录（覆盖配置）")
	listenFlag := flag.String("listen", "", "监听地址（覆盖配置）")
	flag.Parse()

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

	ex := &executor.Executor{Ledger: led}
	if shadow {
		ex.Advisor = shadowAdvisor{}
		log.Printf("executor ready: shadow mode, advisor=stub (record only, never blocks)")
	} else {
		log.Printf("executor ready: live mode, advisor wired below (fatal if key missing)")
	}

	wd := watchdog.New(led, dbPath, nil)
	if res, err := wd.CheckRegistry(dataDir); err != nil {
		log.Printf("watchdog check: %v", err)
	} else {
		log.Printf("watchdog check: %d checked, %d ok, %d broken, %d skipped", res.Checked, res.OK, len(res.Broken), res.Skipped)
	}
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
			log.Printf("advisor ready: model=%s (key found, value never logged)", cfg.AdvisorModel)
		}
	}
	gw := gateway.New(cfg, led, d.handle)
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
