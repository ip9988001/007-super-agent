package summary

// 接入（ingest）是用户 2026-11-24 叫停模型生成后的新流程：
// 简介由 Muse 用网页版 DeepSeek 一篇一篇写好，按 简介/ 布局放好，
// 本模块只做纯本地接入——8 条质检 + 脱敏输出检查 + 哈希 + 发号 +
// 写 manifest。不调任何模型，不烧 API token。
//
// 布局（见 简介/README.md）：顶层一个 简介/ 目录，下分
// 简介/wiki/<类别>/ 与 简介/l3/<类别>/，内部镜像原文的分类子目录，
// 简介文件名与原文完全同名。接入输入就是按这个布局放好的目录；
// 合格的复制进 canonical store（默认就是 简介/ 本身），
// 不合格的只记状态与原因，不静默入库。
//
// 重试熔断（规则⑧，接入版）：retry_count 记连续失败提交次数，
// 由 manifest 跨次带过来；同一篇第 3 次提交仍不合格，改标
// degraded_ok 并用纯本地规则提取兜底入库（标题 + 首段核心句 +
// TF-IDF 高频词，见 FallbackSummary，不调模型），记入 manifest。
// 成功一次则 retry_count 清零。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// IngestOptions 是接入的入参。
type IngestOptions struct {
	MemoryRoot string // 原文记忆目录（只读），如 /root/super-agent-007/data/memory
	InputDir   string // 外部写好的简介目录（简介/ 布局，只读不改）
	StoreDir   string // canonical store（简介/ 布局）；合格与兜底的简介落这里
	// ManifestPath 是写出的 manifest 位置；已存在时读它接续
	// retry_count 与 doc_id（prev-manifest）。
	ManifestPath string
	// SeedManifestPath 是可选的发号种子（如试点 manifest），只用于
	// 续编 doc_id，不改动它。为空则不读种子。
	SeedManifestPath string
	Embedder         Embedder // 规则⑦向量服务（服务器本地 Ollama）；nil 则规则⑦跳过并记明
	Now              time.Time
}

// IngestEntry 是一篇接入记录（manifest 的一条）。
type IngestEntry struct {
	DocID       string        `json:"doc_id"`
	Layer       string        `json:"layer"`
	Category    string        `json:"category"`
	SourcePath  string        `json:"source_path"`
	SummaryPath string        `json:"summary_path"`
	SourceHash  string        `json:"source_hash"`
	SummaryHash string        `json:"summary_hash"`
	Status      Status        `json:"status"`
	RetryCount  int           `json:"retry_count"`
	UpdatedAt   string        `json:"updated_at"`
	Checks      []CheckResult `json:"checks"`
	Note        string        `json:"note,omitempty"`
}

// IngestManifest 是接入总清单。
type IngestManifest struct {
	GeneratedAt string        `json:"generated_at"`
	InputDir    string        `json:"input_dir"`
	StoreDir    string        `json:"store_dir"`
	Total       int           `json:"total"`
	Accepted    int           `json:"accepted"`
	Pending     int           `json:"pending"`
	Degraded    int           `json:"degraded_ok"`
	Entries     []IngestEntry `json:"entries"`
}

// Reconcile 核对“写了多少篇 vs 入库多少篇”（线 B 修复轮第 4 棒）：
// 第 1 批 20 篇只入库 14 篇、静默漏 6 篇的根因不是某一次质检打回，
// 而是没有任何一道关卡核对总数——接入 CLI 对 pending 照常 exit 0、
// 回填对 skipped 照常 exit 0、巡检只查源→简介方向。第 1 批就是
// 这样静默漏过去的（6 篇只在 manifest 里记了 pending，无人报错）。
// 本函数是接入侧的核对：Total 必须等于 Accepted+Degraded+Pending
// （内部一致），且 Pending 必须为 0（写了多少就入库多少；打回的
// 改完重提、重跑，直到 Pending 清零）。调用方（cmd/summary-ingest）
// 在 Ingest 返回后调它，不通过就报错退出，不静默通过。
func (m *IngestManifest) Reconcile() error {
	if m == nil {
		return fmt.Errorf("ingest reconcile: manifest 为空")
	}
	if m.Total != m.Accepted+m.Pending+m.Degraded {
		return fmt.Errorf("ingest reconcile: manifest 计数不一致 total=%d accepted=%d pending=%d degraded_ok=%d",
			m.Total, m.Accepted, m.Pending, m.Degraded)
	}
	if m.Pending > 0 {
		return fmt.Errorf("ingest reconcile: input=%d stored=%d pending=%d：%d 篇未入库（见 manifest 里 pending 条目的 note），改完重提、重跑，直到 pending 清零",
			m.Total, m.Accepted+m.Degraded, m.Pending, m.Pending)
	}
	return nil
}

// layerCategory 从源文件相对路径拆 layer/category，
// 如 wiki/entities/x.md → wiki/entities，l3/lessons/x.md → l3/lessons。
func layerCategory(rel string) (layer, category string) {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) >= 2 {
		return parts[0], parts[1]
	}
	return "", ""
}

