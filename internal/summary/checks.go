package summary

import (
	"fmt"
	"regexp"
	"strings"

	"007-super-agent/internal/sanitize"
)

// CheckResult 是一条质检规则的检查结果：通过 / 打回 + 命中原因。
// Skipped 为真表示该规则因外部依赖不可用（如向量服务不通）未能执行，
// 不计入通过也不计入打回，须在汇报里单列。
type CheckResult struct {
	Rule    int    `json:"rule"`
	Name    string `json:"name"`
	Pass    bool   `json:"pass"`
	Skipped bool   `json:"skipped,omitempty"`
	Reason  string `json:"reason"`
}

func pass(rule int, name, reason string) CheckResult {
	return CheckResult{Rule: rule, Name: name, Pass: true, Reason: reason}
}

func fail(rule int, name, reason string) CheckResult {
	return CheckResult{Rule: rule, Name: name, Pass: false, Reason: reason}
}

// CheckAll 对一篇候选简介跑规则①–⑥ + 脱敏检查（规则⑦同目录查重与
// 规则⑧重试熔断是编排层逻辑，不在这里跑，见 embed.go 与 generate.go）。
// sourceText 是原文全文（规则⑤实体提取用），title 是文档标题。
func CheckAll(s Summary, sourceText, title string) []CheckResult {
	return []CheckResult{
		CheckBoilerplate(s.Summary, title),
		CheckOverlap(s.Summary, s.Triggers()),
		CheckGenericWords(s.Triggers()),
		CheckCounts(s.Summary, s.Triggers(), IsPlaybook(s.SourcePath, title)),
		CheckEntityCoverage(s.Summary, s.Triggers(), sourceText, title),
		CheckPolarity(s.Summary, IsAntiPattern(s.SourcePath)),
		CheckSecret(s.Summary, s.Triggers()),
	}
}

// AllPass 报告全部检查是否通过（跳过的规则不算失败）。
func AllPass(results []CheckResult) bool {
	for _, r := range results {
		if !r.Pass && !r.Skipped {
			return false
		}
	}
	return true
}

// FailedReasons 返回未通过规则的原因列表（供重写提示词用）。
func FailedReasons(results []CheckResult) []string {
	out := []string{}
	for _, r := range results {
		if !r.Pass && !r.Skipped {
			out = append(out, fmt.Sprintf("规则%d（%s）打回：%s", r.Rule, r.Name, r.Reason))
		}
	}
	return out
}

// ---- 规则① 废话模板句拦截 ----

// boilerplatePhrases 是模板化空话句式（v2.1 §6 规则①）。
var boilerplatePhrases = []string{
	"本文介绍了", "本文主要", "这是一篇", "本教程", "总的来说", "综上所述",
	"本文档记录了", "以下内容", "本文将", "众所周知", "需要注意的是",
	"本文旨在", "本篇文章", "这篇文档", "主要内容是", "简单来说",
	"总而言之", "一文读懂", "手把手教你",
}

// CheckBoilerplate：出现模板化空话句式、与标题复读、只有背景没有用途
// 和问题指向的，判废话并打回重写。标题复读按“简介以标题开头或包含
// 标题全文”判定；用途/问题指向按是否含动作词或症状词粗判，
// 命中任一即不判“只有背景”（机械规则，误杀时由重写提示说明）。
func CheckBoilerplate(summary, title string) CheckResult {
	for _, p := range boilerplatePhrases {
		if strings.Contains(summary, p) {
			return fail(1, "废话模板句拦截", fmt.Sprintf("含模板空话句式 %q", p))
		}
	}
	t := strings.TrimSpace(title)
	if t != "" {
		if strings.HasPrefix(summary, t) || strings.Contains(summary, t) {
			return fail(1, "废话模板句拦截", "简介复读标题，未写出用途与问题指向")
		}
	}
	if !HasActionWord(summary, nil) && !hasSymptomWord(summary) {
		return fail(1, "废话模板句拦截", "简介只有背景，没有用途（动作词）也没有问题指向（症状/报错词）")
	}
	return pass(1, "废话模板句拦截", "无模板空话，含用途或问题指向")
}

// symptomWords 是症状/报错类词（规则①“问题指向”粗判用）。
// 试点第二轮教训：只列报错词会误杀“讲后果、讲限制、讲代价”的简介，
// 所以补上后果/限制类指向词（导致、否则、风险等）。
var symptomWords = []string{
	"报错", "失败", "异常", "超时", "拒绝", "拦截", "卡死", "退不出",
	"找不到", "查不到", "不生效", "失效", "冲突", "乱码", "断开", "掉线",
	"误", "坑", "问题", "故障", "崩", "死锁", "泄漏", "过期", "风控",
	"导致", "引起", "造成", "否则", "不然", "容易", "注意", "当心",
	"区别", "风险", "隐患", "代价", "限制", "必须", "只能", "避免",
	"钉住", "挂住", "永不退出", "无法退出", "无响应", "没反应", "假死",
}

