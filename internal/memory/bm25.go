package memory

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// Go 内 BM25（线 B 第二阶段第 3 棒）：全文层、reference 通道、L1 通道
// 共用。分词与 memory.keywords 同一切法（ASCII 词 + 中文二字滑窗），
// 保证查询词与文档词同一口径。全文层 b=0.75（v2.1 §7），
// 简介层 b=0.25–0.35 走 FTS5（见 store.go SearchFTS），两层分开调。

// bm25Terms 把文本切成计分词：ASCII 字母/数字连续串为一个词
// （小写化），汉字按二字滑窗切；单字 ASCII 词与纯数字串不计
// （与 summary.Tokenize 的 isJunkToken 同一口径，防“a”“2”
// 这类噪音词进 BM25）。返回含重复的词表（供词频统计）。
func bm25Terms(text string) []string {
	var out []string
	var word []rune
	var hanRun []rune
	flushWord := func() {
		if len(word) > 0 {
			w := strings.ToLower(string(word))
			// 纯数字串只留 3 位及以上（端口号/版本号，如 38999、200；
			// “2”“10”这类 1–2 位数字是噪音）；字母数字混杂的词都留。
			if len(word) >= 2 && (!isAllDigits(w) || len(word) >= 3) {
				out = append(out, w)
			}
			word = word[:0]
		}
	}
	flushHan := func() {
		for i := 0; i+1 < len(hanRun); i++ {
			out = append(out, string(hanRun[i:i+2]))
		}
		hanRun = hanRun[:0]
	}
	for _, r := range text {
		switch {
		case unicode.Is(unicode.Han, r):
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
	return out
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// bm25Doc 是一篇待计分的文档（词频表 + 词数）。
type bm25Doc struct {
	ID     string
	TF     map[string]int
	Length int
}

// bm25Index 是建好的 BM25 索引：文档词频 + 文档频率 + 平均词数。
type bm25Index struct {
	docs   []bm25Doc
	df     map[string]int
	avgLen float64
	n      int
}

// newBM25Index 建索引：texts 与 ids 一一对应（ids[i] 是第 i 篇的标识）。
func newBM25Index(ids []string, texts []string) *bm25Index {
	idx := &bm25Index{df: map[string]int{}, n: len(ids)}
	totalLen := 0
	for i, text := range texts {
		terms := bm25Terms(text)
		d := bm25Doc{ID: ids[i], TF: map[string]int{}, Length: len(terms)}
		seen := map[string]bool{}
		for _, t := range terms {
			d.TF[t]++
			if !seen[t] {
				seen[t] = true
				idx.df[t]++
			}
		}
		totalLen += len(terms)
		idx.docs = append(idx.docs, d)
	}
	if idx.n > 0 {
		idx.avgLen = float64(totalLen) / float64(idx.n)
	}
	return idx
}

// bm25Hit 是一篇文档的 BM25 得分。
type bm25Hit struct {
	ID    string
	Score float64
}

// score 对全部文档算 BM25（k1=1.2，b 由调用方给：全文层 0.75），
// 返回按得分降序的全部命中（得分 >0 才回）。queryTerms 是查询词
// （已展开，见 bm25Terms 口径；调用方先做缩写展开）。
func (idx *bm25Index) score(queryTerms []string, k1, b float64) []bm25Hit {
	if idx.n == 0 || len(queryTerms) == 0 {
		return nil
	}
	// 查询词去重保序。
	seen := map[string]bool{}
	terms := make([]string, 0, len(queryTerms))
	for _, t := range queryTerms {
		if t != "" && !seen[t] {
			seen[t] = true
			terms = append(terms, t)
		}
	}
	// IDF：ln(1 + (N - df + 0.5) / (df + 0.5))，经典平滑版，
	// 常见词 IDF 趋近 0，罕见词权重高。
	idf := map[string]float64{}
	for _, t := range terms {
		df := idx.df[t]
		idf[t] = math.Log(1 + (float64(idx.n)-float64(df)+0.5)/(float64(df)+0.5))
	}
	hits := make([]bm25Hit, 0, idx.n)
	for _, d := range idx.docs {
		if d.Length == 0 {
			continue
		}
		norm := 1 - b + b*float64(d.Length)/idx.avgLen
		s := 0.0
		for _, t := range terms {
			tf := d.TF[t]
			if tf == 0 {
				continue
			}
			s += idf[t] * (float64(tf) * (k1 + 1)) / (float64(tf) + k1*norm)
		}
		if s > 0 {
			hits = append(hits, bm25Hit{ID: d.ID, Score: s})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].ID < hits[j].ID
	})
	return hits
}

// bm25K1 是 BM25 的 k1（词频饱和参数），全文层与简介层共用。
const bm25K1 = 1.2

// bm25FullB 是全文层 b（v2.1 §7：全文层 b=0.75）。
const bm25FullB = 0.75

// bm25SummaryB 是简介层 b（v2.1 §7：简介层 b=0.25–0.35，取 0.30，
// FTS5 侧见 store.go SearchFTS）。
const bm25SummaryB = 0.30
