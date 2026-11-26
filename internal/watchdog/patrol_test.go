package watchdog

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"007-super-agent/internal/ledger"
	"007-super-agent/internal/memory"
)

func sha256Str(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// patrolOnceFixture 搭最小记忆根 + 主库（一篇 ok 文档）+ 账本。
func patrolOnceFixture(t *testing.T) (root, storePath string, led *ledger.Ledger) {
	t.Helper()
	root = t.TempDir()
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
	writeOld("简介/wiki/entities/a.md", "summary: A 简介\ntriggers: A\n")
	storePath = filepath.Join(root, "summaries.db")
	st, err := memory.OpenSummaryStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertDoc(memory.SummaryDoc{
		DocID: "W-0001", Layer: "wiki", Category: "entities", Title: "A", Summary: "A 简介", Triggers: "A",
		SummaryPath: "简介/wiki/entities/a.md", SourcePath: "wiki/entities/a.md",
		SourceHash: sha256Str("AAA 内容\n"), SummaryHash: sha256Str("summary: A 简介\ntriggers: A\n"), Status: "ok",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	led, err = ledger.Open(filepath.Join(root, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ledger.Close(led) })
	return root, storePath, led
}

// TestShouldPatrolFull：全量只在每天 02:00–02:15 且当天未跑过时跑。
func TestShouldPatrolFull(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	mk := func(h, m int) time.Time {
		return time.Date(2026, 11, 17, h, m, 0, 0, loc)
	}
	if !shouldPatrolFull(mk(2, 0), "") {
		t.Fatal("02:00 should patrol full")
	}
	if !shouldPatrolFull(mk(2, 14), "2026-11-16") {
		t.Fatal("02:14 yesterday marker should patrol full")
	}
	if shouldPatrolFull(mk(2, 15), "") {
		t.Fatal("02:15 should not patrol full")
	}
	if shouldPatrolFull(mk(1, 59), "") {
		t.Fatal("01:59 should not patrol full")
	}
	if shouldPatrolFull(mk(3, 30), "") {
		t.Fatal("03:30 should not patrol full（备份时段）")
	}
	if shouldPatrolFull(mk(2, 5), "2026-11-17") {
		t.Fatal("already patrolled today should not patrol full")
	}
}

// TestPatrolOnceRunsAndRecords：PatrolOnce 跑巡检、写 patrol_log；
// 主库不存在时报错（不建空库）。
func TestPatrolOnceRunsAndRecords(t *testing.T) {
	root, storePath, led := patrolOnceFixture(t)
	// 改源文件，巡检应检出并记账。
	old := time.Now().Add(-10 * time.Second)
	p := filepath.Join(root, "wiki", "entities", "a.md")
	if err := os.WriteFile(p, []byte("AAA 改过的内容\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	res, err := PatrolOnce(root, storePath, false, led, "s-patrol", "patrol-run")
	if err != nil {
		t.Fatalf("PatrolOnce: %v", err)
	}
	if res.Modified != 1 {
		t.Fatalf("应检出改动 Modified=1，得到 %+v", res)
	}
	rows, err := led.ListPatrolByTask("patrol-run", 5)
	if err != nil {
		t.Fatalf("ListPatrolByTask: %v", err)
	}
	if len(rows) != 1 || rows[0].Modified != 1 || rows[0].Full {
		t.Fatalf("patrol_log 应有 1 行增量、Modified=1，得到 %+v", rows)
	}
	// 主库不存在 → 报错，不建空库。
	if _, err := PatrolOnce(root, filepath.Join(root, "no-such.db"), false, led, "s-patrol", "patrol-run"); err == nil {
		t.Fatal("主库不存在时 PatrolOnce 应报错")
	}
	if _, err := os.Stat(filepath.Join(root, "no-such.db")); !os.IsNotExist(err) {
		t.Fatal("PatrolOnce 不应建空库")
	}
	// led 为 nil 时只跑巡检、不记账，不报错。
	if _, err := PatrolOnce(root, storePath, true, nil, "s-patrol", "patrol-run"); err != nil {
		t.Fatalf("led 为 nil 时 PatrolOnce 不应报错：%v", err)
	}
}

// TestShouldPatrolIncremental：增量只在每小时 :17/:47 跑，同一槽位只跑一次，
// 整点/半点（全量巡检、备份、精炼、自进化窗口）一律不跑。
func TestShouldPatrolIncremental(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	mk := func(h, m int) time.Time {
		return time.Date(2026, 11, 24, h, m, 0, 0, loc)
	}
	if !shouldPatrolIncremental(mk(3, 17), time.Time{}, false) {
		t.Fatal("03:17 无上次记录时应跑增量")
	}
	if !shouldPatrolIncremental(mk(4, 47), mk(3, 47), true) {
		t.Fatal("04:47 上次在 03:47 时应跑增量")
	}
	if shouldPatrolIncremental(mk(4, 0), time.Time{}, false) {
		t.Fatal("04:00 整点不应跑增量（精炼窗口）")
	}
	if shouldPatrolIncremental(mk(3, 30), time.Time{}, false) {
		t.Fatal("03:30 半点不应跑增量（备份窗口）")
	}
	if shouldPatrolIncremental(mk(2, 0), time.Time{}, false) {
		t.Fatal("02:00 整点不应跑增量（全量巡检窗口）")
	}
	if shouldPatrolIncremental(mk(2, 17), mk(2, 17), true) {
		t.Fatal("同一 :17 槽位内不应重复跑")
	}
	if !shouldPatrolIncremental(mk(2, 47), mk(2, 17), true) {
		t.Fatal("02:47 上次在 02:17 时应跑（不同槽位）")
	}
	if shouldPatrolIncremental(mk(2, 16), time.Time{}, false) {
		t.Fatal("02:16 不应跑增量")
	}
}

// TestPatrolMuIncrementalSkipsWhileFullLocked：增量与全量共用 patrolMu，
// 全量持有锁时增量的 TryLock 应失败（对应 PatrolLoop 里增量直接跳过的分支）。
// 注：PatrolLoop 本身是 60 秒 tick + 北京时间的常驻循环，无法在单元测试里
// 等真实 :17/:47 到来，故只测锁原语与槽位判断两个可测部分。
func TestPatrolMuIncrementalSkipsWhileFullLocked(t *testing.T) {
	w := &Watchdog{}
	w.patrolMu.Lock()
	held := w.patrolMu.TryLock()
	w.patrolMu.Unlock()
	if held {
		t.Fatal("全量持有 patrolMu 时增量 TryLock 应失败")
	}
	if !w.patrolMu.TryLock() {
		t.Fatal("锁空闲时增量 TryLock 应成功")
	}
	w.patrolMu.Unlock()
}