func hasSymptomWord(summary string) bool {
	for _, w := range symptomWords {
		if strings.Contains(summary, w) {
			return true
		}
	}
	return false
}

// ---- 规则② 低重合拦截 ----

// exemptOverlapEntry 是低重合计算的豁免条目：整项是纯缩写（2–8 位全大写
// ASCII，如 CF、API、TG）或整项恰好等于一个动作词（v2.1 §5 豁免口径）。
// 豁免条目分子分母都不计。
func exemptOverlapEntry(entry string) bool {
	if len(entry) >= 2 && len(entry) <= 8 {
		allUpper := true
		for _, r := range entry {
			if r < 'A' || r > 'Z' {
				allUpper = false
				break
			}
		}
		if allUpper {
			return true
		}
	}
	for _, w := range actionWords {
		if entry == w {
			return true
		}
	}
	return false
}

// OverlapRate 算 triggers 与 summary 的整词重合率：每个触发词条目
// （按顿号切分后的整项）原样出现在 summary 文本中（子串匹配）才算
// 命中；豁免条目分子分母都不计。重合率 = 命中条目数 / 计入条目总数。
//
// 口径与写简介的提示词一致（至少一半触发词须是简介里没有的词），
// 写的人能控制，质检与提示词不再打架。旧词元级口径（触发词分词后
// 算词元占比）量的是主题相似度：触发词本来就是文档的主题词，任何
// 正常简介都会与它共享大量词元，正常写法必然被打回，
// 2026-11-24 校准为整词级。
func OverlapRate(summary string, triggers []string) float64 {
	total, hit := 0, 0
	for _, trg := range triggers {
		trg = strings.TrimSpace(trg)
		if trg == "" || exemptOverlapEntry(trg) {
			continue
		}
		total++
		if strings.Contains(summary, trg) {
			hit++
		}
	}
	if total == 0 {
		return 0
	}
	return float64(hit) / float64(total)
}

// CheckOverlap：重合率 > 50% 打回（50% 及以下通过）。
func CheckOverlap(summary string, triggers []string) CheckResult {
	rate := OverlapRate(summary, triggers)
	if rate > 0.50 {
		return fail(2, "低重合拦截", fmt.Sprintf("触发词与简介重合率 %.0f%% > 50%%，须至少一半触发词是简介里没有的词", rate*100))
	}
	return pass(2, "低重合拦截", fmt.Sprintf("重合率 %.0f%% ≤ 50%%", rate*100))
}

// ---- 规则③ 泛词拦截 ----

// genericWords 是禁用泛词（v2.1 §5/§6）：系统/配置/记录/问题/方案/教程。
var genericWords = []string{"系统", "配置", "记录", "问题", "方案", "教程"}

// CheckGenericWords：触发词含禁用泛词的，整行打回。
func CheckGenericWords(triggers []string) CheckResult {
	for _, trg := range triggers {
		for _, w := range genericWords {
			if strings.Contains(trg, w) {
				return fail(3, "泛词拦截", fmt.Sprintf("触发词 %q 含禁用泛词 %q", trg, w))
			}
		}
	}
	return pass(3, "泛词拦截", "触发词无禁用泛词")
}

// ---- 规则④ 字数与数量校验 ----

// CheckCounts：summary 不在 30–100 单位内、等效宽度 >120、触发词不在
// 3–8 个内、剧本类缺动作词的，全部自动打回。
func CheckCounts(summary string, triggers []string, isPlaybook bool) CheckResult {
	units := CountUnits(summary)
	if units < 30 || units > 100 {
		return fail(4, "字数与数量校验", fmt.Sprintf("简介 %d 单位，不在 30–100 单位内", units))
	}
	if w := EffectiveWidth(summary); w > 120 {
		return fail(4, "字数与数量校验", fmt.Sprintf("简介等效宽度 %.1f > 120", w))
	}
	if n := len(triggers); n < MinTriggers || n > MaxTriggers {
		return fail(4, "字数与数量校验", fmt.Sprintf("触发词 %d 个，不在 3–8 个内", n))
	}
	if isPlaybook && !HasActionWord(summary, triggers) {
		return fail(4, "字数与数量校验", "剧本类简介 summary 与 triggers 均缺动作词（注册/部署/排查/迁移等）")
	}
	return pass(4, "字数与数量校验", fmt.Sprintf("简介 %d 单位、等效宽度 %.1f、触发词 %d 个", units, EffectiveWidth(summary), len(triggers)))
}

// ---- 规则⑤ 核心实体覆盖率 ----

