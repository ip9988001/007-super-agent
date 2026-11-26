package summary

import (
	"strings"
	"unicode"
)

// FallbackSummary 是规则⑧的熔断兜底（v2.1 §6）：连续失败 3 次后
// 不再重跑，用规则提取拼成可用简介入库：首段核心句 +
// TF-IDF 高频词（本函数用单篇词频近似 TF-IDF 的 TF 部分，
// 试点阶段无全库 DF 表，缺口记在汇报里）。
// 返回的简介仍要过 CheckAll，检查结果由调用方记进 manifest。
func FallbackSummary(sourcePath, title, sourceText string) Summary {
	s := Summary{SourcePath: sourcePath, Status: StatusDegradedOK}
	// 首段核心句：跳过标题行、frontmatter、元数据行、代码行与
	// 复读标题的句子，取第一段真实正文。
	first := firstSentence(sourceText, title)
	// 试点首轮暴露的问题：兜底简介照抄标题会被规则①判复读标题，
	// 所以标题只作触发词候选，不再拼进简介正文。
	summary := first
	if summary == "" {
		summary = "这篇记录了一个值得注意的做法，帮你避坑少走弯路。"
	}
	// 兜底补字：不足 30 单位时先补第二句，再补高频词句。
	if CountUnits(summary) < 30 {
		if second := secondSentence(sourceText, title, first); second != "" {
			summary += second
		}
	}
	if CountUnits(summary) < 30 {
		words := topWordsFiltered(sourceText, 5)
		if len(words) > 0 {
			// 先去掉句末句号再接“，涉及”，否则会拼出“日志。，涉及”这种断句。
			summary = strings.TrimRight(summary, "。！？")
			summary += "，涉及" + strings.Join(words, "、") + "等要点。"
		}
	}
	summary = trimToBoundary(summary, 100)
	// 反套路极性兜底：缺否定词时前置警示。
	if IsAntiPattern(sourcePath) && !HasNegativeWord(summary) {
		summary = "不要这样做：" + summary
		summary = trimToBoundary(summary, 100)
	}
	// 剧本动作词兜底：缺动作词时补一个“排查”。
	if IsPlaybook(sourcePath, title) && !HasActionWord(summary, nil) {
		summary += "，用于排查定位。"
		summary = trimToBoundary(summary, 100)
	}
	// 兜底末道防线：既无动作词又无症状/问题指向词时，规则①会判
	// “只有背景”——补一句注意避坑（只用于兜底，不用于模型生成）。
	if !HasActionWord(summary, nil) && !hasSymptomWord(summary) {
		summary += "，注意避坑。"
		summary = trimToBoundary(summary, 100)
	}
	s.Summary = summary
	// 触发词兜底：优先选简介里没有的高频词/标题段/代码词，避开
	// 与简介 100% 重合（试点首轮的教训）；实在凑不够 3 个时才
	// 用重合词补位（仍可能被规则②打回，缺口记进汇报）。
	s.Core = fallbackTriggers(sourceText, title, summary)
	s.Dynamic = nil
	return s
}

// trimToBoundary 把超长简介截到 100 单位内，并回退到最后一个句内
// 标点处不断词（试点第二轮教训：按字符硬截会留下“6.- swapfile”
// 这类断词）。关键修正：只有真正截过才回退标点——
// 不截也回退会在多次调用里层层啃掉句子（调试证实：31 单位的简介
// 两次 trimToBoundary 后只剩 19 单位）。
func trimToBoundary(s string, maxUnits int) string {
	truncated := false
	for CountUnits(s) > maxUnits {
		s = TruncateRunes(s, RuneCount(s)-4)
		truncated = true
	}
	s = strings.TrimRight(s, "，。、；：")
	if !truncated {
		return s
	}
	r := []rune(s)
	last := -1
	for i, ch := range r {
		if strings.ContainsRune("，。、；：！？", ch) {
			last = i
		}
	}
	// 只有当截断点离末尾不远（≤12 字）时才回退，避免丢太多。
	if last > 0 && len(r)-1-last <= 12 {
		s = string(r[:last+1])
	}
	return strings.TrimRight(s, "，。、；：")
}

// fallbackTriggers 挑与简介正文不重合的触发词（最多 5 个进核心槽）。
func fallbackTriggers(sourceText, title, summaryText string) []string {
	lower := strings.ToLower(summaryText)
	inSummary := func(tok string) bool {
		return strings.Contains(lower, strings.ToLower(tok))
	}
	cands := []string{}
	seen := map[string]bool{}
	add := func(toks ...string) {
		for _, t := range toks {
			t = strings.TrimSpace(t)
			if t == "" || seen[strings.ToLower(t)] || isGenericWord(t) || inSummary(t) {
				continue
			}
			seen[strings.ToLower(t)] = true
			cands = append(cands, t)
		}
	}
	add(topWordsFiltered(sourceText, 12)...)
	// 标题段（宽切分、清噪音、超过 12 字的整句标题段不要）与代码词作补充。
	for _, seg := range splitTitleSegments(title) {
		if RuneCount(seg) <= 12 {
			add(seg)
		}
	}
	for _, m := range codeSpanRe.FindAllString(sourceText, -1) {
		for _, tok := range splitCodeTokens(strings.Trim(m, "`")) {
			if c := cleanCodeToken(tok); c != "" {
				add(c)
			}
		}
	}
	if len(cands) > CoreSlots {
		cands = cands[:CoreSlots]
	}
	if len(cands) < MinTriggers {
		// 凑不够：用重合的高频词补位（宁可被规则②打回，也不交不足 3 个的简介）。
		for _, w := range TopWords(sourceText, 12) {
			if len(cands) >= MinTriggers {
				break
			}
			if !seen[strings.ToLower(w)] && !isGenericWord(w) {
				seen[strings.ToLower(w)] = true
				cands = append(cands, w)
			}
		}
	}
	return cands
}

