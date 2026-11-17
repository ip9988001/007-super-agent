package executor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"007-super-agent/internal/ledger"
)

// stubAdvisor 是监工桩：不伪造真实审查，只按预设 verdict 回。
type stubAdvisor struct {
	verdict Verdict
	note    string
	calls   []Trigger
}

func (s *stubAdvisor) CollectEvidenceBundle(task Task, trigger Trigger) (Evidence, error) {
	return Evidence{TaskText: task.Text, Draft: task.Draft}, nil
}

func (s *stubAdvisor) Check(evidence Evidence, trigger Trigger) (CheckResult, error) {
	s.calls = append(s.calls, trigger)
	return CheckResult{Verdict: s.verdict, Note: s.note}, nil
}

func openTestLedger(t *testing.T) *ledger.Ledger {
	t.Helper()
	l, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close(l) })
	return l
}

func newTestExecutor(l *ledger.Ledger, adv Advisor) *Executor {
	return &Executor{Ledger: l, Advisor: adv}
}

// 短任务内联出结果，stdout 正确，账本记了一行。
func TestRunInline(t *testing.T) {
	l := openTestLedger(t)
	ex := newTestExecutor(l, nil)
	r, h, err := ex.Run(RunRequest{
		TaskID: "t-inline", SessionID: "s1",
		Argv: []string{"echo", "hi"}, Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if h != nil {
		t.Fatal("短任务不应返回后台句柄")
	}
	if r.ExitCode != 0 || r.Status != "success" || strings.TrimSpace(r.StdoutTail) != "hi" {
		t.Fatalf("结果不对：%+v", r)
	}
	rows, err := l.ListExecutions("t-inline", 1)
	if err != nil || len(rows) != 1 || rows[0].ExitCode != 0 {
		t.Fatalf("账本未记执行回执：rows=%v err=%v", rows, err)
	}
}

// 长任务后台返回句柄且 30 秒内不等完；Cancel 后 Wait 到失败结果并记账。
func TestRunBackgroundReturnsQuickly(t *testing.T) {
	l := openTestLedger(t)
	ex := newTestExecutor(l, nil)
	start := time.Now()
	_, h, err := ex.Run(RunRequest{
		TaskID: "t-bg", SessionID: "s1",
		Argv: []string{"sleep", "40"}, Timeout: 60 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if h == nil {
		t.Fatal("长任务应返回后台句柄")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("派单即返回被违反：等了 %s", time.Since(start))
	}
	h.Cancel()
	r, err := h.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "failure" {
		t.Fatalf("取消后应为 failure：%+v", r)
	}
	rows, err := l.ListExecutions("t-bg", 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("后台任务未记账：rows=%v err=%v", rows, err)
	}
}

// 超时 1800 秒被杀并记账：把 MaxTimeout 调小，不用真等 1800 秒。
func TestTimeoutKill(t *testing.T) {
	old := MaxTimeout
	MaxTimeout = 2 * time.Second
	defer func() { MaxTimeout = old }()
	l := openTestLedger(t)
	ex := newTestExecutor(l, nil)
	start := time.Now()
	r, h, err := ex.Run(RunRequest{
		TaskID: "t-timeout", SessionID: "s1",
		Argv: []string{"sleep", "30"}, Timeout: 60 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if h != nil {
		t.Fatal("截到 2 秒后应走内联")
	}
	if !r.Killed || r.Status != "failure" || time.Since(start) > 10*time.Second {
		t.Fatalf("超时杀掉未生效：killed=%v status=%s 耗时=%s", r.Killed, r.Status, time.Since(start))
	}
	rows, err := l.ListExecutions("t-timeout", 1)
	if err != nil || len(rows) != 1 || rows[0].Status != "failure" {
		t.Fatalf("超时任务未记账：rows=%v err=%v", rows, err)
	}
}

// 不可逆动作前调了监工接口，NOTE 通过后照常执行，便签记账。
func TestPreActionAdvisorCalled(t *testing.T) {
	l := openTestLedger(t)
	adv := &stubAdvisor{verdict: VerdictNote, note: "路径确认无误"}
	ex := newTestExecutor(l, adv)
	tmp := filepath.Join(t.TempDir(), "del.txt")
	if err := os.WriteFile(tmp, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	r, _, err := ex.Run(RunRequest{
		TaskID: "t-pre", SessionID: "s1",
		Argv: []string{"rm", tmp}, Timeout: 10 * time.Second,
		Irreversible: true, IrreversibleKind: KindDeleteFile, Text: "删临时文件",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(adv.calls) != 1 || adv.calls[0] != TriggerPreAction {
		t.Fatalf("监工前审未调用：calls=%v", adv.calls)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatal("NOTE 通过后文件应已被删")
	}
	if r.AdvisorVerdict != VerdictNote {
		t.Fatalf("便签未随结果记账：%+v", r)
	}
	rows, _ := l.ListExecutions("t-pre", 1)
	if len(rows) != 1 || rows[0].AdvisorVerdict != string(VerdictNote) {
		t.Fatalf("账本 advisor_verdict 不对：%v", rows)
	}
}

// HALT 停止提交：命令不跑，文件还在，账本记 HALT。
func TestHaltBlocksSubmit(t *testing.T) {
	l := openTestLedger(t)
	adv := &stubAdvisor{verdict: VerdictHalt, note: "路径不对，目标是 /tmp/a 不是 /tmp/b"}
	ex := newTestExecutor(l, adv)
	tmp := filepath.Join(t.TempDir(), "keep.txt")
	if err := os.WriteFile(tmp, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	_, _, err := ex.Run(RunRequest{
		TaskID: "t-halt", SessionID: "s1",
		Argv: []string{"rm", tmp}, Timeout: 10 * time.Second,
		Irreversible: true, IrreversibleKind: KindDeleteFile, Text: "删文件",
	})
	if !errors.Is(err, ErrAdvisorHalt) {
		t.Fatalf("应返回 ErrAdvisorHalt，实际 %v", err)
	}
	if _, err := os.Stat(tmp); err != nil {
		t.Fatal("HALT 后文件不应被删")
	}
	rows, _ := l.ListExecutions("t-halt", 1)
	if len(rows) != 1 || rows[0].AdvisorVerdict != string(VerdictHalt) || rows[0].AdvisorRetryCount != 1 {
		t.Fatalf("账本 HALT 记录不对：%v", rows)
	}
}

// HALT 最多打回 2 轮：第 3 次 HALT 即终局。
func TestHaltRetryCap(t *testing.T) {
	l := openTestLedger(t)
	adv := &stubAdvisor{verdict: VerdictHalt, note: "测试不通过"}
	ex := newTestExecutor(l, adv)
	task := Task{TaskID: "t-cap", SessionID: "s1", Text: "交活"}
	for i := 1; i <= 2; i++ {
		res, err := ex.Check(task, TriggerDelivery)
		if !errors.Is(err, ErrAdvisorHalt) || res.RetryCount != i {
			t.Fatalf("第 %d 次 HALT 应返回 ErrAdvisorHalt 且计数 %d，实际 err=%v res=%+v", i, i, err, res)
		}
	}
	res, err := ex.Check(task, TriggerDelivery)
	if !errors.Is(err, ErrAdvisorHaltFinal) || res.RetryCount != 3 {
		t.Fatalf("第 3 次 HALT 应终局，实际 err=%v res=%+v", err, res)
	}
}

// HOME 缺失明确报错，不静默跑下去。
func TestMissingHome(t *testing.T) {
	l := openTestLedger(t)
	ex := newTestExecutor(l, nil)
	ex.Env = []string{"PATH=/usr/bin:/bin"} // 故意缺 HOME
	_, _, err := ex.Run(RunRequest{
		TaskID: "t-env", SessionID: "s1",
		Argv: []string{"echo", "hi"}, Timeout: 10 * time.Second,
	})
	if !errors.Is(err, ErrMissingEnv) {
		t.Fatalf("缺 HOME 应返回 ErrMissingEnv，实际 %v", err)
	}
}

// stdout 只留末尾 20 行，stderr 开头截 600 字。
func TestStdoutTailAndErrHead(t *testing.T) {
	l := openTestLedger(t)
	ex := newTestExecutor(l, nil)
	script := `for i in $(seq 1 30); do echo "line$i"; done; ` +
		`i=0; while [ $i -lt 800 ]; do printf '错' >&2; i=$((i+1)); done`
	r, _, err := ex.Run(RunRequest{
		TaskID: "t-tail", SessionID: "s1",
		Argv: []string{"sh", "-c", script}, Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(r.StdoutTail, "\n"), "\n")
	if len(lines) != 20 || lines[0] != "line11" || lines[19] != "line30" {
		t.Fatalf("stdout 末尾 20 行不对：%d 行，首=%q 尾=%q", len(lines), lines[0], lines[len(lines)-1])
	}
	if len([]rune(r.ErrHead)) != 600 {
		t.Fatalf("stderr 应截到 600 字，实际 %d 字", len([]rune(r.ErrHead)))
	}
}

// TestReviewShardRecordsStage 是 B-06 的 Stage 落盘：分片审通过后，
// 该 Stage 的进度（stage_no + 产物路径）写进 execution_ledger，
// LastStage 能查到，重跑从下一 Stage 接续。
func TestReviewShardRecordsStage(t *testing.T) {
	l := openTestLedger(t)
	ex := newTestExecutor(l, &stubAdvisor{verdict: VerdictNote, note: "ok"})
	res, err := ex.ReviewShard(Task{
		TaskID: "t-stage", SessionID: "s1", ToolID: "tool-a",
		Text: "long job", Command: "run stage 2",
		StageNo: 2, ArtifactPath: "/tmp/stage2.out",
	})
	if err != nil || res.Verdict != VerdictNote {
		t.Fatalf("ReviewShard = %+v, %v", res, err)
	}
	n, p, err := ex.LastStage("t-stage", "tool-a")
	if err != nil || n != 2 || p != "/tmp/stage2.out" {
		t.Fatalf("LastStage = (%d, %q), err = %v, want (2, /tmp/stage2.out)", n, p, err)
	}
}

// TestReviewShardHaltDoesNotRecordStage：分片被 HALT 打回时不落盘。
func TestReviewShardHaltDoesNotRecordStage(t *testing.T) {
	l := openTestLedger(t)
	ex := newTestExecutor(l, &stubAdvisor{verdict: VerdictHalt, note: "no"})
	_, err := ex.ReviewShard(Task{
		TaskID: "t-halt", SessionID: "s1", ToolID: "tool-a",
		Text: "long job", Command: "run stage 1",
		StageNo: 1, ArtifactPath: "/tmp/stage1.out",
	})
	if err == nil {
		t.Fatal("HALT should return an error")
	}
	n, _, err := ex.LastStage("t-halt", "tool-a")
	if err != nil || n != 0 {
		t.Fatalf("LastStage = %d, err = %v, want 0 (no stage recorded)", n, err)
	}
}
