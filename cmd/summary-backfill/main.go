// Command summary-backfill 是简介回填器（线 B 第二阶段第 3 棒）：
// 读接入 CLI（summary-ingest）写出的 manifest，把已验证通过的
// 简介接进 SQLite 简介主库（internal/memory.SummaryStore），
// 简介向量用服务器本地 Ollama qwen3-embedding:0.6b 算，不走 API。
//
// 选型说明（任务二选一，选了本命令）：不复用接入 CLI 的 manifest
// 写入路径——接入只管“质检 + 落简介文件 + 发号”，回填只管
// “manifest → 主库 + 向量”，两步职责分开；复提时 doc_id 靠
// UpsertDoc 的 ON CONFLICT(doc_id) 复用，不重号。
//
// pending 的不入库（等家长改完重提后由接入 CLI 复用 doc_id 再入库，
// 下次回填同一份 manifest 时自动带上）。全量 1693 篇的回填是
// 等后面批次到了再跑的事，本命令只做 machinery + 批次验证，
// 绝不调模型写简介。
//
// 用法（在仓库根目录 /root/super-agent-backup 下）：
//
//	go run ./cmd/summary-backfill \
//	  -manifest /tmp/batch01-store2/简介/manifest.json \
//	  -store /tmp/batch01-store2/简介 \
//	  -db /tmp/line-b-summaries.db \
//	  -memory /tmp/mem-b-line-b
//
// -memory 省略时不校验 source_hash（只用 manifest 里的哈希）；
// 给了则逐篇校验源文件哈希，不一致的跳过并告警（防简介与原文
// 脱节）。本命令只写 -db 指定的库，绝不写生产记忆目录。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"007-super-agent/internal/memory"
	"007-super-agent/internal/sanitize"
	"007-super-agent/internal/summary"
)

type backfillManifest struct {
	Entries []summary.IngestEntry `json:"entries"`
}

func main() {
	manifestPath := flag.String("manifest", "", "接入 manifest JSON（必填）")
	storeDir := flag.String("store", "", "简介 store 目录（简介/ 布局，必填）")
	dbPath := flag.String("db", "", "简介主库 SQLite 路径（必填，staging 用 /tmp 下的库）")
	memoryRoot := flag.String("memory", "", "原文记忆目录（只读；给了则校验 source_hash）")
	ollamaURL := flag.String("ollama", "http://127.0.0.1:11434", "本地 Ollama 地址")
	model := flag.String("model", "qwen3-embedding:0.6b", "向量模型")
	statuses := flag.String("status", "ok,degraded_ok", "回填哪些状态（逗号分隔；pending 永不入库）")
	allowPartial := flag.Bool("allow-partial", false, "允许 skipped(pending)/failed 存在时仍 exit 0（显式放行用；默认严格：数量对不上就报错退出，不静默通过）")
	flag.Parse()
	if *manifestPath == "" || *storeDir == "" || *dbPath == "" {
		fmt.Fprintln(os.Stderr, "summary-backfill: -manifest、-store、-db 必填")
		flag.Usage()
		os.Exit(2)
	}
	want := map[string]bool{}
	for _, s := range strings.Split(*statuses, ",") {
		want[strings.TrimSpace(s)] = true
	}
	if want["pending"] {
		fmt.Fprintln(os.Stderr, "summary-backfill: pending 永不入库，拒绝执行")
		os.Exit(2)
	}

	raw, err := os.ReadFile(*manifestPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "summary-backfill: 读 manifest: %v\n", err)
		os.Exit(1)
	}
	var m backfillManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		fmt.Fprintf(os.Stderr, "summary-backfill: 解析 manifest: %v\n", err)
		os.Exit(1)
	}

	st, err := memory.OpenSummaryStore(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "summary-backfill: 开主库: %v\n", err)
		os.Exit(1)
	}
	defer st.Close()

	embedder := summary.NewOllamaEmbedder()
	embedder.BaseURL = *ollamaURL
	embedder.Model = *model

	now := time.Now().UTC().Format(time.RFC3339)
	docsIn, vecsIn, skipped, failed, wantCount := 0, 0, 0, 0, 0
	for _, e := range m.Entries {
		if !want[string(e.Status)] {
			skipped++
			continue
		}
		wantCount++
		if err := backfillOne(context.Background(), st, embedder, *model, *storeDir, *memoryRoot, e, now); err != nil {
			fmt.Fprintf(os.Stderr, "FAIL %s %s: %v\n", e.DocID, e.SourcePath, err)
			failed++
			continue
		}
		docsIn++
		vecsIn++
		fmt.Printf("OK %s %s\n", e.DocID, e.SourcePath)
	}
	_ = st.SetMeta("model", *model)
	_ = st.SetMeta("built_at", now)
	_ = st.SetMeta("manifest", *manifestPath)
	fmt.Printf("done: docs=%d vectors=%d skipped=%d failed=%d db=%s\n",
		docsIn, vecsIn, skipped, failed, *dbPath)
	// 防丢核对（线 B 修复轮第 4 棒）：回填后逐篇回读主库确认落地，
	// manifest 里应入库的篇数必须等于实际入库并回读到的篇数；
	// skipped（pending 永不入库）与 failed 有任何一项都不静默通过。
	verified := 0
	for _, e := range m.Entries {
		if !want[string(e.Status)] {
			continue
		}
		if _, err := st.GetDoc(e.DocID); err != nil {
			fmt.Fprintf(os.Stderr, "RECONCILE MISSING %s %s: %v\n", e.DocID, e.SourcePath, err)
			continue
		}
		verified++
	}
	fmt.Printf("reconcile: manifest_total=%d want=%d indexed=%d verified=%d skipped=%d failed=%d db=%s\n",
		len(m.Entries), wantCount, docsIn, verified, skipped, failed, *dbPath)
	if skipped > 0 || failed > 0 || verified != wantCount {
		if !*allowPartial {
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "summary-backfill: -allow-partial 已给，显式放行（数量对不上，须重提重跑）")
	}
}

