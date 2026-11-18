// Package memory 是 007 超级 Agent 的记忆子系统：三层存储 + 单一写入口 + 换页。
//
// 设计依据（《超级Agent-全新设计方案》v1.5）：
//   - L1 7 天日志：append-only，按天分文件，过期自动清理。
//   - L3 Wiki：实体 / 概念 / 来源 / 综合四类分目录，文件名即词条名。
//   - 换页：会话超 200000 token 时压成 ≤6000 字符摘要，先扣页脚再截断，
//     旧页链只留最近 8 条。摘要的语义压缩由调用方（模型）完成，本包只
//     负责封顶与落链，不伪造“已压缩”的结论。
//   - 单一写入口：所有写 Wiki 只走 WriteWiki，写完落一条 L1 日志。
//   - 检索：混合检索，词法必有；语义是注入接口，未注入时只用词法，
//     绝不伪造向量结果。默认 top_k=3，每条 ≤240 字符（先转 []rune 按
//     字符切），Wiki 只回 Summary。模型不调用 Recall 就不主动塞上下文。
//   - 第 6 轮坑 2：所有截断先转 []rune，防中文从字节中间劈开乱码。
//
// 铁律：写失败返回错误，不许静默吞掉；不写任何密钥、Token。
package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// 换页与检索的硬上限（v1.5 定稿口径）。
const (
	RolloverThreshold = 200000 // 会话 token 超过此值触发换页
	MaxSummaryRunes   = 6000   // 换页摘要上限（字符）
	MaxCarryover      = 8      // 换页摘要链只留最近 8 条
	MaxResultRunes    = 240    // 单条检索结果上限（字符）
	TopK              = 3      // 检索默认取前 3 条
	L1RetentionDays   = 7      // L1 日志保留天数
)

// WikiKind 是 Wiki 四类之一。
type WikiKind string

const (
	KindEntities  WikiKind = "entities"  // 实体
	KindConcepts  WikiKind = "concepts"  // 概念
	KindSources   WikiKind = "sources"   // 来源
	KindSynthesis WikiKind = "synthesis" // 综合
)

// WikiEntry 是一条 Wiki 词条。
type WikiEntry struct {
	Kind    WikiKind
	Title   string // 词条名，即文件名；不含 / 与 ..
	Summary string // 一句话摘要；检索时 Wiki 只回这一项
	Body    string // 正文
}

// Result 是一条检索结果。
type Result struct {
	Source string // 如 wiki/concepts/测试词条 或 l1/2026-11-17.log
	Text   string // 已截断到 ≤240 字符
	Score  int    // 关键词命中次数
}

// SemanticSearchFunc 是语义检索的注入接口，由调用方接向量库。
// 未注入时 Recall 只用词法检索，绝不伪造语义结果。
type SemanticSearchFunc func(query string) ([]Result, error)

// Store 是记忆的唯一持有者。写操作由 mu 串行化。
type Store struct {
	root     string
	mu       sync.Mutex
	seq      int
	semantic SemanticSearchFunc
}

// Open 创建记忆目录（l1/、wiki/四类、carryover/）并返回 Store。
// root 不存在时自动创建。
func Open(root string) (*Store, error) {
	dirs := []string{
		filepath.Join(root, "l1"),
		filepath.Join(root, "carryover"),
	}
	for _, k := range []WikiKind{KindEntities, KindConcepts, KindSources, KindSynthesis} {
		dirs = append(dirs, filepath.Join(root, "wiki", string(k)))
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("memory mkdir %s: %w", d, err)
		}
	}
	return &Store{root: root}, nil
}

// WithSemantic 注入语义检索函数（链式配置，未注入则纯词法）。
func (s *Store) WithSemantic(fn SemanticSearchFunc) *Store {
	s.semantic = fn
	return s
}

// validKind 校验 Wiki 分类。
func validKind(k WikiKind) bool {
	switch k {
	case KindEntities, KindConcepts, KindSources, KindSynthesis:
		return true
	}
	return false
}

