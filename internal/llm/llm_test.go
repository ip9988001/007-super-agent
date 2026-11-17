package llm

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"007-super-agent/internal/ledger"
	"007-super-agent/internal/registry"
	"007-super-agent/internal/triage"
)

// stubServer 是本地桩 API：按 handler 给定的 usage 与 content 回包，
// 从不真调外部 API。tests 里不放任何真实密钥，只用 "test-key" 桩值。
func stubServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("unexpected Authorization header")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func chatOKBody(content string, hit, miss, out int64) string {
	return fmt.Sprintf(`{"choices":[{"message":{"content":%s}}],"usage":{"prompt_tokens":%d,"prompt_cache_hit_tokens":%d,"prompt_cache_miss_tokens":%d,"completion_tokens":%d}}`,
		mustJSON(content), hit+miss, hit, miss, out)
}

func mustJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// TestZoneOrder：请求体按 A → A2 → B → C 拼，冻结前缀最前，
// 历史只追加不改写，尾部最后。顺序错了缓存前缀就断了。
func TestZoneOrder(t *testing.T) {
	msgs := BuildMessages(Zones{
		Frozen:    "A-宪法摘要+L1",
		Carryover: "A2-换页摘要",
		History: []Message{
			{Role: "user", Content: "B-旧问"},
			{Role: "assistant", Content: "B-旧答"},
		},
		Tail: "C-本次原话",
	})
	want := []string{"A-宪法摘要+L1", "换页摘要（上轮会话压缩，200K 换页链）：\nA2-换页摘要", "B-旧问", "B-旧答", "C-本次原话"}
	if len(msgs) != len(want) {
		t.Fatalf("got %d messages, want %d: %+v", len(msgs), len(want), msgs)
	}
	for i, w := range want {
		if msgs[i].Content != w {
			t.Errorf("msgs[%d] = %q, want %q", i, msgs[i].Content, w)
		}
	}
	if msgs[0].Role != "system" || msgs[len(msgs)-1].Role != "user" {
		t.Errorf("roles wrong: first=%s last=%s", msgs[0].Role, msgs[len(msgs)-1].Role)
	}
	// 空区不占位：只有 Tail 时只剩一条 user 消息。
	only := BuildMessages(Zones{Tail: "hi"})
	if len(only) != 1 || only[0].Role != "user" {
		t.Errorf("tail-only = %+v, want 1 user message", only)
	}
}

// TestCostMilli：缓存命中 1/50 价。100 万命中 token = 20 毫分（¥0.02），
// 100 万未命中 = 1000 毫分（¥1），100 万输出 = 4000 毫分（¥4）；高峰 2 倍。
func TestCostMilli(t *testing.T) {
	if got := CostMilli(1000000, 0, 0, false); got != 20 {
		t.Errorf("hit 1M = %d milli, want 20", got)
	}
	if got := CostMilli(0, 1000000, 0, false); got != 1000 {
		t.Errorf("miss 1M = %d milli, want 1000", got)
	}
	if got := CostMilli(0, 0, 1000000, false); got != 4000 {
		t.Errorf("out 1M = %d milli, want 4000", got)
	}
	if got := CostMilli(1000000, 0, 0, true); got != 40 {
		t.Errorf("hit 1M peak = %d milli, want 40", got)
	}
	// 混合：50 万命中 + 50 万未命中 + 10 万输出 = 10+500+400 = 910 毫分。
	if got := CostMilli(500000, 500000, 100000, false); got != 910 {
		t.Errorf("mixed = %d milli, want 910", got)
	}
}