// backfillOne 回填一篇：读简介文件 → 解析 → （可选）校验源哈希 →
// 入库 → 向量化简介（标题 + 一句话简介）→ 存向量。
func backfillOne(ctx context.Context, st *memory.SummaryStore, embedder summary.Embedder, model, storeDir, memoryRoot string, e summary.IngestEntry, now string) error {
	rawSummary, err := os.ReadFile(filepath.Join(storeDir, e.SourcePath))
	if err != nil {
		return fmt.Errorf("读简介文件: %w", err)
	}
	cand, err := summary.Parse(string(rawSummary))
	if err != nil {
		return fmt.Errorf("简介格式解析: %w", err)
	}
	// 简介输出检查（脱敏口径：简介输出检查处掩码/打回）：入库前
	// 再过一遍密钥检查，含密钥字样直接打回不入库（接入时已检过，
	// 这里是入库边界的二次把关）。
	if sanitize.ContainsSecret(cand.Summary + " " + strings.Join(cand.Triggers(), " ")) {
		return fmt.Errorf("简介含密钥字样，打回不入库")
	}
	var source []byte
	if memoryRoot != "" {
		source, err = os.ReadFile(filepath.Join(memoryRoot, e.SourcePath))
		if err != nil {
			return fmt.Errorf("读源文件: %w", err)
		}
		if got := summary.SHA256Hex(source); got != e.SourceHash {
			return fmt.Errorf("source_hash 不一致：manifest=%s 现算=%s（原文已变，等重提）", e.SourceHash, got)
		}
	}
	title := ""
	if len(source) > 0 {
		title = summary.TitleFromSource(e.SourcePath, source)
	}
	if title == "" {
		title = summary.TitleFromSource(e.SourcePath, nil)
	}
	doc := memory.SummaryDoc{
		DocID: e.DocID, Layer: e.Layer, Category: e.Category,
		Title: title, Summary: cand.Summary,
		Triggers:    strings.Join(cand.Triggers(), "，"),
		SummaryPath: e.SummaryPath, SourcePath: e.SourcePath,
		SourceHash: e.SourceHash, SummaryHash: e.SummaryHash,
		Status: string(e.Status), RetryCount: e.RetryCount,
		UpdatedAt: now,
	}
	if err := st.UpsertDoc(doc); err != nil {
		return fmt.Errorf("入库: %w", err)
	}
	vec, err := embedder.Embed(ctx, title+"\n"+cand.Summary)
	if err != nil {
		return fmt.Errorf("向量化（本地 Ollama）: %w", err)
	}
	if err := st.UpsertVector(e.DocID, vec, model); err != nil {
		return fmt.Errorf("存向量: %w", err)
	}
	return nil
}
