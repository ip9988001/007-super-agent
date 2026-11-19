package forge

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Blueprint 是蓝图（规格 §3，存 data/forge/blueprints/<gap-id>.md）。
// 模型输出 JSON，解析校验后渲染成 Markdown 落盘。
type Blueprint struct {
	ID                 string `json:"id"`
	Name               string `json:"name"` // 工具名，英文 kebab-case
	Description        string `json:"description"`
	GapAnalysis        string `json:"gap_analysis"`
	SolutionDesign     string `json:"solution_design"`
	ImplementationPlan string `json:"implementation_plan"`
	RiskLevel          string `json:"risk_level"` // low / medium / high
	RiskMitigation     string `json:"risk_mitigation"`
	SafetyChecks       string `json:"safety_checks"`
	Credentials        string `json:"credentials"` // 会访问的凭据/环境变量，没有写 none，必须声明
	SourceGap          string `json:"source_gap"`
	ReuseSearch        string `json:"reuse_search"` // 检索留痕引用 + 分流结论 + 参考链接清单
	ScriptName         string `json:"script_name"`  // 试造脚本文件名，如 tool.py
	Script             string `json:"script"`       // 完整脚本内容（自造分支）
}

// blueprintPromptHead 是蓝图生成的冻结提示词。
const blueprintPromptHead = `你是 007 的锻造器。根据下面的缺口与检索留痕，写一份工具蓝图，只输出一个 JSON 对象，不要任何解释、不要 markdown 代码围栏。
字段（全部必填，缺一不可）：
{"name":"英文 kebab-case 工具名，如 pdf-to-markdown",
 "description":"70–200 字英文，一句话说清工具是干什么的（与注册表 one_liner 同口径）",
 "gap_analysis":"缺口是什么、证据是什么",
 "solution_design":"怎么做：脚本还是 Go 函数；输入/输出；依赖",
 "implementation_plan":"分步骤写清",
 "risk_level":"low/medium/high（low=纯只读本地计算；medium=本地文件写或低成本外部只读 API；high=删文件/发消息/花钱/改系统配置/Go 编译）",
 "risk_mitigation":"风险怎么缓解",
 "safety_checks":"安全检查点",
 "credentials":"本工具会访问哪些凭据/环境变量，没有就写 none，必须声明，不许留空",
 "script_name":"试造脚本文件名，如 tool.py（入口脚本）",
 "script":"完整脚本内容（可直接保存运行；优先 python3 脚本，只用标准库，不要外部依赖）"}
要求：
- 脚本必须完整可运行：只用标准库，不联网、不写死绝对路径（工作目录用 argv 传入）；
- 不要编造不存在的库或命令；不确定的用法先写最保守的办法；
- risk_level 按上面的三分法实事求是地评，不要为了过审往低了评。`

// ParseBlueprint 解析模型输出的蓝图 JSON（模型常在 JSON 外包一层解释文字，
// 先取最外层 JSON 对象）。必填字段缺失、risk_level 非法都返回错误。
// flexString 接受 JSON 字符串或字符串数组（数组按行拼接成一段文本）。
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*f = flexString(s)
		return nil
	}
	var arr []string
	if err := json.Unmarshal(b, &arr); err == nil {
		*f = flexString(strings.Join(arr, "\n"))
		return nil
	}
	return fmt.Errorf("既不是字符串也不是字符串数组")
}

