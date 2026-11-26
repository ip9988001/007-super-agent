// Command summary-pilot 是线 B 第二阶段第 2 步的试点运行器：
// 对 20 篇试点文档生成简介、跑 8 条质检、写简介文件与 manifest。
//
// 安全边界：
//   - 只读生产记忆目录（/root/super-agent-007/data/memory），绝不写；
//   - 简介只写进本仓库分支内的 pilot-summaries/，绝不写生产记忆；
//   - 模型客户端不配 Ledger，不写生产账本，成本只按 Usage 在本地算；
//   - 不安装、不部署、不重启服务；新二进制只编到 /tmp 验证。
//   - 密钥只经 llm.ResolveAPIKey 读入内存，绝不打印、绝不写入任何文件。
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

	"007-super-agent/internal/llm"
	"007-super-agent/internal/summary"
)

// pilotItem 是一篇试点文档（doc_id 按类别顺序编发，改名搬家不变；
// 全量阶段由主库按既有账本续编，不重复发放）。
type pilotItem struct {
	DocID      string `json:"doc_id"`
	SourcePath string `json:"source_path"`
}

var pilotList = []pilotItem{
	{DocID: "W-0001", SourcePath: "wiki/entities/cf-新账号注册-邮箱验证-取-global-api-key-全流程-camoufox-版.md"},
	{DocID: "W-0002", SourcePath: "wiki/entities/cf-账号-gtop-gtop-dev-专用.md"},
	{DocID: "W-0003", SourcePath: "wiki/entities/internlm-书生浦语-api-完整配置.md"},
	{DocID: "W-0004", SourcePath: "wiki/concepts/鑫鑫跨机认证信道-xinxin-link-二号-三号.md"},
	{DocID: "W-0005", SourcePath: "wiki/concepts/lx-cjs-派单回执排障法-arg-200-字截断-真实结果体位置-链式命令假红票.md"},
	{DocID: "W-0006", SourcePath: "wiki/synthesis/号码卡业务-香港卡竞争格局与用户产品护城河-2026-10-01-调研-用户.md"},
	{DocID: "W-0007", SourcePath: "wiki/playbooks/抓取调研/2026-10-07-网页抓取决策阶梯.md"},
	{DocID: "L3-0001", SourcePath: "l3/lessons/2026-10-09-lesson-012b9c899f2e.md"},
	{DocID: "L3-0002", SourcePath: "l3/lessons/2026-10-10-lesson-653a7c3a1f5c.md"},
	{DocID: "L3-0003", SourcePath: "l3/lessons/2026-10-03-lesson-ab34c16a8998.md"},
	{DocID: "L3-0004", SourcePath: "l3/cases/2026-10-03-fact-1b12b3c3f0ae.md"},
	{DocID: "L3-0005", SourcePath: "l3/cases/2026-11-05-fact-bbbc63b557a7.md"},
	{DocID: "L3-0006", SourcePath: "l3/cases/2026-09-27-fact-271be979bb90.md"},
	{DocID: "L3-0007", SourcePath: "l3/patterns/2026-10-21-pattern-2daca55946f8.md"},
	{DocID: "L3-0008", SourcePath: "l3/patterns/2026-10-26-pattern-9f2c1f080249.md"},
	{DocID: "L3-0009", SourcePath: "l3/decisions/2026-10-27-decision-0edc96f362ca.md"},
	{DocID: "L3-0010", SourcePath: "l3/decisions/2026-11-05-preference-edf690ef7dde.md"},
	{DocID: "L3-0011", SourcePath: "l3/anti-patterns/setInterval不加unref.md"},
	{DocID: "L3-0012", SourcePath: "l3/procedures/向量库跨平台路径修正.md"},
	{DocID: "L3-0013", SourcePath: "l3/procedures/systemd服务化常驻与定时任务.md"},
}

