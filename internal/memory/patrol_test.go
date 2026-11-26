package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func sha256Str(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// patrolTestRoot 搭最小记忆根：两篇源文件 + 两篇简介 + 主库两行（ok）。
// 所有文件 mtime 拨到 10 秒前（过 3 秒静默期）。
func patrolTestRoot(t *testing.T) (string, *SummaryStore) {
	t.Helper()
	root := t.TempDir()
	old := time.Now().Add(-10 * time.Second)
	writeOld := func(rel, content string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	writeOld("wiki/entities/a.md", "AAA 内容\n")
	writeOld("l3/lessons/b.md", "BBB 内容\n")
	writeOld("简介/wiki/entities/a.md", "summary: A 简介\ntriggers: A\n")
	writeOld("简介/l3/lessons/b.md", "summary: B 简介\ntriggers: B\n")
	st, err := OpenSummaryStore(filepath.Join(root, "summaries.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, d := range []SummaryDoc{
		{DocID: "W-0001", Layer: "wiki", Category: "entities", Title: "A", Summary: "A 简介", Triggers: "A",
			SummaryPath: "简介/wiki/entities/a.md", SourcePath: "wiki/entities/a.md",
			SourceHash: sha256Str("AAA 内容\n"), SummaryHash: sha256Str("summary: A 简介\ntriggers: A\n"), Status: "ok"},
		{DocID: "L3-0001", Layer: "l3", Category: "lessons", Title: "B", Summary: "B 简介", Triggers: "B",
			SummaryPath: "简介/l3/lessons/b.md", SourcePath: "l3/lessons/b.md",
			SourceHash: sha256Str("BBB 内容\n"), SummaryHash: sha256Str("summary: B 简介\ntriggers: B\n"), Status: "ok"},
	} {
		if err := st.UpsertDoc(d); err != nil {
			t.Fatal(err)
		}
	}
	return root, st
}

func mustDoc(t *testing.T, st *SummaryStore, docID string) SummaryDoc {
	t.Helper()
	d, err := st.GetDoc(docID)
	if err != nil {
		t.Fatalf("GetDoc %s: %v", docID, err)
	}
	return d
}

// TestPatrolNoChange：无改动时两次巡检都 0 计数（第二次靠 state 预过滤跳过）。
func TestPatrolNoChange(t *testing.T) {
	root, st := patrolTestRoot(t)
	now := time.Now()
	res, err := PatrolSummaryStore(root, st, PatrolOptions{Now: now})
	if err != nil {
		t.Fatalf("巡检: %v", err)
	}
	if res.Scanned != 2 || res.New != 0 || res.Modified != 0 || res.Renamed != 0 || res.Deleted != 0 || res.StaleMarked != 0 || res.PendingMarked != 0 {
		t.Fatalf("无改动巡检应全 0，得到 %+v", res)
	}
	if _, err := os.Stat(filepath.Join(root, "patrol-state.json")); err != nil {
		t.Fatalf("应写 patrol-state.json：%v", err)
	}
	res2, err := PatrolSummaryStore(root, st, PatrolOptions{Now: now})
	if err != nil {
		t.Fatalf("第二次巡检: %v", err)
	}
	if res2.Scanned != 2 || res2.New != 0 || res2.Modified != 0 || res2.StaleMarked != 0 || res2.PendingMarked != 0 {
		t.Fatalf("第二次巡检（state 命中）应全 0，得到 %+v", res2)
	}
}

// TestPatrolRenameInheritsDocID：源文件改名（内容不变）→ 按哈希继承
// doc_id、简介与触发词，只更新路径；简介文件同步移动。
func TestPatrolRenameInheritsDocID(t *testing.T) {
	root, st := patrolTestRoot(t)
	oldPath := filepath.Join(root, "wiki", "entities", "a.md")
	newPath := filepath.Join(root, "wiki", "entities", "a-renamed.md")
	if err := os.Rename(oldPath, newPath); err != nil {
		t.Fatal(err)
	}
	res, err := PatrolSummaryStore(root, st, PatrolOptions{Now: time.Now()})
	if err != nil {
		t.Fatalf("巡检: %v", err)
	}
	if res.Renamed != 1 || res.New != 0 || res.Deleted != 0 {
		t.Fatalf("改名应 Renamed=1、New=0、Deleted=0，得到 %+v", res)
	}
	d := mustDoc(t, st, "W-0001")
	if d.SourcePath != "wiki/entities/a-renamed.md" || d.SummaryPath != "简介/wiki/entities/a-renamed.md" {
		t.Fatalf("改名后路径未更新：%+v", d)
	}
	if d.Summary != "A 简介" || d.Triggers != "A" {
		t.Fatalf("改名应保留简介与触发词：%+v", d)
	}
	if _, err := os.Stat(filepath.Join(root, "简介", "wiki", "entities", "a-renamed.md")); err != nil {
		t.Fatalf("简介文件应同步移动：%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "简介", "wiki", "entities", "a.md")); !os.IsNotExist(err) {
		t.Fatal("旧简介文件应已移走")
	}
}

// TestPatrolDeleteSyncsSummaryAndStore：源删后第 1 次巡检只记缺失、不删
// （连续 2 次缺失再删，防误删）；第 2 次仍缺失才删简介文件 + 库行。
func TestPatrolDeleteSyncsSummaryAndStore(t *testing.T) {
	root, st := patrolTestRoot(t)
	if err := os.Remove(filepath.Join(root, "l3", "lessons", "b.md")); err != nil {
		t.Fatal(err)
	}
	res, err := PatrolSummaryStore(root, st, PatrolOptions{Now: time.Now()})
	if err != nil {
		t.Fatalf("巡检: %v", err)
	}
	if res.Deleted != 0 || res.DeleteDeferred != 1 {
		t.Fatalf("首见缺失应 Deleted=0、DeleteDeferred=1，得到 %+v", res)
	}
	if _, err := os.Stat(filepath.Join(root, "简介", "l3", "lessons", "b.md")); err != nil {
		t.Fatal("第 1 次缺失时简介文件应还在")
	}
	if _, err := st.GetDoc("L3-0001"); err != nil {
		t.Fatal("第 1 次缺失时库行应还在")
	}
	res, err = PatrolSummaryStore(root, st, PatrolOptions{Now: time.Now()})
	if err != nil {
		t.Fatalf("巡检: %v", err)
	}
	if res.Deleted != 1 {
		t.Fatalf("连续 2 次缺失应 Deleted=1，得到 %+v", res)
	}
	if _, err := os.Stat(filepath.Join(root, "简介", "l3", "lessons", "b.md")); !os.IsNotExist(err) {
		t.Fatal("简介文件应同步删除")
	}
	if _, err := st.GetDoc("L3-0001"); err == nil {
		t.Fatal("库行应同步删除")
	}
	n, err := st.CountDocs()
	if err != nil || n != 1 {
		t.Fatalf("库应剩 1 篇，得到 %d（err=%v）", n, err)
	}
}

// TestPatrolNewFileReportedNotInvented：全新文件只记 New，不发明 doc_id。
func TestPatrolNewFileReportedNotInvented(t *testing.T) {
	root, st := patrolTestRoot(t)
	old := time.Now().Add(-10 * time.Second)
	p := filepath.Join(root, "wiki", "entities", "c.md")
	if err := os.WriteFile(p, []byte("CCC 新内容\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	res, err := PatrolSummaryStore(root, st, PatrolOptions{Now: time.Now()})
	if err != nil {
		t.Fatalf("巡检: %v", err)
	}
	if res.New != 1 {
		t.Fatalf("新增应 New=1，得到 %+v", res)
	}
	n, err := st.CountDocs()
	if err != nil || n != 2 {
		t.Fatalf("巡检不应建行，库应仍 2 篇，得到 %d（err=%v）", n, err)
	}
}

// TestPatrolSourceModifiedMarksStale：源文件改了 → Modified + 标 stale。
func TestPatrolSourceModifiedMarksStale(t *testing.T) {
	root, st := patrolTestRoot(t)
	old := time.Now().Add(-10 * time.Second)
	p := filepath.Join(root, "wiki", "entities", "a.md")
	if err := os.WriteFile(p, []byte("AAA 改过的内容\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	res, err := PatrolSummaryStore(root, st, PatrolOptions{Now: time.Now()})
	if err != nil {
		t.Fatalf("巡检: %v", err)
	}
	if res.Modified != 1 || res.StaleMarked != 1 {
		t.Fatalf("源改应 Modified=1、StaleMarked=1，得到 %+v", res)
	}
	if d := mustDoc(t, st, "W-0001"); d.Status != "stale" {
		t.Fatalf("源改后状态应为 stale，得到 %q", d.Status)
	}
}

// TestPatrolSummaryMissingMarksPending：简介文件缺失 → 标 pending。
func TestPatrolSummaryMissingMarksPending(t *testing.T) {
	root, st := patrolTestRoot(t)
	if err := os.Remove(filepath.Join(root, "简介", "wiki", "entities", "a.md")); err != nil {
		t.Fatal(err)
	}
	res, err := PatrolSummaryStore(root, st, PatrolOptions{Now: time.Now()})
	if err != nil {
		t.Fatalf("巡检: %v", err)
	}
	if res.PendingMarked != 1 {
		t.Fatalf("缺简介应 PendingMarked=1，得到 %+v", res)
	}
	if d := mustDoc(t, st, "W-0001"); d.Status != "pending" {
		t.Fatalf("缺简介后状态应为 pending，得到 %q", d.Status)
	}
}

// TestPatrolSummaryChangedMarksStale：简介文件改了（哈希不符）→ 标 stale。
func TestPatrolSummaryChangedMarksStale(t *testing.T) {
	root, st := patrolTestRoot(t)
	old := time.Now().Add(-10 * time.Second)
	p := filepath.Join(root, "简介", "wiki", "entities", "a.md")
	if err := os.WriteFile(p, []byte("summary: A 改过的简介\ntriggers: A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	res, err := PatrolSummaryStore(root, st, PatrolOptions{Now: time.Now()})
	if err != nil {
		t.Fatalf("巡检: %v", err)
	}
	if res.StaleMarked != 1 {
		t.Fatalf("简介改应 StaleMarked=1，得到 %+v", res)
	}
	if d := mustDoc(t, st, "W-0001"); d.Status != "stale" {
		t.Fatalf("简介改后状态应为 stale，得到 %q", d.Status)
	}
}

// TestPatrolGeneratingLock：锁未超时跳过；锁超时 5 分钟自动释放改 stale。
func TestPatrolGeneratingLock(t *testing.T) {
	root, st := patrolTestRoot(t)
	now := time.Now()
	old := now.Add(-10 * time.Second)
	// 源改了，但锁未超时 → 跳过，不标 stale。
	if err := st.SetGenerating("W-0001", now.Add(-1*time.Minute).UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "wiki", "entities", "a.md")
	if err := os.WriteFile(p, []byte("AAA 改过的内容\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	res, err := PatrolSummaryStore(root, st, PatrolOptions{Now: now})
	if err != nil {
		t.Fatalf("巡检: %v", err)
	}
	if res.SkippedGenerating != 1 || res.Modified != 0 || res.StaleMarked != 0 {
		t.Fatalf("锁未超时应跳过（SkippedGenerating=1），得到 %+v", res)
	}
	if d := mustDoc(t, st, "W-0001"); d.Status != "generating" || d.LockedAt == "" {
		t.Fatalf("锁未超时状态应仍为 generating 且有锁，得到 %+v", d)
	}
	// 锁超时（10 分钟前置位）→ 释放改 stale，源改动随即被检出。
	if err := st.SetGenerating("W-0001", now.Add(-10*time.Minute).UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	res2, err := PatrolSummaryStore(root, st, PatrolOptions{Now: now})
	if err != nil {
		t.Fatalf("第二次巡检: %v", err)
	}
	if res2.Modified != 1 || res2.StaleMarked < 1 {
		t.Fatalf("锁超时应释放并检出改动，得到 %+v", res2)
	}
	d := mustDoc(t, st, "W-0001")
	if d.Status != "stale" || d.LockedAt != "" {
		t.Fatalf("锁超时后状态应为 stale 且锁清空，得到 %+v", d)
	}
}

// TestPatrolQuietPeriod：3 秒内改过的文件跳过，不计 New。
func TestPatrolQuietPeriod(t *testing.T) {
	root, st := patrolTestRoot(t)
	now := time.Now()
	p := filepath.Join(root, "wiki", "entities", "fresh.md")
	if err := os.WriteFile(p, []byte("刚写的内容\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := PatrolSummaryStore(root, st, PatrolOptions{Now: now})
	if err != nil {
		t.Fatalf("巡检: %v", err)
	}
	if res.New != 0 {
		t.Fatalf("静默期内新文件不应计 New，得到 %+v", res)
	}
	res2, err := PatrolSummaryStore(root, st, PatrolOptions{Now: now.Add(10 * time.Second)})
	if err != nil {
		t.Fatalf("第二次巡检: %v", err)
	}
	if res2.New != 1 {
		t.Fatalf("静默期过后新文件应计 New=1，得到 %+v", res2)
	}
}

// TestPatrolFullRehashes：全量忽略 state，全部重算哈希。
// 模拟 state 盲区：改源文件内容但保持尺寸与 mtime 不变
// （mtime+size 预过滤看不出），增量漏检，全量检出。
func TestPatrolFullRehashes(t *testing.T) {
	root, st := patrolTestRoot(t)
	now := time.Now()
	if _, err := PatrolSummaryStore(root, st, PatrolOptions{Now: now}); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "wiki", "entities", "a.md")
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	origMtime := info.ModTime()
	// 同尺寸（11 字节）改内容，mtime 拨回原值。
	if err := os.WriteFile(p, []byte("AAC 内容\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, origMtime, origMtime); err != nil {
		t.Fatal(err)
	}
	res, err := PatrolSummaryStore(root, st, PatrolOptions{Now: now})
	if err != nil {
		t.Fatalf("增量巡检: %v", err)
	}
	if res.Modified != 0 {
		t.Fatalf("增量（state 命中）不应检出改动，得到 %+v", res)
	}
	res2, err := PatrolSummaryStore(root, st, PatrolOptions{Full: true, Now: now})
	if err != nil {
		t.Fatalf("全量巡检: %v", err)
	}
	if res2.Modified != 1 {
		t.Fatalf("全量应检出改动 Modified=1，得到 %+v", res2)
	}
}

// TestSummaryStorePatrolMethods：GetByHash / ListByStatus / DeleteDoc
// （含向量与 FTS 行同步清）。
func TestSummaryStorePatrolMethods(t *testing.T) {
	root, st := patrolTestRoot(t)
	_ = root
	if err := st.UpsertVector("W-0001", []float32{0.1, 0.2}, "test"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetGenerating("W-0001", ""); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetByHash(sha256Str("AAA 内容\n"))
	if err != nil || len(got) != 1 || got[0].DocID != "W-0001" {
		t.Fatalf("GetByHash 应回 1 篇 W-0001，得到 %+v（err=%v）", got, err)
	}
	gen, err := st.ListByStatus("generating")
	if err != nil || len(gen) != 1 || gen[0].DocID != "W-0001" {
		t.Fatalf("ListByStatus(generating) 应回 1 篇，得到 %+v（err=%v）", gen, err)
	}
	// 删前 FTS 应命中 W-0001（“简介”在 summary 里），否则删后断言是空断言。
	preHits, err := st.SearchFTS([]string{"简介"}, 5)
	if err != nil {
		t.Fatal(err)
	}
	preFound := false
	for _, h := range preHits {
		if h.DocID == "W-0001" {
			preFound = true
		}
	}
	if !preFound {
		t.Fatalf("删前 FTS 应命中 W-0001，得到 %+v", preHits)
	}
	if err := st.DeleteDoc("W-0001"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetDoc("W-0001"); err == nil {
		t.Fatal("DeleteDoc 后应查不到")
	}
	if n, _ := st.CountVectors(); n != 0 {
		t.Fatalf("DeleteDoc 后向量应清 0，得到 %d", n)
	}
	hits, err := st.SearchFTS([]string{"简介"}, 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.DocID == "W-0001" {
			t.Fatalf("DeleteDoc 后 FTS 不应再命中 W-0001：%+v", hits)
		}
	}
}

// TestPatrolMissingThenBackDoesNotDelete：源消失 1 次后又恢复，
// 不应删，且缺失计数清零；之后再删，从第 1 次缺失重新数。
func TestPatrolMissingThenBackDoesNotDelete(t *testing.T) {
	root, st := patrolTestRoot(t)
	bPath := filepath.Join(root, "l3", "lessons", "b.md")
	content, err := os.ReadFile(bPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(bPath); err != nil {
		t.Fatal(err)
	}
	res, err := PatrolSummaryStore(root, st, PatrolOptions{Now: time.Now()})
	if err != nil {
		t.Fatalf("巡检: %v", err)
	}
	if res.Deleted != 0 || res.DeleteDeferred != 1 {
		t.Fatalf("首见缺失应 Deleted=0、DeleteDeferred=1，得到 %+v", res)
	}
	// 文件恢复（同内容，mtime 拨回 10 秒前，过静默期）。
	old := time.Now().Add(-10 * time.Second)
	if err := os.WriteFile(bPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(bPath, old, old); err != nil {
		t.Fatal(err)
	}
	res, err = PatrolSummaryStore(root, st, PatrolOptions{Now: time.Now()})
	if err != nil {
		t.Fatalf("巡检: %v", err)
	}
	if res.Deleted != 0 || res.DeleteDeferred != 0 {
		t.Fatalf("文件恢复后不应删也不应暂缓，得到 %+v", res)
	}
	// 再删一次 → 从第 1 次缺失重新数（只暂缓，不直接删）。
	if err := os.Remove(bPath); err != nil {
		t.Fatal(err)
	}
	res, err = PatrolSummaryStore(root, st, PatrolOptions{Now: time.Now()})
	if err != nil {
		t.Fatalf("巡检: %v", err)
	}
	if res.Deleted != 0 || res.DeleteDeferred != 1 {
		t.Fatalf("重删后应重新记第 1 次缺失，得到 %+v", res)
	}
	if _, err := st.GetDoc("L3-0001"); err != nil {
		t.Fatal("重删第 1 次时库行应还在")
	}
}

// TestPatrolFullDeleteNeedsTwoMisses：全量巡检也要连续 2 次缺失才删，
// 第 1 次全量只记数（计数经 patrol-state.json 跨全量轮次保留）。
func TestPatrolFullDeleteNeedsTwoMisses(t *testing.T) {
	root, st := patrolTestRoot(t)
	if err := os.Remove(filepath.Join(root, "l3", "lessons", "b.md")); err != nil {
		t.Fatal(err)
	}
	res, err := PatrolSummaryStore(root, st, PatrolOptions{Full: true, Now: time.Now()})
	if err != nil {
		t.Fatalf("全量巡检: %v", err)
	}
	if res.Deleted != 0 || res.DeleteDeferred != 1 {
		t.Fatalf("全量首见缺失应 Deleted=0、DeleteDeferred=1，得到 %+v", res)
	}
	if _, err := st.GetDoc("L3-0001"); err != nil {
		t.Fatal("全量第 1 次缺失时库行应还在")
	}
	res, err = PatrolSummaryStore(root, st, PatrolOptions{Full: true, Now: time.Now()})
	if err != nil {
		t.Fatalf("全量巡检: %v", err)
	}
	if res.Deleted != 1 {
		t.Fatalf("全量连续 2 次缺失应 Deleted=1，得到 %+v", res)
	}
	if _, err := st.GetDoc("L3-0001"); err == nil {
		t.Fatal("库行应同步删除")
	}
}
