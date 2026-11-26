// Command summary-ingest 是简介接入器（用户 2026-11-24 叫停模型生成后的新流程）：
// 输入是外部写好的简介目录（简介/ 布局，见 简介/README.md），
// 逐篇做 8 条质检 + 脱敏输出检查，合格的入库，
// 不合格的只记状态与原因，第 3 次连续不合格改标 degraded_ok
// 并用纯本地规则兜底入库，最后写 manifest JSON。
//
// 全程不调模型、不烧 API token：质检是纯本地规则，
// 规则⑦向量走服务器本地 Ollama（qwen3-embedding:0.6b）。
// 脱敏检查用 internal/sanitize（简介里绝不能出现密钥字样）。
//
// 用法（在仓库根目录 /root/super-agent-backup 下）：
//
//	go run ./cmd/summary-ingest \
//	  -summaries /path/to/外部简介目录 \
//	  -memory-root /root/super-agent-007/data/memory \
//	  -store ./简介 \
//	  -manifest ./简介/manifest.json
//
// 或先编到 /tmp 再跑：
//
//	go build -o /tmp/summary-ingest ./cmd/summary-ingest
//	/tmp/summary-ingest -summaries ... -memory-root ... -store ... -manifest ...
//
// 发号：wiki 用 W-####，l3 用 L3-####；已发过的沿用，新的按
// 已有最大号续编（种子默认读 pilot-summaries/pilot-manifest.json，
// 即模型试点那 20 篇的号，避免重号）。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"007-super-agent/internal/summary"
)

func main() {
	summaries := flag.String("summaries", "", "外部写好的简介目录（简介/ 布局，必填）")
	memoryRoot := flag.String("memory-root", "/root/super-agent-007/data/memory", "原文记忆目录（只读）")
	store := flag.String("store", "./简介", "canonical store（简介/ 布局）；合格与兜底的简介落这里")
	manifest := flag.String("manifest", "", "manifest 写出路径（默认 <store>/manifest.json）")
	seed := flag.String("seed-manifest", "pilot-summaries/pilot-manifest.json", "发号种子（试点 manifest，不存在则从 1 开始）")
	noEmbed := flag.Bool("no-embed", false, "不用 Ollama 向量，规则⑦全部跳过并记明（Ollama 不可用时用）")
	allowPending := flag.Bool("allow-pending", false, "允许 pending 存在时仍 exit 0（重提循环中途显式放行用；默认严格：pending>0 就报错退出，不静默通过）")
	flag.Parse()

	if *summaries == "" {
		fmt.Fprintln(os.Stderr, "summary-ingest: -summaries 必填（外部写好的简介目录）")
		flag.Usage()
		os.Exit(2)
	}
	manifestPath := *manifest
	if manifestPath == "" {
		manifestPath = *store + "/manifest.json"
	}
	var embedder summary.Embedder
	if !*noEmbed {
		embedder = summary.NewOllamaEmbedder()
	}
	m, err := summary.Ingest(context.Background(), summary.IngestOptions{
		MemoryRoot:       *memoryRoot,
		InputDir:         *summaries,
		StoreDir:         *store,
		ManifestPath:     manifestPath,
		SeedManifestPath: *seed,
		Embedder:         embedder,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "summary-ingest: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("done: total=%d accepted=%d pending=%d degraded_ok=%d manifest=%s\n",
		m.Total, m.Accepted, m.Pending, m.Degraded, manifestPath)
	for _, e := range m.Entries {
		fmt.Printf("%s %s status=%s retry=%d", e.DocID, e.SourcePath, e.Status, e.RetryCount)
		if e.Note != "" {
			fmt.Printf(" note=%s", e.Note)
		}
		fmt.Println()
	}
	// 防丢核对（线 B 修复轮第 4 棒）：写了多少篇就必须入库多少篇，
	// pending>0 说明有篇子没入库，默认报错退出，不静默通过。
	if err := m.Reconcile(); err != nil {
		fmt.Fprintln(os.Stderr, "summary-ingest:", err)
		if !*allowPending {
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "summary-ingest: -allow-pending 已给，显式放行（pending 仍在 manifest 里，须重提重跑）")
		return
	}
	fmt.Printf("reconcile: OK input=%d stored=%d\n", m.Total, m.Accepted+m.Degraded)
}