// TestChatRecordsExpense：桩服务器回 usage（含命中/未命中），
// Chat 成功且费用按官方价如实记进 expense_ledger（毫分整数）。
func TestChatRecordsExpense(t *testing.T) {
	srv := stubServer(t, http.StatusOK, chatOKBody("hello", 800000, 200000, 50000))
	defer srv.Close()
	db := t.TempDir() + "/ledger.db"
	led, err := ledger.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close(led)
	c := New("deepseek-flash", "test-key")
	c.BaseURL = srv.URL
	c.Ledger = led
	c.SessionID = "s1"
	c.TaskID = "t1"
	reply, u, err := c.Chat(t.Context(), []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if reply != "hello" {
		t.Errorf("reply = %q, want hello", reply)
	}
	if u.CacheHitTokens != 800000 || u.CacheMissTokens != 200000 || u.CompletionTokens != 50000 {
		t.Errorf("usage = %+v", u)
	}
	// 费用：800000*20 + 200000*1000 + 50000*4000 = 16+200+200 百万毫分
	// = 416,000,000 / 1,000,000 = 416 毫分。
	sum, err := led.SummarizeExpense("s1")
	if err != nil {
		t.Fatal(err)
	}
	if sum.Calls != 1 || sum.CostCNYMilli != 416 {
		t.Errorf("expense summary = %+v, want calls=1 cost=416", sum)
	}
	if sum.CacheHitTokens != 800000 || sum.CacheMissTokens != 200000 {
		t.Errorf("expense tokens = %+v", sum)
	}
}

// TestChatBadResponse：非法 JSON 回复报错，不伪造回答；
// 非 200 也报错，且错误里不含密钥。
func TestChatBadResponse(t *testing.T) {
	srv := stubServer(t, http.StatusOK, `this is not json`)
	defer srv.Close()
	c := New("deepseek-flash", "test-key")
	c.BaseURL = srv.URL
	if _, _, err := c.Chat(t.Context(), []Message{{Role: "user", Content: "hi"}}); err == nil {
		t.Error("non-JSON response should error")
	}
	srv2 := stubServer(t, http.StatusUnauthorized, `{"error":"bad key"}`)
	defer srv2.Close()
	c.BaseURL = srv2.URL
	_, _, err := c.Chat(t.Context(), []Message{{Role: "user", Content: "hi"}})
	if err == nil {
		t.Error("401 should error")
	}
	if err != nil && strings.Contains(err.Error(), "test-key") {
		t.Error("error must not contain API key")
	}
	// 无密钥时直接报错，不发请求。
	c2 := New("deepseek-flash", "")
	if _, _, err := c2.Chat(t.Context(), []Message{{Role: "user", Content: "hi"}}); err == nil {
		t.Error("empty API key should error")
	}
}

// TestTriageBadJSON：模型回了非法 JSON，分诊解析必须报错，
// 不猜 route、不编 tool_id。
func TestTriageBadJSON(t *testing.T) {
	srv := stubServer(t, http.StatusOK, chatOKBody("我不是 JSON", 0, 100, 10))
	defer srv.Close()
	c := New("deepseek-flash", "test-key")
	c.BaseURL = srv.URL
	reply, _, _, err := c.TriageCall("用户原话：你好")
	if err != nil {
		t.Fatalf("TriageCall: %v", err)
	}
	reg := loadTestRegistry(t)
	if _, err := triage.ParseTriageReply(reply, reg); err == nil {
		t.Error("ParseTriageReply should reject non-JSON")
	}
	// 未知 tool_id 也报错。
	if _, err := triage.ParseTriageReply(`{"route":"TOOL","tool_id":"cap-does-not-exist","reason":"x"}`, reg); err == nil {
		t.Error("ParseTriageReply should reject unknown tool_id")
	}
}

// loadTestRegistry 用第 1 步的真实总账（只读），供分诊校验用。
func loadTestRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	reg, err := registry.Load("/home/hatch/workspace/super-agent-007/step1")
	if err != nil {
		// 测试机上第 1 步输出在部署目录的数据区。
		reg, err = registry.Load("/root/super-agent-007/data")
	}
	if err != nil {
		t.Skipf("no step1 registry available: %v", err)
	}
	return reg
}

// TestValidateArgs：参数含 shell 元字符一律拒绝；
// 干净参数与空参数列表放行。
func TestValidateArgs(t *testing.T) {
	bad := []string{
		"a;b", "a&b", "a|b", "a`b`", "a$b", "a(b)", "a\nb", "a\rb", "",
	}
	for _, a := range bad {
		if err := ValidateArgs([]string{a}); err == nil {
			t.Errorf("ValidateArgs(%q) should reject", a)
		}
	}
	if err := ValidateArgs([]string{"--limit", "10", "plain-text_1.txt"}); err != nil {
		t.Errorf("ValidateArgs clean args: %v", err)
	}
	if err := ValidateArgs(nil); err != nil {
		t.Errorf("ValidateArgs nil: %v", err)
	}
}

// TestBuildArgv：entry_point + 参数拼命令；非法参数不拼、不执行；
// 不可用工具与不存在的工具都报错。
func TestBuildArgv(t *testing.T) {
	reg := loadTestRegistry(t)
	argv, err := BuildArgv(reg, "cap-code-semgrep", []string{"--limit", "5"})
	if err != nil {
		t.Fatalf("BuildArgv: %v", err)
	}
	if len(argv) < 1 || argv[len(argv)-2] != "--limit" || argv[len(argv)-1] != "5" {
		t.Errorf("argv = %v", argv)
	}
	if _, err := BuildArgv(reg, "cap-code-semgrep", []string{"a;b"}); err == nil {
		t.Error("shell meta arg should be rejected before exec")
	}
	if _, err := BuildArgv(reg, "cap-memory-playbooks", nil); err == nil {
		t.Error("available:false tool should be rejected")
	}
	if _, err := BuildArgv(reg, "cap-does-not-exist", nil); err == nil {
		t.Error("unknown tool should be rejected")
	}
}

// TestResolveAPIKey：环境变量优先找到密钥；值只返回，不写日志。
func TestResolveAPIKey(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "test-key")
	if k, ok := ResolveAPIKey(); !ok || k != "test-key" {
		t.Errorf("ResolveAPIKey = (%q, %v), want (test-key, true)", k, ok)
	}
}
