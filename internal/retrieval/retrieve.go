package retrieval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// GapInput 是一次检索的输入。
type GapInput struct {
	GapID    string // gap-YYYY-MM-DD-NNN
	TaskText string // 缺口任务原话（截断 ≤500 字）
	// Keywords：显式搜索关键词（验收用缺口可指定）；空时从 TaskText 机械抽取。
	Keywords []string
	// LocalCheck：第零层本地检查（调用方注入：查自家注册表 / wiki /
	// 隔离记录）。命中时返回 (true, 说明)，不做任何外部搜索。
	// 为 nil 时跳过第零层。
	LocalCheck func(taskText string) (hit bool, detail string)
	// ResumeQuery / ResumeReason：锻造侧模型判定后要求追加一轮检索时用：
	// 下一轮改用该查询与理由（仍计入 ≤3 次总额度）。
	ResumeQuery  string
	ResumeReason string
}

// SearchRound 是一轮搜索的留痕。
type SearchRound struct {
	Query      string            `json:"query"`
	Page       int               `json:"page"`
	Ecosystem  string            `json:"ecosystem,omitempty"` // 本轮的语言生态层（python/javascript/go…）
	Reason     string            `json:"reason"`              // 首轮 ""；重搜 OFF_TOPIC / ALL_BLOCKED_REDLINE / PARTIAL_COVERAGE
	Top10      []string          `json:"top10"`
	Eliminated map[string]string `json:"eliminated"` // full_name -> 淘汰原因
	Kept       []string          `json:"kept"`
}

// Trace 是一次检索的完整留痕（存 data/forge/retrieval/<gap-id>.json，
// 不进 wiki、不向量化）。
type Trace struct {
	GapID          string          `json:"gap_id"`
	Layer0Hit      bool            `json:"layer0_hit"`
	Layer0Detail   string          `json:"layer0_detail,omitempty"`
	Rounds         []SearchRound   `json:"rounds"`
	Candidates     []CandidateEval `json:"candidates"`
	Recommended    string          `json:"recommended,omitempty"`
	SearchesUsed   int             `json:"searches_used"`
	CoreUsed       int             `json:"core_used"`
	Outcome        string          `json:"outcome"` // recommend / exhausted / deferred / layer0
	DeferredReason string          `json:"deferred_reason,omitempty"`
	DurationMs     int64           `json:"duration_ms"`
}

// Summary 是一行检索留痕摘要（供蓝图 reuse_search 字段与分流提示词用）。
func (tr *Trace) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "检索结果=%s 搜索=%d 次 核心=%d 次", tr.Outcome, tr.SearchesUsed, tr.CoreUsed)
	if tr.CoreUsed == 0 && len(tr.Candidates) > 0 {
		allCached := true
		for _, c := range tr.Candidates {
			if !c.FromCache {
				allCached = false
				break
			}
		}
		if allCached {
			fmt.Fprintf(&b, "（候选元数据来自 7 天缓存，本次核心 0 次）")
		}
	}
	if tr.Layer0Detail != "" && !tr.Layer0Hit {
		fmt.Fprintf(&b, " 第零层未命中：%s", tr.Layer0Detail)
	}
	if tr.Layer0Hit {
		fmt.Fprintf(&b, " 第零层命中：%s", tr.Layer0Detail)
	}
	if tr.DeferredReason != "" {
		fmt.Fprintf(&b, " 顺延原因：%s", tr.DeferredReason)
	}
	for i, c := range tr.Candidates {
		fmt.Fprintf(&b, " 候选%d=%s stars=%d 许可=%s 过=%v 红线=%v", i+1, c.FullName, c.Stars, c.License, c.Passed, c.RedLines)
		var raw []string
		if c.ReadmeOK {
			raw = append(raw, "README")
		}
		if c.LicenseFileOK {
			raw = append(raw, "LICENSE")
		}
		if c.DepsFile != "" {
			raw = append(raw, c.DepsFile)
		}
		if len(raw) > 0 {
			fmt.Fprintf(&b, " raw直读=%s", strings.Join(raw, "+"))
		}
	}
	if tr.Recommended != "" {
		fmt.Fprintf(&b, " 推荐=%s", tr.Recommended)
	}
	for i, r := range tr.Rounds {
		fmt.Fprintf(&b, " 第%d轮：eco=%s query=%q page=%d reason=%s top10=%d kept=%d", i+1, r.Ecosystem, r.Query, r.Page, r.Reason, len(r.Top10), len(r.Kept))
	}
	return b.String()
}

