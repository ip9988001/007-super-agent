package sanitize

import (
	"strings"
	"testing"
)

// 本文件全部用合成假密钥做测试，绝不用真实密钥值。

// TestScrubOutboundHighRiskPatterns：Gemini 审阅点名的高危固定模式
// （sk-、AKIA、ghp_、Bearer、BEGIN PRIVATE KEY）与新增的 sk-proj-
// 变体，出站前必须全部被掩码，假值原文不得残留。
func TestScrubOutboundHighRiskPatterns(t *testing.T) {
	cases := []struct {
		name string
		in   string
		fake string // 必须从出站文本里消失的假值片段
	}{
		{"sk-", "配置里写着 sk-FAKEFAKEFAKE1234567890，请勿外泄。", "sk-FAKE"},
		{"sk-proj-", "项目密钥 sk-proj-FAKEFAKEFAKE1234567890 已轮换。", "sk-proj-FAKE"},
		{"AKIA", "AWS 账号 AKIAFAKEFAKEFAKEFAKE 已停用。", "AKIAFAKE"},
		{"ghp_", "令牌 ghp_FAKEFAKEFAKEFAKEFAKEFAKE 不要贴出来。", "ghp_FAKE"},
		{"Bearer", "调用时带 Bearer FAKEFAKEFAKE1234567890 请求。", "Bearer FAKE"},
		{"PEM", "私钥如下：\n-----BEGIN PRIVATE KEY-----\nFAKEFAKEFAKEFAKE\n-----END PRIVATE KEY-----\n请妥善保管。", "BEGIN PRIVATE KEY"},
		{"kv", "配置：api_key = FAKEFAKE1234567890，请核对。", "FAKEFAKE1234567890"},
	}
	for _, c := range cases {
		out, n, blocked := ScrubOutbound(c.in)
		if blocked {
			t.Fatalf("%s：不应整封拦截", c.name)
		}
		if n < 1 {
			t.Fatalf("%s：掩码处数应 ≥1，实为 %d", c.name, n)
		}
		if strings.Contains(out, c.fake) {
			t.Fatalf("%s：假值未脱掉：%q", c.name, out)
		}
		if !strings.Contains(out, Redacted) {
			t.Fatalf("%s：应含掩码标记：%q", c.name, out)
		}
	}
}

// TestScrubOutboundMultiple：一次多处命中要全遮掉并如数上报。
func TestScrubOutboundMultiple(t *testing.T) {
	in := "密钥 A 是 sk-FAKEFAKEFAKE1234567890，密钥 B 是 AKIAFAKEFAKEFAKEFAKE，完。"
	out, n, blocked := ScrubOutbound(in)
	if blocked || n != 2 {
		t.Fatalf("应掩码 2 处、不拦截，实为 n=%d blocked=%v：%q", n, blocked, out)
	}
	if strings.Count(out, Redacted) != 2 {
		t.Fatalf("应恰好遮 2 处，得 %q", out)
	}
}

// TestScrubOutboundCleanPassthrough：普通文本（无密钥）必须原样通过，
// 不得多改一个字；短串 sk-abc（不足 10 位）不是密钥，不应误伤。
func TestScrubOutboundCleanPassthrough(t *testing.T) {
	in := "任务完成：共整理 12 条记录，详见附件。编号 sk-abc 是短串，网址 https://example.com/docs 保持原样。"
	out, n, blocked := ScrubOutbound(in)
	if blocked || n != 0 {
		t.Fatalf("普通文本不应掩码/拦截，实为 n=%d blocked=%v：%q", n, blocked, out)
	}
	if out != in {
		t.Fatalf("普通文本应原样返回，实为 %q", out)
	}
}

// TestScrubOutboundIdempotent：脱敏后复查无残留、二次扫描稳定。
func TestScrubOutboundIdempotent(t *testing.T) {
	in := "密钥 sk-FAKEFAKEFAKE1234567890 已记录。"
	out1, _, b1 := ScrubOutbound(in)
	out2, n2, b2 := ScrubOutbound(out1)
	if b1 || b2 || n2 != 0 || out1 != out2 {
		t.Fatalf("出站扫描应幂等：%q / %q (n2=%d)", out1, out2, n2)
	}
	if ContainsSecret(out1) {
		t.Fatalf("脱敏后应无残留：%q", out1)
	}
}

// TestBlockedReplyIsClean：兜底文本本身必须过得了扫描，不会自触发拦截。
func TestBlockedReplyIsClean(t *testing.T) {
	out, n, blocked := ScrubOutbound(BlockedReply)
	if blocked || n != 0 || out != BlockedReply {
		t.Fatalf("兜底文本应原样通过，实为 n=%d blocked=%v：%q", n, blocked, out)
	}
}
