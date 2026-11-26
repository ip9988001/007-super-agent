package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// search_memory / read_memory：只读补搜工具（v2.1 §7，线 B 第二阶段
// 第 4 棒）。模型开工后中途遇到新情况自行调用，不必等开工前那一次
// 召回包办全程。
//
// 接线方式（与前置六个内置工具、主循环 expand 同一机制）：模型在
// 主循环里直接发 {"action":"tool","tool_id":"search_memory",...}，
// agent.Run 在 ResolveTool 之前截获、在进程内调本包的方法，不走
// executor、不进注册表——注册表只列 executor 能跑的 CLI/service
// 工具（call_type=tool 的 recall_memory 当年就是进了注册表却没有
// 可执行入口，规划器挑中即失败，见 registry.go 注释）；内置工具
// 的发现靠冻结提示词里的用法说明（与 front 的 recall/registry/
// read/bash/dispatch/reply 同一做法），不另起一套调用通道。
//
// 只读保证：两个工具只查简介主库（search_memory）或按 doc_id 读
// 一篇原文文件（read_memory），绝不写记忆、改主库、改文件。
// search_memory 的输出走主库查（FTS5 词法 + 简介向量语义；全文
// BM25 腿复用两阶段召回的 corpus 快照，与 RecallTwoStage 同一
// 原料），不直接扫磁盘；read_memory 是唯一的例外，按 doc_id
// 找到源路径后读原文文件。
//
// 脱敏口径（用户 2026-11-24 09:12 纠正）：交付给模型的卡片与全文
// 绝不掩码（模型照常看、照常用密钥）；掩码只留在“会留存的东西”
// 上——召回账本写前（ledger.RecordRecall）、简介输出检查、
// 报告/文档。本包不调任何脱敏函数，不要恢复已被删掉的第 4 节点
// 掩码与 WriteWiki 落盘前掩码。

// MaxSearchCards 是 search_memory 单次返回的卡片上限（v2.1 §7：5 条）。
const MaxSearchCards = 5

// MaxSearchPerTask / MaxReadPerTask 是一次任务内两个工具各自的调用
// 上限（用户 2026-11-24 08:40 定：各 3 次）。超限时返回明确提示，
// 不静默失败、不报错中断任务——模型看到提示就知道不用再调。
const MaxSearchPerTask = 3
const MaxReadPerTask = 3

// SearchBudget 是一次任务的补搜计数（按任务隔离）：Agent.Run 开始
// 时新建一个（reset 点），任务结束即丢弃；新任务清零。计数本身
// 用互斥锁保护——网关串行 + 派单 worker 单线程，正常只有一个
// 任务在跑，锁只是防并发误用。
type SearchBudget struct {
	mu         sync.Mutex
	SearchUsed int
	ReadUsed   int
}

// Reset 清零计数（复用同一个 SearchBudget 时调；正常是每任务新建）。
func (b *SearchBudget) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.SearchUsed = 0
	b.ReadUsed = 0
}

// Counts 返回已用次数（search, read），供测试与日志用。
func (b *SearchBudget) Counts() (search, read int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.SearchUsed, b.ReadUsed
}

// MemoryCard 是 search_memory 返回的一张简介卡片：只含标题、
// 一句话简介和编号，不含全文（v2.1 §7）。
type MemoryCard struct {
	DocID      string // 主库编号（如 W-0001 / L3-0013）；未入库时为空
	Title      string
	Summary    string // 一句话简介
	SourcePath string // 磁盘源路径（去.md，与旧 Recall 源标识一致）
}

// Display 是卡片交付文本：标题：简介。未入库的文档用源路径
// 当编号（与 RecallCard.Display 同一口径）。
func (c MemoryCard) Display() string {
	id := c.DocID
	if id == "" {
		id = c.SourcePath
	}
	return "【" + id + "】" + c.Title + "：" + c.Summary
}

// searchExhaustedNote / readExhaustedNote 是超限时返回给模型的明确
// 提示（不静默失败，也不报错中断任务）。
const searchExhaustedNote = "本任务 search_memory 已用完 3 次（上限 3 次），不要再调；用已有卡片继续，或在新任务中再查。"
const readExhaustedNote = "本任务 read_memory 已用完 3 次（上限 3 次），不要再调；用已读到的全文继续，或在新任务中再查。"

