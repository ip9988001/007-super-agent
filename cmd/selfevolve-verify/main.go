// Command selfevolve-verify：第 20 步沙箱验证——
// 用含重复失败的种子账本跑一次 ReviewDaily（桩模型，不真调 API、
// 不联网、不读任何密钥），再跑一次 RebuildWeekly，打印提议、
// 费用行与重建差异，断言失败时非 0 退出。
//
// 桩模型模拟一次 Flash 调用：prompt 1000 token（800 命中 + 200 未命中）、
// completion 300 token，费用 7 毫分——数字是桩的模拟用量，不是真实 API
// 用量；真实用量等 04:30 在线上跑起来后从 expense_ledger 读。
package main

import (
	"context"
	"crypto/md5"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"007-super-agent/internal/ledger"
	"007-super-agent/internal/memory"
	"007-super-agent/internal/registry"
	"007-super-agent/internal/selfevolve"

	_ "modernc.org/sqlite"
)

func beijingNow() time.Time {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		loc = time.FixedZone("CST", 8*3600)
	}
	return time.Now().In(loc)
}

func fail(format string, args ...any) {
	log.Printf("FAIL: "+format, args...)
	os.Exit(1)
}

func main() {
	dir, err := os.MkdirTemp("", "severify")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)
	log.Printf("sandbox dir: %s", dir)

	led, err := ledger.Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer ledger.Close(led)

	// ---- 种子账本：重复失败 ----
	seed := func(toolID, command, verdict string, n int) {
		for i := 0; i < n; i++ {
			status := "failure"
			exitCode := 1
			if verdict != "" {
				status = "success"
				exitCode = 0
			}
			if _, err := led.RecordExecution(ledger.ExecutionRecord{
				SessionID: "seed", TaskID: fmt.Sprintf("seed-%s-%s-%d", toolID, command, i),
				ToolID: toolID, Command: command, ExitCode: exitCode,
				Status: status, AdvisorVerdict: verdict, ErrHead: "seed failure " + toolID,
			}); err != nil {
				log.Fatal(err)
			}
		}
	}
	seed("cap-ops-true", "true --bad", "", 3)         // exec_fail ×3
	seed("cap-ops-true", "probe cap-ops-true", "", 3) // probe_fail ×3
	seed("cap-ops-true", "true", "HALT", 3)           // advisor_halt ×3
	seed("cap-ops-git", "git --bad", "", 2)           // 低于阈值

	// ---- 种子费用：今天 300 毫分；前 7 天两天 300、五天 50（均值 121，
	// 今天 300 > 2×121，突增；7 天内突增 3 天，够提议阈值）----
	if _, err := led.RecordExpense(ledger.ExpenseRecord{
		SessionID: "seed", TaskID: "t-today", Model: "deepseek-flash", CostCNYMilli: 300,
	}); err != nil {
		log.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "ledger.db"))
	if err != nil {
		log.Fatal(err)
	}
	loc := beijingNow().Location()
	nowBJ := beijingNow()
	prior := []int64{300, 300, 50, 50, 50, 50, 50}
	for d := 1; d <= 7; d++ {
		day := nowBJ.AddDate(0, 0, -d)
		ts := time.Date(day.Year(), day.Month(), day.Day(), 12, 0, 0, 0, loc).UTC().Format(time.RFC3339)
		if _, err := db.Exec(`INSERT INTO expense_ledger
			(ts, session_id, task_id, model, prompt_tokens, cache_hit_tokens,
			 cache_miss_tokens, completion_tokens, cost_cny_milli)
			VALUES (?, 'seed', ?, 'deepseek-flash', 0, 0, 0, 0, ?)`,
			ts, fmt.Sprintf("t-prior-%d", d), prior[d-1]); err != nil {
			log.Fatal(err)
		}
	}
	db.Close()

	// ---- 最小注册表（与 selfevolve_test.go 的 miniRegistry 同口径）----
	regDir := filepath.Join(dir, "reg")
	regJSON := `{"version":"v1.1-test","generated":"2026-11-19","total":2,"entries":[
{"tool_id":"cap-ops-true","category":"ops","subcategory":"shell","call_type":"cli","trigger_prefix":"","pre_command":"","entry_point":"/nonexistent/xyz","param_schema":{},"output_contract":"exit 0","status_triad":{"installed":false,"runnable":false,"verified":false},"available":true,"last_verified_at":"","source_file":"test","one_liner":"true 命令占位","verified_basis":"","note":""},
{"tool_id":"cap-ops-git","category":"ops","subcategory":"vcs","call_type":"cli","trigger_prefix":"","pre_command":"","entry_point":"git","param_schema":{},"output_contract":"exit 0","status_triad":{"installed":true,"runnable":true,"verified":false},"available":true,"last_verified_at":"","source_file":"test","one_liner":"git","verified_basis":"","note":""}
]}`
	if err := os.MkdirAll(filepath.Join(regDir, "registry"), 0o755); err != nil {
		log.Fatal(err)
	}
	regJSONPath := filepath.Join(regDir, "registry", "registry.json")
	if err := os.WriteFile(regJSONPath, []byte(regJSON), 0o600); err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(regDir, "index", "L3"), 0o755); err != nil {
		log.Fatal(err)
	}
	for _, id := range []string{"cap-ops-true", "cap-ops-git"} {
		content := "# " + id + "\n\n输入：无\n\n步骤：跑命令\n\n验收：exit 0\n"
		if err := os.WriteFile(filepath.Join(regDir, "index", "L3", id+".md"), []byte(content), 0o600); err != nil {
			log.Fatal(err)
		}
	}
	regJSONMD5Before := fmt.Sprintf("%x", md5.Sum([]byte(regJSON)))
	reg, err := registry.Load(regDir)
	if err != nil {
		log.Fatal(err)
	}
	mem, err := memory.Open(filepath.Join(dir, "memory"))
	if err != nil {
		log.Fatal(err)
	}

	// ---- 桩模型：模拟一次 Flash 调用（记费用行 7 毫分），回固定 JSON ----
	reply := `{"lessons":["入口路径坏了要先验再改","费用突增要查哪类任务在烧钱"],"proposals":[
{"signature":"exec_fail:cap-ops-true","kind":"registry_entrypoint_fix","tool_id":"cap-ops-true","new_entry_point":"true","text":"把入口改为 true"},
{"signature":"probe_fail:cap-ops-true","kind":"constitution","text":"宪法加一条探针口径"},
{"signature":"advisor_halt:cap-ops-true","kind":"triage_prompt","text":"分诊提示词加一条"},
{"signature":"expense_spike","kind":"other","text":"费用连续突增，查一下哪类任务在烧钱"},
{"signature":"exec_fail:cap-ops-git","kind":"registry_entrypoint_fix","tool_id":"cap-ops-git","new_entry_point":"true","text":"低于阈值应被丢弃"}
]}`
	review := func(_ context.Context, taskID, _ string) (string, error) {
		if _, err := led.RecordExpense(ledger.ExpenseRecord{
			SessionID: "verify", TaskID: taskID, Model: "deepseek-flash",
			PromptTokens: 1000, CacheHitTokens: 800, CacheMissTokens: 200,
			CompletionTokens: 300, CostCNYMilli: 7,
		}); err != nil {
			return "", err
		}
		return reply, nil
	}

	loop := selfevolve.New(led, reg, mem, dir, "verify-session", review, false)
	date := beijingNow().Format("2006-01-02")

	// ---- 跑一次 ReviewDaily ----
	res, err := loop.ReviewDaily(context.Background(), date)
	if err != nil {
		fail("ReviewDaily: %v", err)
	}
	fmt.Printf("review %s: signals=%d lessons=%d proposals=%d applied=%d pending=%d rejected=%d cost=%d 毫分\n",
		date, res.Signals, res.Lessons, res.Proposals, res.Applied, res.Pending, res.Rejected, res.CostMilli)
	if res.Proposals != 4 || res.Applied != 1 || res.Pending != 3 {
		fail("提议数 proposals=%d applied=%d pending=%d, want 4/1/3", res.Proposals, res.Applied, res.Pending)
	}
	props, err := led.ListProposals("", 10)
	if err != nil {
		log.Fatal(err)
	}
	for _, p := range props {
		fmt.Printf("proposal %s | %s | %s | 证据 %d 次\n", p.ProposalID, p.Signature, p.Status, p.EvidenceCount)
	}
	rows, err := led.ListExpenseByTask("selfevolve-"+date, 10)
	if err != nil {
		log.Fatal(err)
	}
	for _, r := range rows {
		fmt.Printf("expense task=%s model=%s prompt=%d hit=%d miss=%d completion=%d cost=%d 毫分\n",
			r.TaskID, r.Model, r.PromptTokens, r.CacheHitTokens, r.CacheMissTokens, r.CompletionTokens, r.CostCNYMilli)
	}
	if len(rows) != 2 {
		fail("费用行 %d 条， want 2（调用行 + 汇总行）", len(rows))
	}
	// 模拟用量：prompt 1000 + completion 300 = 1300 token，7 毫分。
	fmt.Printf("measured (stub-simulated): 1300 tokens, 7 毫分 = ¥0.007\n")
	if e, _ := reg.Entry("cap-ops-true"); e.EntryPoint != "true" {
		fail("内存入口 = %q, want true（自动应用未生效）", e.EntryPoint)
	}
	raw, err := os.ReadFile(regJSONPath)
	if err != nil {
		log.Fatal(err)
	}
	if fmt.Sprintf("%x", md5.Sum(raw)) != regJSONMD5Before {
		fail("registry.json 被改动（只许动内存覆盖层）")
	}

	// ---- 再跑一次 RebuildWeekly ----
	rb, err := loop.RebuildWeekly(context.Background(), date)
	if err != nil {
		fail("RebuildWeekly: %v", err)
	}
	fmt.Printf("rebuild %s: checked=%d ok=%d broken=%d newly_broken=%d newly_ok=%d discovered=%d\n",
		date, rb.Checked, rb.OK, rb.Broken, len(rb.NewlyBroken), len(rb.NewlyOK), len(rb.Discovered))
	// 注意：自查已把 cap-ops-true 的入口自动修成 true，所以重建时两条都通；
	// 若自查未修好，这里应是 ok=1 broken=1。
	if rb.Checked != 2 || rb.OK != 2 || rb.Broken != 0 {
		fail("重建 checked=%d ok=%d broken=%d, want 2/2/0（自查已修好坏入口）", rb.Checked, rb.OK, rb.Broken)
	}
	fmt.Println("selfevolve-verify: ALL PASS")
}
