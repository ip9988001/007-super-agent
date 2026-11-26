package memory

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"007-super-agent/internal/registry"
)

// ---- fixture ----

// fakeEmbed 是确定性假向量（dim=4）：按文本关键词定向，
// 供语义腿与去重单测用，不碰 Ollama。
func fakeEmbed(texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		switch {
		case strings.Contains(text, "Cloudflare") || strings.Contains(text, "CF") || strings.Contains(text, "Camoufox"):
			out[i] = []float32{1, 0, 0, 0}
		case strings.Contains(text, "setInterval") || strings.Contains(text, "unref") || strings.Contains(text, "定时"):
			out[i] = []float32{0, 1, 0, 0}
		case strings.Contains(text, "复制") || strings.Contains(text, "嵌套"):
			out[i] = []float32{0, 0, 1, 0}
		default:
			// 未知文本给零向量：与任何向量余弦都是 0，不进语义腿。
			// （若给固定非零向量，所有未知文本会互撞 cos=1，
			// 负样本会被语义腿误捞。）
			out[i] = []float32{0, 0, 0, 0}
		}
	}
	return out, nil
}

func writeFixture(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", rel, err)
	}
}

// newTwoStageFixture 建最小记忆目录 + 简介主库（3 篇 wiki/l3 简介
// 带向量）。extra 是追加的磁盘文档（rel → 内容），
// extraSummaries 是追加的简介（带向量）。
func newTwoStageFixture(t *testing.T, extra map[string]string, extraSummaries []SummaryDoc) (*Store, *SummaryStore) {
	t.Helper()
	root := t.TempDir()
	st, err := Open(root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	writeFixture(t, root, "wiki/entities/cf-test.md",
		"# CF 注册流程\n\nSummary: 用 Camoufox 过 Cloudflare 注册验证。\n\n用 Camoufox 打开 Cloudflare 注册页，过 Turnstile 验证，邮箱验证后取 Global API Key。Cloudflare 新账号注册流程完整记录。\n")
	writeFixture(t, root, "wiki/entities/other-cloud.md",
		"# 其他云文档\n\nSummary: 云服务器日常维护。\n\n云服务器维护，备份快照，重启服务。只提一次 Cloudflare。\n")
	writeFixture(t, root, "l3/lessons/2026-06-21-lesson-cp.md",
		"# cp 复制目录嵌套\n\n## 内容\n\ncp -a 复制目录目标已存在产生嵌套，用 \"$1/.\" 写法避免。cp -a 嵌套问题记录。\n")
	writeFixture(t, root, "l3/lessons/2026-06-21-lesson-timer.md",
		"# 定时器进程退不出\n\n## 内容\n\nsetInterval 不加 unref 进程无法退出，事件循环定时器问题。setInterval unref 定时器。\n")
	writeFixture(t, root, "l3/anti-patterns/setInterval不加unref.md",
		"# setInterval 不加 unref\n\n## 内容\n\n反套路：setInterval 不加 unref 会导致进程无法退出，不要这样写。定时器 unref。\n")
	writeFixture(t, root, "reference/protocols/TEST-PROTO.md",
		"# 测试协议\n\n## 目标\n\n## 步骤\n\n测试协议内容：热点记忆，最常用的记忆条目优先检索展示。\n")
	writeFixture(t, root, "l1/2026-11-24.log",
		"2026-11-24T10:00:00+08:00 task verify-20261124-ctxread-001 route ERROR front_error\n2026-11-24T10:01:00+08:00 日常巡检完成，无异常\n")
	// 元文件：堆满关键词，旧词法下会霸榜；新全文腿必须排除它。
	writeFixture(t, root, "wiki/lint-report.md",
		"# lint-report\n\n"+strings.Repeat("Cloudflare 注册 验证 邮箱 密钥 定时器 复制 ", 200))
	for rel, content := range extra {
		writeFixture(t, root, rel, content)
	}

	store := openTestStore(t)
	summaries := []SummaryDoc{
		{DocID: "W-0001", Layer: "wiki", Category: "entities", Title: "CF 注册流程",
			Summary:     "用 Camoufox 过 Cloudflare 注册验证，取 Global API Key。",
			Triggers:    "CF注册，Turnstile，Cloudflare开号",
			SummaryPath: "简介/wiki/entities/cf-test.md", SourcePath: "wiki/entities/cf-test.md",
			SourceHash: "aa", SummaryHash: "bb", Status: "ok"},
		{DocID: "W-0002", Layer: "wiki", Category: "entities", Title: "其他云文档",
			Summary:     "云服务器日常维护与备份快照。",
			Triggers:    "云服务器，备份",
			SummaryPath: "简介/wiki/entities/other-cloud.md", SourcePath: "wiki/entities/other-cloud.md",
			SourceHash: "aa", SummaryHash: "bb", Status: "ok"},
		{DocID: "L3-0001", Layer: "l3", Category: "lessons", Title: "cp 复制目录嵌套",
			Summary:     "cp -a 复制目录目标已存在会产生嵌套。",
			Triggers:    "cp -a，嵌套",
			SummaryPath: "简介/l3/lessons/2026-06-21-lesson-cp.md", SourcePath: "l3/lessons/2026-06-21-lesson-cp.md",
			SourceHash: "aa", SummaryHash: "bb", Status: "ok"},
		{DocID: "L3-0002", Layer: "l3", Category: "lessons", Title: "定时器进程退不出",
			Summary:     "setInterval 不加 unref 导致进程退不出。",
			Triggers:    "setInterval，unref，定时器",
			SummaryPath: "简介/l3/lessons/2026-06-21-lesson-timer.md", SourcePath: "l3/lessons/2026-06-21-lesson-timer.md",
			SourceHash: "aa", SummaryHash: "bb", Status: "ok"},
	}
	summaries = append(summaries, extraSummaries...)
	for _, d := range summaries {
		if err := store.UpsertDoc(d); err != nil {
			t.Fatalf("UpsertDoc %s: %v", d.DocID, err)
		}
		vecs, err := fakeEmbed([]string{d.Summary})
		if err != nil {
			t.Fatalf("fakeEmbed: %v", err)
		}
		if err := store.UpsertVector(d.DocID, vecs[0], "fake"); err != nil {
			t.Fatalf("UpsertVector %s: %v", d.DocID, err)
		}
	}
	st.WithSummaryStore(store, fakeEmbed)
	old := SemanticEnabled
	SemanticEnabled = true
	t.Cleanup(func() { SemanticEnabled = old })
	return st, store
}

func findCard(cards []RecallCard, source string) *RecallCard {
	for i := range cards {
		if cards[i].SourcePath == source {
			return &cards[i]
		}
	}
	return nil
}

// ---- RRF 融合 ----

func TestRRFScoreHandComputed(t *testing.T) {
	// 三路名次 1/2/3、权重全 1、k=60、wiki 先验 1.0：手算对照。
	got := rrfScore(1, 2, 3, "wiki", false)
	want := 1.0/61.0 + 1.0/62.0 + 1.0/63.0
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("RRF 手算不符：got=%v want=%v", got, want)
	}
	// 未进的腿不贡献。
	if got := rrfScore(1, 0, 0, "wiki", false); math.Abs(got-1.0/61.0) > 1e-9 {
		t.Fatalf("单腿 RRF 应为 1/61，实为 %v", got)
	}
	// 旧文档降权 80%（剩 20%）。
	if got := rrfScore(1, 0, 0, "wiki", true); math.Abs(got-1.0/61.0*0.2) > 1e-9 {
		t.Fatalf("旧文档应 ×0.2，实为 %v", got)
	}
	// 分类先验在 ±10% 内：l1 取 0.9，再乘无简介层补偿 2.4。
	if got := rrfScore(1, 0, 0, "l1", false); math.Abs(got-1.0/61.0*2.4*0.9) > 1e-9 {
		t.Fatalf("l1 应 ×2.4×0.9，实为 %v", got)
	}
	// reference 同理：先验 0.95，再乘无简介层补偿 2.4。
	if got := rrfScore(1, 0, 0, "reference", false); math.Abs(got-1.0/61.0*2.4*0.95) > 1e-9 {
		t.Fatalf("reference 应 ×2.4×0.95，实为 %v", got)
	}
	for _, layer := range []string{"wiki", "l3", "reference", "l1", "l2"} {
		p := categoryPrior(layer)
		if p < 0.9 || p > 1.1 {
			t.Fatalf("分类先验 %s=%.2f 超出 ±10%%", layer, p)
		}
	}
}

