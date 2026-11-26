package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- search_memory / read_memory（线 B 第二阶段第 4 棒） ----

// disableSemanticForTest 关掉语义腿（生产现状），用完恢复。
func disableSemanticForTest(t *testing.T) {
	t.Helper()
	old := SemanticEnabled
	SemanticEnabled = false
	t.Cleanup(func() { SemanticEnabled = old })
}

func TestSearchMemoryHybridReturnsCards(t *testing.T) {
	st, _ := newTwoStageFixture(t, nil, nil)
	disableSemanticForTest(t)
	b := &SearchBudget{}
	cards, note, err := st.SearchMemory(b, "Cloudflare 新账号注册", "", 0)
	if err != nil {
		t.Fatalf("SearchMemory: %v", err)
	}
	if len(cards) == 0 {
		t.Fatalf("hybrid 应返回卡片，实为 0 张（note=%q）", note)
	}
	if len(cards) > MaxSearchCards {
		t.Fatalf("单次返回超过 %d 条：%d 条", MaxSearchCards, len(cards))
	}
	found := false
	for _, c := range cards {
		if c.DocID == "" || c.Title == "" || c.Summary == "" {
			t.Fatalf("卡片缺字段：%+v", c)
		}
		if c.DocID == "W-0001" {
			found = true
		}
		if strings.Contains(c.Display(), "Cloudflare 新账号注册流程完整记录") {
			t.Fatalf("卡片不应含全文：%q", c.Display())
		}
	}
	if !found {
		t.Fatalf("CF 注册文档应在卡片里：%v", cards)
	}
	// 卡片必须带编号（模型靠它调 read_memory）：Display 以【W-0001】开头。
	for _, c := range cards {
		if c.DocID == "W-0001" && !strings.HasPrefix(c.Display(), "【W-0001】") {
			t.Fatalf("卡片 Display 应以【W-0001】开头，实为 %q", c.Display())
		}
	}
	// 语义腿停用时 hybrid 应说明退化，不报错。
	if !strings.Contains(note, "退化") {
		t.Fatalf("hybrid 退化时应有说明，实为 %q", note)
	}
}

func TestSearchMemoryLimitClampedToFive(t *testing.T) {
	st, _ := newTwoStageFixture(t, nil, nil)
	disableSemanticForTest(t)
	b := &SearchBudget{}
	cards, _, err := st.SearchMemory(b, "Cloudflare", "lexical", 2)
	if err != nil {
		t.Fatalf("SearchMemory: %v", err)
	}
	if len(cards) > 2 {
		t.Fatalf("limit=2 却返回 %d 条", len(cards))
	}
	b2 := &SearchBudget{}
	cards, _, err = st.SearchMemory(b2, "Cloudflare", "lexical", 99)
	if err != nil {
		t.Fatalf("SearchMemory: %v", err)
	}
	if len(cards) > MaxSearchCards {
		t.Fatalf("limit=99 应夹到 %d 条，实为 %d 条", MaxSearchCards, len(cards))
	}
}

func TestSearchMemoryModes(t *testing.T) {
	st, _ := newTwoStageFixture(t, nil, nil)
	disableSemanticForTest(t)
	// lexical：只走词法腿，应命中。
	b := &SearchBudget{}
	cards, _, err := st.SearchMemory(b, "Cloudflare 新账号注册", "lexical", 5)
	if err != nil {
		t.Fatalf("lexical: %v", err)
	}
	found := false
	for _, c := range cards {
		if c.DocID == "W-0001" {
			found = true
		}
	}
	if !found {
		t.Fatalf("lexical 应命中 W-0001：%v", cards)
	}
	// semantic：腿停用时退化为词法，不报错，且有说明。
	b2 := &SearchBudget{}
	cards, note, err := st.SearchMemory(b2, "Cloudflare 新账号注册", "semantic", 5)
	if err != nil {
		t.Fatalf("semantic 退化不应报错：%v", err)
	}
	if len(cards) == 0 {
		t.Fatalf("semantic 退化后应有卡片")
	}
	if !strings.Contains(note, "退化") {
		t.Fatalf("semantic 退化时应有说明，实为 %q", note)
	}
	// 未知 mode 报错（不计次数）。
	b3 := &SearchBudget{}
	if _, _, err := st.SearchMemory(b3, "Cloudflare", "weird", 5); err == nil {
		t.Fatalf("未知 mode 应报错")
	}
	if s, _ := b3.Counts(); s != 0 {
		t.Fatalf("报错的调用不应计数，实为 %d 次", s)
	}
	// 空查询报错。
	if _, _, err := st.SearchMemory(b3, "  ", "hybrid", 5); err == nil {
		t.Fatalf("空查询应报错")
	}
}