// Retrieve 跑完一次复用优先检索（第零层 → 外部层），返回留痕。
// 流程：
//  1. 第零层命中 → 直接返回，不做外部搜索；
//  2. 外部层：每轮 1 次搜索（≤3 次），先看透前十：粗筛留 3–5 个候选，
//     深挖（每候选 3 次核心调用，封顶 15 次/缺口）；有 ≥1 个过全部可查
//     门槛与红线即停（停机条件），推荐第一个（最新鲜的）；
//  3. 无合格候选且搜索未用完：下一轮翻第 2 页或换词重搜（理由结构化）；
//  4. 3 搜用完仍无合格候选 → outcome=exhausted（转自造）；
//  5. 到限（重置 >15 分钟）→ outcome=deferred（顺延次日，不转自造）。
//
// 到限与次级限制退避耗尽时返回 (*Trace, error)，Trace 里已记下当时的
// 轮次与调用数，调用方据此记 SEARCH_DEFERRED_RATE_LIMIT。
func (c *Client) Retrieve(ctx context.Context, in GapInput) (*Trace, error) {
	start := time.Now()
	tr := &Trace{GapID: in.GapID}

	// 第零层：强制全查，本地零成本。命中即停，不做外部搜索；
	// 未命中也把检查摘要写进留痕（监工要核验“确实查过”）。
	if in.LocalCheck != nil {
		hit, detail := in.LocalCheck(in.TaskText)
		tr.Layer0Detail = detail
		if hit {
			tr.Layer0Hit = true
			tr.Outcome = "layer0"
			tr.DurationMs = time.Since(start).Milliseconds()
			return tr, nil
		}
	}

	keywords := in.Keywords
	if len(keywords) == 0 {
		keywords = ExtractKeywords(in.TaskText)
	}
	query := strings.Join(keywords, " ")
	if strings.TrimSpace(query) == "" {
		query = in.TaskText
	}
	// 外部层按语言生态动态顺序（规格 §2A）：每轮搜一个生态层，
	// 缺口里明确提到的语言排最前，其余默认 python → javascript → go。
	ecos := ecosystemOrder(keywords, in.TaskText)
	ecoIdx := 0
	paged := false // 当前生态层是否已翻过第 2 页

	// 追加轮（锻造侧模型要求）：先用它，再走机械轮次。
	pendingQuery, pendingReason := in.ResumeQuery, in.ResumeReason

	for tr.SearchesUsed < c.MaxSearchesPerGap {
		if err := ctx.Err(); err != nil {
			return tr, err
		}
		var q, eco string
		var page int
		var reason string
		if pendingQuery != "" {
			q, page, reason = pendingQuery, 1, pendingReason
			pendingQuery, pendingReason = "", ""
		} else if len(tr.Rounds) == 0 {
			eco = ecos[0]
			q, page, reason = ecosystemQuery(query, eco), 1, ""
		} else {
			last := tr.Rounds[len(tr.Rounds)-1]
			switch {
			case len(last.Top10) == 0:
				// 上一层 0 结果 → 换下一生态层（翻页必空）。
				ecoIdx++
				if ecoIdx < len(ecos) {
					eco = ecos[ecoIdx]
					q, page, reason = ecosystemQuery(query, eco), 1, tr.nextReason()
				} else {
					q, page, reason = refineQuery(query), 1, tr.nextReason()
				}
				paged = false
			case len(last.Kept) == 0 && !paged:
				// 有结果但 0 留存 → 同生态翻第 2 页（每层仅一次）。
				eco = last.Ecosystem
				q, page, reason = last.Query, 2, tr.nextReason()
				paged = true
			default:
				// 留存>0 但全未过关（红线/硬门槛）→ 下一生态层。
				ecoIdx++
				if ecoIdx < len(ecos) {
					eco = ecos[ecoIdx]
					q, page, reason = ecosystemQuery(query, eco), 1, tr.nextReason()
				} else {
					q, page, reason = refineQuery(query), 1, tr.nextReason()
				}
				paged = false
			}
		}
		round, err := c.searchRound(ctx, tr, q, page, reason)
		if err != nil {
			tr.DurationMs = time.Since(start).Milliseconds()
			if rl, ok := asRateLimited(err); ok {
				tr.Outcome = "deferred"
				tr.DeferredReason = rl.Error()
				return tr, err
			}
			return tr, err
		}
		round.Ecosystem = eco
		tr.Rounds = append(tr.Rounds, round)
		tr.SearchesUsed++
		// 深挖本轮粗筛留下的候选（核心预算内）。
		if err := c.deepDiveRound(ctx, tr, round); err != nil {
			tr.DurationMs = time.Since(start).Milliseconds()
			if rl, ok := asRateLimited(err); ok {
				tr.Outcome = "deferred"
				tr.DeferredReason = rl.Error()
				return tr, err
			}
			return tr, err
		}
		// 停机条件：有 ≥1 个过全部可查门槛与红线 → 停，推荐最新鲜的那个。
		if rec := firstPassing(tr.Candidates); rec != "" {
			tr.Recommended = rec
			tr.Outcome = "recommend"
			tr.DurationMs = time.Since(start).Milliseconds()
			return tr, nil
		}
		// 本轮无合格候选：继续下一轮（搜索未用完时）。
	}
	tr.Outcome = "exhausted"
	tr.DurationMs = time.Since(start).Milliseconds()
	return tr, nil
}

