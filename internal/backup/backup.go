// Package backup 是 007 超级 Agent 的记忆自动备份：第 15 步。
//
// 每天 04:00 精炼成功后，RefineLoop 经钩子调 Runner.Run，把当天新增/改动
// 的记忆文件（wiki/ 与 L1/ 增量文本）推到私密仓库的 memories/<server_id>/
// 目录。公开部署默认关闭——公开库里绝对不能出现记忆与密钥。
//
// 施工避坑（Gemini 第 11 轮，交互-20 第二节），本包全部落实：
//  1. Token 绝不写进 .git/config 的 remote URL：从 600 权限 .env 的指定键名
//     读，推送时用临时 GIT_ASKPASS 脚本（只含 echo 环境变量，不含 Token
//     本身）注入内存；双保险校验——开关显式开启 + 校验 remote 确为指定
//     私密仓库名，防误推公开仓。
//  2. 路径白名单 + 防误推：只复制白名单内的文件（wiki/ 下的 .md、
//     l1/<date>.log），memories/.gitignore 兜底屏蔽 *.db*、.env、锁文件；
//     提交前 git diff --cached --quiet，无变更直接跳过不发空提交。
//  3. 多机冲突：按服务器分目录 memories/<server_id>/；推送前固定先跑
//     git pull --rebase --autostash 拉齐历史再 push。
//  4. 旁路容错与记账：push 最多重试 1 次（间隔 10 秒）；仍失败则把错误摘要
//     写进 execution_ledger 并记一条告警日志，绝不阻塞精炼收尾与 04:30 体检。
//
// 铁律：Token 值永不进日志、不进账本、不进仓库、不进聊天；所有记账与日志
// 文本都过 sanitize，万一沾上 Token 值会被替换成 [redacted]。
package backup

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"007-super-agent/internal/ledger"
)

// 默认值。
const (
	DefaultRepo     = "ip9988001/super-agent-007" // 私密库：框架 + 二号记忆 + 预配密钥
	DefaultTokenKey = "GITHUB_API_TOKEN"          // 只存键名，值在 .env（600 权限）
	DefaultTimeout  = 5 * time.Minute            // 一次备份的最长耗时，超时即失败记账
	PushRetryDelay  = 10 * time.Second           // push 失败后重试间隔
	MaxPushAttempts = 2                          // 首次 + 重试 1 次
)

// gitignoreBlock 是 memories/.gitignore 的兜底屏蔽内容（第 2 点）。
// 注意：绝不能屏蔽 *.log，否则 l1/<date>.log 会被挡在暂存区外。
const gitignoreBlock = `# 007 memory backup blocklist（自动备份兜底，勿删）
*.db*
.db
.env
*.lock
*.tmp
*.bak
*.swp
`

// Config 是记忆备份的配置（全部来自 .env / 环境变量，默认关闭）。
type Config struct {
	Enabled bool // 开关，默认关；只在私密部署上显式打开
	Repo    string // 期望的私密仓库名 "owner/repo"，默认 DefaultRepo
	// ServerID 是本机在 memories/ 下的子目录名；为空时取 hostname。
	ServerID string
	// TokenEnvKey 是读 Token 的环境变量键名（只存键名）；默认 DefaultTokenKey。
	TokenEnvKey string
	// DataDir 是守护进程数据目录；记忆根为 <DataDir>/memory，标记文件放 <DataDir>。
	DataDir string
	// RepoDir 是 git 工作区（部署目录）；为空时取 DataDir 的父目录。
	RepoDir string
	// AllowLocalRemote 仅测试用：允许本地路径远端（不校验仓库名）；生产必须 false。
	AllowLocalRemote bool
}

// Result 是一次备份的结果。
type Result struct {
	Skipped   bool   // 开关关闭，什么都没做
	NoChange  bool   // 暂存区无变更，未提交未推送
	Committed bool   // 已提交并推送成功
	Files     int    // 本次复制进 memories/ 的候选文件数
	Branch    string // 推送的分支
}

// gitRunner 是 git 命令的执行接口；生产用 execRunner，测试可注入假实现。
type gitRunner interface {
	run(ctx context.Context, dir string, args []string, env []string) (string, error)
}

// execRunner 是真实的 git 执行器。
type execRunner struct{}