// SearchMemory 是 search_memory(query, mode, limit)：只读检索，
// 单次返回上限 5 条简介卡片（标题 + 一句话简介 + doc_id，不含全文）。
// mode 支持 lexical/semantic/hybrid（空串按 hybrid）；limit ≤0 或
// >5 时夹到 5。返回值 note：正常为空；语义腿停用导致退化时说明
// 退化方式；次数用完时是超限提示（此时 cards 为空、err 为空）。
// err 只用于真正的调用错误（查询为空、简介主库未接线等），超限不走 err。
func (s *Store) SearchMemory(b *SearchBudget, query, mode string, limit int) ([]MemoryCard, string, error) {
	if strings.TrimSpace(query) == "" {
		return nil, "", fmt.Errorf("search_memory: 查询为空")
	}
	if s.summaryStore == nil {
		return nil, "", fmt.Errorf("search_memory: 简介主库未接线")
	}
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "hybrid", "lexical", "semantic":
	default:
		return nil, "", fmt.Errorf("search_memory: 未知 mode %q（只支持 lexical/semantic/hybrid）", mode)
	}
	if limit <= 0 || limit > MaxSearchCards {
		limit = MaxSearchCards
	}
	if b == nil {
		b = &SearchBudget{}
	}
	b.mu.Lock()
	if b.SearchUsed >= MaxSearchPerTask {
		b.mu.Unlock()
		return nil, searchExhaustedNote, nil
	}
	b.SearchUsed++
	b.mu.Unlock()

	m := strings.ToLower(strings.TrimSpace(mode))
	if m == "" {
		m = "hybrid"
	}
	expanded, _ := expandAbbreviations(query)
	qTerms := bm25Terms(expanded)

	// 三腿的名次表（leg → doc key → 名次），最后按 RRF 合并。
	type legRank struct {
		key  string
		rank int
		card MemoryCard
	}
	legs := map[string][]legRank{}
	note := ""

	// 词法腿：简介主库 FTS5（b=0.30，与两阶段召回同一口径）。
	// lexical 与 hybrid 跑本腿；semantic 模式只跑语义腿。
	searchLex := func() error {
		lexHits, err := s.summaryStore.SearchFTS(qTerms, 20)
		if err != nil {
			return fmt.Errorf("search_memory 词法腿: %w", err)
		}
		for i, h := range lexHits {
			doc, derr := s.summaryStore.GetDoc(h.DocID)
			if derr != nil {
				continue
			}
			legs["lex"] = append(legs["lex"], legRank{key: doc.DocID, rank: i + 1, card: MemoryCard{
				DocID: doc.DocID, Title: doc.Title, Summary: doc.Summary,
				SourcePath: strings.TrimSuffix(doc.SourcePath, ".md"),
			}})
		}
		return nil
	}
	if m == "lexical" || m == "hybrid" {
		if err := searchLex(); err != nil {
			return nil, "", err
		}
	}

	// 语义腿：简介向量（SemanticEnabled 开关管，与两阶段召回同一开关；
	// 当前停用，保持关闭，不要擅自打开）。semantic 与 hybrid 跑本腿。
	semanticOn := false
	if m != "lexical" && SemanticEnabled && s.summaryEmbed != nil {
		semanticOn = true
		semHits, serr := s.summarySemantic(expanded)
		if serr != nil {
			// Ollama 挂了：本腿置空，不报错（与两阶段召回同一降级口径）。
			semanticOn = false
		} else {
			for i, h := range semHits {
				doc, derr := s.summaryStore.GetDoc(h.DocID)
				if derr != nil {
					continue
				}
				legs["sem"] = append(legs["sem"], legRank{key: doc.DocID, rank: i + 1, card: MemoryCard{
					DocID: doc.DocID, Title: doc.Title, Summary: doc.Summary,
					SourcePath: strings.TrimSuffix(doc.SourcePath, ".md"),
				}})
			}
		}
	}
	if m == "semantic" && !semanticOn {
		// semantic 模式点名要语义腿，但腿停用了：退化为词法，
		// 不报错（与 hybrid 同一口径），并明说。
		note = "语义腿当前停用，semantic 已退化为词法检索。"
		if err := searchLex(); err != nil {
			return nil, "", err
		}
	}

	// 全文 BM25 腿：hybrid 才跑（lexical 不跑，semantic 也不跑）。
	// 退化口径：语义腿停用时 hybrid = 词法 + 全文 BM25，不报错。
	if m == "hybrid" {
		snap, cerr := s.corpus()
		if cerr != nil {
			return nil, "", fmt.Errorf("search_memory 全文腿: %w", cerr)
		}
		fullHits := snap.full.score(qTerms, bm25K1, bm25FullB)
		for i, h := range fullHits {
			if i >= 20 {
				break
			}
			pc := newPoolCard(s, snap, h.ID)
			card := MemoryCard{DocID: pc.DocID, Title: pc.Title, Summary: pc.Summary, SourcePath: pc.SourcePath}
			key := card.DocID
			if key == "" {
				key = card.SourcePath
			}
			legs["full"] = append(legs["full"], legRank{key: key, rank: i + 1, card: card})
		}
		if !semanticOn {
			note = "语义腿当前停用，hybrid 已退化为词法 + 全文 BM25。"
		}
	}
	// RRF 合并三腿（k=60，与两阶段召回同一常数；权重全 1）：
	// 同一文档多腿命中只占一张卡，得分累加。
	rrf := map[string]float64{}
	best := map[string]MemoryCard{}
	for _, rs := range legs {
		for _, r := range rs {
			rrf[r.key] += 1.0 / (RRFK + float64(r.rank))
			if _, ok := best[r.key]; !ok {
				best[r.key] = r.card
			}
		}
	}
	keys := make([]string, 0, len(rrf))
	for k := range rrf {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if rrf[keys[i]] != rrf[keys[j]] {
			return rrf[keys[i]] > rrf[keys[j]]
		}
		return keys[i] < keys[j]
	})
	if len(keys) > limit {
		keys = keys[:limit]
	}
	out := make([]MemoryCard, 0, len(keys))
	for _, k := range keys {
		out = append(out, best[k])
	}
	return out, note, nil
}

