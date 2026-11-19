package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"007-super-agent/internal/executor"
	"007-super-agent/internal/registry"
	"007-super-agent/internal/retrieval"
	"time"
)

// installPromptHead 是安装方案生成的冻结提示词：模型读 README，
// 输出如何把上游工具装进 staging（只输出 JSON）。
const installPromptHead = `你是 007 的锻造安装员。根据下面的候选仓库信息与 README，输出一个安装方案，只输出一个 JSON 对象，不要任何解释、不要 markdown 代码围栏。
字段：
{"method":"raw/pip/npm",
 "files":["raw 方式时要下载的仓库相对路径（按序试错，第一个下载成功的即为入口；如 [\"src/tool.py\",\"tool.py\",\"main.py\"]）"],
 "package":"pip/npm 方式时的包名（如 requests-tool）",
 "entry":"安装后 staging 里的入口文件名（如 cli.py 或 node_modules/.bin/tool）",
 "wrapper":"包装分支时 ≤50 行的包装脚本完整内容（采用分支填空串）；包装脚本只做参数翻译与调用，不重写功能",
 "risk_level":"low/medium/high（low=纯只读本地计算；medium=本地文件写或低成本外部只读 API；high=删文件/发消息/花钱/改系统配置/Go 编译）",
 "category":"9 大类之一（fetch/code/seo/design/security/runtime/media/ops/memory）",
 "subcategory":"该大类的子类",
 "offline_capable":true/false（工具核心功能是否离线可跑；离线可跑的 staging 强制断网试跑）}
硬规则：
- method 只能是 raw/pip/npm；二进制 Release 资产不在 V1 采用范围——若只能下二进制，method 填 raw 且 files 为空，entry 为空串（流水线会据此转自造）；
- 不要编造不存在的文件名或包名；README 里没写清楚的，method 填 raw 且 files 只列 README 里明确出现的文件；
- wrapper 超过 50 行就不要写 wrapper，entry 为空串（流水线转自造）；
- 许可为 GPL/AGPL/LGPL/MPL 的候选：只能独立 CLI 进程调用，绝不把源码抄进公开库——安装方式必须是 raw 下载独立脚本或 pip/npm 装包，entry 指向独立进程。`

// InstallPlan 是安装方案的模型输出。
type InstallPlan struct {
	Method         string   `json:"method"` // raw / pip / npm
	Files          []string `json:"files"`
	Package        string   `json:"package"`
	Entry          string   `json:"entry"`
	Wrapper        string   `json:"wrapper"`
	RiskLevel      string   `json:"risk_level"`
	Category       string   `json:"category"`
	Subcategory    string   `json:"subcategory"`
	OfflineCapable bool     `json:"offline_capable"`
}

// ParseInstallPlan 解析安装方案 JSON，校验 method/risk。
func ParseInstallPlan(reply string) (*InstallPlan, error) {
	start := strings.Index(reply, "{")
	end := strings.LastIndex(reply, "}")
	if start < 0 || end < 0 || end < start {
		return nil, fmt.Errorf("forge install: 回复里没有 JSON 对象")
	}
	var plan InstallPlan
	if err := json.Unmarshal([]byte(reply[start:end+1]), &plan); err != nil {
		return nil, fmt.Errorf("forge install: JSON 非法: %w", err)
	}
	switch plan.Method {
	case "raw", "pip", "npm":
	default:
		return nil, fmt.Errorf("forge install: method %q 非法", plan.Method)
	}
	switch plan.RiskLevel {
	case "low", "medium", "high":
	default:
		return nil, fmt.Errorf("forge install: risk_level %q 非法", plan.RiskLevel)
	}
	if plan.Wrapper != "" && len(strings.Split(plan.Wrapper, "\n")) > 50 {
		return nil, fmt.Errorf("forge install: 包装脚本 %d 行，超 50 行上限",
			len(strings.Split(plan.Wrapper, "\n")))
	}
	return &plan, nil
}

// rawBranches 返回 raw 下载的分支尝试顺序：候选的默认分支优先，
// 再试 main / master（去重）。深挖时已把默认分支存进留痕。
func rawBranches(cand *retrieval.CandidateEval) []string {
	seen := map[string]bool{}
	var out []string
	for _, b := range []string{cand.DefaultBranch, "main", "master"} {
		if b == "" || seen[b] {
			continue
		}
		seen[b] = true
		out = append(out, b)
	}
	return out
}

