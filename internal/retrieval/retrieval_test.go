package retrieval

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// mockTransport 是测试用的 HTTP 桩：按 URL 模式返回预设响应，
// 并记录所有请求（断言绝不带 Authorization 头）。
type mockTransport struct {
	handler  func(req *http.Request) (*http.Response, error)
	requests []*http.Request
}

func (m *mockTransport) Do(req *http.Request) (*http.Response, error) {
	m.requests = append(m.requests, req)
	return m.handler(req)
}

func respJSON(status int, body string, headers map[string]string) *http.Response {
	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	return &http.Response{
		StatusCode: status,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func rateHeaders(resource string, remaining int, resetUnix int64) map[string]string {
	return map[string]string{
		"X-RateLimit-Resource":  resource,
		"X-RateLimit-Remaining": fmt.Sprintf("%d", remaining),
		"X-RateLimit-Reset":     fmt.Sprintf("%d", resetUnix),
	}
}

func newTestClient(mt *mockTransport) *Client {
	c := New(mt, "")
	c.Sleep = func(time.Duration) {} // 测试里不真等
	c.Now = func() time.Time { return time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC) }
	return c
}

func assertNoAuth(t *testing.T, mt *mockTransport) {
	t.Helper()
	for _, req := range mt.requests {
		if req.Header.Get("Authorization") != "" {
			t.Fatalf("请求 %s 带了 Authorization 头（铁律：检索器绝不带 Token）", req.URL)
		}
	}
}

const searchBody = `{"total_count":2,"items":[
{"full_name":"acme/md2html","description":"Markdown to HTML CLI","stargazers_count":1200,"archived":false,"pushed_at":"2026-10-01T00:00:00Z","default_branch":"main","html_url":"https://github.com/acme/md2html","license":{"spdx_id":"MIT"}},
{"full_name":"old/stale-tool","description":"old tool","stargazers_count":30,"archived":true,"pushed_at":"2020-01-01T00:00:00Z","default_branch":"main","html_url":"https://github.com/old/stale-tool","license":null}
]}`

func detailsBody(fullName string, lic string, archived bool, pushed string) string {
	return fmt.Sprintf(`{"full_name":%q,"archived":%v,"stargazers_count":800,"pushed_at":%q,"default_branch":"main","description":"d","html_url":"https://github.com/%s","license":{"spdx_id":%q}}`,
		fullName, archived, pushed, fullName, lic)
}

// githubMock 是“顺风局”桩：搜索出 1 个合格候选 + 1 个 Archived 的；
// 深挖的三次核心调用与 raw 直读都返回正常内容。
func githubMock(t *testing.T) *mockTransport {
	reset := time.Date(2026, 11, 19, 11, 0, 0, 0, time.UTC).Unix()
	return &mockTransport{handler: func(req *http.Request) (*http.Response, error) {
		u := req.URL.String()
		switch {
		case strings.Contains(u, "/search/repositories"):
			return respJSON(200, searchBody, rateHeaders("search", 50, reset)), nil
		case strings.Contains(u, "/repos/acme/md2html/commits"):
			return respJSON(200, `[{"commit":{"committer":{"date":"2026-10-01T00:00:00Z"}}}]`, rateHeaders("core", 55, reset)), nil
		case strings.Contains(u, "/repos/acme/md2html/releases/latest"):
			return respJSON(200, `{"tag_name":"v1.2.0","published_at":"2026-09-01T00:00:00Z","html_url":"https://github.com/acme/md2html/releases/v1.2.0"}`, rateHeaders("core", 54, reset)), nil
		case strings.Contains(u, "/repos/acme/md2html"):
			return respJSON(200, detailsBody("acme/md2html", "MIT", false, "2026-10-01T00:00:00Z"), rateHeaders("core", 56, reset)), nil
		case strings.Contains(u, "raw.githubusercontent.com/acme/md2html"):
			// raw 响应头根本没有 X-RateLimit 字段（实测结论）。
			if strings.HasSuffix(u, "README.md") {
				return respJSON(200, "# md2html\nUsage: md2html in.md out.html\n", nil), nil
			}
			if strings.HasSuffix(u, "LICENSE") {
				return respJSON(200, "MIT License", nil), nil
			}
			if strings.HasSuffix(u, "package.json") {
				return respJSON(200, `{"name":"md2html","bin":{"md2html":"./cli.js"}}`, nil), nil
			}
			return respJSON(404, "404: Not Found", nil), nil
		default:
			t.Fatalf("桩收到未预期的请求: %s", u)
			return nil, fmt.Errorf("unexpected %s", u)
		}
	}}
}

func TestNoAuthHeader(t *testing.T) {
	mt := githubMock(t)
	c := newTestClient(mt)
	_, err := c.Retrieve(t.Context(), GapInput{GapID: "gap-2026-11-19-001", TaskText: "markdown to html cli", Keywords: []string{"markdown", "html", "cli"}})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	assertNoAuth(t, mt)
}

func TestRetrieveFairWind(t *testing.T) {
	mt := githubMock(t)
	c := newTestClient(mt)
	tr, err := c.Retrieve(t.Context(), GapInput{GapID: "gap-2026-11-19-001", TaskText: "markdown to html cli", Keywords: []string{"markdown", "html", "cli"}})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if tr.Outcome != "recommend" {
		t.Fatalf("outcome=%q, want recommend (trace=%+v)", tr.Outcome, tr)
	}
	if tr.Recommended != "acme/md2html" {
		t.Fatalf("recommended=%q, want acme/md2html", tr.Recommended)
	}
	if tr.SearchesUsed != 1 {
		t.Fatalf("searches=%d, want 1", tr.SearchesUsed)
	}
	if tr.CoreUsed != 3 {
		t.Fatalf("core=%d, want 3", tr.CoreUsed)
	}
	if len(tr.Candidates) != 1 || !tr.Candidates[0].Passed {
		t.Fatalf("candidates=%+v, want 1 passing", tr.Candidates)
	}
	if tr.Candidates[0].DepsFile != "package.json" || tr.Candidates[0].RuntimeNeed != "node" {
		t.Fatalf("deps=%q runtime=%q", tr.Candidates[0].DepsFile, tr.Candidates[0].RuntimeNeed)
	}
}

func TestLayer0HitNoExternalCalls(t *testing.T) {
	mt := githubMock(t)
	c := newTestClient(mt)
	tr, err := c.Retrieve(t.Context(), GapInput{
		GapID: "gap-2026-11-19-002", TaskText: "markdown to html cli",
		LocalCheck: func(context.Context, string) (bool, string, error) {
			return true, "已有工具 cap-fetch-md2html", nil
		},
	})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if tr.Outcome != "layer0" || !tr.Layer0Hit {
		t.Fatalf("outcome=%q layer0=%v", tr.Outcome, tr.Layer0Hit)
	}
	if len(mt.requests) != 0 {
		t.Fatalf("第零层命中后仍发了 %d 次外部请求", len(mt.requests))
	}
}

func TestRetrieveExhaustedAfterThreeSearches(t *testing.T) {
	reset := time.Date(2026, 11, 19, 11, 0, 0, 0, time.UTC).Unix()
	mt := &mockTransport{handler: func(req *http.Request) (*http.Response, error) {
		u := req.URL.String()
		if strings.Contains(u, "/search/repositories") {
			// 每次都只返回 Archived 的候选，粗筛全淘汰。
			return respJSON(200, `{"total_count":1,"items":[
{"full_name":"old/stale-tool","description":"old","stargazers_count":30,"archived":true,"pushed_at":"2020-01-01T00:00:00Z","default_branch":"main","html_url":"https://github.com/old/stale-tool","license":null}
]}`, rateHeaders("search", 50, reset)), nil
		}
		t.Fatalf("不应有深挖请求: %s", u)
		return nil, fmt.Errorf("unexpected")
	}}
	c := newTestClient(mt)
	tr, err := c.Retrieve(t.Context(), GapInput{GapID: "gap-2026-11-19-003", TaskText: "some obscure tool", Keywords: []string{"obscure"}})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if tr.Outcome != "exhausted" {
		t.Fatalf("outcome=%q, want exhausted", tr.Outcome)
	}
	if tr.SearchesUsed != 3 {
		t.Fatalf("searches=%d, want 3", tr.SearchesUsed)
	}
	if len(tr.Rounds) != 3 || tr.Rounds[1].Page != 2 {
		t.Fatalf("rounds=%+v, want 3 轮且第 2 轮 page=2", tr.Rounds)
	}
	// 重搜理由结构化：第二轮理由须是三选一之一。
	for _, r := range tr.Rounds[1:] {
		switch r.Reason {
		case "OFF_TOPIC", "ALL_BLOCKED_REDLINE", "PARTIAL_COVERAGE":
		default:
			t.Fatalf("第 2/3 轮理由 %q 不在结构化三选一内", r.Reason)
		}
	}
}

func TestProtectRemainingDefersWhenResetFar(t *testing.T) {
	// Remaining=5（≤10 保护线），重置在 30 分钟后（>15 分钟）→ 顺延。
	reset := time.Date(2026, 11, 19, 10, 30, 0, 0, time.UTC).Unix()
	mt := &mockTransport{handler: func(req *http.Request) (*http.Response, error) {
		return respJSON(200, searchBody, rateHeaders("search", 5, reset)), nil
	}}
	c := newTestClient(mt)
	// 先让客户端知道额度状态：调一次搜索（Remaining 5 会被记下），
	// 第二次调用前检查应直接报 ErrRateLimited（不再发请求）。
	_, err := c.SearchRepos(t.Context(), "markdown html", 1)
	if err != nil {
		t.Fatalf("第一次搜索: %v", err)
	}
	if n := len(mt.requests); n != 1 {
		t.Fatalf("requests=%d, want 1", n)
	}
	_, err = c.SearchRepos(t.Context(), "markdown html", 1)
	var rl *ErrRateLimited
	if !asErr(err, &rl) {
		t.Fatalf("第二次搜索 err=%v, want ErrRateLimited", err)
	}
	if len(mt.requests) != 1 {
		t.Fatalf("到限后又发了请求（应直接顺延不发）")
	}
}

func asErr(err error, target **ErrRateLimited) bool {
	if err == nil {
		return false
	}
	for err != nil {
		if e, ok := err.(*ErrRateLimited); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestProtectRemainingWaitsWhenResetNear(t *testing.T) {
	// Remaining=5，重置在 2 分钟后（≤15 分钟）→ 原地等（Sleep 被记录，
	// 不真等），然后继续发请求。
	var slept []time.Duration
	reset := time.Date(2026, 11, 19, 10, 2, 0, 0, time.UTC).Unix()
	mt := &mockTransport{handler: func(req *http.Request) (*http.Response, error) {
		return respJSON(200, searchBody, rateHeaders("search", 50, reset)), nil
	}}
	c := newTestClient(mt)
	c.Sleep = func(d time.Duration) { slept = append(slept, d) }
	// 先把额度状态打到 Remaining=5。
	mt.handler = func(req *http.Request) (*http.Response, error) {
		return respJSON(200, searchBody, rateHeaders("search", 5, reset)), nil
	}
	if _, err := c.SearchRepos(t.Context(), "markdown html", 1); err != nil {
		t.Fatalf("第一次搜索: %v", err)
	}
	// 第二次：保护线触发，重置 2 分钟后 → 应 Sleep 约 2 分钟再发请求。
	mt.handler = func(req *http.Request) (*http.Response, error) {
		return respJSON(200, searchBody, rateHeaders("search", 50, reset)), nil
	}
	if _, err := c.SearchRepos(t.Context(), "markdown html", 1); err != nil {
		t.Fatalf("第二次搜索: %v", err)
	}
	// 两次 Sleep：8 秒搜索间隔 + 约 2 分钟原地等待（重置 ≤15 分钟）。
	foundWait := false
	for _, d := range slept {
		if d >= time.Minute && d <= 3*time.Minute {
			foundWait = true
		}
	}
	if !foundWait {
		t.Fatalf("重置 ≤15 分钟时没有原地等待，slept=%v", slept)
	}
}

func TestSecondaryLimitRetryAfter(t *testing.T) {
	// 403 + Remaining>0 + Retry-After: 5 → 次级限制，按 Retry-After 等 1 次后成功。
	calls := 0
	mt := &mockTransport{handler: func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			h := rateHeaders("search", 8, time.Date(2026, 11, 19, 11, 0, 0, 0, time.UTC).Unix())
			h["Retry-After"] = "5"
			return respJSON(403, `{"message":"You have exceeded a secondary rate limit"}`, h), nil
		}
		return respJSON(200, searchBody, rateHeaders("search", 8, time.Date(2026, 11, 19, 11, 0, 0, 0, time.UTC).Unix())), nil
	}}
	var slept []time.Duration
	c := newTestClient(mt)
	c.Sleep = func(d time.Duration) { slept = append(slept, d) }
	if _, err := c.SearchRepos(t.Context(), "markdown html", 1); err != nil {
		t.Fatalf("SearchRepos: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d, want 2（重试 1 次）", calls)
	}
	if len(slept) != 1 || slept[0] != 5*time.Second {
		t.Fatalf("slept=%v, want [5s]", slept)
	}
	assertNoAuth(t, mt)
}

func TestSecondaryLimitNoRateLimitHeader(t *testing.T) {
	// 次级限制的 403 有时根本不带 X-RateLimit 头（B.1 第 3 条修正）：
	// 仍须判为次级限制、走退避，而不是当成普通错误。
	calls := 0
	mt := &mockTransport{handler: func(req *http.Request) (*http.Response, error) {
		calls++
		if calls <= 2 {
			return respJSON(403, `{"message":"You have triggered an abuse detection mechanism"}`, nil), nil
		}
		return respJSON(200, searchBody, rateHeaders("search", 50, time.Date(2026, 11, 19, 11, 0, 0, 0, time.UTC).Unix())), nil
	}}
	var slept []time.Duration
	c := newTestClient(mt)
	c.Sleep = func(d time.Duration) { slept = append(slept, d) }
	if _, err := c.SearchRepos(t.Context(), "markdown html", 1); err != nil {
		t.Fatalf("SearchRepos: %v", err)
	}
	// 第 1 次 403 → 等 60s 重试；第 2 次 403 → 等 120s 重试；第 3 次成功。
	if calls != 3 {
		t.Fatalf("calls=%d, want 3", calls)
	}
	if len(slept) != 2 || slept[0] != 60*time.Second || slept[1] != 120*time.Second {
		t.Fatalf("slept=%v, want [60s 120s]", slept)
	}
}

func TestCacheAvoidsRepeatDeepDive(t *testing.T) {
	dir := t.TempDir()
	mt := githubMock(t)
	c := newTestClient(mt)
	c.CacheDir = dir
	tr := &Trace{GapID: "gap-x"}
	ce1, err := c.DeepDive(t.Context(), tr, "acme/md2html")
	if err != nil {
		t.Fatalf("DeepDive: %v", err)
	}
	if ce1.FromCache {
		t.Fatalf("第一次深挖不应命中缓存")
	}
	coreUsed := tr.CoreUsed
	if coreUsed != 3 {
		t.Fatalf("第一次 core=%d, want 3", coreUsed)
	}
	// 第二次深挖同一候选：缓存命中，0 次核心调用。
	tr2 := &Trace{GapID: "gap-y"}
	ce2, err := c.DeepDive(t.Context(), tr2, "acme/md2html")
	if err != nil {
		t.Fatalf("DeepDive 2: %v", err)
	}
	if !ce2.FromCache {
		t.Fatalf("第二次深挖应命中缓存")
	}
	if tr2.CoreUsed != 0 {
		t.Fatalf("缓存命中后 core=%d, want 0", tr2.CoreUsed)
	}
}

func TestEvaluateHardThresholds(t *testing.T) {
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		ce   CandidateEval
		want string // "" 表示通过
	}{
		{"archived H2", CandidateEval{Archived: true, Stars: 800}, "H2"},
		{"stars<700 H0", CandidateEval{Stars: 100, License: "MIT", LicenseFileOK: true, ReadmeOK: true, DepsFile: "package.json", LastCommit: "2026-10-01T00:00:00Z"}, "H0"},
		{"no license passes (record only)", CandidateEval{Stars: 800, License: "", LicenseFileOK: false, ReadmeOK: true, DepsFile: "package.json", LastCommit: "2026-10-01T00:00:00Z"}, ""},
		{"stale H3 warning only", CandidateEval{Stars: 800, License: "MIT", LicenseFileOK: true, ReadmeOK: true, DepsFile: "package.json", LastCommit: "2024-01-01T00:00:00Z"}, ""},
		{"no license field passes (record only)", CandidateEval{Stars: 800, License: "NOASSERTION", LicenseFileOK: true, ReadmeOK: true, DepsFile: "package.json", LastCommit: "2026-10-01T00:00:00Z"}, ""},
		{"gpl passes (record only)", CandidateEval{Stars: 800, License: "GPL-3.0", LicenseFileOK: true, ReadmeOK: true, DepsFile: "package.json", LastCommit: "2026-10-01T00:00:00Z"}, ""},
		{"mit passes", CandidateEval{Stars: 800, License: "MIT", LicenseFileOK: true, ReadmeOK: true, DepsFile: "package.json", LastCommit: "2026-10-01T00:00:00Z"}, ""},
		{"security flag R2", CandidateEval{Stars: 800, License: "MIT", LicenseFileOK: true, ReadmeOK: true, DepsFile: "package.json", LastCommit: "2026-10-01T00:00:00Z", SecurityFlag: "curl|bash"}, "R2"},
		{"heavy runtime R3", CandidateEval{Stars: 800, License: "MIT", LicenseFileOK: true, ReadmeOK: true, DepsFile: "package.json", LastCommit: "2026-10-01T00:00:00Z", RuntimeNeed: "heavy:cargo"}, "R3"},
		{"no readme no deps R5", CandidateEval{Stars: 800, License: "MIT", LicenseFileOK: true, LastCommit: "2026-10-01T00:00:00Z"}, "R5"},
	}
	for _, tc := range cases {
		ce := tc.ce
		EvaluateCandidate(&ce, now)
		got := ce.HardThreshold
		if got == "" && len(ce.RedLines) > 0 {
			got = ce.RedLines[0]
		}
		if got != tc.want {
			t.Errorf("%s: got %q, want %q (passed=%v)", tc.name, got, tc.want, ce.Passed)
		}
		if tc.want == "" && !ce.Passed {
			t.Errorf("%s: 应通过却未通过", tc.name)
		}
	}
	// H3 降为警告：超 18 个月未更新仍通过，但必须记 stale。
	stale := CandidateEval{Stars: 800, License: "MIT", LicenseFileOK: true, ReadmeOK: true, DepsFile: "package.json", LastCommit: "2024-01-01T00:00:00Z"}
	EvaluateCandidate(&stale, now)
	if !stale.Passed || !stale.Stale || stale.StaleWarning == "" {
		t.Errorf("stale 应通过且记警告，得到 passed=%v stale=%v warning=%q", stale.Passed, stale.Stale, stale.StaleWarning)
	}
}