func ParseBlueprint(reply, gapID string) (*Blueprint, error) {
	start := strings.Index(reply, "{")
	end := strings.LastIndex(reply, "}")
	if start < 0 || end < 0 || end < start {
		return nil, fmt.Errorf("forge blueprint: 回复里没有 JSON 对象")
	}
	// 模型偶尔把文本字段写成字符串数组（如 safety_checks 列成清单），
	// 用 flexString 容忍两种形式（数组按行拼接）。2026-11-19 缺口 1
	// 第四轮：safety_checks 回了数组，整轮直接失败。
	var raw struct {
		Name               string     `json:"name"`
		Description        flexString `json:"description"`
		GapAnalysis        flexString `json:"gap_analysis"`
		SolutionDesign     flexString `json:"solution_design"`
		ImplementationPlan flexString `json:"implementation_plan"`
		RiskLevel          string     `json:"risk_level"`
		RiskMitigation     flexString `json:"risk_mitigation"`
		SafetyChecks       flexString `json:"safety_checks"`
		Credentials        string     `json:"credentials"`
		ScriptName         string     `json:"script_name"`
		Script             flexString `json:"script"`
		Category           string     `json:"category"`
		Subcategory        string     `json:"subcategory"`
		ReuseSearch        flexString `json:"reuse_search"`
	}
	if err := json.Unmarshal([]byte(reply[start:end+1]), &raw); err != nil {
		return nil, fmt.Errorf("forge blueprint: JSON 非法: %w", err)
	}
	bp := Blueprint{
		Name: raw.Name, Description: string(raw.Description),
		GapAnalysis:        string(raw.GapAnalysis),
		SolutionDesign:     string(raw.SolutionDesign),
		ImplementationPlan: string(raw.ImplementationPlan),
		RiskLevel:          raw.RiskLevel,
		RiskMitigation:     string(raw.RiskMitigation),
		SafetyChecks:       string(raw.SafetyChecks),
		Credentials:        raw.Credentials, ScriptName: raw.ScriptName,
		Script:      string(raw.Script),
		ReuseSearch: string(raw.ReuseSearch),
	}
	bp.ID = "bp-" + gapID
	bp.SourceGap = gapID
	for field, v := range map[string]string{
		"name": bp.Name, "description": bp.Description,
		"gap_analysis": bp.GapAnalysis, "solution_design": bp.SolutionDesign,
		"implementation_plan": bp.ImplementationPlan,
		"risk_level":          bp.RiskLevel, "risk_mitigation": bp.RiskMitigation,
		"safety_checks": bp.SafetyChecks, "credentials": bp.Credentials,
		"script_name": bp.ScriptName, "script": bp.Script,
	} {
		if strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("forge blueprint: 字段 %q 为空", field)
		}
	}
	switch bp.RiskLevel {
	case "low", "medium", "high":
	default:
		return nil, fmt.Errorf("forge blueprint: risk_level %q 非法", bp.RiskLevel)
	}
	if len([]rune(bp.Description)) < 10 || len([]rune(bp.Description)) > 1200 {
		return nil, fmt.Errorf("forge blueprint: description 长度 %d 不在合理范围",
			len([]rune(bp.Description)))
	}
	return &bp, nil
}

// RenderMarkdown 把蓝图渲染成 Markdown（落盘格式，规格 §3）。
func (bp *Blueprint) RenderMarkdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# 蓝图 %s\n\n", bp.ID)
	fmt.Fprintf(&b, "- 工具名：%s\n- 风险等级：%s\n- 来源缺口：%s\n- 凭据声明：%s\n\n",
		bp.Name, bp.RiskLevel, bp.SourceGap, bp.Credentials)
	fmt.Fprintf(&b, "## 描述\n%s\n\n", bp.Description)
	fmt.Fprintf(&b, "## 缺口分析\n%s\n\n", bp.GapAnalysis)
	fmt.Fprintf(&b, "## 方案设计\n%s\n\n", bp.SolutionDesign)
	fmt.Fprintf(&b, "## 实施计划\n%s\n\n", bp.ImplementationPlan)
	fmt.Fprintf(&b, "## 风险缓解\n%s\n\n", bp.RiskMitigation)
	fmt.Fprintf(&b, "## 安全检查\n%s\n\n", bp.SafetyChecks)
	fmt.Fprintf(&b, "## 复用检索\n%s\n\n", bp.ReuseSearch)
	fmt.Fprintf(&b, "## 试造脚本（%s）\n```\n%s\n```\n", bp.ScriptName, bp.Script)
	return b.String()
}

// WriteBlueprint 写蓝图文件（0600）。
func (s *Store) WriteBlueprint(bp *Blueprint) error {
	if err := s.EnsureDirs(); err != nil {
		return err
	}
	return os.WriteFile(
		filepath.Join(s.BlueprintsDir(), bp.SourceGap+".md"),
		[]byte(bp.RenderMarkdown()), 0o600)
}