func TestSearchMemorySemanticLegOn(t *testing.T) {
	st, _ := newTwoStageFixture(t, nil, nil)
	// fixture 已开语义腿（fakeEmbed）：Cloudflare 查询应走语义腿命中。
	b := &SearchBudget{}
	cards, note, err := st.SearchMemory(b, "Cloudflare 注册", "semantic", 5)
	if err != nil {
		t.Fatalf("semantic: %v", err)
	}
	if note != "" {
		t.Fatalf("语义腿开时不应有退化说明，实为 %q", note)
	}
	found := false
	for _, c := range cards {
		if c.DocID == "W-0001" {
			found = true
		}
	}
	if !found {
		t.Fatalf("semantic 应命中 W-0001：%v", cards)
	}
}

func TestSearchMemoryBudgetCap(t *testing.T) {
	st, _ := newTwoStageFixture(t, nil, nil)
	disableSemanticForTest(t)
	b := &SearchBudget{}
	for i := 0; i < MaxSearchPerTask; i++ {
		cards, note, err := st.SearchMemory(b, "Cloudflare", "lexical", 5)
		if err != nil {
			t.Fatalf("第 %d 次 SearchMemory: %v", i+1, err)
		}
		if note == searchExhaustedNote {
			t.Fatalf("第 %d 次不应超限", i+1)
		}
		_ = cards
	}
	// 第 4 次：超限提示，不报错、不静默。
	cards, note, err := st.SearchMemory(b, "Cloudflare", "lexical", 5)
	if err != nil {
		t.Fatalf("第 4 次超限不应走 err：%v", err)
	}
	if len(cards) != 0 {
		t.Fatalf("超限时应返回 0 张卡片，实为 %d 张", len(cards))
	}
	if note != searchExhaustedNote {
		t.Fatalf("第 4 次应返回超限提示，实为 %q", note)
	}
	if s, _ := b.Counts(); s != MaxSearchPerTask {
		t.Fatalf("计数应停在 %d，实为 %d", MaxSearchPerTask, s)
	}
	// 新任务（新 Budget）清零。
	b2 := &SearchBudget{}
	if _, note, err := st.SearchMemory(b2, "Cloudflare", "lexical", 5); err != nil || note == searchExhaustedNote {
		t.Fatalf("新任务应能再查：err=%v note=%q", err, note)
	}
}

func TestSearchMemoryRequiresStore(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.SearchMemory(&SearchBudget{}, "Cloudflare", "hybrid", 5); err == nil {
		t.Fatalf("未接简介主库时 search_memory 应报错")
	}
}

