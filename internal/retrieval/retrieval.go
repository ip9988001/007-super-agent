// Package retrieval 是第 23 步锻造的复用优先检索器：匿名 GitHub 检索
// （搜索 + 核心 API + raw 直读），独立模块，供第 24 步学习回路直接复用。
//
// 铁律：
//   - 不配 Token、不用现有 PAT：所有请求都不带 Authorization 头
//     （见 TestNoAuthHeader）；现有 PAT 只用于推代码，绝不注入检索器。
//   - 匿名额度（实测 2026-11-18）：核心 60 次/小时、搜索 10 次/分钟。
//   - 节流：串行 + 搜索最小间隔 8 秒；砍掉本地计数器，直接读响应头
//     X-RateLimit-Remaining，≤10 作全机共享保护线；分桶读（搜索桶与
//     核心桶独立，不能拿搜索响应的 Remaining 去判断核心额度）。
//   - raw.githubusercontent.com 直读不占核心 API 额度（2026-11-18
//     测试机实测：连抓 5 个 raw 文件后 core/search/code_search 分毫未动）。
//   - 上层评估与出口逻辑（采用/包装 vs 自造）不在本包，在 internal/forge；
//     本包只做低层抓取 + 节流 + 缓存 + 留痕。
package retrieval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 匿名限额与节流常量（交互-23 附录 B.3 定稿）。
const (
	SearchAPIURL = "https://api.github.com/search/repositories"
	CoreAPIURL   = "https://api.github.com"
	RawBaseURL   = "https://raw.githubusercontent.com"

	DefaultMinSearchInterval = 8 * time.Second // 搜索最小间隔（串行 + 8 秒天然 ≤7.5 次/分钟）
	DefaultProtectRemaining  = 10              // 响应头 Remaining ≤10 即保护
	DefaultMaxSearchesPerGap = 3               // 每缺口总搜索 ≤3 次
	DefaultMaxCorePerGap     = 15              // 每缺口核心 API ≤15 次
	DefaultCacheTTL          = 7 * 24 * time.Hour
	WaitThreshold            = 15 * time.Minute // 重置 ≤15 分钟原地等，>15 分钟顺延
	MaxRetryAfter            = 180 * time.Second
)

// ErrRateLimited 是主限流到限且重置 >15 分钟：调用方记
// SEARCH_DEFERRED_RATE_LIMIT 顺延次日，不转自造。
type ErrRateLimited struct {
	Resource string
	ResetAt  time.Time
}

func (e *ErrRateLimited) Error() string {
	return fmt.Sprintf("retrieval: %s 限额已到保护线，重置于 %s（>15 分钟，顺延次日）",
		e.Resource, e.ResetAt.Format(time.RFC3339))
}

// ErrSecondaryLimited 是次级限制退避耗尽：调用方同样记顺延。
type ErrSecondaryLimited struct {
	Resource string
	Attempts int
}

func (e *ErrSecondaryLimited) Error() string {
	return fmt.Sprintf("retrieval: %s 次级限制退避 %d 次仍触发，顺延次日", e.Resource, e.Attempts)
}

// HTTPDoer 是可注入的 HTTP 执行器：生产用 *http.Client，测试用桩。
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client 是匿名 GitHub 检索客户端。零值不可用，用 New 建。
// Sleep 与 Now 可注入（测试里换成记录器/固定时钟，不真等 15 分钟）。
type Client struct {
	HTTP              HTTPDoer
	Sleep             func(time.Duration)
	Now               func() time.Time
	MinSearchInterval time.Duration
	ProtectRemaining  int
	MaxSearchesPerGap int
	MaxCorePerGap     int
	CacheTTL          time.Duration
	CacheDir          string // "" 表示不缓存

	mu         sync.Mutex
	lastSearch time.Time
	remaining  map[string]int       // resource -> 最近一次响应头的 Remaining
	resetAt    map[string]time.Time // resource -> 最近一次响应头的 Reset

	// 本次检索的覆盖度判定上下文（Retrieve 进入时设、退出时清；
	// 锻造是串行的，同一时间只跑一个缺口）。
	coverageJudge    CoverageJudge
	coverageTaskText string
}

