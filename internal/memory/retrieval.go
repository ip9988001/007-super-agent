package memory

import (
	"fmt"
	"hash/fnv"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 两阶段召回（线 B 第二阶段第 3 棒，v2.1 §3 一字不差 + §7 分层参数）。
//
// 第一段 40 席：简介双路 26 席起（含回补）+ 全文精确 BM25 直通车
// 0～5 席 + reference 通道最多 6 席 + L1 通道最多 3 席；全局按
// doc_id 去重补齐 40 篇；同一文档被多路命中只占 1 席，但保留各路
// 原始排名供第二段融合。（2026-11-24 调参：轻量通道扩容 4 席，
// 简介回补下限 30→26 平衡总席位。）
// 第二段：加权 RRF 融合三路（简介词法、简介语义、对 40 篇候选即时
// 算的全文 BM25）+ 无简介层缺腿补偿 + 分类先验 ±10% 以内；融合前过拒推阈值
// （有向量层三选一保留，无向量层独立门槛），最终允许交付
// 0～5 张卡片（0 张即拒推）；精确直通车保送；同层同类去重；
// 每张卡片只含标题、一句话简介和编号，Top 5 卡片总量 ≤380 tokens。
//
// 例外（第一阶段复核结论，见 phase1-doc §8 第 2 条）：
// wiki/lint-report、wiki/log、wiki/index 三个元文件在旧词法下
// 单问 820 分长期霸榜前 3，是噪音不是记忆——全文腿（直通车、
// 回补、reference/L1 之外的全文 BM25 排名）排除这三篇；
// 简介腿不受影响（它们本就没有简介）。

// 两阶段召回的硬参数。
const (
	Stage1TotalSeats     = 40 // 第一段总席位
	Stage1SummarySeats   = 26 // 简介双路席位（含回补；2026-11-24 调参 30→26）
	Stage1SummaryLexTop  = 20 // 简介词法腿取前 20
	Stage1SummarySemTop  = 20 // 简介语义腿取前 20
	Stage1ExpressSeats   = 5  // 直通车最多 5 席
	Stage1ReferenceSeats = 6  // reference 通道最多 6 席（2026-11-24 调参 3→6）
	Stage1L1Seats        = 3  // L1 通道最多 3 席（2026-11-24 调参 2→3）

	MaxDeliveredCards  = 5   // 最终最多交付 5 张卡片
	MaxDeliveredTokens = 380 // Top 5 卡片总量上限（EstimateTokens 口径；2026-11-24 调参 400→380）

	RRFK              = 60.0 // RRF 阻尼常数
	RRFWeightLexical  = 1.0  // 简介词法腿权重
	RRFWeightSemantic = 1.0  // 简介语义腿权重
	RRFWeightFullText = 1.0  // 全文 BM25 腿权重

	// 拒推阈值（2026-11-24 调参，Gemini 方案 1.2，按 70 题实测分布标定）：
	// 有向量层（wiki/l3）三选一保留：语义 ≥0.55，或词法（全文/简介
	// 取大者）≥19.0，或语义 ≥0.50 且词法 ≥10.0；无向量层
	// （reference/l1）独立门槛 12.0 + 通道/全文前 3 保护。
	// BM25 基准按库规模缩放（scaleBM25，生产库 N≈1693 时因子 ≈1）；
	// 语义余弦是归一化相似度，不随库规模变，不缩放。
	// 旧 15.0/0.30 口径失效原因：小模型余弦基线高（负样本最高相似度
	// 中位 0.40），0.30 门槛形同虚设；N10/N19 靠单腿词法 15.5–18.6
	// 穿透 15.0 门槛。
	RejectMinCosine    = 0.55 // 有向量层语义硬过线
	RejectMinBM25      = 19.0 // 有向量层词法硬过线（基准）
	RejectJointCosine  = 0.50 // 双信号联合过线：语义门槛
	RejectJointBM25    = 10.0 // 双信号联合过线：BM25 门槛（基准）
	RejectMinBM25NoVec = 12.0 // 无向量层独立 BM25 门槛（基准）
	GrayZoneMaxCards   = 2    // 灰区（边界带）最多交付 2 张
	NoSummaryRRFBoost  = 2.4  // 无简介层单腿 RRF 补偿系数

	// 直通车保送阈值（全局全文 BM25 口径，N≈1694、罕见词
	// IDF≈7：单精确 token 约 5–7 分，双 token 约 10 分以上）：
	// 全文 BM25 ≥ 12.0 且 ≥ 第 2 名得分 ×1.5。注意：40 篇候选
	// 子集的 BM25（N=40，IDF≈3.3）只用于第二段 RRF 的全文腿
	// 排名，绝不用于阈值——池子小时子集 IDF 会塌（N=1 时
	// IDF≈0.29，满分匹配也只有 ~1.3 分，会把真目标拒掉）。
	// 12.0 是生产库口径；小库按 scaleBM25 同一 IDF 因子缩放
	// （见 expressWinner），否则小库精确命中够不着保送线。
	ExpressMinBM25 = 12.0
	// 直通车放行阈值（全局全文 BM25 口径，N≈1694、罕见词
	// IDF≈7）：无精确 token 时，全文 BM25 顶分过此值才放行。
	// 2026-11-24 调参：保持 8.0 不动（Gemini 只让动 ExpressMinBM25）。
	ExpressOpenBM25 = 8.0
	// reference / L1 通道的最低门槛（BM25 得分，L1 为乘时间衰减后）。
	ReferenceMinBM25 = 1.0
	L1MinBM25        = 1.0

	// 同层同类去重阈值：简介向量余弦 >0.90 只留排名靠前者。
	// （2026-11-24 调参 0.85→0.90：小模型余弦空间压缩，同主题
	// 互补文档易落在 0.85–0.89 之间，旧线会误去重。）
	DedupCosine = 0.90
	// superseded_by 降权：旧文档 RRF 得分只剩 20%（降权 80%）。
	SupersededFactor = 0.2
	// 语义腿最低相似度（与旧 SemanticMinSim 同一口径）。
	// （2026-11-24 调参 0.30→0.42：0.30–0.45 是纯噪声区，
	// 放进来占 30 席还拉低排序纯度；正样本最低分 0.50，留 0.08 边际。）
	SemanticLegMinSim = 0.42
)

// historyWords 是显式问历史的词：查询含这些词时，不过期前缀
// 也不降权（用户就是要找旧版）。
var historyWords = []string{"历史", "以前", "旧版", "为什么改"}

// metaExcludedSources 是全文腿排除的元文件（见包头注释）。
var metaExcludedSources = map[string]bool{
	"wiki/lint-report": true,
	"wiki/log":         true,
	"wiki/index":       true,
}

// categoryPrior 是分类先验（v2.1 §3：±10% 以内）：精炼过的层
// （wiki/l3）取 1.0，参考协议层 0.95（多为流程骨架），易变的
// 当日日志层 0.9。乘在 RRF 得分上，恒在 [0.9, 1.1] 内。
func categoryPrior(layer string) float64 {
	switch layer {
	case "reference":
		return 0.95
	case "l1":
		return 0.90
	default:
		return 1.0
	}
}

// RecallCard 是一张交付/候选卡片：只含标题、一句话简介和编号，
// 另带各路原始排名与得分（供融合、记账与评测用，不进卡片文本）。
type RecallCard struct {
	DocID        string `json:"doc_id"`
	SourcePath   string `json:"source_path"`
	Layer        string `json:"layer"`
	Category     string `json:"category"`
	Title        string `json:"title"`   // 已含过期前缀（如适用）
	Summary      string `json:"summary"` // 一句话简介
	SupersededBy string `json:"superseded_by"`

	PoolRank int `json:"pool_rank"` // 第一段池内名次（1 起）
	LexRank  int `json:"lex_rank"`  // 简介词法腿名次，0=未进
	SemRank  int `json:"sem_rank"`  // 简介语义腿名次，0=未进
	FullRank int `json:"full_rank"` // 全文 BM25 腿名次，0=未进

	SemCos   float64 `json:"sem_cos"`   // 简介语义余弦（无向量为 0）
	FullBM25 float64 `json:"full_bm25"` // 全文 BM25 得分
	SumBM25  float64 `json:"sum_bm25"`  // 简介 FTS BM25 正分
	RRF      float64 `json:"rrf"`
	Express  bool    `json:"express"` // 是否直通车保送
	// FromLightChannel 报告本卡是否经 reference/L1 轻量通道进池
	// （2026-11-24 调参：无向量层门槛里，通道进池的卡不以池内
	// 全文子集排名论生死，见 fuseSecondStage）。
	FromLightChannel bool `json:"from_light_channel"`
}

// Display 是卡片交付文本：标题：一句话简介。
// 有 doc_id 用 doc_id，没入库的文档用源路径当编号。
func (c RecallCard) Display() string {
	id := c.DocID
	if id == "" {
		id = c.SourcePath
	}
	return "【" + id + "】" + c.Title + "：" + c.Summary
}

// Tokens 是卡片文本的 token 估算（与 registry.EstimateTokens
// 同一口径：汉字 0.6、其余字符 0.25，见 retrieval_test.go 的
// 对照单测）。
func (c RecallCard) Tokens() int { return estimateTokens(c.Display()) }

// estimateTokens 是 registry.EstimateTokens 的同口径本地实现
// （memory 不引 registry，避免底层包依赖上层包）。
func estimateTokens(s string) int {
	total := 0.0
	for _, r := range s {
		if (r >= 0x4E00 && r <= 0x9FFF) ||
			(r >= 0x3400 && r <= 0x4DBF) ||
			(r >= 0xF900 && r <= 0xFAFF) {
			total += 0.6
		} else {
			total += 0.25
		}
	}
	return int(total + 0.5)
}

// TwoStageResult 是一次两阶段召回的结果。
type TwoStageResult struct {
	Query         string       `json:"query"`
	ExpandedQuery string       `json:"expanded_query"`
	Cards         []RecallCard `json:"cards"` // 交付 0～5 张
	Pool          []RecallCard `json:"pool"`  // 第一段 40 篇候选池
	MaxCos        float64      `json:"max_cos"`
	MaxFullBM25   float64      `json:"max_full_bm25"`
	MaxSumBM25    float64      `json:"max_sum_bm25"`
	SemanticLegOn bool         `json:"semantic_leg_on"`
	LatencyMs     int64        `json:"latency_ms"`
	// 置信度（2026-11-24 调参，Gemini 方案 1.3 + 第 3 节）：
	// 只做到结果结构返回 + front 日志打印；recall_log 加列
	// （confidence_band/top1_sem_cos/top1_bm25/rejected_cnt）
	// 需正规迁移，列为遗留缺口，不在本轮硬改 schema。
	ConfidenceBand string  `json:"confidence_band"` // high / gray / rejected
	Top1SemCos     float64 `json:"top1_sem_cos"`    // 交付头名语义相似度（0 张时为 0）
	Top1BM25       float64 `json:"top1_bm25"`       // 交付头名 BM25（全文/简介取大者，0 张时为 0）
	RejectedCount  int     `json:"rejected_count"`  // 第二段拒推丢弃的卡片数
}

// fullDoc 是磁盘记忆里的一篇全文文档（第一段各腿的计分原料）。
type fullDoc struct {
	Source   string
	Layer    string
	Category string
	Title    string
	Text     string   // 全文（L1 为全文件文本）
	Headers  string   // reference 腿：标题 + 章节头
	Lines    []string // L1 行（仅 l1 层）
	Summary  string   // 展示用一行（## 内容节或正文前几行）
	Mtime    time.Time
}

// corpusSnapshot 是记忆目录的一次快照：全文 BM25 索引 +
// reference 索引 + L1 行索引，指纹命中时复用（70 条评测每条
// 都扫 1694 篇全文的话，分词成本会把 P95 顶上去；指纹是文件
// 清单 + 大小 + mtime，写 Wiki/精炼都会改变指纹，天然失效）。
type corpusSnapshot struct {
	fingerprint string
	docs        []fullDoc
	bySource    map[string]int
	full        *bm25Index
	ref         *bm25Index
	refDocIdx   []int // ref 索引的文档在 docs 里的下标
	l1Line      *bm25Index
	l1LineOwner []string // 行索引每行的所属源
	l1LineNo    []int
}

// snapshotCorpus 扫记忆目录建快照（跳过三个元文件）。
func (s *Store) snapshotCorpus() (*corpusSnapshot, error) {
	fp, docs, err := s.scanCorpusDocs()
	if err != nil {
		return nil, err
	}
	snap := &corpusSnapshot{fingerprint: fp, docs: docs, bySource: map[string]int{}}
	for i, d := range docs {
		snap.bySource[d.Source] = i
	}
	// 全文索引（全部文档）。
	ids := make([]string, len(docs))
	texts := make([]string, len(docs))
	for i, d := range docs {
		ids[i] = d.Source
		texts[i] = d.Text
	}
	snap.full = newBM25Index(ids, texts)
	// reference 索引（标题 + 章节头）。
	var refIDs, refTexts []string
	for i, d := range docs {
		if d.Layer != "reference" {
			continue
		}
		refIDs = append(refIDs, d.Source)
		refTexts = append(refTexts, d.Title+"\n"+d.Headers)
		snap.refDocIdx = append(snap.refDocIdx, i)
	}
	snap.ref = newBM25Index(refIDs, refTexts)
	// L1 行索引（每行一篇，行号记 owner 里）。
	var lineIDs, lineTexts []string
	for _, d := range docs {
		if d.Layer != "l1" {
			continue
		}
		for ln, line := range d.Lines {
			if strings.TrimSpace(line) == "" {
				continue
			}
			lineIDs = append(lineIDs, fmt.Sprintf("%s\x00%d", d.Source, ln))
			lineTexts = append(lineTexts, line)
			snap.l1LineOwner = append(snap.l1LineOwner, d.Source)
			snap.l1LineNo = append(snap.l1LineNo, ln)
		}
	}
	snap.l1Line = newBM25Index(lineIDs, lineTexts)
	return snap, nil
}

// corpus 取快照（指纹命中复用，否则重建）。
func (s *Store) corpus() (*corpusSnapshot, error) {
	fp, err := s.corpusFingerprint()
	if err != nil {
		return nil, err
	}
	s.corpusMu.Lock()
	defer s.corpusMu.Unlock()
	if s.corpusCache != nil && s.corpusCache.fingerprint == fp {
		return s.corpusCache, nil
	}
	snap, err := s.snapshotCorpus()
	if err != nil {
		return nil, err
	}
	if snap.fingerprint != fp {
		// 扫描中途文件变了：本次结果仍可用，下次重建。
		log.Printf("memory corpus: 指纹在扫描中变化，下次重建")
	}
	s.corpusCache = snap
	return snap, nil
}

// corpusFingerprint 只 stat 不读文件：相对路径 + 大小 + mtime。
func (s *Store) corpusFingerprint() (string, error) {
	h := fnv.New64a()
	add := func(rel string, size int64, mtime int64) {
		fmt.Fprintf(h, "%s|%d|%d;", rel, size, mtime)
	}
	for _, root := range []string{
		filepath.Join(s.root, "wiki"),
		filepath.Join(s.root, "l3"),
		filepath.Join(s.root, "l2"),
		filepath.Join(s.root, "reference"),
		filepath.Join(s.root, "l1"),
	} {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			name := d.Name()
			if !(strings.HasSuffix(name, ".md") || strings.HasSuffix(name, ".log")) {
				return nil
			}
			rel, _ := filepath.Rel(s.root, path)
			rel = strings.TrimSuffix(rel, ".md")
			if metaExcludedSources[rel] {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			add(rel, info.Size(), info.ModTime().UnixNano())
			return nil
		})
		if err != nil {
			return "", fmt.Errorf("memory corpus fingerprint: %w", err)
		}
	}
	return fmt.Sprintf("%x", h.Sum64()), nil
}

