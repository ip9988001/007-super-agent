package summary

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"007-super-agent/internal/llm"
)

// fakeChat 按顺序返回预设回复（每调一次取一条），供生成器单测用。
type fakeChat struct {
	replies []string
	errs    []error
	calls   int
	models  []string
}

func (f *fakeChat) ChatTask(ctx context.Context, msgs []llm.Message, taskID string) (string, llm.Usage, error) {
	i := f.calls
	f.calls++
	f.models = append(f.models, taskID)
	if i < len(f.errs) && f.errs[i] != nil {
		return "", llm.Usage{}, f.errs[i]
	}
	if i < len(f.replies) {
		return f.replies[i], llm.Usage{PromptTokens: 1000, CompletionTokens: 100}, nil
	}
	return "", llm.Usage{}, errors.New("fake chat: no more replies")
}

const pilotSource = "# Cloudflare 新账号注册\n\n用 Camoufox 打开 Cloudflare 注册页，过邮箱验证后进后台取 Global API Key。\n" +
	"报错时看 Turnstile 按钮文字是不是 View，换个网络重试。\n" +
	"Cloudflare Cloudflare 邮箱验证 邮箱验证\n"

const goodReply = "summary: 用 Camoufox 浏览器注册 Cloudflare 新账号，先过邮箱验证，再进后台取回全局密钥，遇到验证框卡住就换个干净网络重试。\n" +
	"triggers: 新号怎么注册，CF 过盾，邮箱收不到验证码，Global API Key，Turnstile 点不动\n"

func TestGenerateSuccessFirstTry(t *testing.T) {
	flash := &fakeChat{replies: []string{goodReply}}
	g := &Generator{Flash: flash, FlashModel: "deepseek-flash", Collision: NewCollisionChecker(nil)}
	doc := PilotDoc{DocID: "W-0001", SourcePath: "wiki/entities/cf-test.md", Title: "Cloudflare 新账号注册", Content: []byte(pilotSource)}
	s, rep, err := g.Generate(context.Background(), doc)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if rep.Status != StatusOK {
		t.Fatalf("status = %s, want ok; checks: %+v", rep.Status, rep.FinalChecks)
	}
	if rep.Attempts != 1 || rep.RetryCount != 0 {
		t.Fatalf("attempts=%d retry=%d, want 1/0", rep.Attempts, rep.RetryCount)
	}
	if s.SummaryHash == "" || s.SourceHash == "" {
		t.Fatalf("hashes missing")
	}
	if len(s.Core) != 5 {
		t.Fatalf("core triggers = %d, want 5", len(s.Core))
	}
	// 费用：prompt 1000（全按未命中）+ 输出 100，按 Flash 价：
	// (1000*1000 + 100*4000)/1e6 = 1.4 毫分，四舍五入 1 毫分。
	if rep.CostMilli != 1 {
		t.Fatalf("cost = %d milli, want 1", rep.CostMilli)
	}
}

func TestGenerateRetryThenStrongThenFallback(t *testing.T) {
	// 三轮都回不合格简介（只有背景、无动作词、无实体），最终熔断降级。
	badReply := "summary: 这是一段只有背景的描述，没有讲到任何用途。\n" +
		"triggers: 系统，配置，记录\n"
	flash := &fakeChat{replies: []string{badReply, badReply}}
	strong := &fakeChat{replies: []string{badReply}}
	g := &Generator{
		Flash: flash, FlashModel: "deepseek-flash",
		Strong: strong, StrongModel: "deepseek-v4-pro", StrongAvailable: true,
		Collision: NewCollisionChecker(nil),
	}
	doc := PilotDoc{DocID: "W-0002", SourcePath: "wiki/entities/cf-test2.md", Title: "Cloudflare 新账号注册", Content: []byte(pilotSource)}
	s, rep, err := g.Generate(context.Background(), doc)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if rep.Status != StatusDegradedOK {
		t.Fatalf("status = %s, want degraded_ok", rep.Status)
	}
	if rep.Attempts != 3 || rep.RetryCount != 3 {
		t.Fatalf("attempts=%d retry=%d, want 3/3", rep.Attempts, rep.RetryCount)
	}
	if !rep.StrongUsed {
		t.Fatalf("strong model should have been used on the last attempt")
	}
	if len(rep.AttemptChecks) != 3 {
		t.Fatalf("attempt checks = %d, want 3", len(rep.AttemptChecks))
	}
	if s.Summary == "" || len(s.Triggers()) < 3 {
		t.Fatalf("fallback summary incomplete: %q %v", s.Summary, s.Triggers())
	}
}