func TestExtractKeywords(t *testing.T) {
	kw := ExtractKeywords("把 PDF 文件转成 markdown 格式的命令行工具")
	if len(kw) == 0 {
		t.Fatalf("没抽出关键词")
	}
	for _, k := range kw {
		if k == "把" || k == "的" {
			t.Fatalf("停用词未去掉: %v", kw)
		}
	}
}

// TestFuzzyGapRescueByResearch 是模糊缺口重搜救回：首轮查询太模糊，
// 第 1 页 0 条（OFF_TOPIC）；第 2 页翻出成熟候选，深挖通过 → recommend。
// 断言：只用了 2 次搜索，第 2 轮理由是结构化 OFF_TOPIC。
// TestZeroKeptGoesToPageTwo：首轮有结果但 0 留存（全部 Archived）
// → 第 2 轮翻同一查询的第 2 页（不是换词）。
func TestZeroKeptGoesToPageTwo(t *testing.T) {
	reset := time.Date(2026, 11, 19, 11, 0, 0, 0, time.UTC).Unix()
	mt := &mockTransport{handler: func(req *http.Request) (*http.Response, error) {
		u := req.URL.String()
		switch {
		case strings.Contains(u, "/search/repositories"):
			if strings.Contains(u, "&page=2&") {
				return respJSON(200, searchBody, rateHeaders("search", 49, reset)), nil
			}
			// 首轮 page=1：有 1 条，但是 Archived（粗筛淘汰，kept=0）。
			return respJSON(200, `{"total_count":1,"items":[{"id":9,"full_name":"old/dead-repo","html_url":"https://github.com/old/dead-repo","description":"dead","stargazers_count":5,"archived":true,"license":null,"pushed_at":"2020-01-01T00:00:00Z","default_branch":"main"}]}`, rateHeaders("search", 50, reset)), nil
		case strings.Contains(u, "/repos/acme/md2html/commits"):
			return respJSON(200, `[{"commit":{"committer":{"date":"2026-10-01T00:00:00Z"}}}]`, rateHeaders("core", 55, reset)), nil
		case strings.Contains(u, "/repos/acme/md2html/releases/latest"):
			return respJSON(200, `{"tag_name":"v1.2.0","published_at":"2026-09-01T00:00:00Z","html_url":"https://github.com/acme/md2html/releases/v1.2.0"}`, rateHeaders("core", 54, reset)), nil
		case strings.Contains(u, "/repos/acme/md2html"):
			return respJSON(200, detailsBody("acme/md2html", "MIT", false, "2026-10-01T00:00:00Z"), rateHeaders("core", 56, reset)), nil
		case strings.Contains(u, "raw.githubusercontent.com/acme/md2html"):
			if strings.HasSuffix(u, "README.md") {
				return respJSON(200, "# md2html\nUsage: md2html in.md out.html\n", nil), nil
			}
			if strings.HasSuffix(u, "LICENSE") {
				return respJSON(200, "MIT License", nil), nil
			}
			if strings.HasSuffix(u, "package.json") {
				return respJSON(200, `{"name":"md2html","bin":{"md2html":"./cli.js"}}`, nil), nil
			}
			return respJSON(404, "404: Not Found", nil), nil
		default:
			t.Fatalf("桩收到未预期的请求: %s", u)
			return nil, fmt.Errorf("unexpected %s", u)
		}
	}}
	c := newTestClient(mt)
	tr, err := c.Retrieve(t.Context(), GapInput{GapID: "gap-2026-11-19-010", TaskText: "vague thing converter", Keywords: []string{"vague", "thing", "converter"}})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if tr.Outcome != "recommend" {
		t.Fatalf("outcome=%q, want recommend（第 2 页救回）", tr.Outcome)
	}
	if len(tr.Rounds) != 2 || tr.Rounds[1].Page != 2 || tr.Rounds[1].Query != tr.Rounds[0].Query {
		t.Fatalf("第 2 轮应为同一查询 page=2，得到 %+v", tr.Rounds)
	}
	if tr.Recommended != "acme/md2html" {
		t.Fatalf("recommended=%q, want acme/md2html", tr.Recommended)
	}
}