// scanCorpusDocs 读全部记忆文件（跳过元文件），返回指纹与文档。
func (s *Store) scanCorpusDocs() (string, []fullDoc, error) {
	h := fnv.New64a()
	var docs []fullDoc
	for _, item := range []struct {
		root   string
		layer  string
		suffix string
	}{
		{filepath.Join(s.root, "wiki"), "wiki", ".md"},
		{filepath.Join(s.root, "l3"), "l3", ".md"},
		{filepath.Join(s.root, "l2"), "l2", ".md"},
		{filepath.Join(s.root, "reference"), "reference", ".md"},
		{filepath.Join(s.root, "l1"), "l1", ".log"},
	} {
		root, layer, suffix := item.root, item.layer, item.suffix
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), suffix) {
				return nil
			}
			rel, _ := filepath.Rel(s.root, path)
			src := strings.TrimSuffix(rel, ".md")
			if metaExcludedSources[src] {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			info, _ := d.Info()
			var mt time.Time
			if info != nil {
				mt = info.ModTime()
			}
			fmt.Fprintf(h, "%s|%d|%d;", src, len(data), mt.UnixNano())
			text := string(data)
			fd := fullDoc{
				Source: src, Layer: layer, Mtime: mt,
				Title: docTitle(src, text), Text: text,
			}
			parts := strings.Split(strings.TrimPrefix(src, layer+"/"), "/")
			if len(parts) >= 2 {
				fd.Category = parts[1]
			} else if len(parts) == 1 {
				fd.Category = parts[0]
			}
			if layer == "reference" {
				fd.Headers = sectionHeaders(text)
			}
			if layer == "l1" {
				fd.Lines = strings.Split(text, "\n")
			}
			fd.Summary = contentSection(text)
			if fd.Summary == "" {
				fd.Summary = firstBodyText(text)
			}
			docs = append(docs, fd)
			return nil
		})
		if err != nil {
			return "", nil, fmt.Errorf("memory corpus scan %s: %w", root, err)
		}
	}
	return fmt.Sprintf("%x", h.Sum64()), docs, nil
}

