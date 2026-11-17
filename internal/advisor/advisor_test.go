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

// stubModel 是一台本地桩模型：按请求里的证据内容回 verdict。
// 请求 JSON 里含 does_not_assert_fixed_field → HALT；否则 NOTE。
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
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}]}`,
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
	ev, err := adv.CollectEvidenceBundle(task)
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
