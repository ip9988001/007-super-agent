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

// 3b. L3 参考（二号记忆文件）参与检索：回 reference 源，文本取 ## 内容 节。
func TestRecallReferenceContent(t *testing.T) {
	st := openTest(t)
	refDir := filepath.Join(st.root, "reference", "agent", "lessons")
	if err := os.MkdirAll(refDir, 0o755); err != nil {
		t.Fatalf("MkdirAll reference: %v", err)
	}
	body := "# [候选] 抓验证页教训\n\n## 来源\n- **日期**: 2026-11-18\n\n## 内容\n抓验证页要用反检测浏览器，普通 curl 只回空壳。\n\n## 状态\n- **状态**: active\n"
	if err := os.WriteFile(filepath.Join(refDir, "lesson-test.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile reference: %v", err)
	}
	res, err := st.Recall("验证页反检测浏览器")
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	found := false
	for _, r := range res {
		if strings.HasPrefix(r.Source, "reference/") {
			found = true
			if !strings.Contains(r.Text, "反检测浏览器") {
				t.Fatalf("reference 文本应取 ## 内容 节，实为 %q", r.Text)
			}
		}
	}
	if !found {
		t.Fatalf("应返回 reference 源，实为 %v", res)
	}
}

// 3d. 线 B 第一阶段：l3/ 进入召回扫描根，l3 文件能被词法召回；
// l2/ 为空也不影响召回；Open 建出 l3 七类与 l2 目录。
func TestRecallL3Lessons(t *testing.T) {
	st := openTest(t)
	for _, c := range L3Categories {
		if _, err := os.Stat(filepath.Join(st.root, "l3", c)); err != nil {
			t.Fatalf("Open 应建出 l3/%s: %v", c, err)
		}
	}
	if _, err := os.Stat(filepath.Join(st.root, "l2")); err != nil {
		t.Fatalf("Open 应建出 l2: %v", err)
	}
	l3Dir := filepath.Join(st.root, "l3", "lessons")
	body := "# [教训] 抓验证页教训\n\n## 来源\n- **日期**: 2026-11-18\n\n## 内容\n抓验证页要用反检测浏览器，普通 curl 只回空壳。\n\n## 状态\n- **状态**: active\n"
	if err := os.WriteFile(filepath.Join(l3Dir, "lesson-l3-test.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile l3: %v", err)
	}
	res, err := st.Recall("验证页反检测浏览器")
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	found := false
	for _, r := range res {
		if r.Source == "l3/lessons/lesson-l3-test" {
			found = true
			if !strings.Contains(r.Text, "反检测浏览器") {
				t.Fatalf("l3 文本应取 ## 内容 节，实为 %q", r.Text)
			}
		}
	}
	if !found {
		t.Fatalf("l3 文件应进召回候选，实为 %v", res)
	}
}

// 3e. 线 B 第一阶段：RecallCandidates 是评测专用入口，返回完整排序候选；
// Recall 仍只交付前 3 条，生产交付语义不变。
func TestRecallCandidatesDoesNotChangeDelivery(t *testing.T) {
	st := openTest(t)
	for _, title := range []string{"甲词条", "乙词条", "丙词条", "丁词条", "戊词条"} {
		if err := st.WriteWiki(WikiEntry{Kind: KindConcepts, Title: title,
			Summary: "候选测试摘要", Body: "候选测试正文"}); err != nil {
			t.Fatalf("WriteWiki %s: %v", title, err)
		}
	}
	all, err := st.RecallCandidates("候选测试", 40)
	if err != nil {
		t.Fatalf("RecallCandidates: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("RecallCandidates(40) 应返回全部 5 条候选，实为 %d", len(all))
	}
	top, err := st.Recall("候选测试")
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(top) != 3 {
		t.Fatalf("Recall 仍应只交付 3 条，实为 %d", len(top))
	}
	for i := range top {
		if top[i] != all[i] {
			t.Fatalf("交付前 3 条须与候选排序一致：%v vs %v", top, all)
		}
	}
}

// 1b. 线 B 第二阶段第 3 棒（用户 2026-11-24 09:12 纠正）：
// WriteWiki 落盘前不再掩码（旧节点 1 已取消）——密钥必须保持可用，
// 007 要用密钥调 API 才能干活。合成假密钥原样落盘，模型照常用。
func TestWriteWikiKeepsSecretsUsable(t *testing.T) {
	st := openTest(t)
	// 合成假密钥，只用于验证落盘口径。
	fake := "sk-FAKEKEY1234567890abcdef"
	err := st.WriteWiki(WikiEntry{Kind: KindConcepts, Title: "脱敏词条",
		Summary: "测试摘要", Body: "调用时用的 key 是 " + fake + "，用完即换。"})
	if err != nil {
		t.Fatalf("WriteWiki: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(st.root, "wiki", "concepts", "脱敏词条.md"))
	if err != nil {
		t.Fatalf("wiki 文件未落盘: %v", err)
	}
	if !strings.Contains(string(data), fake) {
		t.Fatalf("假密钥原文应原样落盘（09:12 纠正：密钥保持可用）：%q", string(data))
	}
	if strings.Contains(string(data), "[REDACTED]") {
		t.Fatalf("落盘文件不应含掩码：%q", string(data))
	}
}
func TestRecallFallbackText(t *testing.T) {
	st := openTest(t)
	refDir := filepath.Join(st.root, "reference", "protocols")
	if err := os.MkdirAll(refDir, 0o755); err != nil {
		t.Fatalf("MkdirAll reference: %v", err)
	}
	body := "# 验证规则\n\n本文件规定验证页兜底流程，普通抓取失败时换反检测浏览器。\n"
	if err := os.WriteFile(filepath.Join(refDir, "rules-test.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile reference: %v", err)
	}
	res, err := st.Recall("验证页兜底流程")
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(res) == 0 {
		t.Fatalf("应返回命中，实为 0")
	}
	if res[0].Text == "" {
		t.Fatalf("高分命中不应回空文本（%s）", res[0].Source)
	}
	if !strings.Contains(res[0].Text, "反检测浏览器") {
		t.Fatalf("兜底文本应含正文内容，实为 %q", res[0].Text)
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
// SemanticEnabled 默认关闭（线 B 第二阶段第 1 步临时停用），本测试为验证
// 合并逻辑，临时打开开关、测完恢复。
func TestRecallSemanticOptional(t *testing.T) {
	st := openTest(t)
	if err := st.WriteWiki(WikiEntry{Kind: KindEntities, Title: "向量词条", Summary: "向量测试摘要", Body: "向量测试正文"}); err != nil {
		t.Fatal(err)
	}
	res, err := st.Recall("向量测试")
	if err != nil || len(res) == 0 {
		t.Fatalf("纯词法应命中：res=%v err=%v", res, err)
	}
	oldEnabled := SemanticEnabled
	SemanticEnabled = true
	defer func() { SemanticEnabled = oldEnabled }()
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