// manifestEntry 是一篇试点文档的 manifest 记录。
type manifestEntry struct {
	DocID       string                `json:"doc_id"`
	SourcePath  string                `json:"source_path"`
	SummaryPath string                `json:"summary_path"`
	SourceHash  string                `json:"source_hash"`
	SummaryHash string                `json:"summary_hash"`
	Status      summary.Status        `json:"status"`
	RetryCount  int                   `json:"retry_count"`
	Checks      []summary.CheckResult `json:"final_checks"`
	Run         *summary.RunReport    `json:"run"`
}

// manifest 是试点总清单。
type manifest struct {
	GeneratedAt           string          `json:"generated_at"`
	MemoryRoot            string          `json:"memory_root"`
	FlashModel            string          `json:"flash_model"`
	StrongModel           string          `json:"strong_model"`
	StrongAvailable       bool            `json:"strong_model_available"`
	StrongProbeNote       string          `json:"strong_probe_note,omitempty"`
	TotalCostMilli        int64           `json:"total_cost_milli"`
	TotalCostCNY          float64         `json:"total_cost_cny"`
	MeanCostMilli         float64         `json:"mean_cost_milli_per_doc"`
	Extrapolated1693Milli int64           `json:"extrapolated_1693_cost_milli"`
	Extrapolated1693CNY   float64         `json:"extrapolated_1693_cost_cny"`
	Entries               []manifestEntry `json:"entries"`
}

func main() {
	allowModel := flag.Bool("allow-model-calls", false, "显式允许模型调用（用户 2026-11-24 已叫停模型生成简介；不带此旗默认拒绝，避免误烧 API token）")
	memoryRoot := flag.String("memory-root", "/root/super-agent-007/data/memory", "生产记忆目录（只读）")
	outDir := flag.String("out", ".", "仓库根目录，简介写进 <out>/pilot-summaries/")
	manifestPath := flag.String("manifest", "pilot-summaries/pilot-manifest.json", "manifest 写出路径（相对 -out）")
	flashModel := flag.String("flash-model", "deepseek-flash", "小模型（首轮）")
	strongModel := flag.String("strong-model", "deepseek-v4-pro", "强模型（打回重写用；不可用则用 flash + 严格提示词）")
	probeStrong := flag.Bool("probe-strong", true, "启动时用一次最小调用探测强模型是否可用")
	flag.Parse()

	// 停用 guard：用户 2026-11-24 已叫停 007 调模型写简介，改由 Muse
	// 用网页版 DeepSeek 写。默认拒绝运行，防误烧 API token。
	if !*allowModel {
		fmt.Fprintln(os.Stderr, "summary-pilot: 已停用——用户 2026-11-24 决定简介不再由 007 调模型写，改由 Muse 用网页版 DeepSeek 写；请改用 cmd/summary-ingest 接入外部写好的简介。如确需恢复模型试点，显式加 -allow-model-calls。")
		os.Exit(2)
	}

	if err := run(*memoryRoot, *outDir, *manifestPath, *flashModel, *strongModel, *probeStrong); err != nil {
		fmt.Fprintf(os.Stderr, "summary-pilot: %v\n", err)
		os.Exit(1)
	}
}