// docTitle 从正文取标题：首个 "# " 标题优先，否则文件名
// （去掉.md 与开头的 YYYY-MM-DD- 日期前缀）。
func docTitle(source, text string) string {
	for _, ln := range strings.Split(text, "\n") {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "# ") {
			if t := strings.TrimSpace(strings.TrimPrefix(ln, "# ")); t != "" {
				return t
			}
		}
	}
	base := source
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	if len(base) > 11 && base[4] == '-' && base[7] == '-' {
		base = base[11:]
	}
	return base
}

// sectionHeaders 取文档的全部 ## 章节头（reference 腿计分文本）。
func sectionHeaders(text string) string {
	var b strings.Builder
	for _, ln := range strings.Split(text, "\n") {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "## ") {
			b.WriteString(strings.TrimPrefix(ln, "## "))
			b.WriteString("\n")
		}
	}
	return b.String()
}

// ---- 第一段：40 席 ----

// RecallTwoStage 是两阶段召回的 Store 入口：须先接简介主库
// （WithSummaryStore），否则返回错误、调用方走旧 Recall 路径。
func (s *Store) RecallTwoStage(query string) (*TwoStageResult, error) {
	if s.summaryStore == nil {
		return nil, fmt.Errorf("memory recall two-stage: 未接简介主库")
	}
	return s.recallTwoStage(query, time.Now())
}