// New 建一个检索客户端。httpDoer 为 nil 时用默认 HTTP 客户端
// （30 秒超时，不带任何认证头）。
func New(httpDoer HTTPDoer, cacheDir string) *Client {
	if httpDoer == nil {
		httpDoer = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{
		HTTP: httpDoer, Sleep: time.Sleep, Now: time.Now,
		MinSearchInterval: DefaultMinSearchInterval,
		ProtectRemaining:  DefaultProtectRemaining,
		MaxSearchesPerGap: DefaultMaxSearchesPerGap,
		MaxCorePerGap:     DefaultMaxCorePerGap,
		CacheTTL:          DefaultCacheTTL,
		CacheDir:          cacheDir,
		remaining:         map[string]int{},
		resetAt:           map[string]time.Time{},
	}
}

// rateState 读某额度桶的最近状态（调用方须已持有 c.mu 或刚更新完）。
func (c *Client) rateState(resource string) (remaining int, reset time.Time, known bool) {
	r, ok := c.remaining[resource]
	if !ok {
		return 0, time.Time{}, false
	}
	return r, c.resetAt[resource], true
}

// updateRateState 从响应头更新某额度桶的状态（X-RateLimit-Resource
// 缺失时按调用方给的 resource 记）。
func (c *Client) updateRateState(resp *http.Response, resource string) {
	res := resp.Header.Get("X-RateLimit-Resource")
	if res == "" {
		res = resource
	}
	remStr := resp.Header.Get("X-RateLimit-Remaining")
	resetStr := resp.Header.Get("X-RateLimit-Reset")
	if remStr == "" || resetStr == "" {
		return
	}
	var rem int
	var resetUnix int64
	if _, err := fmt.Sscanf(remStr, "%d", &rem); err != nil {
		return
	}
	if _, err := fmt.Sscanf(resetStr, "%d", &resetUnix); err != nil {
		return
	}
	c.mu.Lock()
	c.remaining[res] = rem
	c.resetAt[res] = time.Unix(resetUnix, 0)
	c.mu.Unlock()
}

// checkBeforeCall 在调用某额度桶的 API 前检查保护线：
// Remaining ≤10 时算到限；重置 ≤15 分钟原地等（Sleep），>15 分钟
// 返回 ErrRateLimited（调用方顺延）。额度未知（还没调过）时放行。
func (c *Client) checkBeforeCall(resource string) error {
	c.mu.Lock()
	rem, reset, known := c.rateState(resource)
	c.mu.Unlock()
	if !known || rem > c.ProtectRemaining {
		return nil
	}
	wait := reset.Sub(c.Now())
	if wait < 0 {
		wait = 0
	}
	if wait > WaitThreshold {
		return &ErrRateLimited{Resource: resource, ResetAt: reset}
	}
	if wait > 0 {
		c.Sleep(wait)
	}
	return nil
}

// isSecondaryLimit 按三选一判次级限制（交互-23 附录 B.1 第 3 条的修正）：
// 403/429 且 (a) Remaining>0，或 (b) 带 Retry-After，或 (c) 响应体含
// secondary rate limit / abuse。不能只认 Remaining>0——次级限制的
// 403 有时根本不带 X-RateLimit 响应头。
func isSecondaryLimit(resp *http.Response, body []byte) bool {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return false
	}
	if resp.Header.Get("Retry-After") != "" {
		return true
	}
	if remStr := resp.Header.Get("X-RateLimit-Remaining"); remStr != "" {
		var rem int
		if _, err := fmt.Sscanf(remStr, "%d", &rem); err == nil && rem > 0 {
			return true
		}
	} else {
		// 根本不带 X-RateLimit 头的 403/429：不是主限流（主限流一定带
		// Remaining: 0），按次级限制处理。
		return true
	}
	lower := strings.ToLower(string(body))
	return strings.Contains(lower, "secondary rate limit") || strings.Contains(lower, "abuse")
}