func run(memoryRoot, outDir, manifestPath, flashModel, strongModel string, probeStrong bool) error {
	apiKey, ok := llm.ResolveAPIKey()
	if !ok {
		return fmt.Errorf("未找到 DEEPSEEK_API_KEY（环境变量或 /etc/super-agent/.env），无法调用模型")
	}
	ctx := context.Background()
	flash := llm.New(flashModel, apiKey)
	strong := llm.New(strongModel, apiKey)

	strongAvailable := false
	probeNote := ""
	if probeStrong {
		// 最小调用探测：只让模型回一个字，确认强模型名是否被服务端接受。
		msgs := llm.BuildMessages(llm.Zones{Tail: "只回复一个字：ok"})
		reply, _, err := strong.ChatTask(ctx, msgs, "summary-pilot-probe-strong")
		if err != nil {
			probeNote = "强模型探测失败（" + truncateRunes(err.Error(), 200) + "），本轮全部用 flash + 严格重写提示词"
		} else if strings.TrimSpace(reply) == "" {
			probeNote = "强模型探测返回空回复，视为不可用，本轮全部用 flash + 严格重写提示词"
		} else {
			strongAvailable = true
			probeNote = "强模型探测成功（收到非空回复），打回的样本可升强模型重写"
		}
		fmt.Println("probe:", probeNote)
	} else {
		probeNote = "未探测强模型（-probe-strong=false），本轮全部用 flash"
	}

	gen := &summary.Generator{
		Flash:           flash,
		FlashModel:      flashModel,
		Strong:          strong,
		StrongModel:     strongModel,
		StrongAvailable: strongAvailable,
		StrongProbeNote: probeNote,
		Collision:       summary.NewCollisionChecker(summary.NewOllamaEmbedder()),
		MaxInputRunes:   6000,
	}

	m := manifest{
		GeneratedAt:     time.Now().UTC().Format(time.RFC3339),
		MemoryRoot:      memoryRoot,
		FlashModel:      flashModel,
		StrongModel:     strongModel,
		StrongAvailable: strongAvailable,
		StrongProbeNote: probeNote,
	}
	for _, item := range pilotList {
		content, err := os.ReadFile(filepath.Join(memoryRoot, item.SourcePath))
		if err != nil {
			return fmt.Errorf("读源文件 %s: %w", item.SourcePath, err)
		}
		title := summary.TitleFromSource(item.SourcePath, content)
		doc := summary.PilotDoc{DocID: item.DocID, SourcePath: item.SourcePath, Title: title, Content: content}
		s, rep, err := gen.Generate(ctx, doc)
		if err != nil {
			return fmt.Errorf("生成 %s: %w", item.DocID, err)
		}
		// 落盘简介（分支内目录，与源文件镜像分类、文件名一致）。
		rel := "pilot-summaries/" + item.SourcePath
		abs := filepath.Join(outDir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return fmt.Errorf("建目录 %s: %w", filepath.Dir(abs), err)
		}
		text := summary.Format(s)
		if err := os.WriteFile(abs, []byte(text), 0o644); err != nil {
			return fmt.Errorf("写简介 %s: %w", rel, err)
		}
		m.TotalCostMilli += rep.CostMilli
		m.Entries = append(m.Entries, manifestEntry{
			DocID: item.DocID, SourcePath: item.SourcePath, SummaryPath: rel,
			SourceHash: s.SourceHash, SummaryHash: s.SummaryHash,
			Status: s.Status, RetryCount: rep.RetryCount,
			Checks: rep.FinalChecks, Run: rep,
		})
		fmt.Printf("%s %s status=%s attempts=%d retry=%d strong=%v cost_milli=%d\n",
			item.DocID, item.SourcePath, rep.Status, rep.Attempts, rep.RetryCount, rep.StrongUsed, rep.CostMilli)
	}
	m.TotalCostCNY = float64(m.TotalCostMilli) / 1000
	n := float64(len(m.Entries))
	if n > 0 {
		m.MeanCostMilli = float64(m.TotalCostMilli) / n
		// 外推全库 1693 篇（含试点 20 篇在内按均值计）。
		m.Extrapolated1693Milli = int64(m.MeanCostMilli * 1693)
		m.Extrapolated1693CNY = float64(m.Extrapolated1693Milli) / 1000
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	absManifest := filepath.Join(outDir, manifestPath)
	if err := os.MkdirAll(filepath.Dir(absManifest), 0o755); err != nil {
		return fmt.Errorf("建 manifest 目录: %w", err)
	}
	if err := os.WriteFile(absManifest, data, 0o644); err != nil {
		return fmt.Errorf("写 manifest: %w", err)
	}
	fmt.Printf("done: %d docs, total_cost_milli=%d (¥%.3f), mean_milli=%.2f, extrapolated_1693_milli=%d (¥%.3f)\n",
		len(m.Entries), m.TotalCostMilli, m.TotalCostCNY, m.MeanCostMilli, m.Extrapolated1693Milli, m.Extrapolated1693CNY)
	return nil
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