func (execRunner) run(ctx context.Context, dir string, args []string, env []string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// Runner 是一次记忆备份的执行体。
type Runner struct {
	cfg    Config
	led    *ledger.Ledger
	git    gitRunner
	now    func() time.Time // 可注入的时钟，测试用；为空用北京时间
	server string          // 解析后的 ServerID
	repo   string          // 解析后的期望仓库名
}

// NewRunner 建一个备份执行体。cfg.Repo / TokenEnvKey 为空时补默认值；
// ServerID 为空时取 hostname（取不到则为 "unknown"）。
func NewRunner(cfg Config, led *ledger.Ledger) *Runner {
	if cfg.Repo == "" {
		cfg.Repo = DefaultRepo
	}
	if cfg.TokenEnvKey == "" {
		cfg.TokenEnvKey = DefaultTokenKey
	}
	server := strings.TrimSpace(cfg.ServerID)
	if server == "" {
		if h, err := os.Hostname(); err == nil && strings.TrimSpace(h) != "" {
			server = strings.TrimSpace(h)
		} else {
			server = "unknown"
		}
	}
	return &Runner{cfg: cfg, led: led, git: execRunner{}, server: server, repo: cfg.Repo}
}

// RepoName 返回期望的私密仓库名（供启动日志，只含仓库名，无敏感信息）。
func (r *Runner) RepoName() string { return r.repo }

// ServerID 返回本机在 memories/ 下的子目录名。
func (r *Runner) ServerID() string { return r.server }

// repoDir 返回 git 工作区：配置优先，否则取 DataDir 的父目录。
func (r *Runner) repoDir() string {
	if strings.TrimSpace(r.cfg.RepoDir) != "" {
		return r.cfg.RepoDir
	}
	return filepath.Dir(r.cfg.DataDir)
}

// memoryRoot 返回记忆根目录 <DataDir>/memory。
func (r *Runner) memoryRoot() string { return filepath.Join(r.cfg.DataDir, "memory") }

// markerFile 返回“上次备份成功”的标记文件（放数据目录，内容为 RFC3339 时间）。
func (r *Runner) markerFile() string { return filepath.Join(r.cfg.DataDir, "backup_memory_last") }

// beijingNow 取北京时间；系统无 Asia/Shanghai 时区数据时用固定 +8 区兜底。
func beijingNow() time.Time {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		loc = time.FixedZone("CST", 8*3600)
	}
	return time.Now().In(loc)
}

func (r *Runner) nowFunc() time.Time {
	if r.now != nil {
		return r.now()
	}
	return beijingNow()
}

// sanitize 把文本里万一沾上的 Token 值替换掉（记账/日志前的最后一道防线）。
func (r *Runner) sanitize(s string) string {
	tok := os.Getenv(r.cfg.TokenEnvKey)
	if tok == "" || !strings.Contains(s, tok) {
		return s
	}
	return strings.ReplaceAll(s, tok, "[redacted]")
}

// record 把一次备份结果写进 execution_ledger（ToolID backup-memory）。
// led 为 nil 时跳过；记账失败只记日志，不掩盖备份本身的结果。
func (r *Runner) record(date string, exitCode int, note string) {
	if r.led == nil {
		return
	}
	status := "success"
	if exitCode != 0 {
		status = "failure"
	}
	_, err := r.led.RecordExecution(ledger.ExecutionRecord{
		SessionID: "backup",
		TaskID:    "backup-memory-" + date,
		ToolID:    "backup-memory",
		Command:   "backup memory " + date,
		ExitCode:  exitCode,
		Status:    status,
		StdoutTail: r.sanitize(note),
	})
	if err != nil {
		log.Printf("backup-memory record failed: %v", err)
	}
}

// fail 记录一次失败并返回包装后的错误（错误文本已过 sanitize）。
func (r *Runner) fail(date, note string, err error) (Result, error) {
	msg := r.sanitize(note + ": " + err.Error())
	r.record(date, 1, msg)
	log.Printf("backup-memory ALERT: %s", msg)
	return Result{}, fmt.Errorf("backup-memory %s: %s: %w", date, note, err)
}