// parseRetryAfter 解析 Retry-After 头（秒数）；非法或缺失返回 0。
func parseRetryAfter(resp *http.Response) time.Duration {
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0
	}
	var sec int64
	if _, err := fmt.Sscanf(v, "%d", &sec); err != nil || sec < 0 {
		return 0
	}
	return time.Duration(sec) * time.Second
}

// doAPI 发一次 GitHub API 请求（搜索或核心），带主限流保护与次级限制退避。
// resource 是 "search" 或 "core"。返回响应体（调用方解析）。
// 次级限制：有 Retry-After（≤180 秒）按它休眠重试 1 次；无则按
// 60 秒 →120 秒指数退避最多 2 次；仍触发返回 ErrSecondaryLimited。
// 主限流（Remaining=0 的 403/429）：按 checkBeforeCall 的口径处理。
func (c *Client) doAPI(ctx context.Context, resource, rawURL string) ([]byte, *http.Response, error) {
	if err := c.checkBeforeCall(resource); err != nil {
		return nil, nil, err
	}
	var lastErr error
	for attempt := 0; attempt <= 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, nil, err
		}
		// 匿名：绝不带 Authorization 头（铁律，见 TestNoAuthHeader）。
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("User-Agent", "007-super-agent-retrieval")
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, nil, err
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		c.updateRateState(resp, resource)
		if resp.StatusCode == http.StatusOK {
			return body, resp, nil
		}
		if isSecondaryLimit(resp, body) {
			lastErr = &ErrSecondaryLimited{Resource: resource, Attempts: attempt + 1}
			if ra := parseRetryAfter(resp); ra > 0 && ra <= MaxRetryAfter {
				c.Sleep(ra)
				continue // 按 Retry-After 重试 1 次（attempt 0 时）
			}
			if attempt == 0 {
				c.Sleep(60 * time.Second)
				continue
			}
			if attempt == 1 {
				c.Sleep(120 * time.Second)
				continue
			}
			return nil, resp, lastErr
		}
		// 非次级限制的错误（404 归调用方处理；其余直接报错）。
		if resp.StatusCode == http.StatusNotFound {
			return nil, resp, errNotFound
		}
		return nil, resp, fmt.Errorf("retrieval: %s %s 返回 %d: %s",
			resource, rawURL, resp.StatusCode, truncateRunes(string(body), 300))
	}
	return nil, nil, lastErr
}

var errNotFound = errors.New("retrieval: not found")

// doRaw 直读 raw.githubusercontent.com 上的一个文件（不占核心 API 额度，
// 也不走主限流保护——raw 响应头根本没有 X-RateLimit 字段）。
// 404 返回 ("", false, nil)；其他错误返回 error。
func (c *Client) doRaw(ctx context.Context, rawURL string) (string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", false, err
	}
	req.Header.Set("User-Agent", "007-super-agent-retrieval")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", false, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return "", false, fmt.Errorf("retrieval: raw %s 返回 %d: %s",
			rawURL, resp.StatusCode, truncateRunes(string(body), 200))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", false, err
	}
	return string(body), true, nil
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// ---- GitHub 数据结构 ----

// RepoSummary 是搜索结果里一个仓库的摘要。
type RepoSummary struct {
	FullName      string `json:"full_name"`
	Owner         string `json:"owner"`
	Repo          string `json:"repo"`
	Description   string `json:"description"`
	Stars         int    `json:"stars"`
	Archived      bool   `json:"archived"`
	LicenseSPDX   string `json:"license_spdx"`
	PushedAt      string `json:"pushed_at"`
	DefaultBranch string `json:"default_branch"`
	HTMLURL       string `json:"html_url"`
}

// RepoDetails 是核心 API 查到的仓库元数据（1 次核心调用）。
type RepoDetails struct {
	FullName      string `json:"full_name"`
	Archived      bool   `json:"archived"`
	LicenseSPDX   string `json:"license_spdx"`
	Stars         int    `json:"stars"`
	PushedAt      string `json:"pushed_at"`
	DefaultBranch string `json:"default_branch"`
	Description   string `json:"description"`
	HTMLURL       string `json:"html_url"`
}