// 两阶段召回：目标文档应交付首位，卡片 ≤5 张、总量 ≤380 tokens，
// 元文件 wiki/lint-report 绝不能进池。
func TestTwoStageDeliversTarget(t *testing.T) {
	st, _ := newTwoStageFixture(t, nil, nil)
	res, err := st.RecallTwoStage("Cloudflare 新账号注册 邮箱验证 取 Global API Key Camoufox")
	if err != nil {
		t.Fatalf("RecallTwoStage: %v", err)
	}
	if !res.SemanticLegOn {
		t.Fatalf("语义腿应开启（SemanticEnabled=true 且有向量）")
	}
	if len(res.Cards) == 0 {
		t.Fatalf("应交付卡片，实为 0 张")
	}
	if res.Cards[0].SourcePath != "wiki/entities/cf-test" {
		t.Fatalf("首位应为 CF 注册文档，实为 %+v", res.Cards[0])
	}
	if !strings.HasPrefix(res.Cards[0].Display(), "【W-0001】") {
		t.Fatalf("交付卡片须带编号，首位 Display 应以【W-0001】开头，实为 %q", res.Cards[0].Display())
	}
	if len(res.Cards) > MaxDeliveredCards {
		t.Fatalf("交付超过 %d 张", MaxDeliveredCards)
	}
	tokens := 0
	for _, c := range res.Cards {
		tokens += c.Tokens()
	}
	if tokens > MaxDeliveredTokens {
		t.Fatalf("交付总量 %d tokens，超过 %d", tokens, MaxDeliveredTokens)
	}
	if c := findCard(res.Pool, "wiki/lint-report"); c != nil {
		t.Fatalf("元文件 wiki/lint-report 不应进候选池")
	}
	if c := findCard(res.Pool, "wiki/entities/cf-test"); c == nil || c.LexRank == 0 {
		t.Fatalf("CF 文档应进简介词法腿，池内记录：%+v", c)
	}
}

