// Package summary 是线 B 第二阶段第 2 步：试点 20 篇简介 + 8 条自动质检。
//
// 口径来源：v2.1 第五节（简介库）与第六节（质检与脱敏），另加用户
// 2026-11-24 09:12 的脱敏纠正（密钥必须保持可用）：
//   - 送给写简介的模型之前，先用 internal/sanitize 把输入掩码；
//   - 生成的简介输出必须过一遍脱敏检查，含密钥字样打回重写；
//   - 绝不改源文件；
//   - 绝不在卡片/全文交付路径上加掩码（第 4 节点已取消，本包不实现它）；
//   - 召回账本写前掩码保持（RecordRecall 已有，本包不重做）。
//
// 简介格式（v2.1 §5，一字不差）：每个简介文件只写两行，第一行
// `summary: ` 开头一句话简介，第二行 `triggers: ` 开头触发词、逗号分隔。
package summary

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Status 是简介状态（v2.1 §5）。
type Status string

const (
	StatusOK         Status = "ok"
	StatusPending    Status = "pending"
	StatusStale      Status = "stale"
	StatusPinned     Status = "pinned"
	StatusDegradedOK Status = "degraded_ok"
	StatusGenerating Status = "generating"
)

// MaxRetries 是质检重试熔断次数（v2.1 §6 规则⑧）：连续失败 3 次改标
// degraded_ok，用规则提取兜底入库，不再无限重跑。
const MaxRetries = 3

// CoreSlots 是触发词核心保护槽数，DynamicSlots 是动态反哺槽数
// （v2.1 §5/§6：8 个触发词 = 5 核心 + 3 动态；试点阶段动态槽可空）。
const (
	CoreSlots    = 5
	DynamicSlots = 3
	MaxTriggers  = CoreSlots + DynamicSlots
	MinTriggers  = 3
)

// Summary 是一篇简介的完整记录（主库字段口径见 v2.1 §5）。
type Summary struct {
	DocID       string   `json:"doc_id"`       // 全局唯一短 ID：wiki 用 W-####，l3 用 L3-####
	SourcePath  string   `json:"source_path"`  // 源文件相对路径，如 wiki/entities/xxx.md
	SummaryPath string   `json:"summary_path"` // 简介相对路径，镜像源文件分类子目录
	SourceHash  string   `json:"source_hash"`  // 源文件 SHA-256
	SummaryHash string   `json:"summary_hash"` // 简介文件 SHA-256
	Status      Status   `json:"status"`
	RetryCount  int      `json:"retry_count"`
	Summary     string   `json:"summary"`          // 一句话简介（summary: 行正文）
	Core        []string `json:"core_triggers"`    // 5 个核心保护槽（禁止被反哺淘汰）
	Dynamic     []string `json:"dynamic_triggers"` // 3 个动态反哺槽（试点阶段可空）
}