// RecallTwoStageCandidates 是评测专用入口：返回第一段 40 篇候选池
// （按池内名次排序），仅供黄金评测集跑 Recall@40 用。
func (s *Store) RecallTwoStageCandidates(query string) ([]RecallCard, error) {
	res, err := s.RecallTwoStage(query)
	if err != nil {
		return nil, err
	}
	return res.Pool, nil
}

// HasSummaryStore 报告是否接了简介主库（接线点据此决定走新路径
// 还是旧 Recall 路径）。
func (s *Store) HasSummaryStore() bool { return s.summaryStore != nil }

func (s *Store) recallTwoStage(query string, now time.Time) (*TwoStageResult, error) {
	t0 := time.Now()
	res := &TwoStageResult{Query: query}
	expanded, _ := expandAbbreviations(query)
	res.ExpandedQuery = expanded
	qTerms := bm25Terms(expanded)
	exact := exactTokens(query)
	if len(qTerms) == 0 && len(exact) == 0 {
		res.LatencyMs = time.Since(t0).Milliseconds()
		return res, nil
	}
	snap, err := s.corpus()
	if err != nil {
		return nil, err
	}
	// 全文 BM25 全排名（直通车、回补、第二段三路之一共用）。
	fullHits := snap.full.score(qTerms, bm25K1, bm25FullB)
	fullRank := map[string]int{}
	fullScore := map[string]float64{}
	for i, h := range fullHits {
		fullRank[h.ID] = i + 1
		fullScore[h.ID] = h.Score
	}
	for _, h := range fullHits {
		if h.Score > res.MaxFullBM25 {
			res.MaxFullBM25 = h.Score
		}
	}

	// 池：doc_id（无 doc_id 时用源路径）→ 卡片半成品。
	pool := map[string]*RecallCard{}
	note := func(source string, set func(c *RecallCard)) {
		c, ok := pool[source]
		if !ok {
			c = newPoolCard(s, snap, source)
			pool[source] = c
		}
		set(c)
	}

	// 简介双路 26 席：词法腿（FTS5，b=0.30）+ 语义腿（简介向量）。
	// 池键统一用磁盘形式（去 .md）：主库 SourcePath 带 .md，
	// 磁盘扫描去 .md（与旧 Recall 的源标识一致），不归一会
	// 同一篇文档进池两次。
	lexHits, err := s.summaryStore.SearchFTS(qTerms, Stage1SummaryLexTop)
	if err != nil {
		return nil, fmt.Errorf("memory two-stage fts: %w", err)
	}
	for i, h := range lexHits {
		doc, derr := s.summaryStore.GetDoc(h.DocID)
		if derr != nil {
			continue
		}
		if h.Score > res.MaxSumBM25 {
			res.MaxSumBM25 = h.Score
		}
		note(strings.TrimSuffix(doc.SourcePath, ".md"), func(c *RecallCard) {
			c.LexRank = i + 1
			c.SumBM25 = h.Score
		})
	}
	if SemanticEnabled && s.summaryEmbed != nil {
		res.SemanticLegOn = true
		semHits, serr := s.summarySemantic(expanded)
		if serr != nil {
			log.Printf("memory two-stage semantic: %v（本腿置空）", serr)
			res.SemanticLegOn = false
		}
		for i, h := range semHits {
			doc, derr := s.summaryStore.GetDoc(h.DocID)
			if derr != nil {
				continue
			}
			if h.Cos > res.MaxCos {
				res.MaxCos = h.Cos
			}
			note(strings.TrimSuffix(doc.SourcePath, ".md"), func(c *RecallCard) {
				c.SemRank = i + 1
				c.SemCos = h.Cos
			})
		}
	}
	// 双路不足 26 席时，全文 BM25 回补。（2026-11-24 调参：轻量通道
	// 扩容 4 席后回补下限 30→26，平衡 40 篇总席位；标准/口语/
	// 精确 Recall@40 已是 100%，有冗余。）
	if len(pool) < Stage1SummarySeats {
		for _, h := range fullHits {
			if len(pool) >= Stage1SummarySeats {
				break
			}
			if _, ok := pool[h.ID]; ok {
				continue
			}
			note(h.ID, func(c *RecallCard) {
				c.FullRank = fullRank[h.ID]
				c.FullBM25 = h.Score
			})
		}
	}

	// 直通车 0～5 席：查询含精确 token，或全文顶分过阈值。
	if seats := expressSeats(query, exact, snap, fullHits, fullScore); len(seats) > 0 {
		for _, src := range seats {
			note(src, func(c *RecallCard) {
				c.FullRank = fullRank[src]
				c.FullBM25 = fullScore[src]
			})
		}
	}

	// reference 通道最多 6 席：标题 + 章节头 BM25，设最低门槛。
	// （2026-11-24 调参 3→6：X08 在 3 席通道里排第 4 名及以后被截断。）
	// 通道进池的卡置 FromLightChannel，见 fuseSecondStage 无向量层门槛。
	if snap.ref != nil {
		refHits := snap.ref.score(qTerms, bm25K1, bm25FullB)
		n := 0
		for _, h := range refHits {
			if n >= Stage1ReferenceSeats {
				break
			}
			if h.Score < ReferenceMinBM25 {
				break
			}
			note(h.ID, func(c *RecallCard) { c.FromLightChannel = true })
			n++
		}
	}

	// L1 通道最多 3 席：行 BM25 得分乘时间衰减，设最低门槛。
	// （2026-11-24 调参 2→3。）通道进池的卡置 FromLightChannel。
	if snap.l1Line != nil {
		lineHits := snap.l1Line.score(qTerms, bm25K1, bm25FullB)
		best := map[string]float64{}
		for _, h := range lineHits {
			src := strings.SplitN(h.ID, "\x00", 2)[0]
			ageDays := now.Sub(snap.docs[snap.bySource[src]].Mtime).Hours() / 24
			if ageDays < 0 {
				ageDays = 0
			}
			decayed := h.Score / (1 + ageDays)
			if decayed > best[src] {
				best[src] = decayed
			}
		}
		type l1cand struct {
			src   string
			score float64
		}
		var cands []l1cand
		for src, sc := range best {
			if sc >= L1MinBM25 {
				cands = append(cands, l1cand{src, sc})
			}
		}
		sort.Slice(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
		for i, cd := range cands {
			if i >= Stage1L1Seats {
				break
			}
			note(cd.src, func(c *RecallCard) { c.FromLightChannel = true })
		}
	}

	// superseded_by：命中旧文档时，强制把新文档拉入候选池。
	for _, c := range pool {
		if c.SupersededBy == "" {
			continue
		}
		if nd, err := s.summaryStore.GetDoc(c.SupersededBy); err == nil {
			note(strings.TrimSuffix(nd.SourcePath, ".md"), func(nc *RecallCard) {})
		}
	}

	// 全局按 doc_id 去重补齐 40 篇（全文 BM25 排名顺序）。
	if len(pool) < Stage1TotalSeats {
		for _, h := range fullHits {
			if len(pool) >= Stage1TotalSeats {
				break
			}
			if _, ok := pool[h.ID]; ok {
				continue
			}
			note(h.ID, func(c *RecallCard) {
				c.FullRank = fullRank[h.ID]
				c.FullBM25 = h.Score
			})
		}
	}

	// 池排序：各路最好名次最小者在前（同名次按源路径），供评测
	// rank_40 与记账用；Recall@40 只看是否在池内。
	poolList := make([]RecallCard, 0, len(pool))
	for _, c := range pool {
		poolList = append(poolList, *c)
	}
	for i := range poolList {
		poolList[i].PoolRank = bestRank(&poolList[i])
	}
	sort.Slice(poolList, func(i, j int) bool {
		if poolList[i].PoolRank != poolList[j].PoolRank {
			return poolList[i].PoolRank < poolList[j].PoolRank
		}
		return poolList[i].SourcePath < poolList[j].SourcePath
	})
	if len(poolList) > Stage1TotalSeats {
		poolList = poolList[:Stage1TotalSeats]
	}
	for i := range poolList {
		poolList[i].PoolRank = i + 1
	}
	res.Pool = poolList

	// 第二段融合（全局全文 BM25 分数一并传入，供拒推阈值与
	// 直通车保送用绝对口径判断；子集 BM25 只排 RRF 全文腿）。
	// 置信度结果一并回填，供记账与评测用。
	cards, outcome := s.fuseSecondStage(query, qTerms, exact, snap, poolList, fullScore, now)
	res.Cards = cards
	res.ConfidenceBand = outcome.ConfidenceBand
	res.Top1SemCos = outcome.Top1SemCos
	res.Top1BM25 = outcome.Top1BM25
	res.RejectedCount = outcome.RejectedCount
	res.LatencyMs = time.Since(t0).Milliseconds()
	return res, nil
}

// bestRank 是卡片在各路里的最好名次（未进的腿不计；
// 全文回补/补齐的文档用全文名次）。
func bestRank(c *RecallCard) int {
	best := 0
	for _, r := range []int{c.LexRank, c.SemRank, c.FullRank} {
		if r > 0 && (best == 0 || r < best) {
			best = r
		}
	}
	if best == 0 {
		return 1 << 30
	}
	return best
}

// newPoolCard 从快照 + 主库拼一张池卡片（标题/简介/编号）。
func newPoolCard(s *Store, snap *corpusSnapshot, source string) *RecallCard {
	c := &RecallCard{SourcePath: source}
	if idx, ok := snap.bySource[source]; ok {
		d := snap.docs[idx]
		c.Layer, c.Category = d.Layer, d.Category
		c.Title = d.Title
		c.Summary = truncateRunes(d.Summary, 120)
	}
	if s.summaryStore != nil {
		doc, err := s.summaryStore.GetBySource(source)
		if err != nil {
			// 主库 SourcePath 带 .md，磁盘源不带，补一次。
			doc, err = s.summaryStore.GetBySource(source + ".md")
		}
		if err == nil {
			c.DocID = doc.DocID
			c.Title = doc.Title
			c.Summary = doc.Summary
			c.SupersededBy = doc.SupersededBy
			c.Layer, c.Category = doc.Layer, doc.Category
		}
	}
	return c
}

// semHit 是一条简介语义命中。
type semHit struct {
	DocID string
	Cos   float64
}

// summarySemantic 是简介语义腿：问句向量化后对主库简介向量
// 暴力余弦（BLOB + Go 内算，见 store.go 选型注释），取前 20 条
// （相似度 ≥0.42）。Ollama 挂了返回错误，调用方降级（本腿置空）。
func (s *Store) summarySemantic(query string) ([]semHit, error) {
	vecs, err := s.summaryStore.LoadVectors()
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, nil
	}
	qv, err := s.summaryEmbed([]string{QueryInstruction + query})
	if err != nil {
		return nil, fmt.Errorf("summary semantic embed: %w", err)
	}
	if len(qv) == 0 || len(qv[0]) == 0 {
		return nil, nil
	}
	q := normalize(qv[0])
	hits := make([]semHit, 0, len(vecs))
	for _, v := range vecs {
		if len(v.Vec) != len(q) {
			continue
		}
		sim := cosine(q, normalize(v.Vec))
		if sim >= SemanticLegMinSim {
			hits = append(hits, semHit{DocID: v.DocID, Cos: sim})
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Cos > hits[j].Cos })
	if len(hits) > Stage1SummarySemTop {
		hits = hits[:Stage1SummarySemTop]
	}
	return hits, nil
}