// parseGitHubRepo 解析 remote URL：返回 (owner/repo, 是否 http(s), 是否可解析)。
// 接受 https://github.com/o/r(.git)、https://<user>@github.com/o/r(.git)、
// git@github.com:o/r(.git)、ssh://git@github.com/o/r(.git)；
// 本地路径与其他形式返回 ok=false。含 userinfo 的 https 形在解析时剥掉。
func parseGitHubRepo(raw string) (string, bool, bool) {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")
	var rest string
	var isHTTP bool
	switch {
	case strings.HasPrefix(s, "https://"):
		rest, isHTTP = strings.TrimPrefix(s, "https://"), true
	case strings.HasPrefix(s, "http://"):
		rest, isHTTP = strings.TrimPrefix(s, "http://"), true
	case strings.HasPrefix(s, "git@github.com:"):
		rest, isHTTP = strings.TrimPrefix(s, "git@github.com:"), false
	case strings.HasPrefix(s, "ssh://git@github.com/"):
		rest, isHTTP = strings.TrimPrefix(s, "ssh://git@github.com/"), false
	default:
		return "", false, false
	}
	if isHTTP {
		// 剥掉 https://<user>@ 里的 userinfo（只留 host/path）。
		if i := strings.Index(rest, "@"); i >= 0 && !strings.Contains(rest[:i], "/") {
			rest = rest[i+1:]
		}
		if !strings.HasPrefix(rest, "github.com/") {
			return "", false, false
		}
		rest = strings.TrimPrefix(rest, "github.com/")
	}
	if !isOwnerRepo(rest) {
		return "", false, false
	}
	return rest, isHTTP, true
}

func isOwnerRepo(s string) bool {
	parts := strings.Split(s, "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] != ""
}

// verifyRemote 校验 origin：必须是指定的私密仓库（第 1 点双保险之一）。
// 本地路径远端仅在 AllowLocalRemote 时放行（测试用）。
func (r *Runner) verifyRemote(ctx context.Context, repoDir string) (repo string, isHTTP bool, err error) {
	out, err := r.git.run(ctx, repoDir, []string{"remote", "get-url", "origin"}, nil)
	if err != nil {
		return "", false, fmt.Errorf("读 origin 失败: %w", err)
	}
	raw := strings.TrimSpace(out)
	if repo, isHTTP, ok := parseGitHubRepo(raw); ok {
		if !strings.EqualFold(repo, r.repo) {
			return "", false, fmt.Errorf("origin 不是指定的私密仓库（期望 %s），拒绝推送", r.repo)
		}
		return repo, isHTTP, nil
	}
	if r.cfg.AllowLocalRemote {
		return "local-test-remote", false, nil
	}
	return "", false, fmt.Errorf("origin 不是 GitHub 私密仓库地址，拒绝推送")
}

// candidates 列出本次要备份的候选文件（第 2 点白名单）：
//   - L1：<memory>/l1/<date>.log（存在才算）；
//   - Wiki：<memory>/wiki/ 下 ModTime 晚于上次备份标记的 .md 文件
//     （无标记时全量）；
// 白名单外的一切（.db、.env、锁文件、其他后缀）一律不进。
// 返回相对记忆根的路径（如 wiki/concepts/xxx.md、l1/2026-11-17.log）。
func (r *Runner) candidates(date string) ([]string, error) {
	root := r.memoryRoot()
	var since time.Time
	if b, err := os.ReadFile(r.markerFile()); err == nil {
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b))); err == nil {
			since = t
		}
	}
	var out []string
	l1 := filepath.Join(root, "l1", date+".log")
	if st, err := os.Stat(l1); err == nil && !st.IsDir() {
		out = append(out, filepath.Join("l1", date+".log"))
	}
	wikiRoot := filepath.Join(root, "wiki")
	_ = filepath.WalkDir(wikiRoot, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if filepath.Ext(p) != ".md" || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		if !since.IsZero() {
			st, err := d.Info()
			if err != nil || !st.ModTime().After(since) {
				return nil
			}
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		out = append(out, rel)
		return nil
	})
	return out, nil
}

// stageCopy 把候选文件复制进工作区的 memories/<server>/（保持相对路径），
// 并确保 memories/.gitignore 含兜底屏蔽块。返回复制的文件数。
func (r *Runner) stageCopy(repoDir string, rels []string) (int, error) {
	destRoot := filepath.Join(repoDir, "memories", r.server)
	gi := filepath.Join(repoDir, "memories", ".gitignore")
	if b, err := os.ReadFile(gi); err != nil || !strings.Contains(string(b), "007 memory backup blocklist") {
		if err := os.MkdirAll(filepath.Dir(gi), 0o700); err != nil {
			return 0, fmt.Errorf("建 memories 目录失败: %w", err)
		}
		f, err := os.OpenFile(gi, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return 0, fmt.Errorf("写 memories/.gitignore 失败: %w", err)
		}
		if _, err := f.WriteString(gitignoreBlock); err != nil {
			_ = f.Close()
			return 0, fmt.Errorf("写 memories/.gitignore 失败: %w", err)
		}
		_ = f.Close()
	}
	n := 0
	for _, rel := range rels {
		src := filepath.Join(r.memoryRoot(), rel)
		dst := filepath.Join(destRoot, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return n, fmt.Errorf("建目录 %s 失败: %w", filepath.Dir(dst), err)
		}
		b, err := os.ReadFile(src)
		if err != nil {
			return n, fmt.Errorf("读 %s 失败: %w", rel, err)
		}
		if err := os.WriteFile(dst, b, 0o600); err != nil {
			return n, fmt.Errorf("写 %s 失败: %w", rel, err)
		}
		n++
	}
	return n, nil
}

