package summary

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// TestIngestFixtures 用手写合成夹具验证接入全流程（不调模型）：
// 3 篇合格（CF 注册、定时器概念、cp -a 教训），3 篇故意不合格
// （剧本缺动作词、含合成假密钥、过短）。同一批连交 3 次，
// 第 3 次不合格的改标 degraded_ok 并用本地规则兜底入库。
func TestIngestFixtures(t *testing.T) {
	ctx := context.Background()
	memRoot := "testdata/ingest-fixtures/memory"
	inputDir := "testdata/ingest-fixtures/summaries"
	store := t.TempDir() + "/store"
	manifestPath := t.TempDir() + "/manifest.json"

	run := func() *IngestManifest {
		t.Helper()
		m, err := Ingest(ctx, IngestOptions{
			MemoryRoot: memRoot, InputDir: inputDir,
			StoreDir: store, ManifestPath: manifestPath,
			SeedManifestPath: "", Embedder: nil, // nil 向量器：规则⑦跳过并记明
		})
		if err != nil {
			t.Fatalf("Ingest: %v", err)
		}
		return m
	}
	byPath := func(m *IngestManifest) map[string]IngestEntry {
		out := map[string]IngestEntry{}
		for _, e := range m.Entries {
			out[e.SourcePath] = e
		}
		return out
	}
	failedRules := func(e IngestEntry) map[int]bool {
		out := map[int]bool{}
		for _, c := range e.Checks {
			if !c.Pass && !c.Skipped {
				out[c.Rule] = true
			}
		}
		return out
	}

	// 第 1 次提交：3 合格入库，3 待定（retry=1），待定的不进 store。
	m1 := run()
	if m1.Total != 6 || m1.Accepted != 3 || m1.Pending != 3 || m1.Degraded != 0 {
		t.Fatalf("round1: total=%d accepted=%d pending=%d degraded=%d, want 6/3/3/0",
			m1.Total, m1.Accepted, m1.Pending, m1.Degraded)
	}
	e1 := byPath(m1)
	good := []string{
		"wiki/entities/cf-test-doc.md",
		"wiki/concepts/test-concept.md",
		"l3/lessons/test-lesson.md",
	}
	for _, rel := range good {
		e := e1[rel]
		if e.Status != StatusOK {
			t.Fatalf("round1 %s: status=%s, want ok; checks=%+v", rel, e.Status, e.Checks)
		}
		if e.RetryCount != 0 {
			t.Fatalf("round1 %s: retry=%d, want 0", rel, e.RetryCount)
		}
		if e.DocID == "" || e.SourceHash == "" || e.SummaryHash == "" {
			t.Fatalf("round1 %s: missing id/hash", rel)
		}
		// 发号：wiki → W-####，l3 → L3-####。
		if (rel[:4] == "wiki" && e.DocID[:2] != "W-") || (rel[:2] == "l3" && e.DocID[:3] != "L3-") {
			t.Fatalf("round1 %s: doc_id=%s, wrong prefix", rel, e.DocID)
		}
		// 哈希与原文/入库文件实算一致。
		src, _ := os.ReadFile(filepath.Join(memRoot, rel))
		h := sha256.Sum256(src)
		if e.SourceHash != hex.EncodeToString(h[:]) {
			t.Fatalf("round1 %s: source_hash mismatch", rel)
		}
		stored, err := os.ReadFile(filepath.Join(store, rel))
		if err != nil {
			t.Fatalf("round1 %s: accepted file not in store: %v", rel, err)
		}
		h2 := sha256.Sum256(stored)
		if e.SummaryHash != hex.EncodeToString(h2[:]) {
			t.Fatalf("round1 %s: summary_hash mismatch", rel)
		}
		// 规则⑦无向量器时跳过并记明，不阻塞。
		skipped7 := false
		for _, c := range e.Checks {
			if c.Rule == 7 && c.Skipped {
				skipped7 = true
			}
		}
		if !skipped7 {
			t.Fatalf("round1 %s: rule 7 should be skipped without embedder", rel)
		}
	}
	// 坏夹具：只记状态与原因，不进 store。
	bad := map[string]int{
		"wiki/playbooks/test-playbook.md": 4, // 剧本缺动作词
		"l3/anti-patterns/test-anti.md":   0, // 含合成假密钥（脱敏检查）
		"l3/cases/test-case.md":           4, // 过短（<30 单位）
	}
	for rel, wantRule := range bad {
		e := e1[rel]
		if e.Status != StatusPending || e.RetryCount != 1 {
			t.Fatalf("round1 %s: status=%s retry=%d, want pending/1", rel, e.Status, e.RetryCount)
		}
		if !failedRules(e)[wantRule] {
			t.Fatalf("round1 %s: rule %d should fail, checks=%+v", rel, wantRule, e.Checks)
		}
		if _, err := os.Stat(filepath.Join(store, rel)); !os.IsNotExist(err) {
			t.Fatalf("round1 %s: rejected file must not be in store", rel)
		}
		if e.SummaryPath != "" {
			t.Fatalf("round1 %s: rejected entry must not have summary_path", rel)
		}
	}

	// 第 2 次提交：retry 升到 2，仍待定。
	m2 := run()
	for rel := range bad {
		e := byPath(m2)[rel]
		if e.Status != StatusPending || e.RetryCount != 2 {
			t.Fatalf("round2 %s: status=%s retry=%d, want pending/2", rel, e.Status, e.RetryCount)
		}
	}

	// 第 3 次提交：熔断降级，本地规则兜底入库（不调模型）。
	m3 := run()
	if m3.Degraded != 3 || m3.Pending != 0 {
		t.Fatalf("round3: degraded=%d pending=%d, want 3/0", m3.Degraded, m3.Pending)
	}
	for rel := range bad {
		e := byPath(m3)[rel]
		if e.Status != StatusDegradedOK || e.RetryCount != 3 {
			t.Fatalf("round3 %s: status=%s retry=%d, want degraded_ok/3", rel, e.Status, e.RetryCount)
		}
		stored, err := os.ReadFile(filepath.Join(store, rel))
		if err != nil {
			t.Fatalf("round3 %s: fallback file not in store: %v", rel, err)
		}
		if _, err := Parse(string(stored)); err != nil {
			t.Fatalf("round3 %s: fallback file is not valid 2-line summary: %v", rel, err)
		}
	}
	// 合格的 3 篇全程 ok，manifest 落盘可读。
	if _, err := os.ReadFile(manifestPath); err != nil {
		t.Fatalf("manifest not written: %v", err)
	}
}