// asRateLimited 判断错误是否到限类（主限流 / 次级限制退避耗尽）。
func asRateLimited(err error) (error, bool) {
	var rl *ErrRateLimited
	if errors.As(err, &rl) {
		return rl, true
	}
	var sl *ErrSecondaryLimited
	if errors.As(err, &sl) {
		return sl, true
	}
	return nil, false
}

// nextReason 按上一轮的淘汰情况定结构化重搜理由：
// 全部因“跑题”淘汰 → OFF_TOPIC；全部撞红线/硬门槛 → ALL_BLOCKED_REDLINE；
// 其余（部分覆盖、候选太少）→ PARTIAL_COVERAGE。
func (tr *Trace) nextReason() string {
	if len(tr.Rounds) == 0 {
		return "PARTIAL_COVERAGE"
	}
	last := tr.Rounds[len(tr.Rounds)-1]
	if len(last.Eliminated) == 0 && len(last.Kept) == 0 {
		return "OFF_TOPIC"
	}
	allBlocked := len(last.Kept) == 0
	for _, r := range last.Eliminated {
		if !strings.HasPrefix(r, "H") && !strings.HasPrefix(r, "R") {
			allBlocked = false
		}
	}
	if allBlocked && len(last.Eliminated) > 0 {
		return "ALL_BLOCKED_REDLINE"
	}
	return "PARTIAL_COVERAGE"
}

// languageAliases 是语言名 → GitHub language: 限定词（按扫描顺序排列，
// 避免 map 遍历的随机顺序导致生态顺序不稳定）。
var languageAliases = [][2]string{
	{"python", "python"}, {"py", "python"},
	{"javascript", "javascript"}, {"js", "javascript"},
	{"node", "javascript"}, {"nodejs", "javascript"},
	{"typescript", "typescript"}, {"ts", "typescript"},
	{"go", "go"}, {"golang", "go"},
	{"rust", "rust"}, {"java", "java"}, {"ruby", "ruby"}, {"php", "php"},
	{"c++", "c++"}, {"cpp", "c++"}, {"shell", "shell"}, {"bash", "shell"},
}