// ReadBlueprint 读蓝图文件（只读 Markdown 原文，供监工审阅）。
func (s *Store) ReadBlueprint(gapID string) (string, error) {
	b, err := os.ReadFile(filepath.Join(s.BlueprintsDir(), gapID+".md"))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ---- 查询词生成（2026-11-19 修正：关键判断改 LLM 驱动） ----

// queryPromptHead 是查询词生成的冻结提示词：模型按缺口写两条查询。
const queryPromptHead = `你是 007 的锻造查询词写手。根据下面的缺口原文，写两条 GitHub 仓库搜索查询，只输出一个 JSON 对象，不要任何解释、不要 markdown 代码围栏。
字段：
{"focused":"聚焦查询（1-2 个核心词，不要加 language 限定，不要加多余的词）",
 "broad":"放宽查询（去 language 限定，换同义词或更通用的词）"}
硬规则（违反即错）：
- 聚焦查询只能含 1-2 个词，必须是缺口里最核心的功能词；
- 两条查询都不许加 language: 限定；
- 不要把缺口原文整句照抄，不要加 the/and/for 这类停用词。`

// QueryDecision 是查询词生成的模型输出。
type QueryDecision struct {
	Focused string `json:"focused"`
	Broad   string `json:"broad"`
}

// ParseQueryDecision 解析查询词生成 JSON（focused 必填）。
func ParseQueryDecision(reply string) (*QueryDecision, error) {
	start := strings.Index(reply, "{")
	end := strings.LastIndex(reply, "}")
	if start < 0 || end < 0 || end < start {
		return nil, fmt.Errorf("forge queries: 回复里没有 JSON 对象")
	}
	var d QueryDecision
	if err := json.Unmarshal([]byte(reply[start:end+1]), &d); err != nil {
		return nil, fmt.Errorf("forge queries: JSON 非法: %w", err)
	}
	if strings.TrimSpace(d.Focused) == "" {
		return nil, fmt.Errorf("forge queries: focused 为空")
	}
	return &d, nil
}

// rewritePromptHead 是查询重写（0 结果时）的冻结提示词。
const rewritePromptHead = `你是 007 的锻造查询词写手。上一轮搜索 0 结果，根据下面的缺口原文与已用查询，重写一条新的 GitHub 仓库搜索查询，只输出一个 JSON 对象，不要任何解释、不要 markdown 代码围栏。
字段：
{"query":"重写后的查询（1-3 个核心词，不加 language 限定）"}
硬规则（违反即错）：
- 必须换词，不能原样重复已用过的查询；
- 只用缺口里最核心的功能词，去掉可能跑题的修饰词；
- 不加 language: 限定。`

// ParseRewriteDecision 解析查询重写 JSON（query 必填）。
func ParseRewriteDecision(reply string) (string, error) {
	start := strings.Index(reply, "{")
	end := strings.LastIndex(reply, "}")
	if start < 0 || end < 0 || end < start {
		return "", fmt.Errorf("forge rewrite: 回复里没有 JSON 对象")
	}
	var d struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(reply[start:end+1]), &d); err != nil {
		return "", fmt.Errorf("forge rewrite: JSON 非法: %w", err)
	}
	if strings.TrimSpace(d.Query) == "" {
		return "", fmt.Errorf("forge rewrite: query 为空")
	}
	return d.Query, nil
}

// ---- 第零层模型判定（2026-11-19 修正：关键判断改 LLM 驱动） ----

// layer0PromptHead 是第零层判定的冻结提示词：模型判已有工具是否
// 真能覆盖缺口。只输出 JSON，不要解释。
const layer0PromptHead = `你是 007 的锻造第零层检查员。根据下面的缺口与候选工具清单，判断本地是否已有工具能覆盖这个缺口，只输出一个 JSON 对象，不要任何解释、不要 markdown 代码围栏。
字段：
{"hit":true/false,
 "tool_id":"命中的工具 id（未命中时空串）",
 "coverage":0-100（该工具对缺口功能的覆盖度，整数；未命中填 0）,
 "reason":"一句话依据"}
硬规则（违反即错）：
- 只有某个工具确实能干缺口里说的活、覆盖 ≥80%，才判 hit；
- 只看名字像、关键词重叠、功能沾边，都不算 hit；
- 不确定的一律判 no-hit；
- 候选清单里标了“已隔离/已废弃”的工具，判 hit 时要在 reason 里写明它的状态。`

// Layer0Decision 是第零层判定的模型输出。
type Layer0Decision struct {
	Hit      bool   `json:"hit"`
	ToolID   string `json:"tool_id"`
	Coverage int    `json:"coverage"`
	Reason   string `json:"reason"`
}

// ParseLayer0Decision 解析第零层判定 JSON。
func ParseLayer0Decision(reply string) (*Layer0Decision, error) {
	start := strings.Index(reply, "{")
	end := strings.LastIndex(reply, "}")
	if start < 0 || end < 0 || end < start {
		return nil, fmt.Errorf("forge layer0: 回复里没有 JSON 对象")
	}
	var d Layer0Decision
	if err := json.Unmarshal([]byte(reply[start:end+1]), &d); err != nil {
		return nil, fmt.Errorf("forge layer0: JSON 非法: %w", err)
	}
	return &d, nil
}

