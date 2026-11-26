package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openTestStore(t *testing.T) *SummaryStore {
	t.Helper()
	st, err := OpenSummaryStore(filepath.Join(t.TempDir(), "summaries.db"))
	if err != nil {
		t.Fatalf("OpenSummaryStore: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func testDoc(id, source, title, summary string) SummaryDoc {
	layer, category := "wiki", "entities"
	if strings.HasPrefix(source, "l3/") {
		layer = "l3"
		category = strings.Split(strings.TrimPrefix(source, "l3/"), "/")[0]
	}
	return SummaryDoc{
		DocID: id, Layer: layer, Category: category,
		Title: title, Summary: summary, Triggers: "触发词",
		SummaryPath: "简介/" + source, SourcePath: source,
		SourceHash: "aabbcc", SummaryHash: "ddeeff",
		Status: "ok", RetryCount: 0, UpdatedAt: "2026-11-24T00:00:00Z",
	}
}

// 主库开 WAL。
func TestSummaryStoreWAL(t *testing.T) {
	st := openTestStore(t)
	mode, err := st.JournalMode()
	if err != nil {
		t.Fatalf("JournalMode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode 应为 wal，实为 %q", mode)
	}
}

// 复提复用 doc_id：同一 doc_id 再次入库不重号，只更新内容，
// FTS 里旧词查不到、新词查得到。
func TestSummaryStoreUpsertReuseDocID(t *testing.T) {
	st := openTestStore(t)
	d := testDoc("W-0001", "wiki/entities/cf-test.md", "CF 注册", "用 Camoufox 过 Cloudflare 注册验证。")
	if err := st.UpsertDoc(d); err != nil {
		t.Fatalf("UpsertDoc: %v", err)
	}
	d.Summary = "改写后的简介：用浏览器过验证拿密钥。"
	if err := st.UpsertDoc(d); err != nil {
		t.Fatalf("UpsertDoc 复提: %v", err)
	}
	n, err := st.CountDocs()
	if err != nil || n != 1 {
		t.Fatalf("复提后应仍为 1 篇，实为 %d（err=%v）", n, err)
	}
	got, err := st.GetDoc("W-0001")
	if err != nil || got.Summary != d.Summary {
		t.Fatalf("复提后简介应更新：%v / %+v", err, got)
	}
	hits, err := st.SearchFTS(bm25Terms("改写后"), 10)
	if err != nil || len(hits) != 1 || hits[0].DocID != "W-0001" {
		t.Fatalf("FTS 应命中改写后：hits=%v err=%v", hits, err)
	}
	hits, err = st.SearchFTS(bm25Terms("Camoufox"), 10)
	if err != nil {
		t.Fatalf("FTS: %v", err)
	}
	for _, h := range hits {
		if h.DocID == "W-0001" {
			t.Fatalf("FTS 不应再命中旧词 Camoufox：%v", hits)
		}
	}
}

// FTS5 简介层：触发词与简介都能命中，b=0.30 下标题权重更高
// （标题命中排在简介命中前）。
func TestSummaryStoreFTS(t *testing.T) {
	st := openTestStore(t)
	a := testDoc("W-0001", "wiki/entities/cf-test.md", "CF 注册流程", "邮箱验证取密钥。")
	b := testDoc("W-0002", "wiki/entities/other.md", "其他文档", "这里也提到 CF 注册两个字。")
	for _, d := range []SummaryDoc{a, b} {
		if err := st.UpsertDoc(d); err != nil {
			t.Fatalf("UpsertDoc: %v", err)
		}
	}
	hits, err := st.SearchFTS(bm25Terms("CF 注册"), 10)
	if err != nil {
		t.Fatalf("SearchFTS: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("应命中 2 篇，实为 %v", hits)
	}
	if hits[0].DocID != "W-0001" {
		t.Fatalf("标题命中的 W-0001 应排第一，实为 %v", hits)
	}
	if hits[0].Score <= 0 {
		t.Fatalf("FTS 正分应 >0，实为 %v", hits[0].Score)
	}
}

// 向量 BLOB 存取往返 + 计数。
func TestSummaryStoreVectorRoundTrip(t *testing.T) {
	st := openTestStore(t)
	d := testDoc("W-0001", "wiki/entities/cf-test.md", "CF 注册", "简介。")
	if err := st.UpsertDoc(d); err != nil {
		t.Fatalf("UpsertDoc: %v", err)
	}
	vec := []float32{0.1, 0.2, 0.3, 0.4}
	if err := st.UpsertVector("W-0001", vec, "qwen3-embedding:0.6b"); err != nil {
		t.Fatalf("UpsertVector: %v", err)
	}
	n, err := st.CountVectors()
	if err != nil || n != 1 {
		t.Fatalf("向量应为 1 篇，实为 %d（err=%v）", n, err)
	}
	loaded, err := st.LoadVectors()
	if err != nil || len(loaded) != 1 {
		t.Fatalf("LoadVectors: %v / %d", err, len(loaded))
	}
	if len(loaded[0].Vec) != 4 || loaded[0].Vec[2] != 0.3 {
		t.Fatalf("向量往返失真：%v", loaded[0].Vec)
	}
}

// superseded_by 标记与读取。
func TestSummaryStoreSupersededBy(t *testing.T) {
	st := openTestStore(t)
	oldDoc := testDoc("W-0001", "wiki/entities/old.md", "旧版", "旧简介。")
	newDoc := testDoc("W-0002", "wiki/entities/new.md", "新版", "新简介。")
	for _, d := range []SummaryDoc{oldDoc, newDoc} {
		if err := st.UpsertDoc(d); err != nil {
			t.Fatalf("UpsertDoc: %v", err)
		}
	}
	if err := st.SetSupersededBy("W-0001", "W-0002"); err != nil {
		t.Fatalf("SetSupersededBy: %v", err)
	}
	got, err := st.GetDoc("W-0001")
	if err != nil || got.SupersededBy != "W-0002" {
		t.Fatalf("旧文档应记 superseded_by=W-0002：%v / %+v", err, got)
	}
}

// 导出物：JSONL 一行一篇，Markdown 每篇一节。
func TestSummaryStoreExport(t *testing.T) {
	st := openTestStore(t)
	for _, d := range []SummaryDoc{
		testDoc("W-0001", "wiki/entities/a.md", "甲", "简介甲。"),
		testDoc("L3-0001", "l3/lessons/b.md", "乙", "简介乙。"),
	} {
		if err := st.UpsertDoc(d); err != nil {
			t.Fatalf("UpsertDoc: %v", err)
		}
	}
	dir := t.TempDir()
	jl := filepath.Join(dir, "docs.jsonl")
	if err := st.ExportJSONL(jl); err != nil {
		t.Fatalf("ExportJSONL: %v", err)
	}
	data, _ := os.ReadFile(jl)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("JSONL 应为 2 行，实为 %d", len(lines))
	}
	var first SummaryDoc
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("JSONL 解析: %v", err)
	}
	md := filepath.Join(dir, "docs.md")
	if err := st.ExportMarkdown(md); err != nil {
		t.Fatalf("ExportMarkdown: %v", err)
	}
	mdData, _ := os.ReadFile(md)
	if !strings.Contains(string(mdData), "W-0001") || !strings.Contains(string(mdData), "L3-0001") {
		t.Fatalf("Markdown 导出缺篇：%q", string(mdData))
	}
}