func TestGenerateNoStrongModelFallsBackToFlash(t *testing.T) {
	badReply := "summary: 这是一段只有背景的描述，没有讲到任何用途。\n" +
		"triggers: 系统，配置，记录\n"
	flash := &fakeChat{replies: []string{badReply, badReply, badReply}}
	g := &Generator{Flash: flash, FlashModel: "deepseek-flash", StrongAvailable: false, Collision: NewCollisionChecker(nil)}
	doc := PilotDoc{DocID: "W-0003", SourcePath: "wiki/entities/cf-test3.md", Title: "Cloudflare 新账号注册", Content: []byte(pilotSource)}
	_, rep, err := g.Generate(context.Background(), doc)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if rep.StrongUsed {
		t.Fatalf("strong model must not be used when unavailable")
	}
	if rep.Status != StatusDegradedOK {
		t.Fatalf("status = %s, want degraded_ok", rep.Status)
	}
}

func TestCostMilliForUsage(t *testing.T) {
	// 缓存分项缺失时 prompt 全按未命中价。
	u := llm.Usage{PromptTokens: 2000, CompletionTokens: 200}
	if got := CostMilliForUsage("deepseek-flash", u); got != 3 { // (2000*1000+200*4000)/1e6=2.8→3
		t.Fatalf("cost = %d, want 3", got)
	}
	// 有缓存分项时按分项算：命中 1000×20 + 未命中 1000×1000 + 输出 100×4000 = 1.42→1。
	u2 := llm.Usage{PromptTokens: 2000, CacheHitTokens: 1000, CacheMissTokens: 1000, CompletionTokens: 100}
	if got := CostMilliForUsage("deepseek-flash", u2); got != 1 {
		t.Fatalf("cost = %d, want 1", got)
	}
}

// ---- 规则⑦ ----

type fakeEmbedder struct {
	vecs map[string][]float32
	err  error
}

func (f *fakeEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if f.err != nil {
		return nil, f.err
	}
	if v, ok := f.vecs[text]; ok {
		return v, nil
	}
	// 默认返回与文本无关的正交方向（长度 3 的单位向量变体）。
	h := 0
	for _, r := range text {
		h += int(r)
	}
	switch h % 3 {
	case 0:
		return []float32{1, 0, 0}, nil
	case 1:
		return []float32{0, 1, 0}, nil
	default:
		return []float32{0, 0, 1}, nil
	}
}

func TestCollisionPassAndFail(t *testing.T) {
	ctx := context.Background()
	cc := NewCollisionChecker(&fakeEmbedder{vecs: map[string][]float32{
		"简介甲": {1, 0, 0},
		"简介乙": {0.99, 0.14, 0}, // 与甲余弦 ≈0.99 > 0.88
		"简介丙": {0, 1, 0},
	}})
	if err := cc.Accept(ctx, "wiki/entities", "W-0001", "简介甲"); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	// 同目录撞车：打回，原因须附撞车简介。
	if r := cc.CheckCollision(ctx, "wiki/entities", "W-0002", "简介乙"); r.Pass || !strings.Contains(r.Reason, "简介甲") {
		t.Fatalf("collision should fail with collided summary in reason: %+v", r)
	}
	// 不同目录不查重：通过。
	if r := cc.CheckCollision(ctx, "wiki/concepts", "W-0003", "简介乙"); !r.Pass {
		t.Fatalf("other dir should pass: %s", r.Reason)
	}
	// 正交向量：通过。
	if r := cc.CheckCollision(ctx, "wiki/entities", "W-0004", "简介丙"); !r.Pass {
		t.Fatalf("orthogonal summary should pass: %s", r.Reason)
	}
}

func TestCollisionSkippedOnEmbedError(t *testing.T) {
	cc := NewCollisionChecker(&fakeEmbedder{err: fmt.Errorf("ollama down")})
	r := cc.CheckCollision(context.Background(), "wiki/entities", "W-0001", "任意简介")
	if !r.Skipped {
		t.Fatalf("embed error should skip rule 7, got %+v", r)
	}
	cc2 := NewCollisionChecker(nil)
	r2 := cc2.CheckCollision(context.Background(), "wiki/entities", "W-0001", "任意简介")
	if !r2.Skipped {
		t.Fatalf("nil embedder should skip rule 7, got %+v", r2)
	}
}

func TestCosineSimilarity(t *testing.T) {
	if got := CosineSimilarity([]float32{1, 0}, []float32{1, 0}); got != 1 {
		t.Fatalf("identical = %v, want 1", got)
	}
	if got := CosineSimilarity([]float32{1, 0}, []float32{0, 1}); got != 0 {
		t.Fatalf("orthogonal = %v, want 0", got)
	}
	if got := CosineSimilarity([]float32{1}, []float32{1, 2}); got != 0 {
		t.Fatalf("length mismatch should be 0, got %v", got)
	}
}
