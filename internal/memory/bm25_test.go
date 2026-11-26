package memory

import (
	"math"
	"testing"
)

// BM25：精确匹配的文档排第一，标题词权重高于正文闲词。
func TestBM25RanksExactFirst(t *testing.T) {
	ids := []string{"a", "b", "c"}
	texts := []string{
		"今天天气不错适合出门散步",
		"Cloudflare 注册需要邮箱验证还要拿 Global API Key",
		"随便写点别的毫不相关的内容填充长度填充长度填充长度",
	}
	idx := newBM25Index(ids, texts)
	hits := idx.score(bm25Terms("Cloudflare 注册 邮箱验证 Global API Key"), bm25K1, bm25FullB)
	if len(hits) == 0 || hits[0].ID != "b" {
		t.Fatalf("目标文档 b 应排第一，实为 %v", hits)
	}
}

// BM25 分词：单字 ASCII 词与 1–2 位纯数字是噪音，不进词表；
// 3 位以上纯数字（端口号）保留。
func TestBM25TermsNoise(t *testing.T) {
	terms := bm25Terms("cp -a 复制目录 2 38999")
	for _, bad := range []string{"a", "2"} {
		for _, got := range terms {
			if got == bad {
				t.Fatalf("噪音词 %q 不应进词表：%v", bad, terms)
			}
		}
	}
	found := false
	for _, got := range terms {
		if got == "38999" {
			found = true
		}
	}
	if !found {
		t.Fatalf("端口号 38999 应进词表：%v", terms)
	}
}

// BM25 b 参数：b 越大，长文档越吃亏（长度归一越强）。
func TestBM25LengthNorm(t *testing.T) {
	ids := []string{"short", "long"}
	texts := []string{
		"关键词",
		"关键词 " + "填充填充填充填充填充填充填充填充填充填充填充填充填充填充填充填充填充填充填充填充",
	}
	idx := newBM25Index(ids, texts)
	// score() 接已分词的查询词（与调用方传 bm25Terms 的口径一致）。
	q := bm25Terms("关键词")
	lo := idx.score(q, bm25K1, 0.0)
	hi := idx.score(q, bm25K1, 0.75)
	if len(lo) != 2 || len(hi) != 2 {
		t.Fatalf("两篇都应命中：b=0 得 %v，b=0.75 得 %v", lo, hi)
	}
	// b=0 时两篇同分（同词频）；b=0.75 时短篇分更高。
	if math.Abs(lo[0].Score-lo[1].Score) > 1e-9 {
		t.Fatalf("b=0 时同词频应同分：%v", lo)
	}
	if hi[0].ID != "short" {
		t.Fatalf("b=0.75 时短篇应排第一：%v", hi)
	}
}