var codeSpanRe = regexp.MustCompile("`[^`\\n]+`")
var fencedRe = regexp.MustCompile("(?s)```[a-zA-Z]*\\n(.*?)```")

// ExtractEntities 自动提取原文的核心实体前 5 个：标题分词优先，
// 其次代码块 token，再次正文高频专有名词（v2.1 §6 规则⑤）。
// 三路来源每路最多占 2 席，剩下的席位给正文高频词，避免一路噪音
// 挤占全部 5 席（2026-11-24 校准）。
func ExtractEntities(sourceText, title string) []string {
	seen := map[string]bool{}
	out := []string{}
	// add 把 toks 按 cap 限额加入 out（跳过的词不占席位）；满 5 即停。
	add := func(toks []string, cap int) {
		n := 0
		for _, t := range toks {
			if n >= cap || len(out) >= 5 {
				return
			}
			t = strings.TrimSpace(t)
			if t == "" || seen[strings.ToLower(t)] {
				continue
			}
			if RuneCount(t) < 2 {
				continue
			}
			// 泛词不当实体（场景/系统/system 等到处都有的词不配进前 5）。
			if genericTitleWords[strings.ToLower(t)] {
				continue
			}
			seen[strings.ToLower(t)] = true
			out = append(out, t)
			n++
		}
	}
	// 1. 标题段（实体提取与兜底触发词共用 splitTitleSegments）：
	// 长分句（超过 8 字）继续用 Tokenize 切成词，不要整句当实体——
	// 整句简介意译了也永远无法原文命中（2026-11-24 校准）。
	titleToks := []string{}
	for _, seg := range splitTitleSegments(title) {
		if RuneCount(seg) > 8 {
			titleToks = append(titleToks, Tokenize(seg)...)
		} else {
			titleToks = append(titleToks, seg)
		}
	}
	add(titleToks, 2)
	if len(out) >= 5 {
		return out
	}
	// 2. 代码块：行内 `code` 与围栏块里的词，先过 cleanCodeToken
	//（试点第二轮教训：p.replace、system、memory 这类噪音进了前 5，
	// 简介命中 0 个被误杀）。再过质量过滤：只保留在原文出现 ≥2 次
	// 的，或长度 ≥4 且不在泛词表的；偶然出现一次的短标识符
	//（如 dev）不进前 5（2026-11-24 校准）。
	codeToks := []string{}
	for _, m := range codeSpanRe.FindAllString(sourceText, -1) {
		for _, tok := range splitCodeTokens(strings.Trim(m, "`")) {
			if c := cleanCodeToken(tok); c != "" {
				codeToks = append(codeToks, c)
			}
		}
	}
	for _, m := range fencedRe.FindAllStringSubmatch(sourceText, -1) {
		for _, tok := range splitCodeTokens(m[1]) {
			if c := cleanCodeToken(tok); c != "" {
				codeToks = append(codeToks, c)
			}
		}
	}
	lowerSource := strings.ToLower(sourceText)
	qualityCodeToks := []string{}
	for _, tok := range codeToks {
		if strings.Count(lowerSource, strings.ToLower(tok)) >= 2 ||
			(RuneCount(tok) >= 4 && !genericTitleWords[strings.ToLower(tok)]) {
			qualityCodeToks = append(qualityCodeToks, tok)
		}
	}
	add(qualityCodeToks, 2)
	if len(out) >= 5 {
		return out
	}
	// 3. 正文高频词（TokenizeCount 已按频次排好，停用词已除）。
	// 中文 bigram 要求频次 ≥2：单次出现的切分碎片（如“可以回滚”
	// 切出的“以回”）不当实体（与兜底触发词同一口径）。
	freqToks := []string{}
	for _, tf := range TokenFreqs(sourceText) {
		if isHanBigram(tf.Token) && tf.Freq < 2 {
			continue
		}
		freqToks = append(freqToks, tf.Token)
	}
	add(freqToks, 5)
	return out
}

// splitTitleSegments 把标题按标点与连字符切段并清噪音（实体提取与
// 兜底触发词共用）。编号符、括号、度数、纯数字等先清掉：长标题如
// “发布到 Release 前三步：①按县分片…”否则会产出“①按县分片”这类
// 永远命中不了的实体，误杀简介。
func splitTitleSegments(title string) []string {
	segs := []string{}
	for _, seg := range strings.FieldsFunc(title, func(r rune) bool {
		return r == '-' || r == '_' || r == '/' || r == ' ' || r == '·' ||
			r == '：' || r == ':' || r == '、' || r == '，' || r == ',' ||
			r == '（' || r == '(' || r == '）' || r == ')' || r == '—' ||
			r == '>' || r == '<' || r == '=' || r == '"' ||
			r == '。' || r == '？' || r == '！' || r == '；' || r == ';'
	}) {
		seg = cleanEntitySegment(seg)
		if seg != "" {
			segs = append(segs, seg)
		}
	}
	return segs
}

