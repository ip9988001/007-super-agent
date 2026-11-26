package summary

import (
	"strings"
	"testing"
)

func TestCountUnitsAndWidth(t *testing.T) {
	// 12 汉字 + 3 英文词 = 15 单位；等效宽度 12 + ASCII 数×0.5。
	s := "注册新账号要过邮箱验证，取 Global API Key"
	if got := CountUnits(s); got != 15 {
		t.Fatalf("CountUnits = %d, want 15", got)
	}
	// 纯英文：3 个词。
	if got := CountUnits("hello world foo"); got != 3 {
		t.Fatalf("CountUnits(en) = %d, want 3", got)
	}
	w := EffectiveWidth("ab中文")
	// 汉字 2×1 + ASCII 2×0.5 = 3。
	if w != 3 {
		t.Fatalf("EffectiveWidth = %v, want 3", w)
	}
}

func TestParseFormatRoundTrip(t *testing.T) {
	in := "summary: 用 Camoufox 注册 Cloudflare 新账号，过邮箱验证后取回全局密钥，遇到验证框卡住就换个干净网络重试，全程照着步骤走。\n" +
		"triggers: 新号怎么注册，CF 过盾，邮箱收不到验证码，Global API Key，Turnstile 点不动\n"
	s, err := Parse(in)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if CountUnits(s.Summary) < 30 {
		t.Fatalf("roundtrip summary too short: %d units", CountUnits(s.Summary))
	}
	if len(s.Triggers()) != 5 {
		t.Fatalf("triggers = %d, want 5", len(s.Triggers()))
	}
	if len(s.Core) != 5 || len(s.Dynamic) != 0 {
		t.Fatalf("slots core=%d dynamic=%d, want 5/0", len(s.Core), len(s.Dynamic))
	}
	out := Format(s)
	s2, err := Parse(out)
	if err != nil {
		t.Fatalf("re-Parse: %v", err)
	}
	if s2.Summary != s.Summary || strings.Join(s2.Triggers(), ",") != strings.Join(s.Triggers(), ",") {
		t.Fatalf("roundtrip mismatch:\n%+v\n%+v", s, s2)
	}
}

func TestParseBadFormat(t *testing.T) {
	for _, bad := range []string{
		"只有一行",
		"summary: 只有一行简介，没有触发词行\n",
		"简介：xxx\ntriggers: a，b，c\n",
		"summary: a\ntriggers: a\n多余第三行\n",
	} {
		if _, err := Parse(bad); err == nil {
			t.Fatalf("Parse(%q) should fail", bad)
		}
	}
}

func TestNormalizeSlots(t *testing.T) {
	s := Summary{Core: []string{"a", "b", "c", "d", "e", "f", "g", "h"}}
	NormalizeSlots(&s)
	if len(s.Core) != 5 || len(s.Dynamic) != 3 {
		t.Fatalf("slots = %d/%d, want 5/3", len(s.Core), len(s.Dynamic))
	}
	if s.Core[0] != "a" || s.Dynamic[0] != "f" {
		t.Fatalf("slot order wrong: %+v", s)
	}
}

// 好简介：30+ 单位、含动作词与症状词、无泛词、无模板句。
const goodSummary = "用 Camoufox 浏览器注册 Cloudflare 新账号，先过邮箱验证，再进后台取回全局密钥，遇到验证框卡住就换个干净网络重试。"

var goodTriggers = []string{"新号怎么注册", "CF 过盾", "邮箱收不到验证码", "Global API Key", "Turnstile 点不动"}

func TestCheckBoilerplate(t *testing.T) {
	if r := CheckBoilerplate(goodSummary, "CF 注册剧本"); !r.Pass {
		t.Fatalf("good summary should pass rule 1: %s", r.Reason)
	}
	if r := CheckBoilerplate("本文介绍了 Cloudflare 注册的完整流程，总的来说很简单。", "CF 注册"); r.Pass {
		t.Fatalf("template summary should fail rule 1")
	}
	if r := CheckBoilerplate("CF 注册剧本", "CF 注册剧本"); r.Pass {
		t.Fatalf("title-echo summary should fail rule 1")
	}
	if r := CheckBoilerplate("这是一段只有背景的描述，没有讲到任何用途。", "某标题"); r.Pass {
		t.Fatalf("background-only summary should fail rule 1")
	}
}

