package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return st
}

// 1. 写 Wiki 只走单一入口：文件落盘 + L1 留一条日志。
func TestWriteWikiSingleEntry(t *testing.T) {
	st := openTest(t)
	err := st.WriteWiki(WikiEntry{Kind: KindConcepts, Title: "测试词条", Summary: "记忆测试摘要", Body: "超级 Agent 记忆测试正文。\n第二行。"})
	if err != nil {
		t.Fatalf("WriteWiki: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(st.root, "wiki", "concepts", "测试词条.md"))
	if err != nil {
		t.Fatalf("wiki 文件未落盘: %v", err)
	}
	if !strings.Contains(string(data), "Summary: 记忆测试摘要") {
		t.Fatalf("wiki 文件缺 Summary 行: %q", string(data))
	}
	logs, _ := os.ReadDir(filepath.Join(st.root, "l1"))
	if len(logs) != 1 {
		t.Fatalf("L1 日志应为 1 个文件，实为 %d", len(logs))
	}
	logData, _ := os.ReadFile(filepath.Join(st.root, "l1", logs[0].Name()))
	if !strings.Contains(string(logData), "wiki write concepts/测试词条") {
		t.Fatalf("L1 未记录写 Wiki: %q", string(logData))
	}
}

// 2. L1 7 天过期清理：8 天前删掉，今天的留下。
func TestL1PruneSevenDays(t *testing.T) {
	st := openTest(t)
	old := time.Now().AddDate(0, 0, -8).Format("2006-01-02") + ".log"
	newName := time.Now().Format("2006-01-02") + ".log"
	for _, n := range []string{old, newName} {
		if err := os.WriteFile(filepath.Join(st.root, "l1", n), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := st.PruneL1()
	if err != nil {
		t.Fatalf("PruneL1: %v", err)
	}
	if removed != 1 {
		t.Fatalf("应删 1 个过期文件，实删 %d", removed)
	}
	if _, err := os.Stat(filepath.Join(st.root, "l1", old)); !os.IsNotExist(err) {
		t.Fatalf("过期文件未删")
	}
	if _, err := os.Stat(filepath.Join(st.root, "l1", newName)); err != nil {
		t.Fatalf("当天文件被误删: %v", err)
	}
}

// 3. 检索 top_k=3，每条 ≤240 字符（中文长文不断字）。
func TestRecallTopKAndTruncation(t *testing.T) {
	st := openTest(t)
	long := strings.Repeat("记忆测试长文。", 100) // 700 字，必超 240
	for i, title := range []string{"甲词条", "乙词条", "丙词条", "丁词条"} {
		err := st.WriteWiki(WikiEntry{Kind: KindConcepts, Title: title,
			Summary: "记忆测试摘要" + strings.Repeat("长", 300),
			Body:    long + "第" + string(rune('0'+i)) + "条"})
		if err != nil {
			t.Fatalf("WriteWiki %s: %v", title, err)
		}
	}
	res, err := st.Recall("记忆测试")
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(res) != 3 {
		t.Fatalf("应返回 3 条，实为 %d", len(res))
	}
	for _, r := range res {
		if n := len([]rune(r.Text)); n > MaxResultRunes {
			t.Fatalf("结果超 240 字符：%d（%s）", n, r.Source)
		}
		if !strings.HasPrefix(r.Source, "wiki/") {
			t.Fatalf("Wiki 检索应回 wiki 源，实为 %s", r.Source)
		}
	}
}

// 4. 换页摘要 ≤6000 字符，页链只留最近 8 条。
func TestRolloverCapAndChain(t *testing.T) {
	st := openTest(t)
	long := strings.Repeat("换页长文。", 2000) // 10000 字
	sum, rolled, err := st.Rollover(210000, long)
	if err != nil || !rolled {
		t.Fatalf("Rollover 应换页：rolled=%v err=%v", rolled, err)
	}
	if n := len([]rune(sum)); n > MaxSummaryRunes {
		t.Fatalf("摘要超 6000 字符：%d", n)
	}
	if !strings.HasSuffix(sum, "进入新会话。") {
		t.Fatalf("摘要应以页脚收尾")
	}
	// 未超阈值不换页。
	if _, rolled, _ := st.Rollover(199999, long); rolled {
		t.Fatalf("199999 token 不应换页")
	}
	// 再换 9 次，链只留 8 条。
	for i := 0; i < 9; i++ {
		if _, _, err := st.Rollover(210000, long); err != nil {
			t.Fatalf("Rollover %d: %v", i, err)
		}
	}
	chain, err := st.ListCarryover()
	if err != nil {
		t.Fatalf("ListCarryover: %v", err)
	}
	if len(chain) != MaxCarryover {
		t.Fatalf("换页链应留 8 条，实为 %d", len(chain))
	}
}

// 5. 词条名含 .. 与 / 被拒绝。
func TestWriteWikiRejectBadTitle(t *testing.T) {
	st := openTest(t)
	for _, bad := range []string{"../逃逸", "a/b", "..", ""} {
		err := st.WriteWiki(WikiEntry{Kind: KindConcepts, Title: bad, Summary: "x", Body: "y"})
		if err == nil {
			t.Fatalf("词条名 %q 应被拒绝", bad)
		}
	}
	if _, err := os.Stat(filepath.Join(st.root, "wiki", "concepts")); err != nil {
		t.Fatalf("拒绝后目录应仍存在: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(st.root, "wiki", "concepts"))
	if len(entries) != 0 {
		t.Fatalf("拒绝的写入不应落盘，实有 %d 个文件", len(entries))
	}
}

// 附加：未注入语义时只用词法；注入后合并去重。
func TestRecallSemanticOptional(t *testing.T) {
	st := openTest(t)
	if err := st.WriteWiki(WikiEntry{Kind: KindEntities, Title: "向量词条", Summary: "向量测试摘要", Body: "向量测试正文"}); err != nil {
		t.Fatal(err)
	}
	res, err := st.Recall("向量测试")
	if err != nil || len(res) == 0 {
		t.Fatalf("纯词法应命中：res=%v err=%v", res, err)
	}
	st.WithSemantic(func(q string) ([]Result, error) {
		return []Result{{Source: "wiki/entities/向量词条", Text: "语义结果", Score: 99}}, nil
	})
	res, err = st.Recall("向量测试")
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	// 词条本身同源取最高分（99），L1 那行日志另计一条（“测试”命中 1 次）。
	if len(res) != 2 || res[0].Score != 99 || res[0].Source != "wiki/entities/向量词条" {
		t.Fatalf("语义合并应同源取最高分：%v", res)
	}
}
