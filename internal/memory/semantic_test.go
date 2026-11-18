package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubEmbeder 按文本返回固定向量：含 "甲" 的返回 [1,0,0,0]，含 "乙" 返回 [0,1,0,0]，
// 含 "丙" 返回 [0,0,1,0]，否则返回 [0,0,0,1]。问句带指令前缀也按同样规则
// （指令是英文，不含甲乙丙）。
func stubEmbeder(texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		switch {
		case strings.Contains(t, "甲"):
			out[i] = []float32{1, 0, 0, 0}
		case strings.Contains(t, "乙"):
			out[i] = []float32{0, 1, 0, 0}
		case strings.Contains(t, "丙"):
			out[i] = []float32{0, 0, 1, 0}
		default:
			out[i] = []float32{0, 0, 0, 1}
		}
	}
	return out, nil
}

func newTestSemantic(t *testing.T) *Service {
	t.Helper()
	s, err := OpenSemantic(filepath.Join(t.TempDir(), "vectors.db"), "http://127.0.0.1:1", "test-model")
	if err != nil {
		t.Fatalf("OpenSemantic: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.embed = stubEmbeder
	return s
}

func writeTestMemory(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	// wiki 两页：甲与乙。
	if err := os.MkdirAll(filepath.Join(root, "wiki", "concepts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "wiki", "concepts", "甲页.md"),
		[]byte("# 甲页\nSummary: 甲的摘要\n\n甲的正文内容。\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "wiki", "concepts", "乙页.md"),
		[]byte("# 乙页\nSummary: 乙的摘要\n\n乙的正文内容。\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// reference 一页：含 ## 内容 节。
	if err := os.MkdirAll(filepath.Join(root, "reference", "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "reference", "agent", "丙.md"),
		[]byte("# 丙\n\n## 内容\n丙的内容节，讲别的事。\n\n## 其他\n别的。\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// Search：问句命中甲时只回甲，Score = int(sim*100)。
func TestSemanticSearch(t *testing.T) {
	s := newTestSemantic(t)
	memRoot := writeTestMemory(t)
	if _, err := s.Rebuild(memRoot); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if s.Count() != 3 {
		t.Fatalf("Count=%d, want 3", s.Count())
	}
	res, err := s.Search("甲是什么")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 1 || res[0].Source != "wiki/concepts/甲页" {
		t.Fatalf("Search 甲 = %+v, want 只有 wiki/concepts/甲页", res)
	}
	if res[0].Score != 100 {
		t.Fatalf("Score=%d, want 100 (sim 1.0)", res[0].Score)
	}
	// 问句与三篇都不沾边：相似度 0，不回。
	res, err = s.Search("毫不相干的查询")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("不相关查询应回 0 条，got %+v", res)
	}
}

// Rebuild 增量：第二遍无改动时向量化 0 篇；删文件后失效源被清理。
func TestSemanticRebuildIncremental(t *testing.T) {
	s := newTestSemantic(t)
	memRoot := writeTestMemory(t)
	n, err := s.Rebuild(memRoot)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if n != 3 {
		t.Fatalf("首次向量化 %d 篇，want 3", n)
	}
	n, err = s.Rebuild(memRoot)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if n != 0 {
		t.Fatalf("无改动重建应向量化 0 篇，got %d", n)
	}
	// 改一页：只重嵌这一页。
	if err := os.WriteFile(filepath.Join(memRoot, "wiki", "concepts", "甲页.md"),
		[]byte("# 甲页\nSummary: 甲的摘要改过了\n\n甲的新正文。\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err = s.Rebuild(memRoot)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if n != 1 {
		t.Fatalf("改一页重建应向量化 1 篇，got %d", n)
	}
	// 删一页：失效源被清理。
	if err := os.Remove(filepath.Join(memRoot, "reference", "agent", "丙.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Rebuild(memRoot); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if s.Count() != 2 {
		t.Fatalf("删页后 Count=%d, want 2", s.Count())
	}
}

// Search 失败降级：embed 出错时回空、不报错（Recall 只用词法）。
func TestSemanticSearchDegrades(t *testing.T) {
	s := newTestSemantic(t)
	memRoot := writeTestMemory(t)
	if _, err := s.Rebuild(memRoot); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	s.embed = func(texts []string) ([][]float32, error) {
		return nil, errTestEmbed
	}
	res, err := s.Search("甲是什么")
	if err != nil {
		t.Fatalf("embed 失败时 Search 应回空不报错，got err %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("embed 失败时应回 0 条，got %+v", res)
	}
}

var errTestEmbed = &testEmbedError{}

type testEmbedError struct{}

func (e *testEmbedError) Error() string { return "test embed failure" }

// cosine/normalize 基本性质。
func TestCosine(t *testing.T) {
	if got := cosine(normalize([]float32{1, 0, 0, 0}), normalize([]float32{1, 0, 0, 0})); got != 1 {
		t.Fatalf("同向余弦=%v, want 1", got)
	}
	if got := cosine(normalize([]float32{1, 0, 0, 0}), normalize([]float32{0, 1, 0, 0})); got != 0 {
		t.Fatalf("正交余弦=%v, want 0", got)
	}
}