// 拒推阈值：负样本（火星部署基站）交付 0 张。
func TestTwoStageRejectsNegative(t *testing.T) {
	st, _ := newTwoStageFixture(t, nil, nil)
	res, err := st.RecallTwoStage("如何在火星表面部署基站")
	if err != nil {
		t.Fatalf("RecallTwoStage: %v", err)
	}
	if len(res.Cards) != 0 {
		t.Fatalf("负样本应交付 0 张，实为 %d 张：%v", len(res.Cards), res.Cards)
	}
}

// 语义腿关闭时仍能召回（词法 + 全文腿兜底），SemanticLegOn 为 false。
func TestTwoStageSemanticLegOff(t *testing.T) {
	st, _ := newTwoStageFixture(t, nil, nil)
	old := SemanticEnabled
	SemanticEnabled = false
	defer func() { SemanticEnabled = old }()
	res, err := st.RecallTwoStage("Cloudflare 新账号注册 邮箱验证")
	if err != nil {
		t.Fatalf("RecallTwoStage: %v", err)
	}
	if res.SemanticLegOn {
		t.Fatalf("语义腿关闭时 SemanticLegOn 应为 false")
	}
	if findCard(res.Cards, "wiki/entities/cf-test") == nil {
		t.Fatalf("语义腿关闭时 CF 文档仍应交付：%v", res.Cards)
	}
}

// L1 通道：含 task/route/ERROR 行的日志应进池。
func TestTwoStageL1Channel(t *testing.T) {
	st, _ := newTwoStageFixture(t, nil, nil)
	res, err := st.RecallTwoStage("task verify-20261124-ctxread-001 route ERROR front_error")
	if err != nil {
		t.Fatalf("RecallTwoStage: %v", err)
	}
	if findCard(res.Pool, "l1/2026-11-24.log") == nil {
		t.Fatalf("L1 日志应进候选池：pool=%v", res.Pool)
	}
}

// ---- 直通车保送 ----

