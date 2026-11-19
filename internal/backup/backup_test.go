package backup

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"007-super-agent/internal/ledger"

	_ "modernc.org/sqlite"
)

// fakeGit 是测试用的 git 假实现：按 handler 决定每次调用的返回，并记下调用顺序
// 与每次调用拿到的 env（供断言 pull/push 是否带上 askpass 认证环境）。
type fakeGit struct {
	calls   []string
	envs    [][]string
	handler func(args []string) (string, error)
}

func (f *fakeGit) run(_ context.Context, _ string, args []string, env []string) (string, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	f.envs = append(f.envs, env)
	if f.handler != nil {
		return f.handler(args)
	}
	return "", nil
}

// envFor 返回第一次调用中命令包含 substr 那次拿到的 env（没有则返回 nil）。
func (f *fakeGit) envFor(substr string) []string {
	for i, c := range f.calls {
		if strings.Contains(c, substr) {
			return f.envs[i]
		}
	}
	return nil
}

func envHas(env []string, prefix string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

func (f *fakeGit) has(substr string) bool {
	for _, c := range f.calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

func (f *fakeGit) count(substr string) int {
	n := 0
	for _, c := range f.calls {
		if strings.Contains(c, substr) {
			n++
		}
	}
	return n
}

// testLedger 建一个临时账本并返回路径。
func testLedger(t *testing.T) (*ledger.Ledger, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	led, err := ledger.Open(dbPath)
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	return led, dbPath
}

// ledgerBackupRows 读回 backup-memory 的记账行（task_id, status）。
func ledgerBackupRows(t *testing.T, dbPath string) [][2]string {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT task_id, status FROM execution_ledger WHERE tool_id='backup-memory' ORDER BY id`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, [2]string{a, b})
	}
	return out
}

// testMemoryRoot 造一个记忆根：2 条 wiki + 1 条 L1 + 4 个白名单外诱饵文件。
func testMemoryRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "memory")
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "a.md"), "# a\n")
	mustWrite(t, filepath.Join(root, "wiki", "entities", "b.md"), "# b\n")
	mustWrite(t, filepath.Join(root, "l1", "2026-11-17.log"), "line1\n")
	mustWrite(t, filepath.Join(root, "ledger.db"), "fake-db")
	mustWrite(t, filepath.Join(root, ".env"), "SECRET=1")
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "x.lock"), "lock")
	mustWrite(t, filepath.Join(root, "wiki", "concepts", "notes.txt"), "txt")
	return root
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestDisabledDoesNothing(t *testing.T) {
	led, dbPath := testLedger(t)
	fg := &fakeGit{}
	r := NewRunner(Config{Enabled: false, DataDir: t.TempDir()}, led)
	r.git = fg
	res, err := r.Run(context.Background(), "2026-11-17")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Skipped {
		t.Fatalf("期望 Skipped=true，得 %+v", res)
	}
	if len(fg.calls) != 0 {
		t.Fatalf("开关关闭时不应调 git，调用：%v", fg.calls)
	}
	if rows := ledgerBackupRows(t, dbPath); len(rows) != 0 {
		t.Fatalf("开关关闭时不应记账，行：%v", rows)
	}
}

func TestRemoteMismatchRefuses(t *testing.T) {
	led, _ := testLedger(t)
	fg := &fakeGit{handler: func(args []string) (string, error) {
		if len(args) >= 3 && args[0] == "remote" {
			return "https://github.com/other/public.git\n", nil
		}
		return "", nil
	}}
	repoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o700); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	r := NewRunner(Config{Enabled: true, DataDir: t.TempDir(), RepoDir: repoDir}, led)
	r.git = fg
	_, err := r.Run(context.Background(), "2026-11-17")
	if err == nil || !strings.Contains(err.Error(), "不是指定的私密仓库") {
		t.Fatalf("期望 remote 不匹配被拒绝，得 err=%v", err)
	}
	if fg.has("push") {
		t.Fatalf("remote 不匹配时绝不能 push，调用：%v", fg.calls)
	}
}

func TestParseGitHubRepo(t *testing.T) {
	cases := []struct {
		raw     string
		want    string
		wantHTTP bool
		wantOK  bool
	}{
		{"https://github.com/ip9988001/super-agent-007.git", "ip9988001/super-agent-007", true, true},
		{"https://github.com/ip9988001/super-agent-007", "ip9988001/super-agent-007", true, true},
		{"https://ip9988001@github.com/ip9988001/super-agent-007.git", "ip9988001/super-agent-007", true, true},
		{"https://user@github.com/ip9988001/007-super-agent", "ip9988001/007-super-agent", true, true},
		{"https://gitlab.com/ip9988001/super-agent-007.git", "", false, false},
		{"git@github.com:ip9988001/super-agent-007.git", "ip9988001/super-agent-007", false, true},
		{"ssh://git@github.com/ip9988001/007-super-agent.git", "ip9988001/007-super-agent", false, true},
		{"/tmp/some-bare.git", "", false, false},
		{"https://github.com/onlyowner", "", false, false},
		{"", "", false, false},
	}
	for _, c := range cases {
		got, isHTTP, ok := parseGitHubRepo(c.raw)
		if got != c.want || isHTTP != c.wantHTTP || ok != c.wantOK {
			t.Errorf("parseGitHubRepo(%q) = (%q,%v,%v)，期望 (%q,%v,%v)", c.raw, got, isHTTP, ok, c.want, c.wantHTTP, c.wantOK)
		}
	}
}

func TestAskpassScriptHasNoToken(t *testing.T) {
	t.Setenv("GITHUB_API_TOKEN", "SECRET-TOKEN-123")
	p, err := writeAskpass()
	if err != nil {
		t.Fatalf("writeAskpass: %v", err)
	}
	defer os.Remove(p)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read askpass: %v", err)
	}
	if st, err := os.Stat(p); err != nil {
		t.Fatalf("stat askpass: %v", err)
	} else if st.Mode().Perm()&0111 == 0 {
		t.Fatalf("askpass 脚本必须可执行（git 要 exec 它），当前权限 %04o", st.Mode().Perm())
	}
	if strings.Contains(string(b), "SECRET-TOKEN-123") {
		t.Fatalf("askpass 脚本里绝不能出现 Token 值：%q", string(b))
	}
	if !strings.Contains(string(b), "SUPER_AGENT_GIT_TOKEN") {
		t.Fatalf("askpass 脚本应从环境变量读 Token：%q", string(b))
	}
}

func TestSanitizeRedactsToken(t *testing.T) {
	t.Setenv("GITHUB_API_TOKEN", "SECRET-TOKEN-123")
	r := NewRunner(Config{Enabled: true}, nil)
	got := r.sanitize("push 失败：token SECRET-TOKEN-123 被拒")
	if strings.Contains(got, "SECRET-TOKEN-123") {
		t.Fatalf("sanitize 未抹掉 Token：%q", got)
	}
	if !strings.Contains(got, "[redacted]") {
		t.Fatalf("sanitize 应替换为 [redacted]：%q", got)
	}
}

// fakeRunEnv 搭一个能走完 Run 全程的假 git 环境：
// get-url 返回本地远端；无上游（跳过 pull）；暂存区有变更；push 按 pushFails 次数失败。
func fakeRunEnv(pushFails int) *fakeGit {
	pushes := 0
	return &fakeGit{handler: func(args []string) (string, error) {
		switch args[0] {
		case "remote":
			return "/tmp/fake-bare.git\n", nil
		case "symbolic-ref":
			return "main\n", nil
		case "rev-parse":
			if len(args) > 1 && args[1] == "--abbrev-ref" && len(args) > 2 && args[2] == "HEAD" {
				return "main\n", nil
			}
			return "", fmt.Errorf("exit status 128") // 无上游
		case "diff":
			return "", fmt.Errorf("exit status 1") // 有变更
		case "push":
			pushes++
			if pushes <= pushFails {
				return "", fmt.Errorf("exit status 128: Connection timed out")
			}
			return "", nil
		}
		return "", nil
	}}
}

func TestPushRetriesOnceThenSucceeds(t *testing.T) {
	led, dbPath := testLedger(t)
	fg := fakeRunEnv(1)
	memRoot := testMemoryRoot(t)
	dataDir := filepath.Dir(memRoot)
	repoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o700); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	r := NewRunner(Config{Enabled: true, DataDir: dataDir, RepoDir: repoDir, AllowLocalRemote: true}, led)
	r.git = fg
	res, err := r.Run(context.Background(), "2026-11-17")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Committed || res.Files != 3 {
		t.Fatalf("期望 Committed 且 Files=3，得 %+v", res)
	}
	if fg.count("push origin main") != 2 {
		t.Fatalf("push 应恰好调 2 次（首次 + 重试 1 次），调用：%v", fg.calls)
	}
	if fg.has("pull --rebase") {
		t.Fatalf("无上游时应跳过 pull，调用：%v", fg.calls)
	}
	rows := ledgerBackupRows(t, dbPath)
	if len(rows) != 1 || rows[0][1] != "success" {
		t.Fatalf("期望 1 行 success 记账，得 %v", rows)
	}
}

func TestPushFailsTwiceRecordsFailure(t *testing.T) {
	led, dbPath := testLedger(t)
	fg := fakeRunEnv(5) // 永远失败
	memRoot := testMemoryRoot(t)
	dataDir := filepath.Dir(memRoot)
	repoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o700); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	r := NewRunner(Config{Enabled: true, DataDir: dataDir, RepoDir: repoDir, AllowLocalRemote: true}, led)
	r.git = fg
	_, err := r.Run(context.Background(), "2026-11-17")
	if err == nil {
		t.Fatalf("期望 push 两次失败后返回错误")
	}
	if fg.count("push origin main") != 2 {
		t.Fatalf("push 应恰好调 2 次，调用：%v", fg.calls)
	}
	rows := ledgerBackupRows(t, dbPath)
	if len(rows) != 1 || rows[0][0] != "backup-memory-2026-11-17" || rows[0][1] != "failure" {
		t.Fatalf("期望 1 行 failure 记账，得 %v", rows)
	}
}

func TestPullRunsBeforePushWhenUpstreamExists(t *testing.T) {
	led, _ := testLedger(t)
	fg := fakeRunEnv(0)
	// 让 rev-parse @{u} 成功（有上游），pull 成功。
	orig := fg.handler
	fg.handler = func(args []string) (string, error) {
		if args[0] == "rev-parse" && len(args) > 2 && args[2] == "--symbolic-full-name" {
			return "origin/main\n", nil
		}
		return orig(args)
	}
	memRoot := testMemoryRoot(t)
	dataDir := filepath.Dir(memRoot)
	repoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o700); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	r := NewRunner(Config{Enabled: true, DataDir: dataDir, RepoDir: repoDir, AllowLocalRemote: true}, led)
	r.git = fg
	if _, err := r.Run(context.Background(), "2026-11-17"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	pullIdx, pushIdx := -1, -1
	for i, c := range fg.calls {
		if strings.Contains(c, "pull --rebase --autostash") && pullIdx < 0 {
			pullIdx = i
		}
		if strings.Contains(c, "push origin main") && pushIdx < 0 {
			pushIdx = i
		}
	}
	if pullIdx < 0 || pushIdx < 0 || pullIdx > pushIdx {
		t.Fatalf("有上游时 pull 必须在 push 之前，调用：%v", fg.calls)
	}
}

// TestPullGetsAskpassEnvForHTTPRemote：回归测试——HTTP 私密远端时，pull 与 push
// 都必须拿到 askpass 认证环境。第 15 步原版只给 push 配了 askpass，pull 是裸调，
// 2026-11-19 在测试机 main 分支上实测失败：
// "could not read Username for 'https://github.com'"。
func TestPullGetsAskpassEnvForHTTPRemote(t *testing.T) {
	t.Setenv("GITHUB_API_TOKEN", "SECRET-TOKEN-123")
	led, _ := testLedger(t)
	fg := fakeRunEnv(0)
	// HTTP 私密远端（与默认仓库名一致）+ 有上游，pull 必须跑。
	orig := fg.handler
	fg.handler = func(args []string) (string, error) {
		if args[0] == "remote" {
			return "https://github.com/ip9988001/super-agent-007.git\n", nil
		}
		if args[0] == "rev-parse" && len(args) > 2 && args[2] == "--symbolic-full-name" {
			return "origin/main\n", nil
		}
		return orig(args)
	}
	memRoot := testMemoryRoot(t)
	dataDir := filepath.Dir(memRoot)
	repoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o700); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	// 不开 AllowLocalRemote，走真实 HTTP 远端路径。
	r := NewRunner(Config{Enabled: true, DataDir: dataDir, RepoDir: repoDir}, led)
	r.git = fg
	res, err := r.Run(context.Background(), "2026-11-17")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Committed {
		t.Fatalf("期望 Committed，得 %+v", res)
	}
	pullEnv := fg.envFor("pull --rebase --autostash")
	if !envHas(pullEnv, "GIT_ASKPASS=") {
		t.Fatalf("pull 必须带 GIT_ASKPASS")
	}
	if !envHas(pullEnv, "SUPER_AGENT_GIT_TOKEN=") {
		t.Fatalf("pull 必须带 SUPER_AGENT_GIT_TOKEN")
	}
	pushEnv := fg.envFor("push origin main")
	if !envHas(pushEnv, "GIT_ASKPASS=") {
		t.Fatalf("push 必须带 GIT_ASKPASS")
	}
}

func TestNotAGitRepoFails(t *testing.T) {
	led, dbPath := testLedger(t)
	r := NewRunner(Config{Enabled: true, DataDir: t.TempDir(), RepoDir: t.TempDir()}, led)
	_, err := r.Run(context.Background(), "2026-11-17")
	if err == nil || !strings.Contains(err.Error(), "不是 git 仓库") {
		t.Fatalf("期望非仓库报错，得 err=%v", err)
	}
	rows := ledgerBackupRows(t, dbPath)
	if len(rows) != 1 || rows[0][1] != "failure" {
		t.Fatalf("期望 1 行 failure 记账，得 %v", rows)
	}
}

// gitOut 在指定目录跑 git 命令（测试用真实 git）。
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out))
}

func TestRealGitEndToEnd(t *testing.T) {
	ctx := context.Background()
	// 远端：裸库；工作区：克隆。
	bare := filepath.Join(t.TempDir(), "remote.git")
	gitOut(t, t.TempDir(), "init", "--bare", bare)
	workParent := t.TempDir()
	work := filepath.Join(workParent, "work")
	gitOut(t, workParent, "clone", bare, work)

	led, dbPath := testLedger(t)
	memRoot := testMemoryRoot(t)
	dataDir := filepath.Dir(memRoot)
	r := NewRunner(Config{
		Enabled: true, DataDir: dataDir, RepoDir: work,
		ServerID: "test-server-1", AllowLocalRemote: true,
	}, led)
	// 用真实时钟：标记写开始时间（RFC3339 秒精度），后写的文件 mtime
	// 严格晚于标记；同秒内早写的文件即使被重复列为候选，git 也会因无
	// 变更而跳过，不影响断言。

	// 第 1 次：应提交 3 个文件（2 wiki + 1 L1），诱饵绝不能进。
	res, err := r.Run(ctx, "2026-11-17")
	if err != nil {
		t.Fatalf("第 1 次 Run: %v", err)
	}
	if !res.Committed || res.Files != 3 {
		t.Fatalf("第 1 次期望 Committed 且 Files=3，得 %+v", res)
	}
	tree := gitOut(t, work, "--git-dir", bare, "ls-tree", "-r", "--name-only", "HEAD")
	want := []string{
		"memories/.gitignore",
		"memories/test-server-1/l1/2026-11-17.log",
		"memories/test-server-1/wiki/concepts/a.md",
		"memories/test-server-1/wiki/entities/b.md",
	}
	for _, w := range want {
		if !strings.Contains(tree, w) {
			t.Fatalf("远端缺文件 %s，树：\n%s", w, tree)
		}
	}
	for _, bad := range []string{"ledger.db", ".env", "x.lock", "notes.txt"} {
		if strings.Contains(tree, bad) {
			t.Fatalf("白名单外文件 %s 绝不能进远端，树：\n%s", bad, tree)
		}
	}
	gi := gitOut(t, work, "--git-dir", bare, "show", "HEAD:memories/.gitignore")
	for _, pat := range []string{"*.db*", ".env", "*.lock"} {
		if !strings.Contains(gi, pat) {
			t.Fatalf("memories/.gitignore 缺兜底 %s：\n%s", pat, gi)
		}
	}
	if n := gitOut(t, work, "--git-dir", bare, "rev-list", "--count", "HEAD"); n != "1" {
		t.Fatalf("第 1 次后远端应有 1 个提交，得 %s", n)
	}

	// 第 2 次：无变更，应跳过不提交。
	res, err = r.Run(ctx, "2026-11-17")
	if err != nil {
		t.Fatalf("第 2 次 Run: %v", err)
	}
	if !res.NoChange || res.Committed {
		t.Fatalf("第 2 次期望 NoChange，得 %+v", res)
	}
	if n := gitOut(t, work, "--git-dir", bare, "rev-list", "--count", "HEAD"); n != "1" {
		t.Fatalf("无变更时不应新增提交，得 %s", n)
	}

	// 第 3 次：新增一条 wiki，应再提交一次。
	mustWrite(t, filepath.Join(memRoot, "wiki", "concepts", "c.md"), "# c\n")
	res, err = r.Run(ctx, "2026-11-17")
	if err != nil {
		t.Fatalf("第 3 次 Run: %v", err)
	}
	if !res.Committed {
		t.Fatalf("第 3 次期望 Committed，得 %+v", res)
	}
	if n := gitOut(t, work, "--git-dir", bare, "rev-list", "--count", "HEAD"); n != "2" {
		t.Fatalf("新增文件后远端应有 2 个提交，得 %s", n)
	}
	tree = gitOut(t, work, "--git-dir", bare, "ls-tree", "-r", "--name-only", "HEAD")
	if !strings.Contains(tree, "memories/test-server-1/wiki/concepts/c.md") {
		t.Fatalf("第 3 次的新文件未进远端，树：\n%s", tree)
	}

	// 记账：3 次运行应有 3 行（2 success 提交 + 1 success 跳过）。
	rows := ledgerBackupRows(t, dbPath)
	if len(rows) != 3 {
		t.Fatalf("期望 3 行 backup-memory 记账，得 %v", rows)
	}
	for _, row := range rows {
		if row[1] != "success" {
			t.Fatalf("端到端 3 次运行都应记 success，得 %v", rows)
		}
	}
}