// adoptOrWrap 采用/包装分支：装进 staging → 静态检查 → 离线/正常试跑
// → 测试 → 监工审测试结果 → 注册（与自造分支同一套门禁，§7 不简化）。
// 装不上 / 静态检查命中 / 许可不符 → 转自造（红线 R5 无法验证）。
func (p *Pipeline) adoptOrWrap(ctx context.Context, taskID string, gap Gap, tr *retrieval.Trace, decision *TriageDecision) error {
	var cand *retrieval.CandidateEval
	for i := range tr.Candidates {
		if tr.Candidates[i].FullName == decision.Candidate {
			cand = &tr.Candidates[i]
			break
		}
	}
	if cand == nil {
		// 候选不在留痕里：分流依据已失效，转自造。
		_ = p.Store.AppendEvent(Event{
			Event: EvSelfBuild, GapID: gap.GapID,
			Output: "分流候选不在检索留痕里，转自造",
			Result: "ok",
		})
		return p.selfBuild(ctx, taskID, gap, tr, decision)
	}
	// 取 README（raw 直读，不计 API 额度；main 不行试 master）。
	owner, repo, _ := strings.Cut(cand.FullName, "/")
	readme := ""
	for _, branch := range rawBranches(cand) {
		if txt, _, err := p.Ret.RawFile(ctx, owner, repo, branch, "README.md"); err == nil && txt != "" {
			readme = txt
			break
		}
	}
	var tail strings.Builder
	fmt.Fprintf(&tail, "缺口：%s\n候选：%s（运行需求 %s）stars=%d 许可=%s 最后提交=%s\n分流结论：%s（%s）\n",
		gap.TaskText, cand.FullName, cand.RuntimeNeed, cand.Stars, cand.License, cand.LastCommit, decision.Branch, decision.Reason)
	fmt.Fprintf(&tail, "README（截断前 8000 字）：\n%s\n", firstRunes(readme, 8000))
	reply, err := p.callModel(ctx, taskID, installPromptHead, tail.String())
	if err != nil {
		_ = p.Store.UpdateGapStatus(gap.GapID, "pending")
		return err
	}
	plan, err := ParseInstallPlan(reply)
	if err != nil {
		_ = p.Store.UpdateGapStatus(gap.GapID, "pending")
		return fmt.Errorf("forge install %s: %w", gap.GapID, err)
	}
	if plan.Entry == "" {
		// 装不上（二进制分发或文件不明）→ 红线 R5，转自造。
		_ = p.Store.AppendEvent(Event{
			Event: EvSelfBuild, GapID: gap.GapID,
			Output: "安装方案无明确入口（R5 无法验证），转自造",
			Result: "ok",
		})
		return p.selfBuild(ctx, taskID, gap, tr, decision)
	}
	stageDir := filepath.Join(p.Store.StagingDir(), gap.GapID)
	if err := os.MkdirAll(stageDir, 0o700); err != nil {
		return err
	}
	// 安装进 staging（三种方式；任一步失败 → 转自造）。
	if err := p.installIntoStaging(ctx, taskID, gap, stageDir, owner, repo, plan, rawBranches(cand)); err != nil {
		_ = p.Store.AppendEvent(Event{
			Event: EvSelfBuild, GapID: gap.GapID,
			Output: fmt.Sprintf("安装失败（%v），转自造", err),
			Result: "ok",
		})
		return p.selfBuild(ctx, taskID, gap, tr, decision)
	}
	// 静态检查（§2A.5 三阶第一阶）。
	findings, err := ScanStagingFiles(stageDir)
	if err != nil {
		return err
	}
	if len(findings) > 0 {
		_ = p.Store.AppendEvent(Event{
			Event: EvTestFail, GapID: gap.GapID,
			Output: "静态检查命中：" + strings.Join(findings, "; "),
			Result: "fail",
		})
		_ = p.Store.UpdateGapStatus(gap.GapID, "rejected")
		return nil
	}
	// 离线试跑（离线类强制断网）或正常试跑：入口 --help/--version。
	if err := p.smokeTest(ctx, taskID, gap, stageDir, plan); err != nil {
		_ = p.Store.AppendEvent(Event{
			Event: EvSelfBuild, GapID: gap.GapID,
			Output: fmt.Sprintf("试跑失败（%v），转自造", err),
			Result: "ok",
		})
		return p.selfBuild(ctx, taskID, gap, tr, decision)
	}
	// 测试用例（缺口类必须回放 ≥1 条真实历史失败输入）。
	// 把已装入口的全文给模型当接口依据（脚本可能是上游的原文件）。
	replay := p.replayInput(gap)
	toolDesc := cand.FullName
	entryContent := ""
	if b, rerr := os.ReadFile(filepath.Join(stageDir, plan.Entry)); rerr == nil {
		entryContent = string(b)
	}
	tests, err := p.genTests(ctx, taskID, gap, plan.Entry, toolDesc, plan.Entry, entryContent, stageDir, replay)
	if err != nil {
		_ = p.Store.UpdateGapStatus(gap.GapID, "pending")
		return err
	}
	resultMD, err := p.runTests(ctx, taskID, gap, stageDir, plan.Entry, tests, plan.OfflineCapable)
	_ = os.WriteFile(filepath.Join(stageDir, "test-result.md"), []byte(resultMD), 0o600)
	if err != nil {
		// 采用分支的测试失败不修上游代码：直接转自造。
		_ = p.Store.AppendEvent(Event{
			Event: EvSelfBuild, GapID: gap.GapID,
			Output: fmt.Sprintf("采用分支测试未通过（%v），转自造", err),
			Result: "ok",
		})
		return p.selfBuild(ctx, taskID, gap, tr, decision)
	}
	_ = os.WriteFile(filepath.Join(stageDir, "test-result.md"), []byte(resultMD), 0o600)
	// 监工审测试结果（固定第 1 检查项 + ≤2 轮打回重做；留痕前置）。
	err = p.advisorGate(ctx, taskID, "审测试结果（采用）"+gap.GapID, "【复用检索留痕】\n"+tr.Summary()+"\n\n"+resultMD, stageDir,
		EvAdvisorTestHalt, EvAdvisorTestPass, gap.GapID, "",
		func(note string) (string, error) {
			t2, err := p.genTests(ctx, taskID, gap, plan.Entry, toolDesc, plan.Entry, entryContent, stageDir, replay)
			if err != nil {
				return "", err
			}
			md2, err := p.runTests(ctx, taskID, gap, stageDir, plan.Entry, t2, plan.OfflineCapable)
			if err != nil {
				return "", err
			}
			_ = os.WriteFile(filepath.Join(stageDir, "test-result.md"), []byte(md2), 0o600)
			return md2, nil
		})
	if err != nil {
		if errors.Is(err, errAdvisorRejected) {
			_ = p.Store.UpdateGapStatus(gap.GapID, "rejected")
			return nil
		}
		_ = p.Store.UpdateGapStatus(gap.GapID, "pending")
		return err
	}
	// 注册（与自造同一套门禁；GPL/LGPL/MPL 强制 private-cli-only）。
	privateCLI := decision.LicenseTier == "gpl" || decision.LicenseTier == "lgpl_mpl"
	entry := registry.Entry{
		ToolID:         "forge-" + strings.ReplaceAll(repo, "-", ""),
		OneLiner:       toolDesc,
		EntryPoint:     filepath.Join(stageDir, plan.Entry),
		CallType:       "cli",
		OutputContract: "stdout 输出处理结果，exit 0 为成功，非 0 为失败",
		SourceFile:     "forge:" + gap.GapID + " (upstream " + cand.FullName + ")",
		Category:       p.validCategory(plan.Category),
		Subcategory:    p.validSubcategory(plan.Category, plan.Subcategory),
		Forged:         true,
		Experimental:   true,
		BlueprintID:    "",
		Risk:           plan.RiskLevel,
		PrivateCLIOnly: privateCLI,
	}
	// 包装分支：入口改为包装脚本。
	if decision.Branch == "wrap" && plan.Wrapper != "" {
		wrapperPath := filepath.Join(stageDir, "wrapper.sh")
		if err := os.WriteFile(wrapperPath, []byte(plan.Wrapper), 0o700); err != nil {
			return err
		}
		entry.EntryPoint = wrapperPath
	}
	return p.register(ctx, taskID, gap, nil, entry)
}