// docIDPrefix 按 layer 发号：wiki → W-####，l3 → L3-####。
func docIDPrefix(layer string) string {
	if layer == "l3" {
		return "L3-"
	}
	return "W-"
}

func parseDocID(id string) (prefix string, num int) {
	for _, p := range []string{"W-", "L3-"} {
		if strings.HasPrefix(id, p) {
			n, err := strconv.Atoi(strings.TrimPrefix(id, p))
			if err == nil {
				return p, n
			}
		}
	}
	return "", 0
}

// readManifestIDs 从一份 manifest 文件读 {source_path: {doc_id, retry_count, status}}，
// 文件不存在返回空表、不报错；格式错误才报错。
func readManifestIDs(path string) (map[string]IngestEntry, error) {
	out := map[string]IngestEntry{}
	if path == "" {
		return out, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, fmt.Errorf("ingest read manifest %s: %w", path, err)
	}
	var m IngestManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("ingest parse manifest %s: %w", path, err)
	}
	for _, e := range m.Entries {
		out[e.SourcePath] = e
	}
	return out, nil
}

// readPilotIDs 从试点 manifest（旧格式：entries[].doc_id/source_path）
// 读发号种子，只用于续编 doc_id。
func readPilotIDs(path string) (map[string]string, error) {
	out := map[string]string{}
	if path == "" {
		return out, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, fmt.Errorf("ingest read seed manifest %s: %w", path, err)
	}
	var m struct {
		Entries []struct {
			DocID      string `json:"doc_id"`
			SourcePath string `json:"source_path"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("ingest parse seed manifest %s: %w", path, err)
	}
	for _, e := range m.Entries {
		out[e.SourcePath] = e.DocID
	}
	return out, nil
}

// Ingest 跑一轮接入：读 InputDir 下全部简介，逐篇质检，合格入库，
// 不合格只记状态与原因，第 3 次连续不合格改标 degraded_ok 并用
// 本地规则兜底入库。返回 manifest（调用方负责写盘或由本函数写盘——
// 本函数直接写 ManifestPath）。全程不调模型。
func Ingest(ctx context.Context, opts IngestOptions) (*IngestManifest, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	prev, err := readManifestIDs(opts.ManifestPath)
	if err != nil {
		return nil, err
	}
	seed, err := readPilotIDs(opts.SeedManifestPath)
	if err != nil {
		return nil, err
	}
	// 发号计数器：prev + seed 里已出现的最大号，续编。
	nextNum := map[string]int{"W-": 0, "L3-": 0}
	for _, e := range prev {
		if p, n := parseDocID(e.DocID); n > nextNum[p] {
			nextNum[p] = n
		}
	}
	for _, id := range seed {
		if p, n := parseDocID(id); n > nextNum[p] {
			nextNum[p] = n
		}
	}

	// 规则⑦预载：store 里已入库简介的向量。预载条目用“相对路径”
	// 当 DocID——复提同一篇时，CheckCollision 按 DocID 跳过自己
	// 的旧简介，不会跟自己撞车（用 W-#### 当键会跳不过）。
	cc := NewCollisionChecker(opts.Embedder)
	if opts.Embedder != nil {
		_ = filepath.WalkDir(opts.StoreDir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || filepath.Ext(path) != ".md" {
				return nil
			}
			base := filepath.Base(path)
			if base == "manifest.json" || base == "README.md" {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			s, err := Parse(string(data))
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(opts.StoreDir, path)
			_ = cc.Accept(ctx, dirOf(filepath.ToSlash(rel)), filepath.ToSlash(rel), s.Summary)
			return nil
		})
	}

	// 收集输入简介（简介/ 布局，相对路径即源文件相对路径）。
	var rels []string
	err = filepath.WalkDir(opts.InputDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".md" {
			return nil
		}
		rel, err := filepath.Rel(opts.InputDir, path)
		if err != nil {
			return nil
		}
		rels = append(rels, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ingest walk input %s: %w", opts.InputDir, err)
	}
	sort.Strings(rels)

	m := &IngestManifest{
		GeneratedAt: now.Format(time.RFC3339),
		InputDir:    opts.InputDir,
		StoreDir:    opts.StoreDir,
	}
	for _, rel := range rels {
		entry := ingestOne(ctx, opts, cc, rel, prev, seed, nextNum, now)
		m.Entries = append(m.Entries, entry)
		m.Total++
		switch entry.Status {
		case StatusOK:
			m.Accepted++
		case StatusDegradedOK:
			m.Degraded++
		default:
			m.Pending++
		}
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("ingest marshal manifest: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(opts.ManifestPath), 0o755); err != nil {
		return nil, fmt.Errorf("ingest mkdir manifest dir: %w", err)
	}
	if err := os.WriteFile(opts.ManifestPath, data, 0o644); err != nil {
		return nil, fmt.Errorf("ingest write manifest: %w", err)
	}
	return m, nil
}

// ingestOne 接入单篇简介。
func ingestOne(ctx context.Context, opts IngestOptions, cc *CollisionChecker, rel string, prev map[string]IngestEntry, seed map[string]string, nextNum map[string]int, now time.Time) IngestEntry {
	layer, category := layerCategory(rel)
	entry := IngestEntry{
		SourcePath: rel,
		Layer:      layer,
		Category:   category,
		UpdatedAt:  now.Format(time.RFC3339),
	}
	// 发号：已有的沿用（prev 优先，其次 seed），新的续编。
	if p, ok := prev[rel]; ok && p.DocID != "" {
		entry.DocID = p.DocID
		entry.RetryCount = p.RetryCount
	} else if id, ok := seed[rel]; ok && id != "" {
		entry.DocID = id
	} else {
		prefix := docIDPrefix(layer)
		nextNum[prefix]++
		entry.DocID = fmt.Sprintf("%s%04d", prefix, nextNum[prefix])
	}
	storeBase := filepath.Base(opts.StoreDir)

	rawSummary, err := os.ReadFile(filepath.Join(opts.InputDir, rel))
	if err != nil {
		entry.Status = StatusPending
		entry.Note = fmt.Sprintf("读简介失败：%v", err)
		return entry
	}
	source, err := os.ReadFile(filepath.Join(opts.MemoryRoot, rel))
	if err != nil {
		entry.Status = StatusPending
		entry.Note = fmt.Sprintf("源文件不存在或不可读（%s），无法算 source_hash：%v", filepath.Join(opts.MemoryRoot, rel), err)
		return entry
	}
	entry.SourceHash = SHA256Hex(source)
	title := TitleFromSource(rel, source)

	cand, perr := Parse(string(rawSummary))
	if perr != nil {
		entry.RetryCount++
		entry.Checks = []CheckResult{{Rule: 0, Name: "简介格式解析", Pass: false, Reason: perr.Error()}}
		entry.Note = "简介不是两行格式（summary:/triggers:），打回"
		return finishFailed(ctx, opts, cc, entry, rel, title, source)
	}
	cand.DocID = entry.DocID
	cand.SourcePath = rel
	cand.SourceHash = entry.SourceHash
	NormalizeSlots(&cand)
	entry.SummaryHash = SHA256Hex(rawSummary)
	checks := CheckAll(cand, string(source), title)
	if AllPass(checks) && cc != nil {
		// 查重的 DocID 传相对路径，与预载键一致，复提时跳过自己的旧简介。
		ccheck := cc.CheckCollision(ctx, dirOf(rel), rel, cand.Summary)
		checks = append(checks, ccheck)
	}
	entry.Checks = checks
	if AllPass(checks) {
		// 合格入库：原文原样复制进 store（镜像布局、同名）。
		dest := filepath.Join(opts.StoreDir, rel)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			entry.Status = StatusPending
			entry.Note = fmt.Sprintf("建 store 目录失败：%v", err)
			return entry
		}
		if err := os.WriteFile(dest, rawSummary, 0o644); err != nil {
			entry.Status = StatusPending
			entry.Note = fmt.Sprintf("写 store 失败：%v", err)
			return entry
		}
		entry.Status = StatusOK
		entry.RetryCount = 0
		entry.SummaryPath = filepath.Join(storeBase, filepath.ToSlash(rel))
		_ = cc.Accept(ctx, dirOf(rel), entry.DocID, cand.Summary)
		return entry
	}
	// 不合格：记失败次数，不入库。
	entry.RetryCount++
	entry.Note = "质检打回：" + strings.Join(FailedReasons(checks), "；")
	return finishFailed(ctx, opts, cc, entry, rel, title, source)
}

// finishFailed 处理不合格篇：第 3 次连续失败改标 degraded_ok 并用
// 纯本地规则兜底入库；未满 3 次只记 pending，不入库。
func finishFailed(ctx context.Context, opts IngestOptions, cc *CollisionChecker, entry IngestEntry, rel, title string, source []byte) IngestEntry {
	if entry.RetryCount < MaxRetries {
		entry.Status = StatusPending
		return entry
	}
	fb := FallbackSummary(rel, title, string(source))
	fb.DocID = entry.DocID
	fb.SourcePath = rel
	fb.SourceHash = entry.SourceHash
	NormalizeSlots(&fb)
	text := Format(fb)
	dest := filepath.Join(opts.StoreDir, rel)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		entry.Status = StatusPending
		entry.Note += "；第 3 次失败应兜底入库，但建 store 目录失败：" + err.Error()
		return entry
	}
	if err := os.WriteFile(dest, []byte(text), 0o644); err != nil {
		entry.Status = StatusPending
		entry.Note += "；第 3 次失败应兜底入库，但写 store 失败：" + err.Error()
		return entry
	}
	entry.Status = StatusDegradedOK
	entry.SummaryHash = SHA256Hex([]byte(text))
	entry.SummaryPath = filepath.Join(filepath.Base(opts.StoreDir), filepath.ToSlash(rel))
	entry.Checks = CheckAll(fb, string(source), title)
	entry.Note += "；第 3 次提交仍不合格，改标 degraded_ok，纯本地规则兜底入库（标题+首段核心句+高频词，不调模型）"
	_ = cc.Accept(ctx, dirOf(rel), entry.DocID, fb.Summary)
	return entry
}
