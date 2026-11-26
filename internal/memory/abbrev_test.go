package memory

import (
	"strings"
	"testing"
)

// 缩写单向展开：输入 CF 补 Cloudflare 一起查。
func TestAbbreviationExpandsForward(t *testing.T) {
	expanded, added := expandAbbreviations("新号怎么注册 CF 要过邮箱验证")
	if !strings.Contains(expanded, "Cloudflare") {
		t.Fatalf("CF 应展开出 Cloudflare：%q", expanded)
	}
	if len(added) != 1 || added[0] != "Cloudflare" {
		t.Fatalf("追加项应为 [Cloudflare]，实为 %v", added)
	}
	// “CF注册”连写也在词边界上，应展开。
	expanded, _ = expandAbbreviations("CF注册过不去")
	if !strings.Contains(expanded, "Cloudflare") {
		t.Fatalf("连写的 CF 应展开：%q", expanded)
	}
}

// 缩写绝不反向：输入 Cloudflare 不补 CF；CFO 里的 CF 不是词，不展开。
func TestAbbreviationNeverReverse(t *testing.T) {
	expanded, added := expandAbbreviations("Cloudflare 新账号注册流程")
	if len(added) != 0 {
		t.Fatalf("输入全称不应展开，实为 %v", added)
	}
	if strings.Contains(expanded, " CF ") || strings.HasSuffix(expanded, " CF") {
		t.Fatalf("输入全称不应补缩写：%q", expanded)
	}
	_, added = expandAbbreviations("CFO 财务报表怎么做")
	if len(added) != 0 {
		t.Fatalf("CFO 里的 CF 不是独立词，不应展开：%v", added)
	}
}

// 映射表软上限 50 条。
func TestAbbreviationTableCap(t *testing.T) {
	if len(Abbreviations) > maxAbbreviations {
		t.Fatalf("缩写表 %d 条，超过软上限 %d", len(Abbreviations), maxAbbreviations)
	}
}