// defaultEcosystems 是缺口没点名语言时的默认生态顺序。
var defaultEcosystems = []string{"python", "javascript", "go"}

// ecosystemOrder 定外部层的生态顺序：缺口关键词/原话里明确提到的
// 语言排最前，其余按默认顺序补齐；最多 3 个（≤3 次搜索额度）。
func ecosystemOrder(keywords []string, taskText string) []string {
	hay := strings.ToLower(strings.Join(keywords, " ") + " " + taskText)
	// 词元集合：按词精确匹配语言名（子串匹配会误伤：”js” 在 ”json” 里、
	// ”go” 在 ”good” 里）。词元字符集含 +#，使 c++/c# 保持完整。
	words := map[string]bool{}
	for _, w := range strings.FieldsFunc(hay, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '+' || r == '#')
	}) {
		words[w] = true
	}
	var order []string
	seen := map[string]bool{}
	add := func(eco string) {
		if eco != "" && !seen[eco] {
			seen[eco] = true
			order = append(order, eco)
		}
	}
	for _, kw := range keywords {
		for _, a := range languageAliases {
			if strings.EqualFold(kw, a[0]) {
				add(a[1])
			}
		}
	}
	for _, a := range languageAliases {
		if words[a[0]] {
			add(a[1])
		}
	}
	for _, eco := range defaultEcosystems {
		add(eco)
	}
	if len(order) > 3 {
		order = order[:3]
	}
	return order
}

// ecosystemQuery 把基础查询词限定到一个语言生态（GitHub language: 修饰符）。
func ecosystemQuery(base, eco string) string {
	if eco == "" {
		return base
	}
	return base + " language:" + eco
}

// refineQuery 换词重搜的机械办法：去掉最后一个关键词；只剩 1 个时追加 "cli"。
func refineQuery(query string) string {
	parts := strings.Fields(query)
	if len(parts) <= 1 {
		return strings.TrimSpace(query + " cli")
	}
	return strings.Join(parts[:len(parts)-1], " ")
}

// searchRound 跑一轮搜索并粗筛前十：Archived 的直接淘汰；其余按新鲜度
// 排序，留 3–5 个（星数 <50 的排最后，仍可留作候补）。
func (c *Client) searchRound(ctx context.Context, tr *Trace, query string, page int, reason string) (SearchRound, error) {
	round := SearchRound{
		Query: query, Page: page, Reason: reason,
		Eliminated: map[string]string{},
	}
	items, err := c.SearchRepos(ctx, query, page)
	if err != nil {
		return round, err
	}
	for _, it := range items {
		round.Top10 = append(round.Top10, it.FullName)
	}
	// 粗筛：Archived 直接淘汰；其余按新鲜度排序。
	sorted := sortCandidatesByFreshness(items)
	kept := 0
	for _, it := range sorted {
		if kept >= 5 {
			break
		}
		if it.Archived {
			round.Eliminated[it.FullName] = "archived"
			continue
		}
		// 已深挖过的候选不再重复深挖（同一缺口内去重）。
		if tr.hasCandidate(it.FullName) {
			continue
		}
		round.Kept = append(round.Kept, it.FullName)
		kept++
	}
	return round, nil
}

// hasCandidate 判断某候选是否已深挖过。
func (tr *Trace) hasCandidate(fullName string) bool {
	for _, ce := range tr.Candidates {
		if ce.FullName == fullName {
			return true
		}
	}
	return false
}

// firstPassing 返回第一个通过全部可查门槛与红线的候选（按深挖顺序，
// 即新鲜度顺序）。
func firstPassing(cands []CandidateEval) string {
	for _, ce := range cands {
		if ce.Passed {
			return ce.FullName
		}
	}
	return ""
}

