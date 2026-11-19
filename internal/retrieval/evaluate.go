package retrieval

import (
	"strings"
	"time"
)

// 评估常量（规格 §2A.4）。
const (
	// StaleMonths：>18 个月无提交/发布 → 原则上淘汰（H3，估算值，
	// 上线后用检索日志校准）。
	StaleMonths = 18
	// MinStars：星数只作初筛下限，过滤个人玩具仓库。
	MinStars = 50
	// MinCoverage：功能覆盖 ≥80% 才算合格（R4 的反向；覆盖度由锻造侧
	// 模型判定，本包只记录“未判定”，见 CandidateEval.CoverageUnknown）。
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
	HardThreshold string    `json:"hard_threshold,omitempty"` // H1/H2/H3（淘汰原因）
	RedLines      []string  `json:"red_lines,omitempty"`      // 未过的红线
	Passed        bool      `json:"passed"`
	// CoverageUnknown：本包不判功能覆盖（R4 需理解需求语义，由锻造侧
	// 模型判定）；Passed 只表示“可查的硬门槛与红线全过”。
	CoverageUnknown bool `json:"coverage_unknown"`
}

// licenseTier 是许可分级（规格 §2A.4，口径 B）：
//   - permissive：全路放行；
//   - lgpl_mpl：仅限未改动的独立外部进程调用；
//   - gpl：允许独立 CLI 进程调用，仅禁抄代码进公开库；
//   - banned：无许可证或仅限非商用，全路淘汰。
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
// 需要它们即转自造。测试机现有：Python 3.11、Node 22、Java 25；
// Go 未装（只能用预编译二进制，编译归 Muse）。
var heavyRuntimes = []string{"cargo", "rustc", "ruby", "php", "deno", "bun", "dotnet", "swift", "kotlin"}

// InferRuntimeNeed 从依赖清单文件名推断运行需求。
// go.mod → go-binary（只能用预编译二进制）；package.json → node；
// pyproject.toml/setup.py/requirements.txt → python；
// pom.xml/build.gradle → java；其余 unknown。
func InferRuntimeNeed(depsFile string) string {
	switch depsFile {
	case "go.mod":
		return "go-binary"
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
// （不含 R4 功能覆盖——那是锻造侧模型的活）。
// now 由调用方传（测试可固定时钟）。
func EvaluateCandidate(ce *CandidateEval, now time.Time) {
	// H2：Archived / Deprecated → 淘汰。
	if ce.Archived {
		ce.HardThreshold = "H2"
		ce.Passed = false
		return
	}
	// H1：无 LICENSE 文件 → 淘汰（LICENSE 文件以 raw 直读为准；
	// 详情里的 license 字段只是初值，两处都没有才算无）。
	if !ce.LicenseFileOK && ce.License == "" {
		ce.HardThreshold = "H1"
		ce.Passed = false
		return
	}
	// H3：>18 个月无提交/发布 → 原则上淘汰。
	last := ce.LastCommit
	if last == "" {
		last = ce.LastPush
	}
	if last != "" {
		if t, err := time.Parse(time.RFC3339, last); err == nil {
			if now.Sub(t) > time.Duration(StaleMonths)*30*24*time.Hour {
				ce.HardThreshold = "H3"
				ce.Passed = false
				return
			}
		}
	}
	// R1 许可违规：无许可证 / NC / 仅限非商用 → 淘汰；
	// LGPL/MPL 与 GPL 只限制“源码并入 Go 本体 / 抄进公开库”，
	// 独立进程调用放行（口径 B），不在检索层淘汰。
	tier := licenseTier(ce.License)
	if tier == "banned" {
		ce.RedLines = append(ce.RedLines, "R1")
		ce.Passed = false
		return
	}
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