// 保送双条件：精确 token + 全文 BM25 ≥ 阈值且 ≥ 第 2 名 ×1.5。
// 只有一个文档含精确 token 时，第二名得分低，保送成立、最多 1 篇。
func TestExpressLaneBoosts(t *testing.T) {
	extra := map[string]string{
		"l3/lessons/2026-10-09-lesson-argus.md": "# a_bogus ArgusSecurityPlugin 拦截\n\n## 内容\n\n" +
			strings.Repeat("a_bogus uifid ArgusSecurityPlugin 抖音裸调接口被 Argus 拦截 ", 6),
	}
	st, _ := newTwoStageFixture(t, extra, nil)
	res, err := st.RecallTwoStage("a_bogus uifid ArgusSecurityPlugin")
	if err != nil {
		t.Fatalf("RecallTwoStage: %v", err)
	}
	c := findCard(res.Cards, "l3/lessons/2026-10-09-lesson-argus")
	if c == nil {
		t.Fatalf("Argus 文档应交付：%v", res.Cards)
	}
	if !c.Express {
		t.Fatalf("Argus 文档应被保送（Express=true）：%+v", *c)
	}
	if res.Cards[0].SourcePath != c.SourcePath {
		t.Fatalf("保送文档应在交付首位，实为 %v", res.Cards[0])
	}
	n := 0
	for _, card := range res.Cards {
		if card.Express {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("最多保送 1 篇，实为 %d 篇", n)
	}
}

// 两篇含精确 token 的文档得分接近时（比值 <1.5），都不保送。
func TestExpressLaneNoBoostWhenClose(t *testing.T) {
	body := "# a_bogus ArgusSecurityPlugin 拦截\n\n## 内容\n\n" +
		strings.Repeat("a_bogus uifid ArgusSecurityPlugin 抖音裸调接口被 Argus 拦截 ", 6)
	extra := map[string]string{
		"l3/lessons/2026-10-09-lesson-argus.md":  body,
		"l3/lessons/2026-10-10-lesson-argus2.md": body,
	}
	st, _ := newTwoStageFixture(t, extra, nil)
	res, err := st.RecallTwoStage("a_bogus uifid ArgusSecurityPlugin")
	if err != nil {
		t.Fatalf("RecallTwoStage: %v", err)
	}
	for _, card := range res.Cards {
		if card.Express {
			t.Fatalf("两篇得分接近时不应保送，实有保送：%+v", card)
		}
	}
}

// ---- 去重 ----

// 同层同类（l3/lessons）简介向量余弦 >0.90 只留排名靠前者。
func TestDedupSameLayerCategory(t *testing.T) {
	extra := map[string]string{
		"l3/lessons/2026-06-22-lesson-timer2.md": "# 定时器退不出（同义）\n\n## 内容\n\nsetInterval 不加 unref 进程无法退出，事件循环定时器问题。setInterval unref 定时器。\n",
	}
	extraSummaries := []SummaryDoc{
		{DocID: "L3-0004", Layer: "l3", Category: "lessons", Title: "定时器退不出（同义）",
			Summary:     "setInterval 不加 unref 导致进程退不出。",
			Triggers:    "setInterval，unref",
			SummaryPath: "简介/l3/lessons/2026-06-22-lesson-timer2.md", SourcePath: "l3/lessons/2026-06-22-lesson-timer2.md",
			SourceHash: "aa", SummaryHash: "bb", Status: "ok"},
	}
	st, _ := newTwoStageFixture(t, extra, extraSummaries)
	res, err := st.RecallTwoStage("setInterval 不加 unref 定时器 进程退不出")
	if err != nil {
		t.Fatalf("RecallTwoStage: %v", err)
	}
	a := findCard(res.Cards, "l3/lessons/2026-06-21-lesson-timer")
	b := findCard(res.Cards, "l3/lessons/2026-06-22-lesson-timer2")
	if a != nil && b != nil {
		t.Fatalf("同层同类近重复简介应只留一篇，两篇都交付了：%v / %v", *a, *b)
	}
	if a == nil && b == nil {
		t.Fatalf("至少应交付其中一篇：%v", res.Cards)
	}
}

// ---- superseded_by ----

func TestSupersededPullInAndPrefix(t *testing.T) {
	extra := map[string]string{
		"l3/lessons/2026-06-21-lesson-old.md": "# 旧版部署流程\n\n## 内容\n\n旧版流程：用老办法部署，先停服务再覆盖。老办法部署流程。\n",
		"l3/lessons/2026-06-22-lesson-new.md": "# 新版部署流程\n\n## 内容\n\n新版流程：用新办法部署，滚动更新不停服务。新办法部署流程。\n",
	}
	extraSummaries := []SummaryDoc{
		{DocID: "L3-0010", Layer: "l3", Category: "lessons", Title: "旧版部署流程",
			Summary:     "旧版流程：用老办法部署。",
			Triggers:    "老办法，部署",
			SummaryPath: "简介/l3/lessons/2026-06-21-lesson-old.md", SourcePath: "l3/lessons/2026-06-21-lesson-old.md",
			SourceHash: "aa", SummaryHash: "bb", Status: "ok"},
		{DocID: "L3-0011", Layer: "l3", Category: "lessons", Title: "新版部署流程",
			Summary:     "新版流程：用新办法部署。",
			Triggers:    "新办法，部署",
			SummaryPath: "简介/l3/lessons/2026-06-22-lesson-new.md", SourcePath: "l3/lessons/2026-06-22-lesson-new.md",
			SourceHash: "aa", SummaryHash: "bb", Status: "ok"},
	}
	st, store := newTwoStageFixture(t, extra, extraSummaries)
	if err := store.SetSupersededBy("L3-0010", "L3-0011"); err != nil {
		t.Fatalf("SetSupersededBy: %v", err)
	}
	res, err := st.RecallTwoStage("部署流程 老办法")
	if err != nil {
		t.Fatalf("RecallTwoStage: %v", err)
	}
	// 新文档被强制拉入候选池。
	if findCard(res.Pool, "l3/lessons/2026-06-22-lesson-new") == nil {
		t.Fatalf("新文档应被拉入候选池：pool=%v", res.Pool)
	}
	old := findCard(res.Cards, "l3/lessons/2026-06-21-lesson-old")
	if old == nil {
		t.Fatalf("旧文档应交付（降权后仍在前 5 内）：%v", res.Cards)
	}
	if !strings.HasPrefix(old.Title, "[已过期，替代见 L3-0011]") {
		t.Fatalf("旧文档标题应加过期前缀，实为 %q", old.Title)
	}
	// 问历史时不加前缀、不降权。
	res2, err := st.RecallTwoStage("以前部署流程为什么改 老办法")
	if err != nil {
		t.Fatalf("RecallTwoStage: %v", err)
	}
	old2 := findCard(res2.Cards, "l3/lessons/2026-06-21-lesson-old")
	if old2 == nil {
		t.Fatalf("问历史时旧文档应交付：%v", res2.Cards)
	}
	if strings.HasPrefix(old2.Title, "[已过期") {
		t.Fatalf("问历史时不应加过期前缀，实为 %q", old2.Title)
	}
}

// ---- 拒推阈值规模自适应（线 B 第二阶段第 4 棒修复） ----

// rejectBM25Threshold 必须按库规模缩放：生产库 N≈1693 时 ≈19.0
// （行为不变），小库按罕见词 IDF 之比放宽，否则小库会被门槛
// 一刀切成 0 张；且阈值随 N 单调不减、上限 19.0×1.5=28.5
// （2026-11-24 调参：基准 15.0→19.0，上限因子 1.0→1.5）。
func TestRejectBM25ThresholdScalesWithCorpus(t *testing.T) {
	if got := rejectBM25Threshold(1694); got < 18.9 || got > 19.0 {
		t.Fatalf("N=1694 时阈值应为 19.0，实为 %.2f", got)
	}
	if got := rejectBM25Threshold(1693); got < 18.9 || got > 19.0 {
		t.Fatalf("N=1693（生产库）时阈值应 ≈19.0，实为 %.2f", got)
	}
	small := rejectBM25Threshold(9)
	if small < 3.0 || small > 6.0 {
		t.Fatalf("N=9 时阈值应约 5.1，实为 %.2f", small)
	}
	big := rejectBM25Threshold(100000)
	if big != RejectMinBM25*1.5 {
		t.Fatalf("超大库阈值应封顶 28.5，实为 %.2f", big)
	}
	prev := 0.0
	for _, n := range []int{2, 5, 9, 50, 200, 1000, 1694} {
		if got := rejectBM25Threshold(n); got < prev {
			t.Fatalf("阈值应随 N 单调不减：N=%d 时 %.2f < 前值 %.2f", n, got, prev)
		} else {
			prev = got
		}
	}
}

// ---- token 估算口径对照 ----

// estimateTokens 须与 registry.EstimateTokens 同口径（汉字 0.6、
// 其余 0.25），否则 380 token 封顶会算错。
func TestEstimateTokensMatchesRegistry(t *testing.T) {
	for _, s := range []string{
		"CF 注册流程：用 Camoufox 过 Cloudflare 注册验证，取 Global API Key。",
		"纯中文一句话简介，没有英文。",
		"plain ascii only text for testing purposes",
		"混合 Mixed 中文 English 123 数字",
	} {
		if got, want := estimateTokens(s), registry.EstimateTokens(s); got != want {
			t.Fatalf("estimateTokens(%q)=%d，registry=%d", s, got, want)
		}
	}
}
