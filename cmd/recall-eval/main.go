// Command recall-eval 是线 B 第一阶段的黄金评测集跑分器：对
// internal/memory/testdata/golden_eval.json 的 70 条逐条跑现行召回，
// 输出命中情况与耗时，并算出 v2.1 第十一节的 5 项基线数字。
//
// 用法：
//
//	recall-eval -memory <data/memory 目录> -eval <golden_eval.json>
//	    [-out <report.json>] [-ledger <recall 账本 db 路径>]
//	    [-semantic=true] [-top=40] [-fixtures <夹具目录>]
//
//	-memory 指向记忆目录的副本（评测只读，不碰生产目录）；
//	-ledger 省略则不写账本，给了则每条召回写一行 recall_log
//	  （走 ledger.RecordRecall，写入前过脱敏）；
//	-semantic=false 时只跑词法（对照用），默认接 vectors.db + Ollama。
//	-twostage=true 时走新两阶段召回（须给 -store 简介主库路径）：
//	  候选名次取自第一段 40 篇候选池，交付取 0～5 张卡片；
//	  -semantic 控制新简介向量语义腿的开关（memory.SemanticEnabled）。
//
// 延迟口径：production_latency_ms 是生产 Recall（TopK=3）单次耗时；
// 候选名次取自评测专用入口 RecallCandidates（与 Recall 同一排序，
// 交付语义不变，见 memory.RecallCandidates 注释）。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"007-super-agent/internal/ledger"
	"007-super-agent/internal/memory"
	"007-super-agent/internal/registry"
)

type evalItem struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Query       string `json:"query"`
	Target      string `json:"target"`
	AbsentTerm  string `json:"absent_term"`
	MustContain string `json:"must_contain"`
	Why         string `json:"why"`
	// Fixture 是 L1 目标的固化夹具文件名（相对 -fixtures 目录，如
	// "l1-2026-11-24.log"）。目标文件在 -memory 里缺失时，评测器按此
	// 把夹具复制进 -memory 副本（只写副本）；目标在盘时以现盘为准。
	Fixture string `json:"fixture,omitempty"`
}

type evalSet struct {
	Version  string     `json:"version"`
	Positive []evalItem `json:"positive"`
	Negative []evalItem `json:"negative"`
}

type itemResult struct {
	ID             string   `json:"id"`
	Kind           string   `json:"kind"`
	Query          string   `json:"query"`
	Target         string   `json:"target,omitempty"`
	Rank40         int      `json:"rank_40"` // 目标在前 40 候选的名次，0=未进
	Rank3          int      `json:"rank_3"`  // 目标在交付前 3 的名次，0=未进
	Top1           bool     `json:"top_1"`
	Delivered      []string `json:"delivered"`               // 交付卡片 source（前 3）
	DeliveredStale int      `json:"delivered_stale"`         // 交付卡片里磁盘上不存在的条数（语义过期命中）
	Candidates     int      `json:"candidates"`              // 候选总数（≤40）
	StaleCands     int      `json:"stale_candidates"`        // 候选中磁盘上不存在的条数
	TokensEst      int      `json:"tokens_est"`              // 交付 3 卡合计 token 估算（EstimateTokens 口径）
	LatencyMs      int64    `json:"latency_ms"`              // 生产 Recall 单次耗时
	CandLatencyMs  int64    `json:"cand_latency_ms"`         // RecallCandidates(40) 单次耗时
	MaxCos         float64  `json:"max_cos,omitempty"`       // 两阶段：简介语义最高余弦
	MaxFullBM25    float64  `json:"max_full_bm25,omitempty"` // 两阶段：全文 BM25 最高分
	MaxSumBM25     float64  `json:"max_sum_bm25,omitempty"`  // 两阶段：简介 FTS BM25 最高分
	Err            string   `json:"err,omitempty"`
}