func TestCheckBoilerplateActionWords(t *testing.T) {
	// 2026-11-24 校准：动作词表补齐后，写了用途的简介不再被误判为
	// “只有背景”。以下 5 句是第一批 20 篇里曾被规则①误杀的简介原文。
	cases := []string{
		"偏好规则：新工具默认装到 E:\\00-E-Agent-JiYi260609\\00-RuanJian\\ 下的独立子目录以实现环境隔离，装新软件前先按此选路径。",
		"Pi 启动批处理脚本的集成模式：后台拉起 L1 watcher、自动开浏览器访问 WebUI、再启动 Next.js 服务，做一键启动或改启动脚本时参考。",
		"记录 pi v0.99.1 compact 机制的真相：触发判定与切点选择都是机械规则（contextTokens 超阈值、keepRecentTokens 定切点），用于理解上下文压缩为何发生及 L1 污染修复结论。",
		"解释日志里 AGENT、DIRECT、ERROR 三种路由与 done、delivery_halt、max_iterations 等状态的含义，用于看日志判断任务是正常完成、卡迭代还是交付被阻断。",
		"规定 address.123ip.cc 地址卡 10 字段的美式顺序与复制按钮，电话按州区号加 555-01XX 示例段生成，城市名走中英双语策略，改地址卡页面或加城市译名时用。",
	}
	for _, s := range cases {
		if r := CheckBoilerplate(s, "某标题"); !r.Pass {
			t.Fatalf("summary with action word should pass rule 1: %q: %s", s, r.Reason)
		}
	}
	// 仍只有背景的简介继续打回（动作词表没加 是/有/在/和 这类虚词）。
	if r := CheckBoilerplate("这是一段只有背景的描述，没有讲到任何用途。", "某标题"); r.Pass {
		t.Fatalf("background-only summary should fail rule 1")
	}
}

func TestCheckOverlap(t *testing.T) {
	if r := CheckOverlap(goodSummary, goodTriggers); !r.Pass {
		t.Fatalf("good triggers should pass rule 2: %s", r.Reason)
	}
	// 整词级（2026-11-24 校准）：触发词整项原样出现在简介里才算命中；
	// 旧词元级口径量的是主题相似度，正常简介必然被打回。
	// CF 注册剧本：7 个触发词只有 2 个整项出现在简介里 → 2/7 通过。
	cfSummary := "用 Camoufox 过 CF 注册页 Turnstile 的完整流程：注册、邮箱验证、取 Global API Key，含踩坑清单与启动模板，适合需要新开 Cloudflare 账号或复现拿密钥时照抄。"
	cfTriggers := []string{"CF注册", "Turnstile过不去", "Cloudflare开号", "Global API Key", "Camoufox", "cfk_", "邮箱验证链接失效"}
	if rate := OverlapRate(cfSummary, cfTriggers); rate != 2.0/7.0 {
		t.Fatalf("CF playbook overlap = %v, want 2/7", rate)
	}
	if r := CheckOverlap(cfSummary, cfTriggers); !r.Pass {
		t.Fatalf("CF playbook should pass rule 2: %s", r.Reason)
	}
	// 命名表文档：7 个触发词 6 个整项都在简介里 → 6/7 打回。
	nameSummary := "记录钢铁意志四档模式的命名与规模：A档零号约 6k、B档独行约 12k、C档阵斩约 29k、C档加进化加约 36k，配置档位或对名称时查。"
	nameTriggers := []string{"钢铁意志", "四档模式", "零号", "独行", "阵斩", "进化加", "档位命名"}
	if rate := OverlapRate(nameSummary, nameTriggers); rate != 6.0/7.0 {
		t.Fatalf("naming doc overlap = %v, want 6/7", rate)
	}
	if r := CheckOverlap(nameSummary, nameTriggers); r.Pass {
		t.Fatalf("naming doc should fail rule 2: %s", r.Reason)
	}
	// 豁免：纯缩写 CF、API 与恰好等于动作词的条目不计入分子分母。
	rate := OverlapRate("注册 CF 新账号，用 API 查状态", []string{"CF", "API", "注册"})
	if rate != 0 {
		t.Fatalf("exempt entries should give rate 0, got %v", rate)
	}
	// 边界：恰好 50% 通过，超过 50% 打回。
	halfTriggers := []string{"甲一", "乙二", "丙三", "丁四", "戊五", "己六"}
	if r := CheckOverlap("甲一乙二丙三", halfTriggers); !r.Pass {
		t.Fatalf("exactly 50%% overlap should pass rule 2: %s", r.Reason)
	}
	if r := CheckOverlap("甲一乙二丙三丁四", halfTriggers); r.Pass {
		t.Fatalf("over 50%% overlap should fail rule 2: %s", r.Reason)
	}
}