// cleanEntitySegment 清掉实体段里的编号符、度数、括号残留与纯数字段。
func cleanEntitySegment(seg string) string {
	seg = strings.Map(func(r rune) rune {
		if strings.ContainsRune("①②③④⑤⑥⑦⑧⑨⑩°*#'\"`~^$%@|\\[]{}", r) {
			return -1
		}
		return r
	}, seg)
	seg = strings.TrimSpace(seg)
	if seg == "" {
		return ""
	}
	allDigit := true
	for _, r := range seg {
		if r < '0' || r > '9' {
			allDigit = false
			break
		}
	}
	if allDigit {
		return ""
	}
	return seg
}

// genericTitleWords 是不能当核心实体的泛词（标题段与代码词都滤掉）：
// v2.1 禁用泛词 + 场景/情况类空词 + 常见英文噪音词。
var genericTitleWords = map[string]bool{
	"系统": true, "配置": true, "记录": true, "问题": true, "方案": true, "教程": true,
	"场景": true, "情况": true, "说明": true, "介绍": true, "概述": true, "总结": true,
	"system": true, "memory": true, "data": true, "index": true, "json": true,
	"config": true, "file": true, "value": true, "result": true, "object": true,
	"string": true, "function": true, "const": true, "let": true, "var": true,
	"new": true, "this": true, "that": true, "with": true, "from": true, "into": true,
}

// cleanCodeToken 清掉代码词里的噪音：带点的属性链（p.replace）、
// 短词、泛词；返回 "" 表示丢弃。
func cleanCodeToken(tok string) string {
	tok = strings.Trim(tok, "._-")
	if tok == "" || strings.Contains(tok, ".") || RuneCount(tok) < 3 {
		return ""
	}
	if genericTitleWords[strings.ToLower(tok)] {
		return ""
	}
	return tok
}
func splitCodeTokens(code string) []string {
	out := []string{}
	for _, f := range strings.FieldsFunc(code, func(r rune) bool {
		return !(r < 128 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.')) && !isHan(r)
	}) {
		if f == "" {
			continue
		}
		out = append(out, strings.ToLower(f))
	}
	return out
}

// CheckEntityCoverage：summary + triggers 至少命中前 5 实体中的 2 个、
// 或命中率 ≥50%，否则打回。
func CheckEntityCoverage(summary string, triggers []string, sourceText, title string) CheckResult {
	ents := ExtractEntities(sourceText, title)
	if len(ents) == 0 {
		return pass(5, "核心实体覆盖率", "原文未提取到核心实体，跳过覆盖校验")
	}
	joined := strings.ToLower(summary + " " + strings.Join(triggers, " "))
	hit := 0
	for _, e := range ents {
		if strings.Contains(joined, strings.ToLower(e)) {
			hit++
		}
	}
	rate := float64(hit) / float64(len(ents))
	if hit >= 2 || rate >= 0.5 {
		return pass(5, "核心实体覆盖率", fmt.Sprintf("命中 %d/%d 个核心实体（%v）", hit, len(ents), ents))
	}
	return fail(5, "核心实体覆盖率", fmt.Sprintf("只命中 %d/%d 个核心实体（%v），须至少命中 2 个或 ≥50%%", hit, len(ents), ents))
}

// ---- 规则⑥ 反套路极性校验 ----

// CheckPolarity：l3/anti-patterns 的 summary 必须含否定/警示词，
// 缺了就打回；非反套路文档直接通过。
func CheckPolarity(summary string, isAntiPattern bool) CheckResult {
	if !isAntiPattern {
		return pass(6, "反套路极性校验", "非反套路文档，无需极性词")
	}
	if HasNegativeWord(summary) {
		return pass(6, "反套路极性校验", "反套路简介含否定/警示词")
	}
	return fail(6, "反套路极性校验", "反套路简介缺否定/警示词（禁止/避免/不要/失效/勿用/踩坑等）")
}

// ---- 脱敏检查（用户 2026-11-24 09:12 纠正）----

// CheckSecret 是简介输出的脱敏检查：生成的简介（summary + triggers）
// 必须过一遍 sanitize，含密钥字样打回重写。只返回布尔命中，
// 绝不返回命中内容。
func CheckSecret(summary string, triggers []string) CheckResult {
	joined := summary + " " + strings.Join(triggers, " ")
	if sanitize.ContainsSecret(joined) {
		return fail(0, "简介输出脱敏检查", "简介输出含密钥字样（已脱敏判定），打回重写")
	}
	return pass(0, "简介输出脱敏检查", "简介输出无密钥字样")
}