type report struct {
	Version        string                        `json:"version"`
	GeneratedAt    string                        `json:"generated_at"`
	MemoryDir      string                        `json:"memory_dir"`
	TwoStage       bool                          `json:"two_stage"`
	SummaryDocs    int                           `json:"summary_docs"`
	SummaryVectors int                           `json:"summary_vectors"`
	SemanticOn     bool                          `json:"semantic_on"`
	SemanticDocs   int                           `json:"semantic_docs"`
	SemanticDim    int                           `json:"semantic_dim"`
	SemanticModel  string                        `json:"semantic_model"`
	RecallAt40     float64                       `json:"recall_at_40"`
	RecallAt3      float64                       `json:"recall_at_3"`
	Top1Rate       float64                       `json:"top_1_rate"`
	NegFalsePush   float64                       `json:"neg_false_push_rate"`
	LatencyP95Ms   int64                         `json:"latency_p95_ms"`
	TokensMean     float64                       `json:"delivered_tokens_mean"`
	TokensMax      int                           `json:"delivered_tokens_max"`
	StaleTop3      int                           `json:"stale_delivered_total"`  // 70 条里交付卡片过期命中总数
	StaleCandsTot  int                           `json:"stale_candidates_total"` // 70 条里候选过期命中总数
	L1Warnings     []string                      `json:"l1_warnings,omitempty"`  // L1 目标过期告警（只提示，不阻塞）
	ByKind         map[string]map[string]float64 `json:"by_kind"`
	Items          []itemResult                  `json:"items"`
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func existsOnDisk(memDir, source string) bool {
	if strings.HasSuffix(source, ".log") {
		_, err := os.Stat(filepath.Join(memDir, source))
		return err == nil
	}
	if _, err := os.Stat(filepath.Join(memDir, source+".md")); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(memDir, source)); err == nil {
		return true
	}
	return false
}

// stageFixtures 把评测集里带 fixture 的用例固化进 -memory 副本：
// 目标文件已存在时以现盘文件为准（不覆盖）；缺失时才从夹具目录复制一份
// 进来（只写 -memory 副本，绝不碰生产目录），让 L1 目标在 7 天保留期
// 过后仍能跑分（如 X05/X06 的 l1/2026-11-24.log，生产侧 2026-12-02 起
// 被清理）。返回本次补入的目标列表。
func stageFixtures(memDir, fixturesDir string, es evalSet) ([]string, error) {
	var staged []string
	seen := map[string]bool{}
	stage := func(it evalItem) error {
		if it.Fixture == "" || it.Target == "" || seen[it.Target] {
			return nil
		}
		seen[it.Target] = true
		dst := filepath.Join(memDir, it.Target)
		if _, err := os.Stat(dst); err == nil {
			return nil // 现盘文件在，以现盘为准
		}
		src := filepath.Join(fixturesDir, it.Fixture)
		data, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("用例 %s 的夹具缺失 %s: %w", it.ID, src, err)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("用例 %s 建夹具目录失败: %w", it.ID, err)
		}
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			return fmt.Errorf("用例 %s 补夹具失败: %w", it.ID, err)
		}
		fmt.Printf("夹具已补入：%s ← %s（目标缺失，-memory 是副本，只写副本）\n", it.Target, src)
		staged = append(staged, it.Target)
		return nil
	}
	for _, it := range es.Positive {
		if err := stage(it); err != nil {
			return staged, err
		}
	}
	for _, it := range es.Negative {
		if err := stage(it); err != nil {
			return staged, err
		}
	}
	return staged, nil
}

// warnL1Targets 检查评测集里所有 l1/ 目标：文件缺失、文件名日期
// 距今天超过 7 天、或文件 mtime 超过 7 天未更新时打印警告。
// L1 只留 7 天（PruneL1 按文件名日期清理），目标过期后评测会失真
// （如 X05/X06 的 l1/2026-11-24.log 在 2026-12-02 起被清理，
// 且 ctxread/front_error 只在 L1 出现，无稳定等价目标）。
// 纯提示，不阻塞跑分；返回警告列表（写入报告备查）。
func warnL1Targets(memDir string, es evalSet, staged map[string]bool) []string {
	var warns []string
	seen := map[string]bool{}
	check := func(target string) {
		if !strings.HasPrefix(target, "l1/") || seen[target] {
			return
		}
		seen[target] = true
		if staged != nil && staged[target] {
			return // 已由夹具固化：有意冻结的历史快照，不告警过期
		}
		p := filepath.Join(memDir, target)
		info, err := os.Stat(p)
		if err != nil {
			warns = append(warns, fmt.Sprintf("警告：L1 目标缺失 %s（可能已被 7 天清理删掉，评测会失真）", target))
			return
		}
		base := strings.TrimSuffix(filepath.Base(target), ".log")
		if d, err := time.Parse("2006-01-02", base); err == nil {
			if time.Since(d) > 7*24*time.Hour {
				warns = append(warns, fmt.Sprintf("警告：L1 目标 %s 文件名日期距今已超过 7 天（%s），L1 只留 7 天，目标临近/已过保留期，尽快更新目标", target, base))
			}
		}
		if time.Since(info.ModTime()) > 7*24*time.Hour {
			warns = append(warns, fmt.Sprintf("警告：L1 目标 %s 超过 7 天未更新（mtime %s），L1 只留 7 天，目标临近/已过保留期", target, info.ModTime().Format(time.RFC3339)))
		}
	}
	for _, it := range es.Positive {
		check(it.Target)
	}
	for _, it := range es.Negative {
		check(it.Target)
	}
	for _, w := range warns {
		fmt.Println(w)
	}
	return warns
}