func TestCheckGenericWords(t *testing.T) {
	if r := CheckGenericWords(goodTriggers); !r.Pass {
		t.Fatalf("good triggers should pass rule 3: %s", r.Reason)
	}
	for _, bad := range [][]string{{"系统设置"}, {"配置方法"}, {"问题汇总"}, {"教程合集"}, {"方案对比"}, {"记录一下"}} {
		if r := CheckGenericWords(bad); r.Pass {
			t.Fatalf("triggers %v should fail rule 3", bad)
		}
	}
}

func TestCheckCounts(t *testing.T) {
	if r := CheckCounts(goodSummary, goodTriggers, true); !r.Pass {
		t.Fatalf("good summary should pass rule 4: %s", r.Reason)
	}
	if r := CheckCounts("太短了。", goodTriggers, false); r.Pass {
		t.Fatalf("short summary should fail rule 4")
	}
	if r := CheckCounts(goodSummary, []string{"只有", "两个"}, false); r.Pass {
		t.Fatalf("2 triggers should fail rule 4")
	}
	// 剧本缺动作词打回（简介与触发词里都不能含动作词）。
	noAction := "这篇讲 Cloudflare 新账号的邮箱核对与全局密钥拿到手，全程照着步骤走就能完成。"
	noActionTriggers := []string{"新号办理", "CF 过盾", "邮箱收不到信", "Global API Key", "盾框卡住"}
	if r := CheckCounts(noAction, noActionTriggers, true); r.Pass {
		t.Fatalf("playbook without action word should fail rule 4: %q", noAction)
	}
	// 等效宽度超 120 打回：纯英文长串。
	longEN := strings.Repeat("abcdefghij ", 30) // 30 词、等效宽度远超 120
	if r := CheckCounts(longEN, goodTriggers, false); r.Pass {
		t.Fatalf("over-wide summary should fail rule 4")
	}
}

const entitySource = "# Cloudflare 新账号注册\n\n用 Camoufox 打开 Cloudflare 注册页，过邮箱验证后进后台取 Global API Key。\n" +
	"报错时看 `Turnstile` 按钮文字是不是 View，换个网络重试。\n" +
	"Cloudflare Cloudflare Cloudflare 邮箱验证 邮箱验证\n"

func TestCheckEntityCoverage(t *testing.T) {
	ents := ExtractEntities(entitySource, "Cloudflare 新账号注册")
	if len(ents) == 0 {
		t.Fatalf("no entities extracted")
	}
	if r := CheckEntityCoverage(goodSummary, goodTriggers, entitySource, "Cloudflare 新账号注册"); !r.Pass {
		t.Fatalf("good summary should pass rule 5: %s (ents %v)", r.Reason, ents)
	}
	// 简介完全不提核心实体：打回。
	off := "这篇讲怎么给老人挑一张不用实名的流量卡，插卡就能上网，用完就扔。"
	if r := CheckEntityCoverage(off, []string{"老人卡", "免实名", "流量卡"}, entitySource, "Cloudflare 新账号注册"); r.Pass {
		t.Fatalf("off-topic summary should fail rule 5")
	}
}

func TestCheckPolarity(t *testing.T) {
	if r := CheckPolarity("定时器不加 unref 会导致进程退不出来，不要照抄这种写法。", true); !r.Pass {
		t.Fatalf("anti-pattern with warning should pass rule 6: %s", r.Reason)
	}
	if r := CheckPolarity("定时器这样写很方便，进程会一直跑。", true); r.Pass {
		t.Fatalf("anti-pattern without warning should fail rule 6")
	}
	if r := CheckPolarity("随便写都行。", false); !r.Pass {
		t.Fatalf("non-anti-pattern should pass rule 6")
	}
}