// gitAdd 只把指定路径加进暂存区（显式路径，永不用 git add -A）。
func (r *Runner) gitAdd(ctx context.Context, repoDir string, paths []string) error {
	args := append([]string{"add", "--"}, paths...)
	_, err := r.git.run(ctx, repoDir, args, nil)
	return err
}

// hasStagedChanges 用 git diff --cached --quiet 判断暂存区有无变更：
// exit 0 = 无变更，exit 1 = 有变更，其他错误返回错误。
func (r *Runner) hasStagedChanges(ctx context.Context, repoDir string) (bool, error) {
	_, err := r.git.run(ctx, repoDir, []string{"diff", "--cached", "--quiet"}, nil)
	if err == nil {
		return false, nil
	}
	if strings.Contains(err.Error(), "exit status 1") {
		return true, nil
	}
	return false, fmt.Errorf("git diff --cached 失败: %w", err)
}

// currentBranch 取当前分支名；空库未出生分支时用 symbolic-ref（rev-parse
// 在 HEAD 未出生时报错），detached HEAD 时报错。
func (r *Runner) currentBranch(ctx context.Context, repoDir string) (string, error) {
	out, err := r.git.run(ctx, repoDir, []string{"symbolic-ref", "--short", "HEAD"}, nil)
	if err == nil {
		if br := strings.TrimSpace(out); br != "" && br != "HEAD" {
			return br, nil
		}
	}
	out, err = r.git.run(ctx, repoDir, []string{"rev-parse", "--abbrev-ref", "HEAD"}, nil)
	if err != nil {
		return "", fmt.Errorf("读当前分支失败: %w", err)
	}
	br := strings.TrimSpace(out)
	if br == "" || br == "HEAD" {
		return "", fmt.Errorf("当前为 detached HEAD，无法确定推送分支")
	}
	return br, nil
}

// hasUpstream 判断当前分支是否已设上游。
func (r *Runner) hasUpstream(ctx context.Context, repoDir string) bool {
	_, err := r.git.run(ctx, repoDir, []string{"rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"}, nil)
	return err == nil
}

// writeAskpass 写一个临时 GIT_ASKPASS 脚本：脚本只 echo 环境变量，
// 本身不含 Token 值；用后即删。返回脚本路径。
func writeAskpass() (string, error) {
	f, err := os.CreateTemp("", "sa007-askpass-*.sh")
	if err != nil {
		return "", fmt.Errorf("建临时 askpass 脚本失败: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString("#!/bin/sh\nprintf '%s\\n' \"$SUPER_AGENT_GIT_TOKEN\"\n"); err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("写临时 askpass 脚本失败: %w", err)
	}
	if err := os.Chmod(f.Name(), 0o700); err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("chmod 临时 askpass 脚本失败: %w", err)
	}
	return f.Name(), nil
}

// authEnv 为 git 认证准备环境：HTTP 远端时建临时 GIT_ASKPASS 脚本（只 echo
// 环境变量，本身不含 Token 值）并返回 env + 清理函数；非 HTTP 远端只返回
// GIT_TERMINAL_PROMPT=0。pull 与 push 都必须走这套 env——pull 若无 askpass，
// 私有库 HTTPS 远端会因读不到用户名而失败（第 15 步漏了 pull 的认证，
// 2026-11-19 实测：git pull --rebase 在 main 分支上报
// "could not read Username for 'https://github.com'"）。
func (r *Runner) authEnv(token string, isHTTP bool) (env []string, cleanup func(), err error) {
	env = []string{"GIT_TERMINAL_PROMPT=0"}
	cleanup = func() {}
	if !isHTTP {
		return env, cleanup, nil
	}
	askpass, err := writeAskpass()
	if err != nil {
		return nil, nil, err
	}
	cleanup = func() { os.Remove(askpass) }
	env = append(env, "GIT_ASKPASS="+askpass, "SUPER_AGENT_GIT_TOKEN="+token)
	return env, cleanup, nil
}