// expressSeats 算直通车席位（0～5 个源路径）：
//   - 查询含精确 token：含该 token 的文档按全文 BM25 取前 5；
//   - 否则全文顶分 ≥ ExpressOpenBM25 时取全文前 5；
//   - 否则 0 席。
func expressSeats(query string, exact []string, snap *corpusSnapshot, fullHits []bm25Hit, fullScore map[string]float64) []string {
	if len(exact) == 0 {
		if len(fullHits) > 0 && fullHits[0].Score >= ExpressOpenBM25 {
			out := make([]string, 0, Stage1ExpressSeats)
			for i, h := range fullHits {
				if i >= Stage1ExpressSeats {
					break
				}
				out = append(out, h.ID)
			}
			return out
		}
		return nil
	}
	// 含精确 token 的文档（原文子串匹配，不走二字滑窗，见 phase1
	// 复核第 3 条），按全文 BM25 取前 5。
	matched := map[string]bool{}
	for _, d := range snap.docs {
		low := strings.ToLower(d.Text)
		for _, tok := range exact {
			if strings.Contains(low, strings.ToLower(tok)) {
				matched[d.Source] = true
				break
			}
		}
	}
	out := make([]string, 0, Stage1ExpressSeats)
	for _, h := range fullHits {
		if len(out) >= Stage1ExpressSeats {
			break
		}
		if matched[h.ID] {
			out = append(out, h.ID)
		}
	}
	return out
}