// validTitle 校验词条名：非空，不含 / \ ..，不为 .。
func validTitle(t string) bool {
	if t == "" || t == "." || t == ".." {
		return false
	}
	if strings.ContainsAny(t, `/\`) || strings.Contains(t, "..") {
		return false
	}
	return true
}

// truncateRunes 按字符截断到 n 个字（先转 []rune，防中文乱码）。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// appendL1 往当天 L1 日志追加一行（append-only，0600）。
func (s *Store) appendL1(text string) error {
	name := time.Now().Format("2006-01-02") + ".log"
	f, err := os.OpenFile(filepath.Join(s.root, "l1", name),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("memory l1 open: %w", err)
	}
	defer f.Close()
	line := time.Now().Format(time.RFC3339) + "\t" + text + "\n"
	if _, err := f.WriteString(line); err != nil {
		return fmt.Errorf("memory l1 write: %w", err)
	}
	return nil
}

// AppendL1 往当天 L1 日志追加一行原文（框架逐条采集用，不经模型选择）。
// 模型漏记也不丢。写失败返回错误，调用方只记日志、不阻塞回执。
func (s *Store) AppendL1(text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendL1(text)
}

// ReadL1 读指定日期（YYYY-MM-DD）的 L1 日志全文；文件不存在返回错误，
// 调用方据此判定“当天无日志可精炼”而跳过。
func (s *Store) ReadL1(date string) (string, error) {
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return "", fmt.Errorf("memory read l1: 非法日期 %q", date)
	}
	b, err := os.ReadFile(filepath.Join(s.root, "l1", date+".log"))
	if err != nil {
		return "", fmt.Errorf("memory read l1 %s: %w", date, err)
	}
	return string(b), nil
}

// WriteWiki 是写 Wiki 的唯一入口。写完落一条 L1 日志。
func (s *Store) WriteWiki(e WikiEntry) error {
	if !validKind(e.Kind) {
		return fmt.Errorf("memory write wiki: 非法分类 %q", e.Kind)
	}
	if !validTitle(e.Title) {
		return fmt.Errorf("memory write wiki: 非法词条名 %q", e.Title)
	}
	if strings.TrimSpace(e.Body) == "" {
		return fmt.Errorf("memory write wiki: 词条 %q 内容为空", e.Title)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	content := "# " + e.Title + "\n\nSummary: " + strings.TrimSpace(e.Summary) + "\n\n" + e.Body + "\n"
	path := filepath.Join(s.root, "wiki", string(e.Kind), e.Title+".md")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("memory write wiki %s: %w", path, err)
	}
	if err := s.appendL1("wiki write " + string(e.Kind) + "/" + e.Title); err != nil {
		return err
	}
	return nil
}

// PruneL1 清理 7 天前的 L1 日志文件（按文件名日期判断）。
func (s *Store) PruneL1() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().AddDate(0, 0, -L1RetentionDays).Format("2006-01-02")
	entries, err := os.ReadDir(filepath.Join(s.root, "l1"))
	if err != nil {
		return 0, fmt.Errorf("memory prune l1: %w", err)
	}
	removed := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".log") {
			continue
		}
		date := strings.TrimSuffix(name, ".log")
		if date < cutoff {
			if err := os.Remove(filepath.Join(s.root, "l1", name)); err != nil {
				return removed, fmt.Errorf("memory prune l1 %s: %w", name, err)
			}
			removed++
		}
	}
	return removed, nil
}

// keywords 从查询里拆关键词：按空白与 ASCII 标点分词；中文长词再补
// 二字滑窗，保证“超级Agent记忆”这类无空格查询也能命中。
func keywords(q string) []string {
	fields := strings.FieldsFunc(q, func(r rune) bool {
		return unicode.IsSpace(r) || (r < 128 && !unicode.IsLetter(r) && !unicode.IsDigit(r))
	})
	out := []string{}
	seen := map[string]bool{}
	add := func(w string) {
		if w != "" && !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	for _, f := range fields {
		add(f)
		r := []rune(f)
		hasCJK := false
		for _, c := range r {
			if unicode.Is(unicode.Han, c) {
				hasCJK = true
				break
			}
		}
		if hasCJK && len(r) > 2 {
			for i := 0; i+2 <= len(r); i++ {
				add(string(r[i : i+2]))
			}
		}
	}
	return out
}

// score 统计内容里关键词命中次数（大小写不敏感）。
func score(content string, kws []string) int {
	low := strings.ToLower(content)
	n := 0
	for _, k := range kws {
		n += strings.Count(low, strings.ToLower(k))
	}
	return n
}

// firstBodyText 是展示文本的兜底：没有 Summary 行、也没有 ## 内容 节时，
// 取正文前几行有意义的文字（跳过标题行与空行），避免高分命中回空文本。
func firstBodyText(data string) string {
	var b strings.Builder
	for _, ln := range strings.Split(data, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		b.WriteString(ln)
		b.WriteString(" ")
		if len([]rune(b.String())) >= MaxResultRunes {
			break
		}
	}
	return strings.TrimSpace(b.String())
}

// contentSection 取二号 L3 参考文件的 ## 内容 节（到下一个 ## 标题为止）。
// 没有该节时返回空串，调用方只用它做展示文本，计分仍按全文。
func contentSection(data string) string {
	lines := strings.Split(data, "\n")
	start := -1
	for i, ln := range lines {
		if strings.HasPrefix(ln, "## 内容") {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return ""
	}
	var b strings.Builder
	for _, ln := range lines[start:] {
		if strings.HasPrefix(ln, "## ") {
			break
		}
		b.WriteString(ln)
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

// Recall 混合检索：词法必查 L1、Wiki 与 L3 参考（二号记忆文件）；语义已注入时合并（同源取
// 最高分），未注入时只用词法。结果按分数降序取前 3 条，每条 ≤240 字符。
func (s *Store) Recall(query string) ([]Result, error) {
	kws := keywords(query)
	if len(kws) == 0 {
		return nil, fmt.Errorf("memory recall: 查询无有效关键词")
	}
	bySource := map[string]Result{}

	// 词法：Wiki 只读 Summary 行。
	wikiRoot := filepath.Join(s.root, "wiki")
	_ = filepath.WalkDir(wikiRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		summary := ""
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "Summary:") {
				summary = strings.TrimSpace(strings.TrimPrefix(line, "Summary:"))
				break
			}
		}
		if summary == "" {
			summary = firstBodyText(string(data))
		}
		if sc := score(string(data), kws); sc > 0 {
			rel, _ := filepath.Rel(s.root, path)
			// Wiki 源标识去掉 .md，与调用方（语义检索）用的词条路径一致，合并去重才生效。
			rel = strings.TrimSuffix(rel, ".md")
			bySource[rel] = Result{Source: rel, Text: truncateRunes(summary, MaxResultRunes), Score: sc}
		}
		return nil
	})

	// 词法：L1 日志按行计分，每行一条候选。
	l1Root := filepath.Join(s.root, "l1")
	entries, _ := os.ReadDir(l1Root)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(l1Root, e.Name()))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if sc := score(line, kws); sc > 0 {
				src := "l1/" + e.Name()
				if old, ok := bySource[src]; !ok || sc > old.Score {
					bySource[src] = Result{Source: src, Text: truncateRunes(line, MaxResultRunes), Score: sc}
				}
			}
		}
	}

	// 词法：L3 参考（二号记忆文件，按分类保存）：读 ## 内容 节。
	refRoot := filepath.Join(s.root, "reference")
	_ = filepath.WalkDir(refRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if sc := score(string(data), kws); sc > 0 {
			rel, _ := filepath.Rel(s.root, path)
			rel = strings.TrimSuffix(rel, ".md")
			text := contentSection(string(data))
			if text == "" {
				text = firstBodyText(string(data))
			}
			if old, ok := bySource[rel]; !ok || sc > old.Score {
				bySource[rel] = Result{Source: rel, Text: truncateRunes(text, MaxResultRunes), Score: sc}
			}
		}
		return nil
	})

	// 语义：已注入才合并，未注入不伪造。
	if s.semantic != nil {
		sem, err := s.semantic(query)
		if err != nil {
			return nil, fmt.Errorf("memory recall semantic: %w", err)
		}
		for _, r := range sem {
			r.Text = truncateRunes(r.Text, MaxResultRunes)
			if old, ok := bySource[r.Source]; !ok || r.Score > old.Score {
				bySource[r.Source] = r
			}
		}
	}

	all := make([]Result, 0, len(bySource))
	for _, r := range bySource {
		all = append(all, r)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Score != all[j].Score {
			return all[i].Score > all[j].Score
		}
		return all[i].Source < all[j].Source
	})
	if len(all) > TopK {
		all = all[:TopK]
	}
	return all, nil
}

// rolloverFooter 是换页摘要的页脚，先扣掉它的长度再截断正文，
// 保证摘要总长 ≤6000 字符。
const rolloverFooter = "\n\n—— 换页摘要：原文已归档，仅保留本摘要进入新会话。"

// Rollover 会话超 200000 token 时换页：正文先扣页脚再截断到 6000 字符，
// 摘要写入换页链并只留最近 8 条。未超阈值返回 rolled=false，不写链。
func (s *Store) Rollover(sessionTokens int, sessionText string) (string, bool, error) {
	if sessionTokens <= RolloverThreshold {
		return "", false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	budget := MaxSummaryRunes - len([]rune(rolloverFooter))
	summary := truncateRunes(sessionText, budget) + rolloverFooter
	s.seq++
	name := fmt.Sprintf("%s-%03d.md", time.Now().UTC().Format("20060102T150405Z"), s.seq)
	path := filepath.Join(s.root, "carryover", name)
	if err := os.WriteFile(path, []byte(summary), 0o600); err != nil {
		return "", false, fmt.Errorf("memory rollover write: %w", err)
	}
	if err := s.pruneCarryoverLocked(); err != nil {
		return "", false, err
	}
	return summary, true, nil
}

// pruneCarryoverLocked 换页链只留最近 8 条，删最旧的（调用方须持锁）。
func (s *Store) pruneCarryoverLocked() error {
	entries, err := os.ReadDir(filepath.Join(s.root, "carryover"))
	if err != nil {
		return fmt.Errorf("memory prune carryover: %w", err)
	}
	names := []string{}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for len(names) > MaxCarryover {
		oldest := names[0]
		if err := os.Remove(filepath.Join(s.root, "carryover", oldest)); err != nil {
			return fmt.Errorf("memory prune carryover %s: %w", oldest, err)
		}
		names = names[1:]
	}
	return nil
}

// ListCarryover 返回换页链文件名（按时间升序）。
func (s *Store) ListCarryover() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, "carryover"))
	if err != nil {
		return nil, fmt.Errorf("memory list carryover: %w", err)
	}
	names := []string{}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}