// Release 是最新 Release（1 次核心调用；无 Release 时为 nil，调用仍计 1 次）。
type Release struct {
	TagName     string `json:"tag_name"`
	PublishedAt string `json:"published_at"`
	HTMLURL     string `json:"html_url"`
}

type searchResponse struct {
	TotalCount int `json:"total_count"`
	Items      []struct {
		FullName      string `json:"full_name"`
		Description   string `json:"description"`
		Stars         int    `json:"stargazers_count"`
		Archived      bool   `json:"archived"`
		PushedAt      string `json:"pushed_at"`
		DefaultBranch string `json:"default_branch"`
		HTMLURL       string `json:"html_url"`
		License       *struct {
			SPDXID string `json:"spdx_id"`
		} `json:"license"`
	} `json:"items"`
}

// SearchRepos 查一次仓库搜索（1 次搜索调用），返回前 10 个候选。
// 调用前强制 8 秒最小间隔（串行节流）。
func (c *Client) SearchRepos(ctx context.Context, query string, page int) ([]RepoSummary, error) {
	c.mu.Lock()
	last := c.lastSearch
	interval := c.MinSearchInterval
	c.mu.Unlock()
	if !last.IsZero() {
		if since := c.Now().Sub(last); since < interval {
			c.Sleep(interval - since)
		}
	}
	q := url.QueryEscape(query)
	rawURL := fmt.Sprintf("%s?q=%s&per_page=10&page=%d&sort=stars&order=desc", SearchAPIURL, q, page)
	body, _, err := c.doAPI(ctx, "search", rawURL)
	if err != nil {
		return nil, err
	}
	var sr searchResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		return nil, fmt.Errorf("retrieval: 解析搜索结果失败: %w", err)
	}
	c.mu.Lock()
	c.lastSearch = c.Now()
	c.mu.Unlock()
	out := make([]RepoSummary, 0, len(sr.Items))
	for _, it := range sr.Items {
		owner, repo, _ := strings.Cut(it.FullName, "/")
		lic := ""
		if it.License != nil {
			lic = it.License.SPDXID
		}
		out = append(out, RepoSummary{
			FullName: it.FullName, Owner: owner, Repo: repo,
			Description: it.Description, Stars: it.Stars, Archived: it.Archived,
			LicenseSPDX: lic, PushedAt: it.PushedAt,
			DefaultBranch: it.DefaultBranch, HTMLURL: it.HTMLURL,
		})
	}
	return out, nil
}

type repoDetailsResponse struct {
	FullName      string `json:"full_name"`
	Archived      bool   `json:"archived"`
	Stars         int    `json:"stargazers_count"`
	PushedAt      string `json:"pushed_at"`
	DefaultBranch string `json:"default_branch"`
	Description   string `json:"description"`
	HTMLURL       string `json:"html_url"`
	License       *struct {
		SPDXID string `json:"spdx_id"`
	} `json:"license"`
}

// RepoDetailsCall 查仓库详情（1 次核心调用）。
func (c *Client) RepoDetailsCall(ctx context.Context, owner, repo string) (*RepoDetails, error) {
	rawURL := fmt.Sprintf("%s/repos/%s/%s", CoreAPIURL, owner, repo)
	body, _, err := c.doAPI(ctx, "core", rawURL)
	if err != nil {
		return nil, err
	}
	var d repoDetailsResponse
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("retrieval: 解析仓库详情失败: %w", err)
	}
	lic := ""
	if d.License != nil {
		lic = d.License.SPDXID
	}
	return &RepoDetails{
		FullName: d.FullName, Archived: d.Archived, LicenseSPDX: lic,
		Stars: d.Stars, PushedAt: d.PushedAt, DefaultBranch: d.DefaultBranch,
		Description: d.Description, HTMLURL: d.HTMLURL,
	}, nil
}