// exactTokens 从查询里抽精确类 token：报错码 / 命令 / 路径 /
// 端口号。规则（ASCII 连续串，含以下任一特征即精确类）：
//   - 含 _ 或 .（a_bogus、lx.cjs）；含 /（路径）；
//   - 含 - 且带数字（3x-ui）；字母数字混杂（E01、3x）；
//   - 纯数字 3～5 位（端口号/版本号，如 38999、200）；
//   - 大小写混杂的驼峰词（ArgusSecurityPlugin）。
//
// 纯小写英文词（如 uifid）不算——它走普通 BM25。
func exactTokens(query string) []string {
	var runs []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			runs = append(runs, string(cur))
			cur = cur[:0]
		}
	}
	for _, r := range query {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '.' || r == '-' || r == '/' {
			cur = append(cur, r)
		} else {
			flush()
		}
	}
	flush()
	seen := map[string]bool{}
	var out []string
	for _, tok := range runs {
		if len([]rune(tok)) < 3 || seen[tok] {
			continue
		}
		if !isExactToken(tok) {
			continue
		}
		seen[tok] = true
		out = append(out, tok)
	}
	return out
}

func isExactToken(tok string) bool {
	hasDigit, hasLower, hasUpper := false, false, false
	hasUnder, hasDot, hasSlash, hasDash := false, false, false, false
	for _, r := range tok {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case r >= 'a' && r <= 'z':
			hasLower = true
		case r >= 'A' && r <= 'Z':
			hasUpper = true
		case r == '_':
			hasUnder = true
		case r == '.':
			hasDot = true
		case r == '/':
			hasSlash = true
		case r == '-':
			hasDash = true
		}
	}
	if hasSlash || hasUnder || hasDot {
		return true
	}
	if hasDash && hasDigit {
		return true
	}
	if hasDigit && (hasLower || hasUpper) {
		return true
	}
	if hasDigit && !hasLower && !hasUpper {
		// 纯数字：3～5 位当端口号/版本号。
		n := len([]rune(tok))
		return n >= 3 && n <= 5
	}
	// 驼峰：大小写混杂。
	return hasLower && hasUpper
}

// ---- 第二段：加权 RRF 融合 + 拒推 + 保送 + 去重 ----

// rrfScore 是三路加权 RRF 的纯算分：score = Σ w/(k+rank)，
// 再乘无简介层补偿（reference/l1 单腿只有全文一腿，×2.4 归一化）、
// 分类先验（±10% 内）与旧文档降权（×0.2）。rank 为 0 表示
// 该路未进，不贡献。供 fuseSecondStage 与单测手算对照共用。
func rrfScore(lexRank, semRank, fullRank int, layer string, superseded bool) float64 {
	rrf := 0.0
	if lexRank > 0 {
		rrf += RRFWeightLexical / (RRFK + float64(lexRank))
	}
	if semRank > 0 {
		rrf += RRFWeightSemantic / (RRFK + float64(semRank))
	}
	if fullRank > 0 {
		rrf += RRFWeightFullText / (RRFK + float64(fullRank))
	}
	// 无简介层缺腿补偿（2026-11-24 调参，Gemini 方案 2.2）：
	// reference/l1 没有简介向量，语义腿与简介词法腿均为 0，
	// 单腿 RRF 最高只有 1/61×先验，结构性打不过双腿/三腿 wiki
	// 文档；×2.4 后全文第 1 名约 0.0354–0.0374，仍低于三腿
	// wiki 正样本（0.0462–0.0492），只在跨层题上翻盘。
	if layer == "reference" || layer == "l1" {
		rrf *= NoSummaryRRFBoost
	}
	rrf *= categoryPrior(layer)
	if superseded {
		rrf *= SupersededFactor
	}
	return rrf
}

