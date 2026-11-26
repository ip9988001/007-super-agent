package llm

import (
	"database/sql"
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
	reply, _, _, err := c.TriageCall("t1", "你是分诊器。", "用户原话：你好")
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

// TestValidateArgs：只拦空串与 NUL 字节；shell 元字符是普通数据
// （argv 不经过 /bin/sh），正则、URL 带 &、管道等一律放行（B-04）。
func TestValidateArgs(t *testing.T) {
	bad := []string{"", "a\x00b"}
	for _, a := range bad {
		if err := ValidateArgs([]string{a}); err == nil {
			t.Errorf("ValidateArgs(%q) should reject", a)
		}
	}
	// 曾经被误杀的合法参数：正则里的 |()、URL 里的 &、分号等。
	ok := []string{
		`(?<=src=")[^"]+\.js[^"]*|(?<=href=")[^"]+\.css[^"]*`,
		"https://example.com/?a=1&b=2",
		"a;b", "a|b", "a`b`", "a$b", "a(b)", "a\nb", "a\rb",
		"--limit", "10", "plain-text_1.txt",
	}
	if err := ValidateArgs(ok); err != nil {
		t.Errorf("ValidateArgs legit args should pass: %v", err)
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
	if _, err := BuildArgv(reg, "cap-code-semgrep", []string{"a\x00b"}); err == nil {
		t.Error("NUL arg should be rejected before exec")
	}
	// shell 元字符是普通数据（argv 不经过 /bin/sh），应放行（B-04）。
	if _, err := BuildArgv(reg, "cap-code-semgrep", []string{"a;b", `x|y(z)`}); err != nil {
		t.Errorf("shell meta args should pass: %v", err)
	}
	// 模型照着 L3 整行命令抄、把工具名也写进 inputs 时，首参数与
	// 命令名重复的去掉一个（第 5 轮：chrome-nosandbox 被当 URL 打开）。
	argv, err = BuildArgv(reg, "cap-code-semgrep", []string{"semgrep", "--limit", "5"})
	if err != nil {
		t.Fatalf("BuildArgv dup: %v", err)
	}
	count := 0
	for _, a := range argv {
		if a == "semgrep" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("argv = %v, want exactly one 'semgrep'", argv)
	}
	if len(argv) < 1 || argv[len(argv)-2] != "--limit" || argv[len(argv)-1] != "5" {
		t.Errorf("argv after dedup = %v", argv)
	}
	if _, err := BuildArgv(reg, "cap-memory-playbooks", nil); err == nil {
		t.Error("available:false tool should be rejected")
	}
	if _, err := BuildArgv(reg, "cap-does-not-exist", nil); err == nil {
		t.Error("unknown tool should be rejected")
	}
}

// TestCostMilliForModel：监工模型 deepseek-reasoner 是已退役旧别名，
// 按官方定价页重定向到 V4.1 Flash、按 Flash 价计费；V4 Pro 另有价表。
func TestCostMilliForModel(t *testing.T) {
	// reasoner 与 flash 同价：100 万命中 = 20 毫分。
	if got := CostMilliForModel("deepseek-reasoner", 1000000, 0, 0, false); got != 20 {
		t.Errorf("reasoner hit 1M = %d milli, want 20 (flash price)", got)
	}
	if got := CostMilliForModel("deepseek-chat", 0, 1000000, 0, false); got != 1000 {
		t.Errorf("chat miss 1M = %d milli, want 1000 (flash price)", got)
	}
	// V4 Pro：100 万命中 = 147 毫分，未命中 = 4402，输出 = 13207。
	if got := CostMilliForModel("deepseek-v4-pro", 1000000, 0, 0, false); got != 147 {
		t.Errorf("pro hit 1M = %d milli, want 147", got)
	}
	if got := CostMilliForModel("deepseek-v4-pro", 0, 1000000, 0, false); got != 4402 {
		t.Errorf("pro miss 1M = %d milli, want 4402", got)
	}
	if got := CostMilliForModel("deepseek-v4-pro", 0, 0, 1000000, false); got != 13207 {
		t.Errorf("pro out 1M = %d milli, want 13207", got)
	}
	if got := CostMilliForModel("deepseek-v4-pro", 1000000, 0, 0, true); got != 294 {
		t.Errorf("pro hit 1M peak = %d milli, want 294", got)
	}
}

// TestTriageCallZonesAndTask：TriageCall 把 frozen 放第一条 system 消息
// （缓存分区 A 区）、tail 放最后一条 user 消息（C 区）；费用行按传入
// 的 taskID 归属，不按 c.TaskID。
func TestTriageCallZonesAndTask(t *testing.T) {
	var gotMsgs []Message
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotMsgs = req.Messages
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatOKBody(`{"route":"DIRECT","tool_id":"","reason":"x"}`, 100, 10, 5)))
	}))
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
	c.TaskID = "stale-task-id" // 故意放一个旧值：费用行不许用它
	if _, _, _, err := c.TriageCall("t-real", "FROZEN-HEAD", "用户原话：你好"); err != nil {
		t.Fatalf("TriageCall: %v", err)
	}
	if len(gotMsgs) != 2 || gotMsgs[0].Role != "system" || gotMsgs[0].Content != "FROZEN-HEAD" ||
		gotMsgs[1].Role != "user" || gotMsgs[1].Content != "用户原话：你好" {
		t.Fatalf("messages = %+v, want [system FROZEN-HEAD, user tail]", gotMsgs)
	}
	sum, err := led.SummarizeExpense("s1")
	if err != nil {
		t.Fatal(err)
	}
	if sum.Calls != 1 {
		t.Fatalf("expense calls = %d, want 1", sum.Calls)
	}
	// 费用行 task_id 必须是 t-real，不是 stale-task-id。
	rows := queryExpenseTaskIDs(t, db)
	if len(rows) != 1 || rows[0] != "t-real" {
		t.Errorf("expense task_ids = %v, want [t-real]", rows)
	}
}