// pushWithRetry 推送一次，失败隔 10 秒重试 1 次（第 4 点）。
// env 是 authEnv 给的认证环境（含临时 askpass，HTTP 远端才有）。
func (r *Runner) pushWithRetry(ctx context.Context, repoDir, branch string, env []string) error {
	var lastErr error
	for attempt := 1; attempt <= MaxPushAttempts; attempt++ {
		_, err := r.git.run(ctx, repoDir, []string{"push", "origin", branch}, env)
		if err == nil {
			return nil
		}
		lastErr = err
		log.Printf("backup-memory: push 第 %d 次失败: %v", attempt, r.sanitize(err.Error()))
		if attempt < MaxPushAttempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(PushRetryDelay):
			}
		}
	}
	return fmt.Errorf("push 失败（已重试 1 次）: %w", lastErr)
}

// Run 执行一次记忆备份（date 是被精炼的 L1 日期，YYYY-MM-DD）。
// 开关关闭时直接跳过（不碰 git、不记账）；其余失败都记 failure 行 + 告警日志。
func (r *Runner) Run(ctx context.Context, date string) (Result, error) {
	if !r.cfg.Enabled {
		return Result{Skipped: true}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()
	start := r.nowFunc()
	repoDir := r.repoDir()
	if st, err := os.Stat(filepath.Join(repoDir, ".git")); err != nil || !st.IsDir() {
		return r.fail(date, "工作区不是 git 仓库", fmt.Errorf("%s 下无 .git", repoDir))
	}
	_, isHTTP, err := r.verifyRemote(ctx, repoDir)
	if err != nil {
		return r.fail(date, "remote 校验失败", err)
	}
	token := ""
	if isHTTP {
		token = os.Getenv(r.cfg.TokenEnvKey)
		if token == "" {
			return r.fail(date, "Token 未设置", fmt.Errorf("环境变量 %s 为空", r.cfg.TokenEnvKey))
		}
	}
	rels, err := r.candidates(date)
	if err != nil {
		return r.fail(date, "列候选文件失败", err)
	}
	n, err := r.stageCopy(repoDir, rels)
	if err != nil {
		return r.fail(date, "复制候选文件失败", err)
	}
	paths := []string{filepath.Join("memories", ".gitignore")}
	for _, rel := range rels {
		paths = append(paths, filepath.Join("memories", r.server, rel))
	}
	if err := r.gitAdd(ctx, repoDir, paths); err != nil {
		return r.fail(date, "git add 失败", err)
	}
	changed, err := r.hasStagedChanges(ctx, repoDir)
	if err != nil {
		return r.fail(date, "判断暂存区失败", err)
	}
	if !changed {
		r.record(date, 0, fmt.Sprintf("no changes, skipped (files=%d)", n))
		_ = r.writeMarker(start)
		log.Printf("backup-memory %s: 无变更，跳过（files=%d）", date, n)
		return Result{NoChange: true, Files: n}, nil
	}
	branch, err := r.currentBranch(ctx, repoDir)
	if err != nil {
		return r.fail(date, "读分支失败", err)
	}
	msg := fmt.Sprintf("backup memory %s %s", r.server, date)
	_, err = r.git.run(ctx, repoDir, []string{"-c", "user.name=007-agent", "-c", "user.email=007-agent@localhost", "commit", "-m", msg}, nil)
	if err != nil {
		return r.fail(date, "git commit 失败", err)
	}
	// 第 3 点：推送前固定先拉齐历史（有上游才拉；新库无上游时跳过）。
	// pull 与 push 共用一套认证 env（HTTP 远端需 askpass；见 authEnv）。
	env, cleanup, err := r.authEnv(token, isHTTP)
	if err != nil {
		return r.fail(date, "建 git 认证失败", err)
	}
	defer cleanup()
	if r.hasUpstream(ctx, repoDir) {
		if _, err := r.git.run(ctx, repoDir, []string{"pull", "--rebase", "--autostash"}, env); err != nil {
			return r.fail(date, "git pull --rebase 失败（本地提交已保留，下次再推）", err)
		}
	}
	if err := r.pushWithRetry(ctx, repoDir, branch, env); err != nil {
		return r.fail(date, "git push 失败", err)
	}
	r.record(date, 0, fmt.Sprintf("committed files=%d branch=%s", n, branch))
	_ = r.writeMarker(start)
	log.Printf("backup-memory %s: 已推送（files=%d branch=%s）", date, n, branch)
	return Result{Committed: true, Files: n, Branch: branch}, nil
}

// writeMarker 写“上次备份成功”标记（内容为本次开始时间，供下次算增量）。
func (r *Runner) writeMarker(t time.Time) error {
	return os.WriteFile(r.markerFile(), []byte(t.Format(time.RFC3339)+"\n"), 0o600)
}