// fuseSecondStage 做第二段：三路 RRF + 无简介层补偿 + 分类先验 +
// 旧文档降权，过拒推阈值，保送直通车，同层同类去重，灰区限流，
// token 封顶 380，最终交付 0～5 张卡片。返回卡片与置信度结果
// （供记账与评测用，不进交付卡片）。
//
// 拒推阈值按库规模缩放（scaleBM25，见下）：RejectMinBM25 等基准
// 是在 N=1694 的真实库上标定的绝对值，BM25 的 IDF 一项按
// ln(1+(N-df+0.5)/(df+0.5)) 随库规模变——罕见词（df=1）在 N=1694
// 时 IDF≈7.0，在 N=9 时只有 ≈1.9。直接拿 19.0 去卡小库，同一条
// 查询的得分只有生产环境的约 1/4，全部被拒成 0 张（单测 fixture
// 8 篇时三条用例全灭）。按罕见词 IDF 之比缩放阈值，上限
// 19.0×1.5=28.5、下限 19.0×0.05=0.95（因子夹在 [0.05, 1.5]）：
// 生产库 N≈1693 时因子 ≈0.9999，行为不变；小库按比例放宽。
// 语义余弦是归一化相似度，不随库规模变，不缩放。
//
// scaleBM25Factor 是 BM25 阈值随库规模的缩放因子：罕见词 IDF
// 之比，夹在 [0.05, 1.5]。上限 1.5 是 2026-11-24 调参放宽的
// （旧上限 1.0 会让未来库长到 5000–10000 篇时门槛变松）。
func scaleBM25Factor(nDocs int) float64 {
	if nDocs <= 0 {
		return 1
	}
	idf := func(n int) float64 {
		return math.Log(1 + (float64(n)-1+0.5)/1.5)
	}
	f := idf(nDocs) / idf(1694)
	if f > 1.5 {
		f = 1.5
	}
	if f < 0.05 {
		f = 0.05
	}
	return f
}

// scaleBM25 把 BM25 基准阈值按库规模缩放（三个 BM25 阈值共用）。
func scaleBM25(base float64, nDocs int) float64 {
	return base * scaleBM25Factor(nDocs)
}

// rejectBM25Threshold 是 scaleBM25(RejectMinBM25, nDocs) 的旧名，
// 保留供单测与调用方用。
func rejectBM25Threshold(nDocs int) float64 {
	return scaleBM25(RejectMinBM25, nDocs)
}

// fuseOutcome 是 fuseSecondStage 的附加结果：置信度带与头名分数。
type fuseOutcome struct {
	ConfidenceBand string  // high / gray / rejected（0 张时）
	Top1SemCos     float64 // 交付头名语义相似度（0 张时为 0）
	Top1BM25       float64 // 交付头名 BM25（全文/简介取大者，0 张时为 0）
	RejectedCount  int     // 拒推丢弃的卡片数
}

// expressWinner 按保送双条件从池里挑直通车保送文档：全局全文 BM25
// 命中精确 token，且 ≥ ExpressMinBM25（生产库口径 12.0，小库按
// scaleBM25 同一 IDF 因子缩放，否则小库精确命中够不着保送线）、
// 同时 ≥ 池内第 2 名 ×1.5。没有符合的返回 ""。
// 必须在拒推门槛之前算——保送是“直接保送”（v2.1 §3），门槛的活
// 是挡不相关文档，不是挡精确命中；门槛先算会把强精确命中先丢掉，
// 保送没机会救（小库里必现：精确文档全文 BM25 8.8 分过保送线
// 12.0×0.27≈3.2，却不过拒推线）。
func expressWinner(exact []string, snap *corpusSnapshot, pool []RecallCard, globalScore map[string]float64) string {
	if len(exact) == 0 {
		return ""
	}
	var ranked []bm25Hit
	for _, c := range pool {
		if sc := globalScore[c.SourcePath]; sc > 0 {
			ranked = append(ranked, bm25Hit{ID: c.SourcePath, Score: sc})
		}
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].Score > ranked[j].Score })
	minExpress := scaleBM25(ExpressMinBM25, len(snap.docs))
	if len(ranked) == 0 || ranked[0].Score < minExpress || !hasExactHit(snap, ranked[0].ID, exact) {
		return ""
	}
	second := 0.0
	if len(ranked) >= 2 {
		second = ranked[1].Score
	}
	if ranked[0].Score < second*1.5 {
		return ""
	}
	return ranked[0].ID
}