// deepDiveRound 深挖一轮粗筛留下的候选：缓存命中 0 次核心调用；
// 否则每候选 3 次核心调用（详情/提交/Release），封顶 15 次/缺口；
// README/LICENSE/依赖清单走 raw 直读（不占核心额度）。
// 核心预算用完即停（剩下的候选留待下一轮，不报错）。
func (c *Client) deepDiveRound(ctx context.Context, tr *Trace, round SearchRound) error {
	for _, fullName := range round.Kept {
		// 核心预算：每候选 3 次，封顶 15 次/缺口。
		if tr.CoreUsed+3 > c.MaxCorePerGap {
			break
		}
		ce, err := c.DeepDive(ctx, tr, fullName)
		if err != nil {
			return err
		}
		tr.Candidates = append(tr.Candidates, *ce)
	}
	return nil
}

// DeepDive 深挖一个候选：缓存命中直接返回（0 次核心调用）；
// 否则详情 + 提交 + Release（3 次核心调用）+ raw 直读 README、
// LICENSE、依赖清单（package.json / pyproject.toml / go.mod，
// 按顺序命中即停），再做可查的硬门槛与红线判定。
func (c *Client) DeepDive(ctx context.Context, tr *Trace, fullName string) (*CandidateEval, error) {
	if ce, ok := c.loadCache(fullName); ok {
		return ce, nil
	}
	owner, repo, _ := strings.Cut(fullName, "/")
	ce := &CandidateEval{FullName: fullName}

	// 1 次：仓库详情。
	details, err := c.RepoDetailsCall(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	tr.CoreUsed++
	ce.Stars = details.Stars
	ce.Archived = details.Archived
	ce.License = details.LicenseSPDX
	ce.LastPush = details.PushedAt
	branch := details.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	ce.DefaultBranch = branch

	// 1 次：最近提交（per_page=5）。
	commits, err := c.RepoCommitsCall(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	tr.CoreUsed++
	if len(commits) > 0 {
		ce.LastCommit = commits[0]
	}

	// 1 次：最新 Release（无 Release 返回 nil，仍计 1 次）。
	rel, err := c.LatestReleaseCall(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	tr.CoreUsed++
	if rel != nil {
		ce.HasRelease = true
		ce.ReleaseTag = rel.TagName
	}

	// raw 直读（不占核心额度）：README、LICENSE、依赖清单。
	if body, ok, err := c.RawFile(ctx, owner, repo, branch, "README.md"); err == nil && ok {
		ce.ReadmeOK = true
		if f := scanSuspicious(body); f != "" {
			ce.SecurityFlag = f
		}
		if h := HeavyRuntimeNeeded(body); h != "" {
			ce.RuntimeNeed = h
		}
	}
	if _, ok, err := c.RawFile(ctx, owner, repo, branch, "LICENSE"); err == nil && ok {
		ce.LicenseFileOK = true
	} else if _, ok, err := c.RawFile(ctx, owner, repo, branch, "LICENSE.md"); err == nil && ok {
		ce.LicenseFileOK = true
	}
	for _, dep := range []string{"package.json", "pyproject.toml", "go.mod", "setup.py", "requirements.txt", "pom.xml", "build.gradle"} {
		body, ok, err := c.RawFile(ctx, owner, repo, branch, dep)
		if err != nil || !ok {
			continue
		}
		ce.DepsFile = dep
		if ce.RuntimeNeed == "" {
			ce.RuntimeNeed = InferRuntimeNeed(dep)
		}
		if f := scanSuspicious(body); f != "" {
			if ce.SecurityFlag == "" {
				ce.SecurityFlag = f
			} else {
				ce.SecurityFlag += "," + f
			}
		}
		if h := HeavyRuntimeNeeded(body); h != "" {
			ce.RuntimeNeed = h
		}
		break
	}
	// 依赖清单里没写明的，从 README 的安装说明里再看一次运行需求。
	if ce.RuntimeNeed == "" {
		ce.RuntimeNeed = "unknown"
	}

	EvaluateCandidate(ce, c.Now())
	c.saveCache(ce)
	return ce, nil
}
