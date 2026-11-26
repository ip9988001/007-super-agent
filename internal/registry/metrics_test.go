package registry

import (
	"math"
	"strings"
	"testing"
)

// TestEstimateDispatchTokens 保守口径：汉字 1.0、英文词 1.3、其余 ASCII 0.25、
// 其余非 ASCII 1.0，总和 ×1.1 后向上取整。
// （P0 修复：汉字权重 1.5→1.0；3000 上限约容 2727 纯汉字。）
func TestEstimateDispatchTokens(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"hello world", 4},            // 2 词 1 空格：(2.6+0.25)×1.1=3.135→4
		{strings.Repeat("汉", 10), 11}, // 10 个汉字：10×1.1=11
		{"部署 Cloudflare Worker", 6},   // 2 汉字+2 词+2 空格：(2+2.6+0.5)×1.1=5.61→6
		{"a", 2},                      // 1 个词：1.3×1.1=1.43→2
		{"...", 1},                    // 3 个标点：0.75×1.1=0.825→1
		{"max-tokens=3000", 5},        // 3 个词+2 标点：(3.9+0.5)×1.1=4.84→5
	}
	for _, c := range cases {
		if got := EstimateDispatchTokens(c.in); got != c.want {
			t.Errorf("EstimateDispatchTokens(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestEstimateDispatchTokensBudget 3000 上限的纯汉字容量：
// 2727 个纯汉字恰好 3000 token（2727×1.1=2999.7→3000），
// 2728 个就超（2728×1.1=3000.8→3001）。上线后校准的锚点。
func TestEstimateDispatchTokensBudget(t *testing.T) {
	if got := EstimateDispatchTokens(strings.Repeat("汉", 2727)); got != 3000 {
		t.Errorf("2727 纯汉字 = %d token, want 3000", got)
	}
	if got := EstimateDispatchTokens(strings.Repeat("汉", 2728)); got != 3001 {
		t.Errorf("2728 纯汉字 = %d token, want 3001", got)
	}
	// 1200 软告警线约容 1090 个纯汉字。
	if got := EstimateDispatchTokens(strings.Repeat("汉", 1090)); got != 1199 {
		t.Errorf("1090 纯汉字 = %d token, want 1199", got)
	}
}

// TestEstimateDispatchTokensConservative 同一份中文派单，保守口径必须
// 明显大于旧审计口径（旧口径 1 汉字 0.6 会低估真实分词约 40%）。
func TestEstimateDispatchTokensConservative(t *testing.T) {
	s := strings.Repeat("派单目标验收标准工具编号指针约束步骤骨架记忆碎片", 20)
	n := len([]rune(s))
	old := EstimateTokens(s)
	neu := EstimateDispatchTokens(s)
	if neu <= old {
		t.Fatalf("保守口径 %d 应大于旧口径 %d", neu, old)
	}
	// n 个汉字：保守口径 = ceil(n×1.0×1.1)，旧口径 = round(n×0.6)。
	want := int(math.Ceil(float64(n) * 1.0 * 1.1))
	if neu != want {
		t.Fatalf("保守口径 = %d, want %d（%d 汉字）", neu, want, n)
	}
}