// installIntoStaging 把上游工具装进 staging 目录（不装系统全局）。
func (p *Pipeline) installIntoStaging(ctx context.Context, taskID string, gap Gap, stageDir, owner, repo string, plan *InstallPlan, branches []string) error {
	switch plan.Method {
	case "raw":
		// 按序试错：entry 优先，再试 files 里的候选路径；第一个下载
		// 成功的即为入口（单文件工具；多文件包走 pip/npm 或转自造）。
		// 分支顺序：候选默认分支 → main → master。
		tried := []string{}
		if plan.Entry != "" {
			tried = append(tried, plan.Entry)
		}
		tried = append(tried, plan.Files...)
		if len(tried) == 0 {
			return fmt.Errorf("raw 方式未列出文件")
		}
		installed := ""
		for _, f := range tried {
			if f == "" {
				continue
			}
			content := ""
			for _, branch := range branches {
				if txt, _, err := p.Ret.RawFile(ctx, owner, repo, branch, f); err == nil && txt != "" {
					content = txt
					break
				}
			}
			if content == "" {
				continue
			}
			dst := filepath.Join(stageDir, filepath.Base(f))
			if err := os.WriteFile(dst, []byte(content), 0o700); err != nil {
				return err
			}
			installed = filepath.Base(f)
			break
		}
		if installed == "" {
			return fmt.Errorf("raw 下载失败（试过 %v）", tried)
		}
		// 入口以实际装上的文件为准（plan.Entry 猜错路径时以此为准）。
		plan.Entry = installed
		return nil
	case "pip":
		if plan.Package == "" {
			return fmt.Errorf("pip 方式未给包名")
		}
		res, h, err := p.Run.Run(executorRunRequest(taskID, p.SessionID, gap, []string{
			"python3", "-m", "pip", "install", "--target", stageDir, plan.Package,
		}))
		return p.waitInstall(res, h, err, "pip")
	case "npm":
		if plan.Package == "" {
			return fmt.Errorf("npm 方式未给包名")
		}
		res, h, err := p.Run.Run(executorRunRequest(taskID, p.SessionID, gap, []string{
			"npm", "install", "--prefix", stageDir, plan.Package,
		}))
		return p.waitInstall(res, h, err, "npm")
	default:
		return fmt.Errorf("未知安装方式 %q", plan.Method)
	}
}