func TestEcosystemOrder(t *testing.T) {
	// 缺口点名 go → go 最前，再补默认 python/javascript（封顶 3 个）。
	got := ecosystemOrder([]string{"markdown", "go", "cli"}, "build a markdown cli in go")
	if len(got) != 3 || got[0] != "go" || got[1] != "python" || got[2] != "javascript" {
		t.Fatalf("ecosystemOrder=%v, want [go python javascript]", got)
	}
	// 没点名 → 默认 python → javascript → go。
	got = ecosystemOrder([]string{"todo", "markdown", "json"}, "convert todo markdown to json")
	if len(got) != 3 || got[0] != "python" || got[1] != "javascript" || got[2] != "go" {
		t.Fatalf("ecosystemOrder=%v, want [python javascript go]", got)
	}
	// 别名：node → javascript。
	got = ecosystemOrder([]string{"node", "tool"}, "node tool")
	if got[0] != "javascript" {
		t.Fatalf("ecosystemOrder=%v, want javascript first", got)
	}
	if q := ecosystemQuery("todo markdown", "python"); q != "todo markdown language:python" {
		t.Fatalf("ecosystemQuery=%q", q)
	}
}

func TestFuzzyGapRescueByResearch(t *testing.T) {
	reset := time.Date(2026, 11, 19, 11, 0, 0, 0, time.UTC).Unix()
	mt := &mockTransport{handler: func(req *http.Request) (*http.Response, error) {
		u := req.URL.String()
		switch {
		case strings.Contains(u, "/search/repositories"):
			// 生态层 python：0 条；下一生态层 javascript 翻出成熟候选。
			// 注意：per_page=10 里含有 "page=1" 子串，判第 2 页须按 "&page=2&"。
			if strings.Contains(u, "javascript") {
				return respJSON(200, searchBody, rateHeaders("search", 49, reset)), nil
			}
			return respJSON(200, `{"total_count":0,"items":[]}`, rateHeaders("search", 50, reset)), nil
		case strings.Contains(u, "/repos/acme/md2html/commits"):
			return respJSON(200, `[{"commit":{"committer":{"date":"2026-10-01T00:00:00Z"}}}]`, rateHeaders("core", 55, reset)), nil
		case strings.Contains(u, "/repos/acme/md2html/releases/latest"):
			return respJSON(200, `{"tag_name":"v1.2.0","published_at":"2026-09-01T00:00:00Z","html_url":"https://github.com/acme/md2html/releases/v1.2.0"}`, rateHeaders("core", 54, reset)), nil
		case strings.Contains(u, "/repos/acme/md2html"):
			return respJSON(200, detailsBody("acme/md2html", "MIT", false, "2026-10-01T00:00:00Z"), rateHeaders("core", 56, reset)), nil
		case strings.Contains(u, "raw.githubusercontent.com/acme/md2html"):
			if strings.HasSuffix(u, "README.md") {
				return respJSON(200, "# md2html\nUsage: md2html in.md out.html\n", nil), nil
			}
			if strings.HasSuffix(u, "LICENSE") {
				return respJSON(200, "MIT License", nil), nil
			}
			if strings.HasSuffix(u, "package.json") {
				return respJSON(200, `{"name":"md2html","bin":{"md2html":"./cli.js"}}`, nil), nil
			}
			return respJSON(404, "404: Not Found", nil), nil
		default:
			t.Fatalf("桩收到未预期的请求: %s", u)
			return nil, fmt.Errorf("unexpected %s", u)
		}
	}}
	c := newTestClient(mt)
	tr, err := c.Retrieve(t.Context(), GapInput{GapID: "gap-2026-11-19-009", TaskText: "vague thing converter", Keywords: []string{"vague", "thing", "converter"}})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if tr.Outcome != "recommend" {
		t.Fatalf("outcome=%q, want recommend（换词重搜救回）", tr.Outcome)
	}
	if tr.SearchesUsed != 2 {
		t.Fatalf("searches=%d, want 2（首轮 0 条 + 换词重搜救回）", tr.SearchesUsed)
	}
	// 第 2 轮：0 结果不能翻页（翻页必空），必须换下一生态层。
	if len(tr.Rounds) != 2 || tr.Rounds[1].Page != 1 || tr.Rounds[0].Ecosystem != "python" || tr.Rounds[1].Ecosystem != "javascript" || tr.Rounds[1].Reason != "OFF_TOPIC" {
		t.Fatalf("第 2 轮应为 javascript 生态层 page=1 + OFF_TOPIC，得到 %+v", tr.Rounds)
	}
	if tr.Recommended != "acme/md2html" {
		t.Fatalf("recommended=%q, want acme/md2html", tr.Recommended)
	}
}