func TestCheckSecretSynthetic(t *testing.T) {
	// 合成假密钥，绝不用真密钥值做测试。
	fakeKey := "sk-ant-fake0123456789abcdef"
	if r := CheckSecret("简介里不小心贴了 "+fakeKey, goodTriggers); r.Pass {
		t.Fatalf("summary containing synthetic key should fail secret check")
	}
	if r := CheckSecret(goodSummary, []string{"api_key=" + "fake0123456789abcdef0123456789"}); r.Pass {
		t.Fatalf("trigger containing synthetic kv secret should fail secret check")
	}
	if r := CheckSecret(goodSummary, goodTriggers); !r.Pass {
		t.Fatalf("clean summary should pass secret check: %s", r.Reason)
	}
}

func TestCheckAllGood(t *testing.T) {
	s := Summary{SourcePath: "wiki/entities/cf-新账号注册-邮箱验证-取-global-api-key-全流程-camoufox-版.md", Summary: goodSummary, Core: goodTriggers}
	results := CheckAll(s, entitySource, "Cloudflare 新账号注册")
	if !AllPass(results) {
		for _, r := range results {
			if !r.Pass {
				t.Errorf("rule %d (%s) failed: %s", r.Rule, r.Name, r.Reason)
			}
		}
	}
}

func TestExtractEntitiesCalibrated(t *testing.T) {
	// 2026-11-24 校准的三项实体提取修复。
	contains := func(ents []string, want string) bool {
		for _, e := range ents {
			if e == want {
				return true
			}
		}
		return false
	}
	// (a) 标题长分句（超过 8 字）切成短词，不整句当实体。
	longTitleSrc := "# 资产参数需具备时间轴概念，不可用全局静态快照覆盖全章节\n\n资产 资产 时间 时间 漫剧 漫剧\n"
	ents := ExtractEntities(longTitleSrc, "资产参数需具备时间轴概念，不可用全局静态快照覆盖全章节")
	for _, e := range ents {
		if RuneCount(e) > 8 {
			t.Fatalf("long title segment must be split, got entity %q in %v", e, ents)
		}
	}
	if !contains(ents, "资产") {
		t.Fatalf("long title should still yield short entity 资产: %v", ents)
	}
	// (b) 代码标识符质量过滤：偶然出现一次的短标识符（如 dev）不进
	// 前 5；原文出现 ≥2 次的短标识符（如 abc）保留。
	codeSrc := "# 排查测试\n\n先跑 `dev` 看一眼，再跑 `abc` 与 `abc` 对照，`null` 只出现一次。\n进程 进程 进程 定时器 定时器 定时器 退出 退出\n"
	ents = ExtractEntities(codeSrc, "排查测试")
	if contains(ents, "dev") {
		t.Fatalf("one-off short code token dev must be filtered: %v", ents)
	}
	if !contains(ents, "abc") {
		t.Fatalf("code token seen twice must be kept: %v", ents)
	}
	// (c) 三路来源每路最多占 2 席：标题 5 个词只进前 2 个，剩下的席位
	// 给正文高频词。
	multiTitleSrc := "# 苹果，香蕉，橙子，葡萄，西瓜\n\n进程 进程 进程 定时器 定时器 定时器 退出 退出\n"
	ents = ExtractEntities(multiTitleSrc, "苹果，香蕉，橙子，葡萄，西瓜")
	if !contains(ents, "苹果") || !contains(ents, "香蕉") {
		t.Fatalf("first two title words must be kept: %v", ents)
	}
	for _, no := range []string{"橙子", "葡萄", "西瓜"} {
		if contains(ents, no) {
			t.Fatalf("title must take at most 2 seats, %q leaked in: %v", no, ents)
		}
	}
	if !contains(ents, "进程") || !contains(ents, "定时") {
		t.Fatalf("remaining seats must go to body frequent words: %v", ents)
	}
}

func TestExtractEntitiesNoisyTitle(t *testing.T) {
	// 试点首轮教训：长标题里的 ①②③、度数、括号残留不能当实体，
	// 否则模型简介永远命中不了，白白被规则⑤打回。
	src := "# 发布到 Release 前三步：①按县分片 ②压缩 ③发布——州框宽度>180°(如AK)的 Overture 必须先切片\n\n按县分片后压缩再发布到 Release。\n"
	ents := ExtractEntities(src, "发布到 Release 前三步：①按县分片 ②压缩 ③发布——州框宽度>180°(如AK)的 Overture 必须先切片")
	for _, e := range ents {
		if strings.ContainsAny(e, "①②③°") {
			t.Fatalf("entity %q still contains numbering/degree noise", e)
		}
	}
	found := false
	for _, e := range ents {
		if e == "按县分片" || e == "压缩" || e == "Release" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no clean entity found in %v", ents)
	}
}