// Triggers 返回全部触发词：核心槽在前、动态槽在后，去空、去重、保序。
func (s Summary) Triggers() []string {
	seen := map[string]bool{}
	out := []string{}
	for _, t := range append(append([]string{}, s.Core...), s.Dynamic...) {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// Format 把简介写成两行文件内容（v2.1 §5 格式，一字不差）。
func Format(s Summary) string {
	return "summary: " + strings.TrimSpace(s.Summary) + "\n" +
		"triggers: " + strings.Join(s.Triggers(), "，") + "\n"
}

// triggerSplit 切触发词行：中文逗号、英文逗号、顿号、分号都接受。
var triggerSplit = regexp.MustCompile(`[,，、；;]`)

// Parse 解析两行简介文件。容忍模型回复带 ``` 围栏与多余空行，
// 但行前缀与行数必须合规，否则返回错误（由调用方记一次质检失败）。
func Parse(text string) (Summary, error) {
	var s Summary
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(text, "```")
	text = strings.TrimPrefix(text, "```text")
	text = strings.TrimPrefix(text, "```markdown")
	text = strings.TrimSuffix(text, "```")
	lines := []string{}
	for _, ln := range strings.Split(text, "\n") {
		ln = strings.TrimSpace(ln)
		if ln != "" {
			lines = append(lines, ln)
		}
	}
	if len(lines) != 2 {
		return s, fmt.Errorf("summary parse: 需要恰好 2 行，实际 %d 行", len(lines))
	}
	sum, ok := cutPrefixFold(lines[0], "summary:")
	if !ok {
		return s, fmt.Errorf("summary parse: 第一行必须以 summary: 开头")
	}
	trg, ok := cutPrefixFold(lines[1], "triggers:")
	if !ok {
		return s, fmt.Errorf("summary parse: 第二行必须以 triggers: 开头")
	}
	s.Summary = strings.TrimSpace(sum)
	for _, t := range triggerSplit.Split(trg, -1) {
		if t = strings.TrimSpace(t); t != "" {
			s.Core = append(s.Core, t)
		}
	}
	// 解析时全部触发词先放核心槽；调用方按需把超出的 5 个后
	// 移入动态槽（NormalizeSlots）。超过 8 个时只留前 8 个。
	if len(s.Core) > MaxTriggers {
		s.Core = s.Core[:MaxTriggers]
	}
	s.Dynamic = nil
	NormalizeSlots(&s)
	return s, nil
}

// NormalizeSlots 把触发词按 5+3 拆槽：前 5 个进核心槽，其余进动态槽。
func NormalizeSlots(s *Summary) {
	all := s.Triggers()
	s.Core = nil
	s.Dynamic = nil
	for i, t := range all {
		if i < CoreSlots {
			s.Core = append(s.Core, t)
		} else {
			s.Dynamic = append(s.Dynamic, t)
		}
	}
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) {
		return "", false
	}
	if !strings.EqualFold(s[:len(prefix)], prefix) {
		return "", false
	}
	return s[len(prefix):], true
}

// SHA256Hex 返回内容的 SHA-256 十六进制。
func SHA256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// ---- 计量（v2.1 §5）----

// isHan 报告 r 是否汉字（CJK 统一表意文字区）。
func isHan(r rune) bool {
	return unicode.Is(unicode.Han, r)
}

// CountUnits 计简介单位数：1 汉字 = 1 单位，1 个连续英文/数字词 = 1 单位，
// 其余字符（标点、空格）不计。合法区间 30–100。
func CountUnits(s string) int {
	units := 0
	inWord := false
	for _, r := range s {
		switch {
		case isHan(r):
			units++
			inWord = false
		case r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			if !inWord {
				units++
				inWord = true
			}
		default:
			inWord = false
		}
	}
	return units
}

// EffectiveWidth 计等效宽度：汉字数×1 + ASCII 字符数×0.5，须 ≤120。
func EffectiveWidth(s string) float64 {
	han, ascii := 0, 0
	for _, r := range s {
		if isHan(r) {
			han++
		} else if r < 128 {
			ascii++
		}
	}
	return float64(han) + float64(ascii)*0.5
}

// ---- 分词（供重合率、实体覆盖、TF-IDF 兜底共用）----

// stopWords 是不计入高频词的停用词（中文虚词 + 英文虚词）。
var stopWords = map[string]bool{
	"的": true, "了": true, "是": true, "在": true, "和": true, "与": true, "等": true,
	"及": true, "对": true, "就": true, "也": true, "都": true, "把": true, "被": true,
	"给": true, "由": true, "从": true, "向": true, "并": true, "或": true, "但": true,
	"若": true, "则": true, "因": true, "所以": true, "然后": true, "这个": true, "这些": true,
	"我们": true, "你们": true, "他们": true, "以及": true, "可以": true, "需要": true,
	"进行": true, "使用": true, "一个": true, "已经": true, "通过": true, "但是": true,
	"如果": true, "没有": true, "还会": true, "会被": true, "让": true, "当": true,
	"而且": true, "并为": true, "及其": true, "所": true, "为": true, "以": true,
	"之": true, "其": true, "the": true, "and": true, "for": true,
	"with": true, "this": true, "that": true, "from": true, "into": true,
}

// isJunkToken 报告一个词是否为噪音：单字符 ASCII 词（如 cp -a 里的 a）、
// 纯数字串（如 2、10）不能当触发词或实体，试点首轮曾产出这类垃圾词。
func isJunkToken(tok string) bool {
	if tok == "" {
		return true
	}
	allASCII := true
	allDigit := true
	for _, r := range tok {
		if r >= 128 {
			allASCII = false
			allDigit = false
			break
		}
		if r < '0' || r > '9' {
			allDigit = false
		}
	}
	if !allASCII {
		return false
	}
	return RuneCount(tok) < 2 || allDigit
}

// Tokenize 把文本切成词：ASCII 字母/数字连续串为一个词（保留原始大小写，
// 供“核心缩写”豁免等处按原始大小写判定；比较时调用方自行小写化），
// 汉字按 2 字滑窗切 bigram（单字不成词），其余字符作分隔。
// 返回去重保序的词表。供实体覆盖、TF-IDF 兜底共用；
// 重合率已改整词级（2026-11-24 校准），不再用分词。
func Tokenize(text string) []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(tok string) {
		if tok == "" || isJunkToken(tok) || stopWords[strings.ToLower(tok)] || seen[tok] {
			return
		}
		seen[tok] = true
		out = append(out, tok)
	}
	var word []rune
	var hanRun []rune
	flushWord := func() {
		if len(word) > 0 {
			add(string(word))
			word = word[:0]
		}
	}
	flushHan := func() {
		for i := 0; i+1 < len(hanRun); i++ {
			add(string(hanRun[i : i+2]))
		}
		hanRun = hanRun[:0]
	}
	for _, r := range text {
		switch {
		case isHan(r):
			flushWord()
			hanRun = append(hanRun, r)
		case r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			flushHan()
			word = append(word, r)
		default:
			flushWord()
			flushHan()
		}
	}
	flushWord()
	flushHan()
	return out
}

// TokenFreq 是一条带频次的词。
type TokenFreq struct {
	Token string
	Freq  int
}