func TestReadMemoryByDocID(t *testing.T) {
	st, _ := newTwoStageFixture(t, nil, nil)
	disableSemanticForTest(t)
	b := &SearchBudget{}
	text, note, err := st.ReadMemory(b, "W-0001")
	if err != nil {
		t.Fatalf("ReadMemory W-0001: %v", err)
	}
	if note != "" {
		t.Fatalf("正常读取不应有 note，实为 %q", note)
	}
	// 全文不截断：与磁盘文件逐字相同。
	raw, err := os.ReadFile(filepath.Join(st.root, "wiki", "entities", "cf-test.md"))
	if err != nil {
		t.Fatal(err)
	}
	if text != string(raw) {
		t.Fatalf("全文应与磁盘文件逐字相同，实为 %q（文件 %q）", text, string(raw))
	}
	if !strings.Contains(text, "邮箱验证后取 Global API Key") {
		t.Fatalf("全文应含正文关键句：%q", text)
	}
	// 源路径当 doc_id 也能读。
	b2 := &SearchBudget{}
	text2, _, err := st.ReadMemory(b2, "wiki/entities/cf-test")
	if err != nil {
		t.Fatalf("ReadMemory 源路径: %v", err)
	}
	if text2 != text {
		t.Fatalf("源路径读到的全文应与编号一致")
	}
}

func TestReadMemoryBudgetCap(t *testing.T) {
	st, _ := newTwoStageFixture(t, nil, nil)
	disableSemanticForTest(t)
	b := &SearchBudget{}
	for i := 0; i < MaxReadPerTask; i++ {
		if _, _, err := st.ReadMemory(b, "W-0001"); err != nil {
			t.Fatalf("第 %d 次 ReadMemory: %v", i+1, err)
		}
	}
	text, note, err := st.ReadMemory(b, "W-0001")
	if err != nil {
		t.Fatalf("第 4 次超限不应走 err：%v", err)
	}
	if text != "" {
		t.Fatalf("超限时全文应为空")
	}
	if note != readExhaustedNote {
		t.Fatalf("第 4 次应返回超限提示，实为 %q", note)
	}
	if _, r := b.Counts(); r != MaxReadPerTask {
		t.Fatalf("读计数应停在 %d，实为 %d", MaxReadPerTask, r)
	}
}

func TestReadMemoryNotFound(t *testing.T) {
	st, _ := newTwoStageFixture(t, nil, nil)
	disableSemanticForTest(t)
	b := &SearchBudget{}
	if _, _, err := st.ReadMemory(b, "W-9999"); err == nil {
		t.Fatalf("不存在的 doc_id 应报错")
	}
	if _, _, err := st.ReadMemory(b, "../../etc/passwd"); err == nil {
		t.Fatalf(".. 逃逸应拒绝")
	}
	if _, _, err := st.ReadMemory(b, ""); err == nil {
		t.Fatalf("空 doc_id 应报错")
	}
	// 计数口径：参数非法（空、逃逸）不计数；查无此文计一次。
	if _, r := b.Counts(); r != 1 {
		t.Fatalf("查无此文应计 1 次，实为 %d 次", r)
	}
}

func TestParseMemoryToolInputs(t *testing.T) {
	q, m, l := ParseSearchMemoryInputs([]string{"Cloudflare 注册"})
	if q != "Cloudflare 注册" || m != "" || l != MaxSearchCards {
		t.Fatalf("单参解析不对：%q %q %d", q, m, l)
	}
	q, m, l = ParseSearchMemoryInputs([]string{"x", "lexical", "2"})
	if q != "x" || m != "lexical" || l != 2 {
		t.Fatalf("三参解析不对：%q %q %d", q, m, l)
	}
	_, _, l = ParseSearchMemoryInputs([]string{"x", "hybrid", "abc"})
	if l != MaxSearchCards {
		t.Fatalf("limit 非数字应回默认 5，实为 %d", l)
	}
	_, _, l = ParseSearchMemoryInputs([]string{"x", "hybrid", "99"})
	if l != MaxSearchCards {
		t.Fatalf("limit 越界应夹到 5，实为 %d", l)
	}
	if got := ParseReadMemoryInputs([]string{"W-0001"}); got != "W-0001" {
		t.Fatalf("read inputs 解析不对：%q", got)
	}
	if got := ParseReadMemoryInputs(nil); got != "" {
		t.Fatalf("空 inputs 应返回空串，实为 %q", got)
	}
}