func executorRunRequest(taskID, sessionID string, gap Gap, argv []string) executor.RunRequest {
	return executor.RunRequest{
		TaskID: taskID, SessionID: sessionID,
		ToolID: "forge-staging", Argv: argv,
		Timeout: 120 * time.Second, Text: gap.TaskText, // 120s（安装走后台）
	}
}

func (p *Pipeline) waitInstall(res *executor.Result, h *executor.Handle, err error, kind string) error {
	if err != nil {
		return fmt.Errorf("%s 安装失败：%w", kind, err)
	}
	if h != nil {
		var werr error
		res, werr = h.Wait()
		if werr != nil {
			return fmt.Errorf("%s 安装失败：%w", kind, werr)
		}
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s 安装退出码 %d：%s", kind, res.ExitCode, res.ErrHead)
	}
	return nil
}

// smokeTest 试跑入口（--help 或 --version）；离线类用 unshare -n 断网跑。
func (p *Pipeline) smokeTest(ctx context.Context, taskID string, gap Gap, stageDir string, plan *InstallPlan) error {
	entry := filepath.Join(stageDir, plan.Entry)
	argv := []string{entry, "--help"}
	if plan.OfflineCapable {
		argv = append([]string{"unshare", "-n"}, argv...)
	}
	res, h, err := p.Run.Run(executor.RunRequest{
		TaskID: taskID, SessionID: p.SessionID,
		ToolID: "forge-staging", Argv: argv, Dir: stageDir,
		Timeout: 25 * time.Second, Text: gap.TaskText,
	})
	if err != nil {
		return err
	}
	if h != nil {
		var werr error
		res, werr = h.Wait()
		if werr != nil {
			return werr
		}
	}
	// --help 退出码非 0 时再试 --version（有些工具 --help 返回 2）。
	if res.ExitCode != 0 {
		argv2 := []string{entry, "--version"}
		if plan.OfflineCapable {
			argv2 = append([]string{"unshare", "-n"}, argv2...)
		}
		res2, h2, err2 := p.Run.Run(executor.RunRequest{
			TaskID: taskID, SessionID: p.SessionID,
			ToolID: "forge-staging", Argv: argv2,
			Timeout: 25 * time.Second, Text: gap.TaskText,
		})
		if err2 != nil {
			return err2
		}
		if h2 != nil {
			var werr error
			res2, werr = h2.Wait()
			if werr != nil {
				return werr
			}
		}
		if res2.ExitCode != 0 {
			return fmt.Errorf("入口试跑失败：--help 退出 %d，--version 退出 %d", res.ExitCode, res2.ExitCode)
		}
	}
	return nil
}