// ---- 覆盖度判定（2026-11-19 修正：关键判断改 LLM 驱动） ----

// coveragePromptHead 是候选覆盖度判定的冻结提示词。
const coveragePromptHead = `你是 007 的锻造覆盖度评审。根据下面的缺口与候选摘要，判断这个候选能覆盖缺口功能的百分之多少，只输出一个 JSON 对象，不要任何解释、不要 markdown 代码围栏。
字段：
{"coverage":0-100（整数）,
 "reason":"一句话依据：覆盖了哪些、缺了哪些"}
硬规则（违反即错）：
- 只看名字像、关键词重叠、功能沾边，都不能算覆盖；
- 缺了缺口里明确要求的关键能力，覆盖度就要往下扣；
- 不确定时往低了估，不要往高了估。`

// ParseCoverageDecision 解析覆盖度判定 JSON。
func ParseCoverageDecision(reply string) (int, string, error) {
	start := strings.Index(reply, "{")
	end := strings.LastIndex(reply, "}")
	if start < 0 || end < 0 || end < start {
		return 0, "", fmt.Errorf("forge coverage: 回复里没有 JSON 对象")
	}
	var d struct {
		Coverage int    `json:"coverage"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(reply[start:end+1]), &d); err != nil {
		return 0, "", fmt.Errorf("forge coverage: JSON 非法: %w", err)
	}
	if d.Coverage < 0 || d.Coverage > 100 {
		return 0, "", fmt.Errorf("forge coverage: 覆盖度 %d 超出 0-100", d.Coverage)
	}
	return d.Coverage, d.Reason, nil
}

// ---- 分流判定（规格 §2A.4，锻造侧模型调用） ----

// triagePromptHead 是分流判定的冻结提示词：决定采用/包装 vs 自造。
const triagePromptHead = `你是 007 的锻造分流器。根据下面的缺口与检索候选，决定走哪条分支，只输出一个 JSON 对象，不要任何解释、不要 markdown 代码围栏。
字段：
{"branch":"adopt/wrap/self_build",
 "candidate":"候选 full_name（采用/包装分支必填，自造分支空串）",
 "coverage":0-100（候选对缺口功能的覆盖度，整数）,
 "reason":"一句话依据",
 "wrapper_lines":0（包装分支预估胶水行数，自造分支填 0）,
 "license_tier":"permissive/lgpl_mpl/gpl/banned（候选的许可分级）"}
硬规则（违反即错）：
- 覆盖 <80% → 必须 self_build；
- 胶水预计 >100 行 → 必须 self_build；
- 需魔改上游非公开接口、或需为小功能单独装一套新语言运行时 → 必须 self_build；
- 许可证只记录，不构成转自造的理由（2026-11-19 用户定：自迭代工具只进私密仓、不进公开仓、不卖；无许可证、禁商用、GPL/AGPL 都不淘汰，license_tier 字段只做记录）；
- 采用/包装分支：1 条 CLI 命令或 ≤50 行包装脚本内完成目标动作、无需常驻进程或独立数据库、其余红线全过。`

// TriageDecision 是分流判定的模型输出。
type TriageDecision struct {
	Branch       string `json:"branch"` // adopt / wrap / self_build
	Candidate    string `json:"candidate"`
	Coverage     int    `json:"coverage"`
	Reason       string `json:"reason"`
	WrapperLines int    `json:"wrapper_lines"`
	LicenseTier  string `json:"license_tier"`
}

// ParseTriageDecision 解析分流判定 JSON，校验硬规则（模型判错时
// 在这里兜底：覆盖 <80% 或胶水 >100 行却判了采用/包装 → 强制改判自造）。
func ParseTriageDecision(reply string) (*TriageDecision, error) {
	start := strings.Index(reply, "{")
	end := strings.LastIndex(reply, "}")
	if start < 0 || end < 0 || end < start {
		return nil, fmt.Errorf("forge triage: 回复里没有 JSON 对象")
	}
	var d TriageDecision
	if err := json.Unmarshal([]byte(reply[start:end+1]), &d); err != nil {
		return nil, fmt.Errorf("forge triage: JSON 非法: %w", err)
	}
	switch d.Branch {
	case "adopt", "wrap", "self_build":
	default:
		return nil, fmt.Errorf("forge triage: branch %q 非法", d.Branch)
	}
	if d.Branch != "self_build" {
		if strings.TrimSpace(d.Candidate) == "" {
			return nil, fmt.Errorf("forge triage: 采用/包装分支缺 candidate")
		}
		// 硬规则兜底：模型判错时强制改判自造（不静默放行）。
		// 许可证不参与兜底（2026-11-19 用户定：只记录，不否决）。
		if d.Coverage < 80 || d.WrapperLines > 100 {
			d.Branch = "self_build"
			d.Reason = fmt.Sprintf("硬规则兜底改判自造（coverage=%d wrapper_lines=%d license=%s 只记录）：%s",
				d.Coverage, d.WrapperLines, d.LicenseTier, d.Reason)
		}
	}
	return &d, nil
}

// ---- 测试用例生成（规格 §5，staging 用） ----

// testsPromptHead 是测试用例生成的冻结提示词。
// 关键契约（2026-11-19 实测教训）：测试必须调用下面给出的真实脚本文件
// 名与真实命令行接口，不许凭空编一个 tool.py / --input；测试引用的
// 每个输入文件必须在 fixtures 里声明（流水线会先写进 staging），
// 否则测试跑不到输入文件、必然失败。
const testsPromptHead = `你是 007 的锻造测试员。根据下面的工具说明、真实脚本文件与缺口，生成 ≥3 条测试用例，只输出一个 JSON 对象，不要任何解释、不要 markdown 代码围栏。
字段：
{"fixtures":[{"name":"输入文件名，如 todo.md","content":"文件完整内容"}],
 "tests":[{"name":"用例名（正常/边界/失败各至少 1 条）","argv":["python3","<真实脚本文件名>","<按真实接口传参>"],"expect_contains":"期望 stdout 里含有的子串（可空串）","expect_fail":false}]}
硬规则（违反即错）：
- 每条测试的 argv 里必须出现下面给出的真实脚本文件名（它是 staging 里的入口文件，流水线会把它解析成绝对路径）；不许写 tool.py，除非脚本本身就叫 tool.py；
- argv 必须按脚本的真实接口传参：先读下面给出的脚本全文，看它到底要几个参数、参数是位置参数还是 --flag，再写 argv；不要编造脚本没有的 --input 之类选项；
- argv 的每个字符串就是命令行上的一个参数（argv 的一个元素），不要写 shell 语法（不要管道、重定向、&&、$变量）；
- 测试引用的每个输入文件（除 /dev/null 这类系统设备外）必须在 fixtures 里声明内容；没有声明的文件测试跑不到，等于白写；
- 若下面给了“真实历史失败输入”，其中至少 1 条测试必须回放它（用同一输入跑新工具，验证新工具能处理它）；没有给出时正常出 3 条即可；
- 失败用例要真的会失败（比如不存在的输入文件），并把 expect_fail 设为 true（此时通过标准是退出码非 0，且 expect_contains 若非空则在 stdout+stderr 里找）；正常/边界用例 expect_fail 为 false（退出码须为 0）。`

// TestCase 是一条 staging 测试用例。
// ExpectFail 为 true 时通过标准是退出码非 0（工具应正确报错）。
type TestCase struct {
	Name           string   `json:"name"`
	Argv           []string `json:"argv"`
	ExpectContains string   `json:"expect_contains"`
	ExpectFail     bool     `json:"expect_fail,omitempty"`
}

// TestFixture 是测试用的输入文件（流水线先写进 staging 再跑测试）。
type TestFixture struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// TestPlan 是一次测试生成的完整计划：输入文件 + 用例。
type TestPlan struct {
	Tests    []TestCase    `json:"tests"`
	Fixtures []TestFixture `json:"fixtures"`
}

// ParseTestCases 解析测试计划 JSON（须 ≥3 条用例；每条 argv 必须含
// entryName——测试测的是这个工具，不是凭空编的别的文件）。
func ParseTestCases(reply, entryName string) (*TestPlan, error) {
	start := strings.Index(reply, "{")
	end := strings.LastIndex(reply, "}")
	if start < 0 || end < 0 || end < start {
		return nil, fmt.Errorf("forge tests: 回复里没有 JSON 对象")
	}
	var plan TestPlan
	if err := json.Unmarshal([]byte(reply[start:end+1]), &plan); err != nil {
		return nil, fmt.Errorf("forge tests: JSON 非法: %w", err)
	}
	if len(plan.Tests) < 3 {
		return nil, fmt.Errorf("forge tests: 只有 %d 条，须 ≥3 条", len(plan.Tests))
	}
	for i, tc := range plan.Tests {
		if strings.TrimSpace(tc.Name) == "" || len(tc.Argv) == 0 {
			return nil, fmt.Errorf("forge tests: 第 %d 条用例缺 name 或 argv", i+1)
		}
		hit := false
		for _, a := range tc.Argv {
			if a == entryName {
				hit = true
				break
			}
		}
		if entryName != "" && !hit {
			return nil, fmt.Errorf("forge tests: 第 %d 条用例 argv 里没有入口脚本 %q（测试必须测这个工具）", i+1, entryName)
		}
	}
	for i, f := range plan.Fixtures {
		if strings.TrimSpace(f.Name) == "" {
			return nil, fmt.Errorf("forge tests: 第 %d 个 fixture 缺 name", i+1)
		}
	}
	return &plan, nil
}

// ---- 诊断式修复（规格 §5：测试不过时先判哪边错，再改错的那边） ----

// fixPromptHead 是诊断式修复的冻结提示词：模型先判是脚本错、
// 测试错、还是测试引用的输入文件缺失，再返回完整修正集（没错的
// 一边原样返回）。只修脚本的旧路子在接口错配时永远修不好。
const fixPromptHead = `你是 007 的锻造修理工。下面的工具在 staging 测试里失败了，先诊断、再修复，只输出一个 JSON 对象，不要任何解释、不要 markdown 代码围栏。
字段：
{"script_name":"脚本文件名（不变就照原样）","script":"完整修复后的脚本内容（脚本没错就原样返回）",
 "fixtures":[{"name":"输入文件名","content":"文件完整内容"}],
 "tests":[{"name":"用例名","argv":["python3","<真实脚本文件名>","<按真实接口传参>"],"expect_contains":"期望 stdout 里含有的子串","expect_fail":false}]}
诊断顺序（按这个顺序查）：
1. 接口错配：测试的 argv 是不是按脚本的真实接口写的（脚本要几个参数、位置参数还是 --flag）？不是就改测试的 argv，不要改脚本去迎合错的测试；
2. 输入文件缺失：测试引用的输入文件在 staging 里有没有？没有就把它补进 fixtures，不要改脚本；
3. 脚本逻辑错：前两项都没问题、脚本本身算错了，才改脚本；
4. 测试期望错：脚本输出是对的、只是期望的子串写错了，改测试的 expect_contains。
硬规则：每条测试的 argv 里必须出现真实脚本文件名；测试引用的每个输入文件（除 /dev/null 外）必须在 fixtures 里声明；不要编造脚本没有的选项。`

// FixPlan 是诊断式修复的模型输出（脚本 + 测试 + 输入文件全套）。
type FixPlan struct {
	ScriptName string        `json:"script_name"`
	Script     string        `json:"script"`
	Tests      []TestCase    `json:"tests"`
	Fixtures   []TestFixture `json:"fixtures"`
}

// ParseFixPlan 解析诊断式修复 JSON（script 必须非空，tests 须 ≥3 条，
// 每条 argv 须含入口脚本）。
func ParseFixPlan(reply, entryName string) (*FixPlan, error) {
	start := strings.Index(reply, "{")
	end := strings.LastIndex(reply, "}")
	if start < 0 || end < 0 || end < start {
		return nil, fmt.Errorf("forge fix: 回复里没有 JSON 对象")
	}
	var f FixPlan
	if err := json.Unmarshal([]byte(reply[start:end+1]), &f); err != nil {
		return nil, fmt.Errorf("forge fix: JSON 非法: %w", err)
	}
	if strings.TrimSpace(f.Script) == "" {
		return nil, fmt.Errorf("forge fix: script 为空")
	}
	if len(f.Tests) < 3 {
		return nil, fmt.Errorf("forge fix: 测试只有 %d 条，须 ≥3 条", len(f.Tests))
	}
	for i, tc := range f.Tests {
		hit := false
		for _, a := range tc.Argv {
			if a == entryName || a == f.ScriptName {
				hit = true
				break
			}
		}
		if !hit {
			return nil, fmt.Errorf("forge fix: 第 %d 条用例 argv 里没有入口脚本", i+1)
		}
	}
	return &f, nil
}