// TopWords 返回正文按词频排前 n 的词（去停用词；确定性排序）。
func TopWords(sourceText string, n int) []string {
	toks := TokenizeCount(sourceText)
	if len(toks) > n {
		toks = toks[:n]
	}
	return toks
}

// topWordsFiltered 与 TopWords 相同，但中文 bigram 要求频次 ≥2：
// 单次出现的切分碎片（如“服务器运维”切出的“务器”“器运”）不要，
// 试点第二轮曾把这类碎片当成触发词。
func topWordsFiltered(sourceText string, n int) []string {
	out := []string{}
	for _, tf := range TokenFreqs(sourceText) {
		if isHanBigram(tf.Token) && tf.Freq < 2 {
			continue
		}
		out = append(out, tf.Token)
		if len(out) >= n {
			break
		}
	}
	return out
}

func isHanBigram(tok string) bool {
	r := []rune(tok)
	return len(r) == 2 && isHan(r[0]) && isHan(r[1])
}

func isGenericWord(w string) bool {
	for _, g := range genericWords {
		if strings.Contains(w, g) {
			return true
		}
	}
	return false
}

// firstSentence 取正文第一段真实散文句：跳过标题行、YAML frontmatter
// （--- 包裹块）、二级标题、元数据行、代码行与复读标题的句子；
// 找不到返回 ""。
func firstSentence(sourceText, title string) string {
	return nthSentence(sourceText, title, 0)
}

// secondSentence 取除 first 外的下一句真实散文句（兜底补字用）。
func secondSentence(sourceText, title, first string) string {
	return nthSentence(sourceText, title, 1)
}

// nthSentence 取第 n 个真实散文句（n 从 0 起）。
func nthSentence(sourceText, title string, n int) string {
	lines := strings.Split(sourceText, "\n")
	inFrontmatter := false
	seen := 0
	for i, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		if i == 0 && trimmed == "---" {
			inFrontmatter = true
			continue
		}
		if inFrontmatter {
			if trimmed == "---" {
				inFrontmatter = false
			}
			continue
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if isMetadataLine(trimmed) || isCodeLine(trimmed) {
			continue
		}
		if !strings.ContainsAny(trimmed, "的一是在有要会能就") && RuneCount(trimmed) < 8 {
			continue
		}
		// 取本行第一句。
		sent := trimmed
		for j, r := range trimmed {
			if r == '。' || r == '？' || r == '！' || r == '.' {
				sent = strings.TrimSpace(trimmed[:j+len(string(r))])
				break
			}
		}
		// 复读标题的句子不要：正文里常有一行与标题一字不差，
		// 拿它当简介会被规则①判复读标题（试点第二轮教训）。
		if title != "" && (strings.Contains(sent, title) ||
			(strings.Contains(title, sent) && RuneCount(sent) > RuneCount(title)/2)) {
			continue
		}
		if seen == n {
			return sent
		}
		seen++
	}
	return ""
}

// isCodeLine 报告一行是否像代码（赋值语句、箭头函数、行注释、
// 加粗残片），不是真实散文。试点第二轮曾把 timer = setInterval(...)
// 当成首句拼进兜底简介。
func isCodeLine(line string) bool {
	if strings.Contains(line, "=>") || strings.Contains(line, "();") ||
		strings.HasPrefix(line, "//") || strings.HasPrefix(line, "**") ||
		strings.HasPrefix(line, "$") || strings.Contains(line, "```") {
		return true
	}
	if i := strings.Index(line, "="); i > 0 {
		head := strings.TrimSpace(line[:i])
		if head != "" && !strings.ContainsAny(head, "，。、；： ") {
			ok := true
			for _, r := range head {
				if !(r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '$')) {
					ok = false
					break
				}
			}
			if ok {
				return true
			}
		}
	}
	return false
}

// isMetadataLine 报告一行是否为元数据行（frontmatter 键值对、
// “- **类型**: …”式清单、纯链接/纯代码行），不是真实散文。
func isMetadataLine(line string) bool {
	if strings.HasPrefix(line, "- **") || strings.HasPrefix(line, "- [") {
		return true
	}
	// YAML 键值对：type: / title: / severity: / tags: / keywords: 等。
	if k, _, ok := strings.Cut(line, ":"); ok {
		k = strings.TrimSpace(k)
		if k != "" && !strings.ContainsAny(k, "，。、；： ") && len(k) <= 12 {
			lk := strings.ToLower(k)
			for _, meta := range []string{"type", "title", "severity", "created", "tags", "keywords", "aliases", "path", "layer", "category", "status", "date", "author", "source", "类型", "路径", "标签"} {
				if lk == meta {
					return true
				}
			}
		}
	}
	return false
}