// TestRetrieveModelQueries 是模型写查询词的顺风局：QueryGen 给聚焦与
// 放宽两条查询，首轮即推荐；查询不加 language 限定，留痕记 model。
func TestRetrieveModelQueries(t *testing.T) {
	mt := githubMock(t)
	c := newTestClient(mt)
	tr, err := c.Retrieve(t.Context(), GapInput{
		GapID: "gap-2026-11-19-010", TaskText: "markdown to html cli",
		QueryGen: func(context.Context, string) (string, string, error) {
			return "md2html", "markdown html converter", nil
		},
		CoverageJudge: func(context.Context, string, string) (int, string, error) {
			return 95, "covers markdown to html conversion", nil
		},
	})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if tr.QuerySource != "model" {
		t.Fatalf("query_source=%q, want model", tr.QuerySource)
	}
	if len(tr.ModelQueries) != 2 || tr.ModelQueries[0] != "md2html" {
		t.Fatalf("model_queries=%v", tr.ModelQueries)
	}
	if tr.Outcome != "recommend" || tr.Recommended != "acme/md2html" {
		t.Fatalf("outcome=%q recommended=%q", tr.Outcome, tr.Recommended)
	}
	if tr.SearchesUsed != 1 {
		t.Fatalf("searches=%d, want 1（聚焦查询首轮命中）", tr.SearchesUsed)
	}
	if len(tr.Candidates) != 1 || tr.Candidates[0].Coverage != 95 || tr.Candidates[0].CoverageUnknown {
		t.Fatalf("candidates=%+v, want coverage=95 已判定", tr.Candidates)
	}
	// 首轮搜索的查询就是模型写的聚焦查询，且不带 language 限定。
	q := mt.requests[0].URL.Query().Get("q")
	if q != "md2html" {
		t.Fatalf("首轮查询 q=%q, want md2html", q)
	}
}

