package advisor

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"007-super-agent/internal/executor"
	"007-super-agent/internal/ledger"

	_ "modernc.org/sqlite"
)

// stubModel 是一台本地桩模型：按请求里的证据内容回 verdict，
// 并回固定的 usage（供费用记账测试用）。
func stubModel(t *testing.T, haltReason string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		verdict, reason := "NOTE", "干净：测试覆盖了改动的字段"
		for _, m := range payload.Messages {
			if strings.Contains(m.Content, "does_not_assert_fixed_field") {
				verdict, reason = "HALT", haltReason
				break
			}
		}
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}],"usage":{"prompt_tokens":1100000,"prompt_cache_hit_tokens":1000000,"prompt_cache_miss_tokens":100000,"completion_tokens":50000}}`,
			fmt.Sprintf(`{"verdict":%q,"reason":%q,"fix_directive":"给测试补上对该字段的断言"}`, verdict, reason))
	}))
}

func TestCollectEvidenceBundle(t *testing.T) {
	dir := t.TempDir()
	// 临时 git 仓库：提交一次，再改一行，diff 非空
	run := func(name string, args ...string) {
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v (%s)", name, args, err, out)
		}
	}
	run("git", "init", "-q")
	run("git", "config", "user.email", "t@t")
	run("git", "config", "user.name", "t")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("line1\n"), 0600)
	run("git", "add", ".")
	run("git", "commit", "-qm", "init")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("line1\nline2-fixed-bug-field\n"), 0600)
	// 临时账本：写一行执行记录
	dbPath := filepath.Join(dir, "ledger.db")
	led, err := ledger.Open(dbPath)
	if err != nil {
		t.Fatalf("ledger open: %v", err)
	}
	if _, err := led.RecordExecution(ledger.ExecutionRecord{
		TaskID: "t1", ToolID: "cap-code-semgrep", Command: "semgrep --limit 5",
		ExitCode: 0, Status: "success",
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	ledger.Close(led)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql open: %v", err)
	}
	defer db.Close()
	b, err := CollectEvidenceBundle(context.Background(), db, EvidenceRequest{
		TaskID: "t1", TaskPrompt: "修 amount 字段校验", WorkDir: dir,
		ArtifactPaths: []string{filepath.Join(dir, "a.txt")}, ProposedOutput: "已修",
	})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if !strings.Contains(b.GitDiff, "line2-fixed-bug-field") {
		t.Fatalf("GitDiff 没抓到改动: %q", b.GitDiff)
	}
	if !strings.Contains(b.LedgerCommands, "semgrep") {
		t.Fatalf("LedgerCommands 没抓到命令: %q", b.LedgerCommands)
	}
	if !strings.Contains(b.ArtifactStats, "a.txt: size=") {
		t.Fatalf("ArtifactStats 没抓到指纹: %q", b.ArtifactStats)
	}
	if b.TotalTokensEst > 3000 {
		t.Fatalf("证据包超 3000 token: %d", b.TotalTokensEst)
	}
}

func TestRunAdvisorCheckVerdicts(t *testing.T) {
	srv := stubModel(t, "假测试：改了 amount 校验，测试没断言该字段")
	defer srv.Close()
	cfg := AdvisorConfig{Endpoint: srv.URL, APIKey: "test-key", Model: "stub", Timeout: 10 * time.Second}
	// 干净证据包 -> NOTE
	dec, err := RunAdvisorCheck(context.Background(), cfg, EvidenceBundle{TaskAndCriteria: "修 bug"}, 0, "")
	if err != nil || dec.Verdict != "NOTE" || dec.ShouldBlock {
		t.Fatalf("clean: dec=%+v err=%v", dec, err)
	}
	// 带假测试的证据包 -> HALT，第一轮 ShouldBlock
	fake := EvidenceBundle{TaskAndCriteria: "修 bug", GitDiff: "does_not_assert_fixed_field"}
	dec, err = RunAdvisorCheck(context.Background(), cfg, fake, 0, "")
	if err != nil || dec.Verdict != "HALT" || !dec.ShouldBlock || dec.ShouldEscalate {
		t.Fatalf("fake round1: dec=%+v err=%v", dec, err)
	}
	// 同因第二轮 -> ShouldEscalate
	dec, err = RunAdvisorCheck(context.Background(), cfg, fake, 1, "假测试：改了 amount 校验，测试没断言该字段")
	if err != nil || !dec.ShouldEscalate || dec.ShouldBlock {
		t.Fatalf("fake round2 same cause: dec=%+v err=%v", dec, err)
	}
	// 打回封顶（retryCount=2）-> ShouldEscalate
	dec, err = RunAdvisorCheck(context.Background(), cfg, fake, 2, "另一个原因")
	if err != nil || !dec.ShouldEscalate || dec.ShouldBlock {
		t.Fatalf("fake at cap: dec=%+v err=%v", dec, err)
	}
}

func TestAdapterHaltFlow(t *testing.T) {
	srv := stubModel(t, "假测试：改了 amount 校验，测试没断言该字段")
	defer srv.Close()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ledger.db")
	led, err := ledger.Open(dbPath)
	if err != nil {
		t.Fatalf("ledger open: %v", err)
	}
	defer ledger.Close(led)
	adv, err := New(dbPath, AdvisorConfig{Endpoint: srv.URL, APIKey: "test-key", Model: "stub", Timeout: 10 * time.Second}, dir)
	if err != nil {
		t.Fatalf("advisor new: %v", err)
	}
	defer adv.Close()
	task := executor.Task{TaskID: "t9", Text: "修 amount 字段校验", Draft: "已修"}
	// 假测试证据：先收证据（WorkDir 是空 git 目录，diff 为空串占位），再手填 Diff 走 Check
	ev, err := adv.CollectEvidenceBundle(task, executor.TriggerDelivery)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	ev.Diff = "does_not_assert_fixed_field"
	res1, err := adv.Check(ev, executor.TriggerDelivery)
	if err != nil || res1.Verdict != executor.VerdictHalt || res1.RetryCount != 1 {
		t.Fatalf("check1: res=%+v err=%v", res1, err)
	}
	if strings.Contains(res1.Note, "ESCALATE") {
		t.Fatalf("check1 不应早退: %q", res1.Note)
	}
	res2, err := adv.Check(ev, executor.TriggerDelivery)
	if err != nil || res2.Verdict != executor.VerdictHalt || !strings.Contains(res2.Note, "ESCALATE") {
		t.Fatalf("check2 同因二连应早退: res=%+v err=%v", res2, err)
	}
}

// TestAdvisorExpenseRecorded：第 13 步——监工调用的费用进 expense_ledger，
// 模型名、hit/miss、费用毫分齐全，按任务归属。
func TestAdvisorExpenseRecorded(t *testing.T) {
	srv := stubModel(t, "x")
	defer srv.Close()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ledger.db")
	led, err := ledger.Open(dbPath)
	if err != nil {
		t.Fatalf("ledger open: %v", err)
	}
	defer ledger.Close(led)
	adv, err := New(dbPath, AdvisorConfig{Endpoint: srv.URL, APIKey: "test-key", Model: "deepseek-reasoner", Timeout: 10 * time.Second}, dir)
	if err != nil {
		t.Fatalf("advisor new: %v", err)
	}
	defer adv.Close()
	adv.Ledger = led
	task := executor.Task{TaskID: "t-exp", SessionID: "s9", Text: "修 bug", Draft: "已修"}
	ev, err := adv.CollectEvidenceBundle(task, executor.TriggerDelivery)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if _, err := adv.Check(ev, executor.TriggerDelivery); err != nil {
		t.Fatalf("check: %v", err)
	}
	// 桩 usage：hit 1000000 / miss 100000 / out 50000，按 Flash 价
	// (1000000*20 + 100000*1000 + 50000*4000 + 500000) / 1000000 = 320 毫分。
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var model, taskID, sessionID string
	var hit, miss, out, cost int64
	err = db.QueryRow(`SELECT model, task_id, session_id, cache_hit_tokens,
		cache_miss_tokens, completion_tokens, cost_cny_milli
		FROM expense_ledger ORDER BY id DESC LIMIT 1`).
		Scan(&model, &taskID, &sessionID, &hit, &miss, &out, &cost)
	if err != nil {
		t.Fatalf("query expense: %v", err)
	}
	if model != "deepseek-reasoner" || taskID != "t-exp" || sessionID != "s9" ||
		hit != 1000000 || miss != 100000 || out != 50000 || cost != 320 {
		t.Errorf("expense row = model=%s task=%s session=%s hit=%d miss=%d out=%d cost=%d, want reasoner/t-exp/s9/1000000/100000/50000/320",
			model, taskID, sessionID, hit, miss, out, cost)
	}
}

// TestPreActionBundleCarriesCommand：第 13 步——pre-action 预审证据包
// 必须带拟执行命令与目标文件指纹；delivery 不带预审备注。
func TestPreActionBundleCarriesCommand(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ledger.db")
	led, err := ledger.Open(dbPath)
	if err != nil {
		t.Fatalf("ledger open: %v", err)
	}
	defer ledger.Close(led)
	adv, err := New(dbPath, AdvisorConfig{Endpoint: "http://127.0.0.1:1", APIKey: "test-key", Model: "stub", Timeout: time.Second}, dir)
	if err != nil {
		t.Fatalf("advisor new: %v", err)
	}
	defer adv.Close()
	target := filepath.Join(dir, "del.txt")
	if err := os.WriteFile(target, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	task := executor.Task{
		TaskID: "t-pre", SessionID: "s9", Text: "删临时文件",
		Command: "rm -f " + target, Irreversible: true,
		IrreversibleKind: executor.KindDeleteFile,
	}
	ev, err := adv.CollectEvidenceBundle(task, executor.TriggerPreAction)
	if err != nil {
		t.Fatalf("collect pre-action: %v", err)
	}
	for _, want := range []string{"拟执行命令", "rm -f " + target, "【预审】", "不验证完成度"} {
		if !strings.Contains(ev.TaskText, want) {
			t.Errorf("pre-action TaskText %q missing %q", ev.TaskText, want)
		}
	}
	if !strings.Contains(ev.ArtifactFingerprint, "del.txt: size=") {
		t.Errorf("pre-action ArtifactFingerprint %q missing target stat", ev.ArtifactFingerprint)
	}
	// delivery 触发不带预审备注与命令。
	ev2, err := adv.CollectEvidenceBundle(task, executor.TriggerDelivery)
	if err != nil {
		t.Fatalf("collect delivery: %v", err)
	}
	if strings.Contains(ev2.TaskText, "拟执行命令") || strings.Contains(ev2.TaskText, "【预审】") {
		t.Errorf("delivery TaskText %q must not carry pre-action note", ev2.TaskText)
	}
}

// TestDeliveryBundleCarriesWorkDirListing：交活前审的证据包必须带上工作目录
// 落盘文件的指纹（2026-11-18 ping0 第三轮：监工因 artifact_stats 为空而误判无落盘文件）。
func TestDeliveryBundleCarriesWorkDirListing(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ledger.db")
	led, err := ledger.Open(dbPath)
	if err != nil {
		t.Fatalf("ledger open: %v", err)
	}
	defer ledger.Close(led)
	adv, err := New(dbPath, AdvisorConfig{Endpoint: "http://127.0.0.1:1", APIKey: "test-key", Model: "stub", Timeout: time.Second}, dir)
	if err != nil {
		t.Fatalf("advisor new: %v", err)
	}
	defer adv.Close()
	wd := filepath.Join(dir, "workspace", "t1")
	if err := os.MkdirAll(wd, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wd, "home.html"), []byte("<html>"), 0600); err != nil {
		t.Fatal(err)
	}
	task := executor.Task{
		TaskID: "t-del", SessionID: "s9", Text: "抓个站", Draft: "做完了",
		WorkDir: wd,
	}
	ev, err := adv.CollectEvidenceBundle(task, executor.TriggerDelivery)
	if err != nil {
		t.Fatalf("collect delivery: %v", err)
	}
	if !strings.Contains(ev.ArtifactFingerprint, "home.html: size=") {
		t.Errorf("delivery ArtifactFingerprint %q missing workdir file stat", ev.ArtifactFingerprint)
	}
	// 不存在的目录：指纹里要有 MISSING，监工才能看到工作目录根本没建。
	task2 := executor.Task{
		TaskID: "t-del2", SessionID: "s9", Text: "抓个站", Draft: "做完了",
		WorkDir: filepath.Join(dir, "workspace", "t-none"),
	}
	ev2, err := adv.CollectEvidenceBundle(task2, executor.TriggerDelivery)
	if err != nil {
		t.Fatalf("collect delivery missing dir: %v", err)
	}
	if !strings.Contains(ev2.ArtifactFingerprint, "MISSING") {
		t.Errorf("delivery ArtifactFingerprint %q must mark missing workdir", ev2.ArtifactFingerprint)
	}
}

// TestEnsureJSONWord: json_object 模式要求消息里出现 "json" 这个词
// （DeepSeek API 侧校验，缺了直接 400）；2026-11-24 验收发现定稿
// 提示词文件里没有这个词，监工每次调用都失败。
func TestEnsureJSONWord(t *testing.T) {
	if got := ensureJSONWord("你是监工。", `{"task_and_criteria":"x"}`); !strings.Contains(strings.ToLower(got), "json") {
		t.Fatalf("无 json 词时应补一句，得到 %q", got)
	}
	in := "只输出合法 JSON：{}"
	if got := ensureJSONWord(in, "{}"); got != in {
		t.Fatalf("已有 JSON 词时不应动，得到 %q", got)
	}
	if got := ensureJSONWord("你是监工。", `{"note":"json"}`); got != "你是监工。" {
		t.Fatalf("user 里有 json 词时不应动，得到 %q", got)
	}
}

// TestRunAdvisorCheckSendsJSONWord: 即使配置的 SysPrompt 里没有
// "json"，发出的请求也必须含这个词（否则真实 DeepSeek API 会拒绝
// json_object 模式），且 response_format 保持不变。
func TestRunAdvisorCheckSendsJSONWord(t *testing.T) {
	var gotSys, gotFmt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			ResponseFormat map[string]string `json:"response_format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("解请求失败: %v", err)
		}
		for _, m := range payload.Messages {
			if m.Role == "system" {
				gotSys = m.Content
			}
		}
		gotFmt = payload.ResponseFormat["type"]
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}],"usage":{"prompt_tokens":10,"prompt_cache_hit_tokens":0,"prompt_cache_miss_tokens":10,"completion_tokens":5}}`, `{"verdict":"NOTE","reason":"ok","fix_directive":""}`)
	}))
	defer srv.Close()
	cfg := AdvisorConfig{Endpoint: srv.URL, APIKey: "x", Model: "deepseek-flash", Timeout: 10 * time.Second, SysPrompt: "你是监工，只核查逻辑真伪。"}
	dec, err := RunAdvisorCheck(context.Background(), cfg, EvidenceBundle{TaskAndCriteria: "做一件事", ProposedOutput: "已做完"}, 0, "")
	if err != nil {
		t.Fatalf("RunAdvisorCheck: %v", err)
	}
	if dec.Verdict != "NOTE" {
		t.Fatalf("预期 NOTE，得到 %q", dec.Verdict)
	}
	if !strings.Contains(strings.ToLower(gotSys), "json") {
		t.Fatalf("system 消息缺 json 词：%q", gotSys)
	}
	if gotFmt != "json_object" {
		t.Fatalf("response_format 应保持 json_object，得到 %q", gotFmt)
	}
}

// TestAdapterHaltWithoutReasonDowngradesToConcern：监工回 HALT 但
// reason 与 fix_directive 均为空时，不能拿空意见打回重跑（模型只能
// 盲目重跑烧钱），应降级为 CONCERN 并写明原因，信号进账本。
// （2026-11-24 线 B 验收 s5-s2：两次空原因 HALT 导致两轮盲目重跑。）
func TestAdapterHaltWithoutReasonDowngradesToConcern(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}],"usage":{"prompt_tokens":100,"prompt_cache_hit_tokens":0,"prompt_cache_miss_tokens":100,"completion_tokens":10}}`,
			`{"verdict":"HALT","reason":"","fix_directive":""}`)
	}))
	defer srv.Close()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ledger.db")
	led, err := ledger.Open(dbPath)
	if err != nil {
		t.Fatalf("ledger open: %v", err)
	}
	defer ledger.Close(led)
	adv, err := New(dbPath, AdvisorConfig{Endpoint: srv.URL, APIKey: "test-key", Model: "stub", Timeout: 10 * time.Second}, dir)
	if err != nil {
		t.Fatalf("advisor new: %v", err)
	}
	defer adv.Close()
	task := executor.Task{TaskID: "t-noreason", Text: "修 bug", Draft: "已修"}
	ev, err := adv.CollectEvidenceBundle(task, executor.TriggerDelivery)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	res, err := adv.Check(ev, executor.TriggerDelivery)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if res.Verdict != executor.VerdictConcern {
		t.Fatalf("空原因 HALT 应降级为 CONCERN，got verdict=%s note=%q", res.Verdict, res.Note)
	}
	if !strings.Contains(res.Note, "未给原因") {
		t.Fatalf("note 应写明未给原因，got %q", res.Note)
	}
	if res.RetryCount != 0 {
		t.Fatalf("降级后不应计 HALT 次数，got retry=%d", res.RetryCount)
	}
}