func (s *Store) fuseSecondStage(query string, qTerms []string, exact []string, snap *corpusSnapshot, pool []RecallCard, globalScore map[string]float64, now time.Time) ([]RecallCard, fuseOutcome) {
	// 对 40 篇候选即时算全文 BM25（子集 IDF，与全局排名不同）。
	ids := make([]string, len(pool))
	texts := make([]string, len(pool))
	for i, c := range pool {
		ids[i] = c.SourcePath
		if idx, ok := snap.bySource[c.SourcePath]; ok {
			texts[i] = snap.docs[idx].Text
		} else {
			texts[i] = c.Title + "\n" + c.Summary
		}
	}
	sub := newBM25Index(ids, texts)
	subHits := sub.score(qTerms, bm25K1, bm25FullB)
	subRank := map[string]int{}
	subScore := map[string]float64{}
	for i, h := range subHits {
		subRank[h.ID] = i + 1
		subScore[h.ID] = h.Score
	}

	asksHistory := false
	for _, w := range historyWords {
		if strings.Contains(query, w) {
			asksHistory = true
			break
		}
	}

	// 保送文档先定（见 expressWinner）：保送不被拒推门槛拦。
	nDocs := len(snap.docs)
	minBM25 := scaleBM25(RejectMinBM25, nDocs)
	jointBM25 := scaleBM25(RejectJointBM25, nDocs)
	noVecBM25 := scaleBM25(RejectMinBM25NoVec, nDocs)
	expressID := expressWinner(exact, snap, pool, globalScore)

	// Express 防呆（2026-11-24 调参，Gemini 方案第 3 节）：
	// 有向量但语义极低且全文 BM25 未达 18.0，取消保送资格，
	// 降级为普通候选走正常拒推与 RRF。无向量文档 SemCos=0
	// （靠 BM25 信号保命），不触发。BM25 取全局分数（绝对口径），
	// 不取卡片上的 FullBM25——后者只在部分进池腿里才赋值。
	if expressID != "" {
		for i := range pool {
			if pool[i].SourcePath == expressID {
				if pool[i].SemCos > 0 && pool[i].SemCos < 0.35 && globalScore[expressID] < 18.0 {
					expressID = ""
				}
				break
			}
		}
	}

	// RRF 融合 + 拒推（2026-11-24 调参，Gemini 方案 1.2）：
	// 保送免过门槛；无向量层独立门槛 + 通道/全文前 3 保护；
	// 有向量层语义硬过线 / 词法硬过线 / 双信号联合过线，三选一保留。
	kept := make([]RecallCard, 0, len(pool))
	rejected := 0
	for i := range pool {
		c := pool[i]
		// FullBM25 取全局全文分数（绝对口径，供拒推与保送）；
		// FullRank 取子集排名（相对口径，供 RRF 全文腿）。
		c.FullBM25 = globalScore[c.SourcePath]
		c.FullRank = subRank[c.SourcePath]
		c.RRF = rrfScore(c.LexRank, c.SemRank, c.FullRank, c.Layer, c.SupersededBy != "" && !asksHistory)
		if c.SourcePath != expressID {
			if c.Layer == "reference" || c.Layer == "l1" {
				// 无向量层：独立门槛 + 前 3 保护。FullRank 是池内
				// 全文子集排名，通道第 1 名也可能大于 3——通道进池
				// 的卡以 FromLightChannel 为准，不被子集排名误杀；
				// 经回补/直通车进池的卡则看子集排名前 3。
				if c.FullBM25 < noVecBM25 || !((c.FullRank > 0 && c.FullRank <= 3) || c.FromLightChannel) {
					rejected++
					continue
				}
			} else {
				// 有向量层（wiki/l3）：三选一保留。
				topBM25 := c.FullBM25
				if c.SumBM25 > topBM25 {
					topBM25 = c.SumBM25
				}
				passCos := c.SemCos >= RejectMinCosine
				passBM25 := topBM25 >= minBM25
				passJoint := c.SemCos >= RejectJointCosine && topBM25 >= jointBM25
				if !passCos && !passBM25 && !passJoint {
					rejected++
					continue
				}
			}
		}
		// 旧文档卡片标题加过期前缀（问历史时不加）。
		if c.SupersededBy != "" && !asksHistory {
			c.Title = "[已过期，替代见 " + c.SupersededBy + "]" + c.Title
		}
		kept = append(kept, c)
	}
	sort.Slice(kept, func(i, j int) bool {
		if kept[i].RRF != kept[j].RRF {
			return kept[i].RRF > kept[j].RRF
		}
		return kept[i].SourcePath < kept[j].SourcePath
	})

	// 直通车保送：保送文档（门槛前已按双条件 + 防呆定出，见 expressWinner）
	// 直接保送进最终 Top 3（移到首位，最多 1 篇）。不在这里重算：
	// 重算用的 kept 已过拒推门槛，保送文档若被门槛拦过就找不回来。
	if expressID != "" {
		for i, c := range kept {
			if c.SourcePath == expressID {
				kept[i].Express = true
				// 移到首位（保送进 Top 3 的最强形式）。
				kept[0], kept[i] = kept[i], kept[0]
				break
			}
		}
	}

	// 去重：同一 layer/category 内简介向量余弦 >0.90 只留排名靠前者，
	// 跨类别不去重（只有双方都有简介向量时才可算）。
	vecByDoc := map[string][]float32{}
	if s.summaryStore != nil {
		if vecs, err := s.summaryStore.LoadVectors(); err == nil {
			for _, v := range vecs {
				vecByDoc[v.DocID] = v.Vec
			}
		}
	}
	deduped := make([]RecallCard, 0, len(kept))
	for _, c := range kept {
		dup := false
		if c.DocID != "" {
			if v, ok := vecByDoc[c.DocID]; ok {
				for _, k := range deduped {
					if k.DocID == "" || k.Layer != c.Layer || k.Category != c.Category {
						continue
					}
					if kv, ok := vecByDoc[k.DocID]; ok {
						if cosine(normalize(v), normalize(kv)) > DedupCosine {
							dup = true
							break
						}
					}
				}
			}
		}
		if !dup {
			deduped = append(deduped, c)
		}
	}

	// 灰区限流（2026-11-24 调参，Gemini 方案 1.3）：过线后的边界限流，
	// 不是低于硬拒线还给卡。头名处于边界带（语义 <0.60 且 BM25 <22.0）
	// 且非保送时，本轮最多交 2 张，置信度记 gray；否则 high。
	// 0 张时置信度记 rejected。
	outcome := fuseOutcome{ConfidenceBand: "high", RejectedCount: rejected}
	maxCards := MaxDeliveredCards
	if len(deduped) == 0 {
		outcome.ConfidenceBand = "rejected"
	} else {
		top := deduped[0]
		topBM25 := top.FullBM25
		if top.SumBM25 > topBM25 {
			topBM25 = top.SumBM25
		}
		outcome.Top1SemCos = top.SemCos
		outcome.Top1BM25 = topBM25
		if top.SourcePath != expressID && top.SemCos < 0.60 && topBM25 < 22.0 {
			outcome.ConfidenceBand = "gray"
			maxCards = GrayZoneMaxCards
		}
	}

	// 交付 0～5 张（灰区最多 2 张），总量 ≤380 tokens（保送篇不被
	// token cap 挤掉：保送篇在首位，天然优先）。
	out := make([]RecallCard, 0, MaxDeliveredCards)
	tokens := 0
	for _, c := range deduped {
		if len(out) >= maxCards {
			break
		}
		t := c.Tokens()
		if len(out) > 0 && tokens+t > MaxDeliveredTokens {
			continue
		}
		out = append(out, c)
		tokens += t
	}
	return out, outcome
}

// hasExactHit 报告某文档全文是否含任一精确 token（原文子串匹配）。
func hasExactHit(snap *corpusSnapshot, source string, exact []string) bool {
	idx, ok := snap.bySource[source]
	if !ok {
		return false
	}
	low := strings.ToLower(snap.docs[idx].Text)
	for _, tok := range exact {
		if strings.Contains(low, strings.ToLower(tok)) {
			return true
		}
	}
	return false
}

// queryHasHistory 是 historyWords 的导出版（单测与调用方用）。
func queryHasHistory(query string) bool {
	for _, w := range historyWords {
		if strings.Contains(query, w) {
			return true
		}
	}
	return false
}