// TestRetrieveModelQueryFallback 是模型写查询失败时的回退：
// QueryGen 报错 → 机械抽取，留痕记 fallback-mechanical，仍能推荐。
func TestRetrieveModelQueryFallback(t *testing.T) {
	mt := githubMock(t)
	c := newTestClient(mt)
	tr, err := c.Retrieve(t.Context(), GapInput{
		GapID: "gap-2026-11-19-011", TaskText: "markdown to html cli",
		Keywords: []string{"markdown", "html", "cli"},
		QueryGen: func(context.Context, string) (string, string, error) {
			return "", "", fmt.Errorf("model down")
		},
	})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if tr.QuerySource != "fallback-mechanical" {
		t.Fatalf("query_source=%q, want fallback-mechanical", tr.QuerySource)
	}
	if tr.Outcome != "recommend" {
		t.Fatalf("outcome=%q, want recommend（机械回退仍应走通）", tr.Outcome)
	}
}

// TestCoverageJudgeR4 是覆盖度模型判定：判 60% → 不通过且记 R4；
// 判 90% → 通过且 Coverage 有值。
func TestCoverageJudgeR4(t *testing.T) {
	mt := githubMock(t)
	c := newTestClient(mt)
	c.coverageTaskText = "markdown to html cli"
	c.coverageJudge = func(context.Context, string, string) (int, string, error) {
		return 60, "only converts part of the format", nil
	}
	tr := &Trace{GapID: "gap-x"}
	ce, err := c.DeepDive(t.Context(), tr, "acme/md2html")
	if err != nil {
		t.Fatalf("DeepDive: %v", err)
	}
	if ce.Passed || ce.Coverage != 60 || ce.CoverageUnknown {
		t.Fatalf("覆盖 60%% 应不通过，得到 %+v", ce)
	}
	found := false
	for _, r := range ce.RedLines {
		if r == "R4" {
			found = true
		}
	}
	if !found {
		t.Fatalf("覆盖 <80%% 应记 R4，得到 %v", ce.RedLines)
	}
	// 判 90% → 通过。
	c.coverageJudge = func(context.Context, string, string) (int, string, error) {
		return 90, "covers the conversion", nil
	}
	tr2 := &Trace{GapID: "gap-y"}
	ce2, err := c.DeepDive(t.Context(), tr2, "acme/md2html")
	if err != nil {
		t.Fatalf("DeepDive 2: %v", err)
	}
	if !ce2.Passed || ce2.Coverage != 90 || ce2.CoverageUnknown {
		t.Fatalf("覆盖 90%% 应通过，得到 %+v", ce2)
	}
}