// ReadMemory 是 read_memory(doc_id)：按编号读一篇全文（原文全文，
// 不截断）。返回值 note：正常为空；次数用完时是超限提示（此时
// 全文为空、err 为空）。doc_id 先按主库编号查，再按源路径查，
// 最后把 doc_id 本身当相对源路径试读（三种都找不到才报错）。
// 路径一律收敛到记忆根目录内，.. 逃逸拒绝。
func (s *Store) ReadMemory(b *SearchBudget, docID string) (string, string, error) {
	docID = strings.TrimSpace(docID)
	if docID == "" {
		return "", "", fmt.Errorf("read_memory: doc_id 为空")
	}
	if b == nil {
		b = &SearchBudget{}
	}
	// 先定源路径并验逃逸（参数非法不计数）：主库编号 → 源路径 →
	// doc_id 本身当相对源路径（去 .md 或带 .md 都接受）。
	sourcePath := ""
	if s.summaryStore != nil {
		if doc, err := s.summaryStore.GetDoc(docID); err == nil {
			sourcePath = doc.SourcePath
		} else if doc, err := s.summaryStore.GetBySource(docID); err == nil {
			sourcePath = doc.SourcePath
		} else if doc, err := s.summaryStore.GetBySource(docID + ".md"); err == nil {
			sourcePath = doc.SourcePath
		}
	}
	if sourcePath == "" {
		sourcePath = docID
	}
	rel := filepath.ToSlash(sourcePath)
	rel = strings.TrimPrefix(rel, "/")
	clean := filepath.Clean(rel)
	if clean == "." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean == ".." {
		return "", "", fmt.Errorf("read_memory: 非法 doc_id %q", docID)
	}
	// 调用被接受才计数：查无此文也计一次（模型已消耗一次调用），
	// 只有参数非法（空、逃逸）不计数。
	b.mu.Lock()
	if b.ReadUsed >= MaxReadPerTask {
		b.mu.Unlock()
		return "", readExhaustedNote, nil
	}
	b.ReadUsed++
	b.mu.Unlock()
	// 记忆根目录内的 .md / .log 文件才读（l1 日志是 .log，其余是 .md）。
	abs := filepath.Join(s.root, clean)
	if _, err := os.Stat(abs); err != nil {
		absMD := abs + ".md"
		if _, err2 := os.Stat(absMD); err2 != nil {
			return "", "", fmt.Errorf("read_memory: 未找到 doc_id %q（主库与磁盘都没有）", docID)
		}
		abs = absMD
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", "", fmt.Errorf("read_memory: 读 %s 失败: %w", clean, err)
	}
	// 不截断、不掩码：全文原样交付给模型（用户 09:12 纠正）。
	return string(data), "", nil
}

// ParseSearchMemoryInputs 解析模型给 search_memory 的 inputs：
// [query]、[query, mode]、[query, mode, limit]。limit 不是数字
// 或越界时按默认 5 处理，不报错（模型少传参数是常事，缺省即默认）。
func ParseSearchMemoryInputs(inputs []string) (query, mode string, limit int) {
	limit = MaxSearchCards
	if len(inputs) > 0 {
		query = strings.TrimSpace(inputs[0])
	}
	if len(inputs) > 1 {
		mode = strings.TrimSpace(inputs[1])
	}
	if len(inputs) > 2 {
		if n, err := strconv.Atoi(strings.TrimSpace(inputs[2])); err == nil && n >= 1 && n <= MaxSearchCards {
			limit = n
		}
	}
	return query, mode, limit
}

// ParseReadMemoryInputs 解析模型给 read_memory 的 inputs：[doc_id]。
func ParseReadMemoryInputs(inputs []string) string {
	if len(inputs) == 0 {
		return ""
	}
	return strings.TrimSpace(inputs[0])
}