func TestFallbackSkipsCodeAndTitleEcho(t *testing.T) {
	// 试点第二轮教训：兜底曾把 timer = setInterval(...) 代码行当首句，
	// 又把与标题一字不差的正文行当简介。
	src := "---\ntype: anti-pattern\ntitle: setInterval 不加 unref 导致进程无法退出\n---\n# setInterval 不加 unref 导致进程无法退出\n\ntimer = setInterval(() => { doWork(); }, 5000);\n\nsetInterval 不加 unref 导致进程无法退出\n\n定时器会让事件循环一直有活干，进程收不到退出信号，服务只能强制杀掉。\n"
	fb := FallbackSummary("l3/anti-patterns/setInterval不加unref.md", "setInterval 不加 unref 导致进程无法退出", src)
	if strings.Contains(fb.Summary, "timer =") {
		t.Fatalf("fallback picked a code line: %q", fb.Summary)
	}
	if strings.Contains(fb.Summary, "setInterval 不加 unref 导致进程无法退出") {
		t.Fatalf("fallback echoed the title: %q", fb.Summary)
	}
	if u := CountUnits(fb.Summary); u < 30 || u > 100 {
		t.Fatalf("fallback summary %d units: %q", u, fb.Summary)
	}
	if !HasNegativeWord(fb.Summary) {
		t.Fatalf("anti-pattern fallback must carry a warning word: %q", fb.Summary)
	}
	if len(fb.Triggers()) < 3 || len(fb.Triggers()) > 8 {
		t.Fatalf("fallback triggers = %d: %v", len(fb.Triggers()), fb.Triggers())
	}
	if strings.Contains(fb.Summary, "。，") || strings.Contains(fb.Summary, "，，") {
		t.Fatalf("fallback summary has broken punctuation: %q", fb.Summary)
	}
	for _, trg := range fb.Triggers() {
		if len([]rune(trg)) < 2 || trg == "2" || trg == "10" {
			t.Fatalf("junk trigger %q", trg)
		}
	}
}

func TestTrimToBoundary(t *testing.T) {
	long := strings.Repeat("这是一个很长的句子，", 20) // 180 单位
	out := trimToBoundary(long, 100)
	if CountUnits(out) > 100 {
		t.Fatalf("trimmed to %d units, want <= 100", CountUnits(out))
	}
	if !strings.HasSuffix(out, "子") {
		t.Fatalf("trimmed summary should end at a sentence boundary, got %q", out)
	}
}

func TestFallbackSummary(t *testing.T) {
	src := "# setInterval 不加 unref\n\n定时器写了进程退不出来，是因为 setInterval 让事件循环一直有活干。\nsetInterval setInterval 定时器 定时器 进程 进程\n"
	fb := FallbackSummary("l3/anti-patterns/setInterval不加unref.md", "setInterval 不加 unref", src)
	if fb.Status != StatusDegradedOK {
		t.Fatalf("fallback status = %s, want degraded_ok", fb.Status)
	}
	if u := CountUnits(fb.Summary); u < 30 || u > 100 {
		t.Fatalf("fallback summary %d units, want 30-100: %q", u, fb.Summary)
	}
	if !HasNegativeWord(fb.Summary) {
		t.Fatalf("anti-pattern fallback must carry a warning word: %q", fb.Summary)
	}
	if len(fb.Triggers()) < 3 || len(fb.Triggers()) > 8 {
		t.Fatalf("fallback triggers = %d, want 3-8: %v", len(fb.Triggers()), fb.Triggers())
	}
	// 剧本兜底须含动作词。
	fb2 := FallbackSummary("wiki/playbooks/抓取调研/2026-10-07-网页抓取决策阶梯.md", "网页抓取决策阶梯", "# 网页抓取决策阶梯\n\n先判断页面是静态还是动态，再选工具抓取。\n抓取 抓取 工具 工具 动态 动态\n")
	if !HasActionWord(fb2.Summary, fb2.Triggers()) {
		t.Fatalf("playbook fallback must carry an action word: %q / %v", fb2.Summary, fb2.Triggers())
	}
}
