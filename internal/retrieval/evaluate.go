package retrieval

import (
	"fmt"
	"strings"
	"time"
)

// 评估常量（规格 §2A.4，2026-11-19 修正版）。
const (
	// StaleMonths：>18 个月无提交/发布 → 只记 stale 警告，不淘汰
	//（H3 已降为参考：功能符合就照常用，同等功能优先新鲜的）。
	StaleMonths = 18
	// MinStars：星数初筛下限（H0，<700 星直接淘汰；用户 2026-11-19
	// 定：700 星以上、多人认同的开源项目比较安全）。
	MinStars = 700
	// MinCoverage：功能覆盖 ≥80% 才算合格（R4 的反向；覆盖度由锻造侧
	// 模型判定，见 CandidateEval.Coverage / CoverageReason）。
	MinCoverage = 80
)

// CandidateEval 是一个候选的深挖评估结果（进留痕与缓存）。
type CandidateEval struct {
	FullName      string    `json:"full_name"`
	Stars         int       `json:"stars"`
	License       string    `json:"license"`
	Archived      bool      `json:"archived"`
	LastPush      string    `json:"last_push"`
	LastCommit    string    `json:"last_commit"`
	HasRelease    bool      `json:"has_release"`
	ReleaseTag    string    `json:"release_tag,omitempty"`
	ReadmeOK      bool      `json:"readme_ok"`
	LicenseFileOK bool      `json:"license_file_ok"`
	DepsFile      string    `json:"deps_file,omitempty"`      // 命中的依赖清单：package.json / pyproject.toml / go.mod
	SecurityFlag  string    `json:"security_flag,omitempty"`  // 疑似风险（R2 的轻量信号，锻造侧再做三阶检查）
	RuntimeNeed   string    `json:"runtime_need,omitempty"`   // 推断的运行需求：python / node / java / go-binary / unknown / heavy:<name>
	DefaultBranch string    `json:"default_branch,omitempty"` // 默认分支（raw 下载先用它）
	FromCache     bool      `json:"from_cache"`
	CachedAt      time.Time `json:"cached_at"`
	HardThreshold string    `json:"hard_threshold,omitempty"` // H0/H2（淘汰原因）
	RedLines      []string  `json:"red_lines,omitempty"`      // 未过的红线
	Passed        bool      `json:"passed"`
	// Stale：>18 个月无提交/发布（H3 警告，不淘汰）；StaleWarning 记
	// 上次更新日期与警告，供 ledger/trace 备查。
	Stale        bool   `json:"stale,omitempty"`
	StaleWarning string `json:"stale_warning,omitempty"`
	// Coverage / CoverageReason：模型判定的功能覆盖度（0–100）与理由；
	// 未判定时 CoverageUnknown 为 true（R4 需理解需求语义，由锻造侧
	// 模型判定）。Passed 只表示“可查的硬门槛与红线全过”，覆盖度另判。
	Coverage        int    `json:"coverage,omitempty"`
	CoverageReason  string `json:"coverage_reason,omitempty"`
	CoverageUnknown bool   `json:"coverage_unknown"`
}

// licenseTier 是许可分级（只记录，不淘汰：2026-11-19 用户定——
// 自迭代工具只进私密仓、不进公开仓、不卖，许可证只记在账本里备查）：
//   - permissive：MIT/BSD/Apache/ISC/Unlicense；
//   - lgpl_mpl：LGPL/MPL；
//   - gpl：GPL/AGPL；
//   - banned：无许可证或仅限非商用（只记录分级，不再淘汰）。
// LicenseTierOf 的返回值只用于 ledger/trace 记录与分流提示，不作否决。
func licenseTier(spdx string) string {
	switch strings.ToUpper(strings.TrimSpace(spdx)) {
	case "MIT", "BSD-2-CLAUSE", "BSD-3-CLAUSE", "APACHE-2.0", "ISC", "UNLICENSE":
		return "permissive"
	case "LGPL-2.1", "LGPL-3.0", "LGPL-2.1-ONLY", "LGPL-3.0-ONLY", "MPL-2.0":
		return "lgpl_mpl"
	case "GPL-2.0", "GPL-3.0", "AGPL-3.0", "GPL-2.0-ONLY", "GPL-3.0-ONLY", "AGPL-3.0-ONLY":
		return "gpl"
	default:
		return "banned"
	}
}

// LicenseTierOf 供锻造侧直接查某 SPDX 的分级（测试与注册门禁用）。
func LicenseTierOf(spdx string) string { return licenseTier(spdx) }

// heavyRuntimes 是测试机没有的重型运行时（规格 §2A.4 R3）：
// 需要它们即转自造。测试机现有：Python 3.11、Node 22、Java 25、
// Go（2026-11-19 已装，/usr/local/go，go1.27.4；007 本体编译仍归
// Muse 本地交叉编译，不改部署流程）。
var heavyRuntimes = []string{"cargo", "rustc", "ruby", "php", "deno", "bun", "dotnet", "swift", "kotlin"}

// InferRuntimeNeed 从依赖清单文件名推断运行需求。
// go.mod → go（测试机已装 Go，可在服务器上编译）；package.json → node；
// pyproject.toml/setup.py/requirements.txt → python；
// pom.xml/build.gradle → java；其余 unknown。
func InferRuntimeNeed(depsFile string) string {
	switch depsFile {
	case "go.mod":
		return "go"
	case "package.json":
		return "node"
	case "pyproject.toml", "setup.py", "requirements.txt":
		return "python"
	case "pom.xml", "build.gradle":
		return "java"
	default:
		return "unknown"
	}
}