// queryExpenseTaskIDs 读 expense_ledger 全部 task_id（账本包没有读方法）。
func queryExpenseTaskIDs(t *testing.T, dbPath string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT task_id FROM expense_ledger ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

// TestResolveAPIKey：环境变量优先找到密钥；值只返回，不写日志。
func TestResolveAPIKey(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "test-key")
	if k, ok := ResolveAPIKey(); !ok || k != "test-key" {
		t.Errorf("ResolveAPIKey = (%q, %v), want (test-key, true)", k, ok)
	}
}

// 第 20 步：ReviewSelfEvolve 调一次 worker 模型做自查点评，
// 费用按 selfevolve-<日期> 任务号记账；信号为空时不调模型。
func TestReviewSelfEvolve(t *testing.T) {
	want := `{"lessons":["探针失败要先看路径"],"proposals":[]}`
	srv := stubServer(t, http.StatusOK, chatOKBody(want, 1000, 500, 200))
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

	reply, err := c.ReviewSelfEvolve(t.Context(), "selfevolve-2026-11-19", "exec_fail:cap-code-run-audit: 3\n")
	if err != nil {
		t.Fatalf("ReviewSelfEvolve: %v", err)
	}
	if reply != want {
		t.Errorf("reply = %q, want %q", reply, want)
	}
	// 费用行按 selfevolve-<日期> 归属：1000 命中 + 500 未命中 + 200 输出
	// = (1000*20 + 500*1000 + 200*4000) / 1e6 = 1.32 → 四舍五入 1 毫分。
	rows, err := led.ListExpenseByTask("selfevolve-2026-11-19", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].CostCNYMilli != 1 || rows[0].Model != "deepseek-flash" {
		t.Errorf("expense rows = %+v, want 1 row cost=1 model=deepseek-flash", rows)
	}

	// 信号为空：不调模型，直接报错。
	if _, err := c.ReviewSelfEvolve(t.Context(), "selfevolve-2026-11-19", "  "); err == nil {
		t.Error("ReviewSelfEvolve empty signals: want error, got nil")
	}
}

// TestCallTaskFullRoleRecordsRole（规划 v2）：按指定角色记账——
// CallTaskFullRole 的费用行带 role；原客户端的 Role 不变（浅拷贝，
// 并发任务各用各的拷贝，不串角色）。
func TestCallTaskFullRoleRecordsRole(t *testing.T) {
	srv := stubServer(t, http.StatusOK, chatOKBody("hello", 1000, 500, 200))
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
	if c.Role != "" {
		t.Fatalf("新建客户端 Role 应为空，得到 %q", c.Role)
	}
	if _, _, _, _, err := c.CallTaskFullRole("t1", "frozen", "tail", nil, nil, ledger.ExpenseRoleFrontJudge); err != nil {
		t.Fatalf("CallTaskFullRole: %v", err)
	}
	// 原客户端的 Role 不变（withRole 是浅拷贝）。
	if c.Role != "" {
		t.Fatalf("CallTaskFullRole 后原客户端 Role 应仍为空，得到 %q", c.Role)
	}
	rows, err := led.ListExpenseByTask("t1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("费用行应为 1 行，得到 %d", len(rows))
	}
	if rows[0].Role != ledger.ExpenseRoleFrontJudge {
		t.Fatalf("费用行 role=%q, want %q", rows[0].Role, ledger.ExpenseRoleFrontJudge)
	}
	// 再调一次 front_plan：两行各带各的 role，互不串。
	if _, _, _, _, err := c.CallTaskFullRole("t1", "frozen", "tail", nil, nil, ledger.ExpenseRoleFrontPlan); err != nil {
		t.Fatalf("CallTaskFullRole front_plan: %v", err)
	}
	if cost, err := led.TaskCostByRoles("t1", ledger.ExpenseRoleFrontJudge); err != nil || cost != rows[0].CostCNYMilli {
		t.Fatalf("front_judge 费用=%d, want %d, err=%v", cost, rows[0].CostCNYMilli, err)
	}
	if cost, err := led.TaskCostByRoles("t1", ledger.ExpenseRoleFrontPlan, ledger.ExpenseRoleFrontJudge); err != nil || cost != 2*rows[0].CostCNYMilli {
		t.Fatalf("front_plan+front_judge 费用=%d, want %d, err=%v", cost, 2*rows[0].CostCNYMilli, err)
	}
}