func p95(ms []int64) int64 {
	if len(ms) == 0 {
		return 0
	}
	s := append([]int64(nil), ms...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	idx := int(math.Ceil(0.95*float64(len(s)))) - 1
	if idx < 0 {
		idx = 0
	}
	return s[idx]
}

func main() {
	memDir := flag.String("memory", "", "记忆目录（data/memory 的副本，只读）")
	evalPath := flag.String("eval", "", "golden_eval.json 路径")
	outPath := flag.String("out", "", "报告 JSON 输出路径（省略则只打印摘要）")
	ledgerPath := flag.String("ledger", "", "recall 账本 db 路径（省略则不写账本）")
	semanticOn := flag.Bool("semantic", true, "是否接语义臂（旧路径接 vectors.db；两阶段接简介向量）")
	top := flag.Int("top", 40, "候选池大小（评测取前 N 名）")
	twoStage := flag.Bool("twostage", false, "走新两阶段召回（须给 -store）")
	storePath := flag.String("store", "", "简介主库 SQLite 路径（两阶段用）")
	fixturesFlag := flag.String("fixtures", "", "L1 夹具目录（默认取评测集同目录下的 fixtures；无此目录则不补夹具）")
	flag.Parse()
	if *memDir == "" || *evalPath == "" {
		log.Fatal("须给 -memory 与 -eval")
	}
	raw, err := os.ReadFile(*evalPath)
	if err != nil {
		log.Fatalf("读评测集: %v", err)
	}
	var es evalSet
	if err := json.Unmarshal(raw, &es); err != nil {
		log.Fatalf("解析评测集: %v", err)
	}
	// L1 目标固化夹具（线 B 修复轮第 5 棒）：目标缺失时从夹具补入 -memory
	// 副本，再跑分；现盘文件在时以现盘为准，不覆盖。
	fixturesDir := *fixturesFlag
	if fixturesDir == "" {
		if d := filepath.Join(filepath.Dir(*evalPath), "fixtures"); isDir(d) {
			fixturesDir = d
		}
	}
	var staged []string
	if fixturesDir != "" {
		var err error
		staged, err = stageFixtures(*memDir, fixturesDir, es)
		if err != nil {
			log.Fatalf("补 L1 夹具: %v", err)
		}
	}
	stagedSet := map[string]bool{}
	for _, t := range staged {
		stagedSet[t] = true
	}
	// L1 目标过期告警（线 B 第二阶段第 4 棒）：只提示，不阻塞跑分；
	// 已由夹具固化的目标不告警。
	l1Warns := warnL1Targets(*memDir, es, stagedSet)

	st, err := memory.Open(*memDir)
	if err != nil {
		log.Fatalf("memory.Open: %v", err)
	}
	rep := report{
		Version:     es.Version,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		MemoryDir:   *memDir,
		SemanticOn:  *semanticOn,
		L1Warnings:  l1Warns,
		ByKind:      map[string]map[string]float64{},
	}
	if *twoStage {
		if *storePath == "" {
			log.Fatal("两阶段须给 -store 简介主库路径")
		}
		if _, err := os.Stat(*storePath); err != nil {
			log.Fatalf("简介主库不存在: %v", err)
		}
		sumStore, err := memory.OpenSummaryStore(*storePath)
		if err != nil {
			log.Fatalf("开简介主库: %v", err)
		}
		defer sumStore.Close()
		rep.TwoStage = true
		memory.SemanticEnabled = *semanticOn
		st.WithSummaryStore(sumStore, memory.OllamaEmbedFunc("", ""))
		if n, err := sumStore.CountDocs(); err == nil {
			rep.SummaryDocs = n
		}
		if n, err := sumStore.CountVectors(); err == nil {
			rep.SummaryVectors = n
			rep.SemanticDocs = n
		}
		if vecs, err := sumStore.LoadVectors(); err == nil && len(vecs) > 0 {
			rep.SemanticDim = len(vecs[0].Vec)
		}
		rep.SemanticOn = *semanticOn
		rep.SemanticModel = memory.SemanticDefaultModel
		fmt.Printf("two-stage: store=%s docs=%d vectors=%d semantic_leg=%v\n",
			*storePath, rep.SummaryDocs, rep.SummaryVectors, *semanticOn)
	} else if *semanticOn {
		if sem, err := memory.OpenSemantic(
			filepath.Join(*memDir, "vectors.db"),
			memory.SemanticDefaultURL, memory.SemanticDefaultModel); err != nil {
			fmt.Printf("semantic: 不可用（%v），只跑词法\n", err)
			rep.SemanticOn = false
		} else {
			defer sem.Close()
			st.WithSemantic(sem.Search)
			rep.SemanticDocs = sem.Count()
			rep.SemanticDim = sem.Dim()
			rep.SemanticModel = sem.Model()
			fmt.Printf("semantic: model=%s docs=%d dim=%d\n", sem.Model(), sem.Count(), sem.Dim())
		}
	}

	var led *ledger.Ledger
	if *ledgerPath != "" {
		led, err = ledger.Open(*ledgerPath)
		if err != nil {
			log.Fatalf("ledger.Open: %v", err)
		}
		defer ledger.Close(led)
	}

	// runOneTwoStage 是两阶段评测：候选名次取第一段 40 篇池，
	// 交付取 0～5 张卡片（Rank3/Top1 按交付前 3 计，0 张即拒推）。
	runOneTwoStage := func(it evalItem, st *memory.Store, led *ledger.Ledger) itemResult {
		r := itemResult{ID: it.ID, Kind: it.Kind, Query: it.Query, Target: it.Target}
		t0 := time.Now()
		res, err := st.RecallTwoStage(it.Query)
		r.LatencyMs = time.Since(t0).Milliseconds()
		r.CandLatencyMs = r.LatencyMs
		if err != nil {
			r.Err = err.Error()
			return r
		}
		r.MaxCos = res.MaxCos
		r.MaxFullBM25 = res.MaxFullBM25
		r.MaxSumBM25 = res.MaxSumBM25
		r.Candidates = len(res.Pool)
		candStrs := make([]string, 0, len(res.Pool))
		for i, c := range res.Pool {
			candStrs = append(candStrs, fmt.Sprintf("%s=%d", c.SourcePath, c.PoolRank))
			if !existsOnDisk(*memDir, c.SourcePath) {
				r.StaleCands++
			}
			if it.Target != "" && c.SourcePath == it.Target && r.Rank40 == 0 {
				r.Rank40 = i + 1
			}
		}
		for i, c := range res.Cards {
			r.Delivered = append(r.Delivered, c.SourcePath)
			r.TokensEst += registry.EstimateTokens(c.Display())
			if !existsOnDisk(*memDir, c.SourcePath) {
				r.DeliveredStale++
			}
			if it.Target != "" && c.SourcePath == it.Target && r.Rank3 == 0 && i < 3 {
				r.Rank3 = i + 1
			}
		}
		r.Top1 = r.Rank3 == 1
		if led != nil {
			if _, lerr := led.RecordRecall(ledger.RecallRecord{
				SessionID: "recall-eval", TaskID: it.ID,
				Query: it.Query, Candidates: candStrs,
				Delivered: r.Delivered, TokensEst: int64(r.TokensEst),
				LatencyMs: r.LatencyMs,
			}); lerr != nil {
				log.Printf("item %s 记账失败: %v", it.ID, lerr)
			}
		}
		return r
	}

	runOne := func(it evalItem) itemResult {
		r := itemResult{ID: it.ID, Kind: it.Kind, Query: it.Query, Target: it.Target}
		if *twoStage {
			return runOneTwoStage(it, st, led)
		}
		t0 := time.Now()
		cands, err := st.RecallCandidates(it.Query, *top)
		r.CandLatencyMs = time.Since(t0).Milliseconds()
		if err != nil {
			r.Err = err.Error()
			return r
		}
		t1 := time.Now()
		top3, err := st.Recall(it.Query)
		r.LatencyMs = time.Since(t1).Milliseconds()
		if err != nil {
			r.Err = err.Error()
			return r
		}
		r.Candidates = len(cands)
		candStrs := make([]string, 0, len(cands))
		for i, c := range cands {
			candStrs = append(candStrs, fmt.Sprintf("%s=%d", c.Source, c.Score))
			if !existsOnDisk(*memDir, c.Source) {
				r.StaleCands++
			}
			if it.Target != "" && c.Source == it.Target && r.Rank40 == 0 {
				r.Rank40 = i + 1
			}
		}
		for i, t := range top3 {
			r.Delivered = append(r.Delivered, t.Source)
			r.TokensEst += registry.EstimateTokens(t.Source + t.Text)
			if !existsOnDisk(*memDir, t.Source) {
				r.DeliveredStale++
			}
			if it.Target != "" && t.Source == it.Target && r.Rank3 == 0 {
				r.Rank3 = i + 1
			}
		}
		r.Top1 = r.Rank3 == 1
		if led != nil {
			if _, lerr := led.RecordRecall(ledger.RecallRecord{
				SessionID: "recall-eval", TaskID: it.ID,
				Query: it.Query, Candidates: candStrs,
				Delivered: r.Delivered, TokensEst: int64(r.TokensEst),
				LatencyMs: r.LatencyMs,
			}); lerr != nil {
				log.Printf("item %s 记账失败: %v", it.ID, lerr)
			}
		}
		return r
	}

	var latencies []int64
	var tokenSum, tokenMax int
	byKind := map[string][]itemResult{}
	for _, it := range es.Positive {
		r := runOne(it)
		rep.Items = append(rep.Items, r)
		byKind[it.Kind] = append(byKind[it.Kind], r)
		latencies = append(latencies, r.LatencyMs)
		tokenSum += r.TokensEst
		if r.TokensEst > tokenMax {
			tokenMax = r.TokensEst
		}
		rep.StaleTop3 += r.DeliveredStale
		rep.StaleCandsTot += r.StaleCands
	}
	negTotal, negPushed := 0, 0
	for _, it := range es.Negative {
		r := runOne(it)
		rep.Items = append(rep.Items, r)
		byKind[it.Kind] = append(byKind[it.Kind], r)
		latencies = append(latencies, r.LatencyMs)
		negTotal++
		if len(r.Delivered) > 0 {
			negPushed++
		}
		rep.StaleTop3 += r.DeliveredStale
		rep.StaleCandsTot += r.StaleCands
	}

	n := float64(len(es.Positive))
	var at40, at3, top1 float64
	for _, r := range rep.Items {
		if r.Target == "" {
			continue
		}
		if r.Rank40 > 0 {
			at40++
		}
		if r.Rank3 > 0 {
			at3++
		}
		if r.Top1 {
			top1++
		}
	}
	rep.RecallAt40 = at40 / n
	rep.RecallAt3 = at3 / n
	rep.Top1Rate = top1 / n
	if negTotal > 0 {
		rep.NegFalsePush = float64(negPushed) / float64(negTotal)
	}
	rep.LatencyP95Ms = p95(latencies)
	if len(es.Positive) > 0 {
		rep.TokensMean = float64(tokenSum) / float64(len(es.Positive))
	}
	rep.TokensMax = tokenMax
	for kind, rs := range byKind {
		pos := 0
		var a40, a3, t1 int
		for _, r := range rs {
			if r.Target == "" {
				continue
			}
			pos++
			if r.Rank40 > 0 {
				a40++
			}
			if r.Rank3 > 0 {
				a3++
			}
			if r.Top1 {
				t1++
			}
		}
		m := map[string]float64{"n": float64(len(rs))}
		if pos > 0 {
			m["recall@40"] = float64(a40) / float64(pos)
			m["recall@3"] = float64(a3) / float64(pos)
			m["top1"] = float64(t1) / float64(pos)
		}
		rep.ByKind[kind] = m
	}

	if *outPath != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(*outPath, b, 0o600); err != nil {
			log.Fatalf("写报告: %v", err)
		}
	}
	fmt.Printf("基线（%s）：Recall@40=%.1f%% Recall@3=%.1f%% Top-1=%.1f%% 负样本误推率=%.1f%% P95=%dms 交付token均值=%.1f 最大=%d\n",
		es.Version, rep.RecallAt40*100, rep.RecallAt3*100, rep.Top1Rate*100,
		rep.NegFalsePush*100, rep.LatencyP95Ms, rep.TokensMean, rep.TokensMax)
	fmt.Printf("语义臂：docs=%d dim=%d；70 条里交付卡片过期命中 %d 条、候选过期命中 %d 条（源在磁盘上不存在，来自过期 vectors.db）\n",
		rep.SemanticDocs, rep.SemanticDim, rep.StaleTop3, rep.StaleCandsTot)
	for kind, m := range rep.ByKind {
		fmt.Printf("  %-12s n=%.0f recall@40=%.1f%% recall@3=%.1f%% top1=%.1f%%\n",
			kind, m["n"], m["recall@40"]*100, m["recall@3"]*100, m["top1"]*100)
	}
}