// suspiciousPatterns 是 R2 轻量扫描的目标（规格 §2A.5）：
// 安装文件/入口含 curl|bash 下载执行、可疑外连、读取 ~/.ssh / ~/.aws、
// 全量环境变量上传。命中只记 SecurityFlag，锻造侧再做三阶检查。
var suspiciousPatterns = []string{
	"curl|bash", "curl | bash", "wget|bash",
	".ssh", ".aws", "process.env", "os.environ",
}

// scanSuspicious 对取回的文件内容做轻量扫描，返回命中的模式（无则空串）。
func scanSuspicious(content string) string {
	lower := strings.ToLower(content)
	var hits []string
	for _, p := range suspiciousPatterns {
		if strings.Contains(lower, strings.ToLower(p)) {
			hits = append(hits, p)
		}
	}
	return strings.Join(hits, ",")
}

// EvaluateCandidate 对一个已深挖的候选做可查的硬门槛与红线判定
// （不含 R4 功能覆盖——那是锻造侧模型的活，由调用方在 DeepDive 后
// 用 CoverageJudge 补判，见 retrieve.go）。
// 2026-11-19 修正版：H1 与 R1（许可证）已删——许可证只记录不淘汰
//（自迭代工具只进私密仓）；H3（>18 个月未更新）降为 stale 警告。
// 保留的一票否决：H0（星数 <700）、H2（Archived）、R2（安全）、
// R3（重型运行时）、R5（无法验证）。now 由调用方传（测试可固定时钟）。
func EvaluateCandidate(ce *CandidateEval, now time.Time) {
	// H2：Archived / Deprecated → 淘汰（保留）。
	if ce.Archived {
		ce.HardThreshold = "H2"
		ce.Passed = false
		return
	}
	// H0：星数 <700 → 淘汰（用户 2026-11-19 定：700 星以上才比较安全）。
	if ce.Stars < MinStars {
		ce.HardThreshold = "H0"
		ce.Passed = false
		return
	}
	// H3：>18 个月无提交/发布 → 只记 stale 警告，不淘汰；功能符合
	// 就照常用，同等功能优先新鲜的（警告进 ledger/trace 备查）。
	last := ce.LastCommit
	if last == "" {
		last = ce.LastPush
	}
	if last != "" {
		if t, err := time.Parse(time.RFC3339, last); err == nil {
			if now.Sub(t) > time.Duration(StaleMonths)*30*24*time.Hour {
				ce.Stale = true
				ce.StaleWarning = fmt.Sprintf("最后更新 %s，超过 %d 个月未更新（仅作参考标记，不淘汰）", last, StaleMonths)
			}
		}
	}
	// 许可证：只记录分级（licenseTier），不淘汰、不记红线。
	// 无许可证 / NC / GPL 等一律放行进下一步，由模型判覆盖度；
	// 锻造工具只进私密仓，不进公开库。
	_ = licenseTier(ce.License)
	// R2 安全扫描：轻量信号命中 → 记红线（锻造侧三阶检查再定）。
	if ce.SecurityFlag != "" {
		ce.RedLines = append(ce.RedLines, "R2")
		ce.Passed = false
		return
	}
	// R3 无法在测试机运行：需新装重型运行时 → 淘汰。
	if strings.HasPrefix(ce.RuntimeNeed, "heavy:") {
		ce.RedLines = append(ce.RedLines, "R3")
		ce.Passed = false
		return
	}
	// R5 无法验证：README 与依赖清单都读不到 → 淘汰。
	if !ce.ReadmeOK && ce.DepsFile == "" {
		ce.RedLines = append(ce.RedLines, "R5")
		ce.Passed = false
		return
	}
	ce.Passed = true
	ce.CoverageUnknown = true
}

// HeavyRuntimeNeeded 判断一段依赖/安装文本是否要求重型运行时。
func HeavyRuntimeNeeded(text string) string {
	lower := strings.ToLower(text)
	for _, rt := range heavyRuntimes {
		if strings.Contains(lower, rt) {
			return "heavy:" + rt
		}
	}
	return ""
}

// sortCandidatesByFreshness 按粗筛排序：维护新鲜度 > 星数
// （文档/发布完整度在深挖后看）。lastPush 解析失败的排最后。
func sortCandidatesByFreshness(in []RepoSummary) []RepoSummary {
	out := make([]RepoSummary, len(in))
	copy(out, in)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			ti, _ := time.Parse(time.RFC3339, out[j].PushedAt)
			tj, _ := time.Parse(time.RFC3339, out[j-1].PushedAt)
			less := ti.After(tj) || (ti.Equal(tj) && out[j].Stars > out[j-1].Stars)
			if !less {
				break
			}
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// ExtractKeywords 从缺口任务原话里抽搜索关键词（无模型时的机械办法）：
// 按非字母数字切分，去停用词与过短词，取前 6 个。调用方可直接传
// keywords（验收用缺口可显式指定），不走这里。
func ExtractKeywords(taskText string) []string {
	stop := map[string]bool{
		"the": true, "and": true, "for": true, "with": true, "from": true,
		"into": true, "that": true, "this": true, "need": true, "want": true,
		"tool": true, "tools": true, "please": true, "help": true, "make": true,
		"把": true, "一个": true, "需要": true, "想要": true, "工具": true,
		"的": true, "了": true, "是": true, "在": true, "有": true, "和": true,
	}
	fields := strings.FieldsFunc(taskText, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r > 127)
	})
	var out []string
	seen := map[string]bool{}
	for _, f := range fields {
		low := strings.ToLower(f)
		if len([]rune(f)) < 2 || stop[low] || seen[low] {
			continue
		}
		seen[low] = true
		out = append(out, f)
		if len(out) >= 6 {
			break
		}
	}
	return out
}