// RepoCommitsCall 查最近提交（1 次核心调用，per_page=5），返回提交时间列表。
func (c *Client) RepoCommitsCall(ctx context.Context, owner, repo string) ([]string, error) {
	rawURL := fmt.Sprintf("%s/repos/%s/%s/commits?per_page=5", CoreAPIURL, owner, repo)
	body, _, err := c.doAPI(ctx, "core", rawURL)
	if err != nil {
		return nil, err
	}
	var commits []struct {
		Commit struct {
			Committer struct {
				Date string `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(body, &commits); err != nil {
		return nil, fmt.Errorf("retrieval: 解析提交记录失败: %w", err)
	}
	out := make([]string, 0, len(commits))
	for _, cm := range commits {
		out = append(out, cm.Commit.Committer.Date)
	}
	return out, nil
}

// LatestReleaseCall 查最新 Release（1 次核心调用；无 Release 返回 nil,nil，
// 调用仍计 1 次——预算里要留这个数）。
func (c *Client) LatestReleaseCall(ctx context.Context, owner, repo string) (*Release, error) {
	rawURL := fmt.Sprintf("%s/repos/%s/%s/releases/latest", CoreAPIURL, owner, repo)
	body, _, err := c.doAPI(ctx, "core", rawURL)
	if err != nil {
		if errors.Is(err, errNotFound) {
			return nil, nil
		}
		return nil, err
	}
	var r Release
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("retrieval: 解析 Release 失败: %w", err)
	}
	return &r, nil
}

// RawFile 直读一个文件（不占核心 API 额度）。branch 为空时用 "HEAD"
// 占位——调用方应先拿到 default_branch 再调。
func (c *Client) RawFile(ctx context.Context, owner, repo, branch, path string) (string, bool, error) {
	rawURL := fmt.Sprintf("%s/%s/%s/%s/%s", RawBaseURL, owner, repo, branch, path)
	return c.doRaw(ctx, rawURL)
}

// Remaining 返回某额度桶最近一次响应头的 Remaining（测试与断言用）。
func (c *Client) Remaining(resource string) (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.remaining[resource]
	return v, ok
}

// cachePath 返回某候选的缓存文件路径。
func (c *Client) cachePath(fullName string) string {
	safe := strings.ReplaceAll(fullName, "/", "__")
	return filepath.Join(c.CacheDir, safe+".json")
}

// loadCache 读缓存（7 天内有效才返回）。
func (c *Client) loadCache(fullName string) (*CandidateEval, bool) {
	if c.CacheDir == "" {
		return nil, false
	}
	b, err := os.ReadFile(c.cachePath(fullName))
	if err != nil {
		return nil, false
	}
	var ce CandidateEval
	if err := json.Unmarshal(b, &ce); err != nil {
		return nil, false
	}
	if c.Now().Sub(ce.CachedAt) > c.CacheTTL {
		return nil, false
	}
	ce.FromCache = true
	return &ce, true
}

// saveCache 写缓存（目录不存在时创建；写失败只记错，不阻塞检索）。
func (c *Client) saveCache(ce *CandidateEval) {
	if c.CacheDir == "" {
		return
	}
	// 覆盖度是任务相关的：存缓存前剥掉（并去掉因此记的 R4、恢复
	// 硬门槛层面的 Passed），否则别的缺口会读到上一个缺口的覆盖度。
	cp := *ce
	cp.Coverage = 0
	cp.CoverageReason = ""
	cp.CoverageUnknown = true
	// 建新切片，防串改原 ce.RedLines 的底数组。
	var kept []string
	hadR4 := false
	for _, r := range ce.RedLines {
		if r == "R4" {
			hadR4 = true
			continue
		}
		kept = append(kept, r)
	}
	cp.RedLines = kept
	if hadR4 && cp.HardThreshold == "" && len(kept) == 0 {
		cp.Passed = true
	}
	cp.CachedAt = c.Now()
	b, err := json.Marshal(&cp)
	if err != nil {
		return
	}
	_ = os.MkdirAll(c.CacheDir, 0o700)
	_ = os.WriteFile(c.cachePath(cp.FullName), b, 0o600)
}