// TokenFreqs 返回文本的词频表：频次高者在前，频次相同按词典序，
// 保证确定性（与 TokenizeCount 同一切法、同过滤）。
func TokenFreqs(text string) []TokenFreq {
	freq := map[string]int{}
	var word []rune
	var hanRun []rune
	add := func(tok string) {
		if tok == "" || isJunkToken(tok) || stopWords[tok] {
			return
		}
		freq[tok]++
	}
	flushWord := func() {
		if len(word) > 0 {
			add(strings.ToLower(string(word)))
			word = word[:0]
		}
	}
	flushHan := func() {
		for i := 0; i+1 < len(hanRun); i++ {
			add(string(hanRun[i : i+2]))
		}
		hanRun = hanRun[:0]
	}
	for _, r := range text {
		switch {
		case isHan(r):
			flushWord()
			hanRun = append(hanRun, r)
		case r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			flushHan()
			word = append(word, unicode.ToLower(r))
		default:
			flushWord()
			flushHan()
		}
	}
	flushWord()
	flushHan()
	out := make([]TokenFreq, 0, len(freq))
	for t, f := range freq {
		out = append(out, TokenFreq{Token: t, Freq: f})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Freq != out[j].Freq {
			return out[i].Freq > out[j].Freq
		}
		return out[i].Token < out[j].Token
	})
	return out
}

// TokenizeCount 与 Tokenize 同一切法，但返回带频次的词表
// （供 TF 排序用；频次高者在前，频次相同按词典序，保证确定性）。
func TokenizeCount(text string) []string {
	toks := []string{}
	for _, tf := range TokenFreqs(text) {
		toks = append(toks, tf.Token)
	}
	return toks
}

// actionWords 是剧本类必须含的动作词（v2.1 §5：注册/部署/排查/迁移等）。
// 2026-11-24 校准补齐：简介里常见用途动词，缺了会把“写了用途的
// 简介”误判为只有背景（规则①误杀 5 篇，见 TestCheckBoilerplateActionWords）。
// 不加 是/有/在/和 这类虚词，避免规则失效。
var actionWords = []string{
	"注册", "部署", "排查", "迁移", "备份", "安装", "配置", "开通",
	"验证", "抓取", "同步", "恢复", "升级", "回滚", "搭建", "排障",
	"调优", "采集", "发布", "下发", "调用", "登录", "激活", "巡检",
	"改用", "换成",
	"启动", "装", "记录", "理解", "规定", "生成", "解释", "判断",
	"选择", "参考", "执行", "运行", "处理", "创建", "查询", "编写",
	"实现", "设置", "修改", "查看", "获取", "拉起", "访问", "打开",
	"停止", "清理", "检查", "测试", "诊断", "定位", "说明", "区分", "识别",
}

// HasActionWord 报告 summary 或 triggers 是否含至少 1 个动作词。
func HasActionWord(summary string, triggers []string) bool {
	joined := summary + " " + strings.Join(triggers, " ")
	for _, w := range actionWords {
		if strings.Contains(joined, w) {
			return true
		}
	}
	return false
}

// negativeWords 是反套路极性词（v2.1 §6 规则⑥）。
var negativeWords = []string{
	"禁止", "避免", "不要", "失效", "勿用", "踩坑", "别", "切勿", "不可", "千万别",
}

// HasNegativeWord 报告 summary 是否含否定/警示词。
func HasNegativeWord(summary string) bool {
	for _, w := range negativeWords {
		if strings.Contains(summary, w) {
			return true
		}
	}
	return false
}

// IsPlaybook 按 v2.1 §4/§5 判定剧本类：wiki/playbooks 下，或标题含
// 全流程/剧本字样（如 CF 注册剧本现放 entities，日后挪入 playbooks）。
func IsPlaybook(sourcePath, title string) bool {
	if strings.HasPrefix(sourcePath, "wiki/playbooks/") {
		return true
	}
	return strings.Contains(title, "全流程") || strings.Contains(title, "剧本")
}

// IsAntiPattern 报告是否为 l3/anti-patterns 文档。
func IsAntiPattern(sourcePath string) bool {
	return strings.HasPrefix(sourcePath, "l3/anti-patterns/")
}

// TitleFromSource 从源文件取标题：首个 `# ` 标题优先，否则用文件名
// （去掉 .md 后缀与开头的 YYYY-MM-DD- 日期前缀）。
func TitleFromSource(sourcePath string, content []byte) string {
	for _, ln := range strings.Split(string(content), "\n") {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "# ") {
			if t := strings.TrimSpace(strings.TrimPrefix(ln, "# ")); t != "" {
				return t
			}
		}
	}
	base := sourcePath
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSuffix(base, ".md")
	if len(base) > 11 && base[4] == '-' && base[7] == '-' {
		base = base[11:]
	}
	return base
}

// RuneCount 返回字符串的字符数（先转 []rune，防中文乱码）。
func RuneCount(s string) int { return utf8.RuneCountInString(s) }

// TruncateRunes 按字符截断到 n 个字。
func TruncateRunes(s string, n int) string {
	if RuneCount(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