// TestMessageEmptyContentSerialized：空正文消息序列化一定带 "content":""。
// 对齐二号三号回归（cf-register-gonglue-010）：判卷自查时模型回一条
// 只有工具调用、没有正文的 assistant 消息，Content="" 被 omitempty 省掉，
// DeepSeek 整单报 422（messages[N] missing field `content`），
// 前置连着 3 次判不了、烧光 ¥0.05 预算。Content 永不省略。
func TestMessageEmptyContentSerialized(t *testing.T) {
	// assistant 消息：只有工具调用，正文为空。
	m := Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "call-1", Type: "function"}}}
	m.ToolCalls[0].Function.Name = "read"
	m.ToolCalls[0].Function.Arguments = "{}"
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"content":""`) {
		t.Fatalf("空正文消息应带 \"content\":\"\"，实际 %s", string(b))
	}
	// 整单请求体里也一样：user + assistant（空正文带工具调用）+ tool。
	req := chatRequest{Model: "deepseek-flash", Messages: []Message{
		{Role: "user", Content: "判这步"},
		m,
		{Role: "tool", Content: "文件内容", ToolCallID: "call-1"},
	}}
	rb, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rb), `"content":""`) {
		t.Fatalf("请求体里空正文消息应带 \"content\":\"\"，实际 %s", string(rb))
	}
	// 反序列化回来的空正文仍是 ""（不丢字段）。
	var back chatRequest
	if err := json.Unmarshal(rb, &back); err != nil {
		t.Fatal(err)
	}
	if back.Messages[1].Content != "" || len(back.Messages[1].ToolCalls) != 1 {
		t.Fatalf("反序列化后 assistant 消息不对：%+v", back.Messages[1])
	}
}

// TestToolMessageEmptyContent（M-4 回归）：tool 角色消息内容为空时
// 保底填 "(empty output)"，不发空 content（部分网关会拒收）。
func TestToolMessageEmptyContent(t *testing.T) {
	m := ToolMessage("call-1", "")
	if m.Role != "tool" || m.ToolCallID != "call-1" || m.Content != "(empty output)" {
		t.Fatalf("空内容应保底填 (empty output)，得到 %+v", m)
	}
	m2 := ToolMessage("call-2", "  ")
	if m2.Content != "(empty output)" {
		t.Fatalf("空白内容应保底填 (empty output)，得到 %+v", m2)
	}
	m3 := ToolMessage("call-3", "文件内容")
	if m3.Content != "文件内容" {
		t.Fatalf("非空内容应原样保留，得到 %+v", m3)
	}
	// 保底后的消息序列化一定带 content 字段。
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"content":"(empty output)"`) {
		t.Fatalf("序列化应带 content，实际 %s", string(b))
	}
}

// TestChatEmptyArgumentsNormalized（M-4 回归）：模型回的 tool_call
// 若 Arguments 为空串 ""（不是 "{}"），统一保底填 "{}"，
// 否则 DeepSeek 报 422，调用方解析也报错。
func TestChatEmptyArgumentsNormalized(t *testing.T) {
	body := `{"choices":[{"message":{"content":"","tool_calls":[{"id":"call-1","type":"function","function":{"name":"read","arguments":""}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`
	srv := stubServer(t, http.StatusOK, body)
	defer srv.Close()
	c := New("deepseek-flash", "test-key")
	c.BaseURL = srv.URL
	c.TaskID = "t1"
	m, fr, _, err := c.chatWithTaskMetaTools(t.Context(), []Message{{Role: "user", Content: "判这步"}}, "t1", nil)
	if err != nil {
		t.Fatalf("chatWithTaskMetaTools: %v", err)
	}
	if fr != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", fr)
	}
	if len(m.ToolCalls) != 1 {
		t.Fatalf("应有 1 次工具调用，得到 %+v", m.ToolCalls)
	}
	if m.ToolCalls[0].Function.Arguments != "{}" {
		t.Fatalf("空 Arguments 应保底填 {}，得到 %q", m.ToolCalls[0].Function.Arguments)
	}
}
