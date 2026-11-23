package registry

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// fixture 把 step1 的 registry.json + index/ 原样拷进临时数据目录。
func fixture(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	if err := copyDir(filepath.Join("..", "..", "step1", "index"), filepath.Join(dst, "index")); err != nil {
		t.Fatalf("copy index fixture: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dst, "registry"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(filepath.Join("..", "..", "step1", "registry", "registry.json"),
		filepath.Join(dst, "registry", "registry.json")); err != nil {
		t.Fatalf("copy registry fixture: %v", err)
	}
	return dst
}

func copyDir(src, dst string) error {
	ents, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, e := range ents {
		s, d := filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := copyDir(s, d); err != nil {
				return err
			}
		} else if err := copyFile(s, d); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// 1. 加载 90 条（84 + 2026-11-19 三个设计 skill：hallmark / taste-skill / hyperframes-skill；
// 再 + 2026-11-23 改革同步的两个锻造工具：mailbox-provisioner / seo-site-audit，默认隐藏；
// 再 + 2026-11-23 第 4 阶段 dispatch-guard）。
func TestLoad90Entries(t *testing.T) {
	rg, err := Load(fixture(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := rg.Count(); got != 90 {
		t.Fatalf("Count = %d, want 90", got)
	}
	if got := len(Categories()); got != 9 {
		t.Fatalf("Categories = %d, want 9", got)
	}
	want := map[string]int{"fetch": 13, "code": 8, "seo": 8, "design": 11, "security": 5, "runtime": 7, "media": 6, "ops": 18, "memory": 14}
	for cat, n := range want {
		if got := len(rg.EntriesByCategory(cat)); got != n {
			t.Errorf("category %s = %d entries, want %d", cat, got, n)
		}
	}
	// 两个锻造工具默认隐藏（第 16 项）：在册、不可派单。
	for _, id := range []string{"forge-cloudflare-email-mailbox-provisioner", "forge-seo-site-audit"} {
		e, ok := rg.Entry(id)
		if !ok {
			t.Fatalf("锻造工具 %s 不在册", id)
		}
		if !e.Hidden {
			t.Errorf("锻造工具 %s 应默认隐藏", id)
		}
		if rg.Callable(id) {
			t.Errorf("锻造工具 %s 默认隐藏时不应可派单", id)
		}
	}
}

// 2. L1 返回 9 类（v1.7：ops 拆成 security/runtime/media/ops）。
func TestL1NineCategories(t *testing.T) {
	rg, err := Load(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	l1, err := rg.L1()
	if err != nil {
		t.Fatalf("L1: %v", err)
	}
	for _, cat := range []string{"Fetch & Retrieval", "Code & Audit", "SEO & Content", "Design & UI", "Security Scanning", "Language Runtimes", "Documents & Media", "Operations", "Memory & Knowledge"} {
		if !strings.Contains(l1, cat) {
			t.Errorf("L1 missing category %q", cat)
		}
	}
	if n := countLines(l1); n > MaxL1Lines {
		t.Errorf("L1 has %d lines, cap %d", n, MaxL1Lines)
	}
}

// 3. L2 读一份（seo 单份 10 行）；ops 在 v1.7 只剩 16 条，单份，见 TestL2OpsSingleFile。
func TestL2OneCategory(t *testing.T) {
	rg, err := Load(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	l2, err := rg.L2("seo")
	if err != nil {
		t.Fatalf("L2(seo): %v", err)
	}
	if n := countLines(l2); n != 10 {
		t.Errorf("L2(seo) = %d lines, want 10", n)
	}
	if !strings.Contains(l2, "cap-seo-content-scorers") {
		t.Error("L2(seo) missing cap-seo-content-scorers")
	}
	if _, err := rg.L2("fetch"); err != nil {
		t.Errorf("L2(fetch): %v", err)
	}
}

// 4. L3 读一份。
func TestL3OneTool(t *testing.T) {
	rg, err := Load(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	l3, err := rg.L3("cap-fetch-curl-jq")
	if err != nil {
		t.Fatalf("L3: %v", err)
	}
	for _, sec := range []string{"## 一句话", "## 怎么调", "## 坑", "## 实际效果", "## SOP"} {
		if !strings.Contains(l3, sec) {
			t.Errorf("L3 missing section %q", sec)
		}
	}
	if n := len([]rune(l3)); n > MaxL3Runes {
		t.Errorf("L3 has %d runes, cap %d", n, MaxL3Runes)
	}
}

// 5. 缺失文件报错。
func TestMissingFileErrors(t *testing.T) {
	// 总账文件不存在。
	if _, err := Load(t.TempDir()); err == nil {
		t.Error("Load with no registry.json: want error, got nil")
	}
	rg, err := Load(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rg.L2("no-such-category"); !errors.Is(err, ErrNotFound) {
		t.Errorf("L2(unknown): err = %v, want ErrNotFound", err)
	}
	if _, err := rg.L3("cap-no-such-tool"); !errors.Is(err, ErrNotFound) {
		t.Errorf("L3(unknown): err = %v, want ErrNotFound", err)
	}
}

// 6. available:false 不可调用。
func TestUnavailableNotCallable(t *testing.T) {
	rg, err := Load(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	const id = "cap-memory-playbooks"
	if rg.Callable(id) {
		t.Errorf("Callable(%s) = true, want false", id)
	}
	if _, err := rg.L3(id); !errors.Is(err, ErrNotAvailable) {
		t.Errorf("L3(%s): err = %v, want ErrNotAvailable", id, err)
	}
	// 但条目本身照常加载，可查到。
	if e, ok := rg.Entry(id); !ok || e.Available {
		t.Errorf("Entry(%s) missing or wrongly available", id)
	}
}

// 6b. 知识条目（doc/sop/tool/mcp/data）即使 available:true 也不可调用：
// 只能读、不能跑。2026-11-18 ping0 测试：cap-memory-recall-memory
// （call_type=tool，entry_point "recall_memory" 不存在）被规划器挑中，
// 执行器启动即失败。
func TestDocToolNotCallable(t *testing.T) {
	rg, err := Load(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"cap-memory-recall-memory", "cap-seo-design-md", "cap-seo-llms-txt"} {
		e, ok := rg.Entry(id)
		if !ok {
			t.Errorf("Entry(%s) missing from fixture", id)
			continue
		}
		if !e.Available {
			t.Errorf("Entry(%s) should be available:true in fixture", id)
		}
		if e.Executable() {
			t.Errorf("Executable(%s) = true, want false (call_type=%s)", id, e.CallType)
		}
		if rg.Callable(id) {
			t.Errorf("Callable(%s) = true, want false", id)
		}
	}
	// 可执行工具仍可调用。
	if !rg.Callable("cap-code-rea") || !rg.Callable("cap-fetch-curl-jq") {
		t.Errorf("cli tools should stay callable")
	}
}

// 7. 超限记账：L3 超 1600 字符报错，且一行落进 index_metrics。
func TestLimitViolationRecorded(t *testing.T) {
	dir := fixture(t)
	// 把一条 L3 撑到 1601 字符。
	p := filepath.Join(dir, "index", "L3", "cap-fetch-curl-jq.md")
	big := strings.Repeat("啊", 1601)
	if err := os.WriteFile(p, []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "ledger.db")
	rec, err := NewLedgerFileRecorder(dbPath)
	if err != nil {
		t.Fatalf("NewLedgerFileRecorder: %v", err)
	}
	defer rec.Close()
	rg, err := Load(dir, WithMetricsRecorder(rec), WithSession("test-session", "test-task"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rg.L3("cap-fetch-curl-jq"); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("L3 oversize: err = %v, want ErrLimitExceeded", err)
	}
	// 直接查库确认记账行落盘。
	rows := queryIndexMetrics(t, dbPath)
	if len(rows) != 1 {
		t.Fatalf("index_metrics rows = %d, want 1", len(rows))
	}
	if rows[0]["level"] != "L3" || rows[0]["tool_id"] != "cap-fetch-curl-jq" {
		t.Errorf("index_metrics row = %v, want level L3 tool cap-fetch-curl-jq", rows[0])
	}
}

func queryIndexMetrics(t *testing.T, dbPath string) []map[string]string {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT level, tool_id, hit FROM index_metrics ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]string
	for rows.Next() {
		var level, toolID string
		var hit int
		if err := rows.Scan(&level, &toolID, &hit); err != nil {
			t.Fatal(err)
		}
		out = append(out, map[string]string{"level": level, "tool_id": toolID, "hit": fmt.Sprintf("%d", hit)})
	}
	return out
}
func TestLoadValidation(t *testing.T) {
	dir := fixture(t)
	raw, err := os.ReadFile(filepath.Join(dir, "registry", "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	// 缺字段：删掉一条的 entry_point。
	bad := strings.Replace(string(raw), `"entry_point": "curl"`, `"entry_point": ""`, 1)
	if bad == string(raw) {
		t.Skip("fixture entry_point pattern not found, skip")
	}
	if err := os.WriteFile(filepath.Join(dir, "registry", "registry.json"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Error("Load with empty entry_point: want error, got nil")
	}
	// 孤儿 L3 文件。
	dir2 := fixture(t)
	if err := os.WriteFile(filepath.Join(dir2, "index", "L3", "cap-orphan.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir2); err == nil {
		t.Error("Load with orphan L3: want error, got nil")
	}
}

// TestNeedsProbeAndMarkVerified 是 B-03 的探针门：verified=false 的可用工具
// 调用前必须探针；探针通过后本进程内不再重复探针；已验证的工具不探针。
func TestNeedsProbeAndMarkVerified(t *testing.T) {
	rg, err := Load(fixture(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !rg.NeedsProbe("cap-ops-tesseract") {
		t.Fatal("tesseract verified=false, want NeedsProbe=true")
	}
	if rg.NeedsProbe("cap-code-semgrep") {
		t.Fatal("semgrep verified, want NeedsProbe=false")
	}
	if rg.NeedsProbe("no-such-tool") {
		t.Fatal("missing tool, want NeedsProbe=false")
	}
	if rg.NeedsProbe("cap-memory-playbooks") {
		t.Fatal("available=false, want NeedsProbe=false")
	}
	rg.MarkVerified("cap-ops-tesseract")
	if rg.NeedsProbe("cap-ops-tesseract") {
		t.Fatal("after MarkVerified, want NeedsProbe=false")
	}
}

// TestProbeEntry 是探针本身的口径：首词在 PATH 或文件存在即通过，
// 否则失败；空 entry_point 失败。
func TestProbeEntry(t *testing.T) {
	if err := ProbeEntry(Entry{EntryPoint: "echo hi"}); err != nil {
		t.Fatalf("ProbeEntry(echo): %v", err)
	}
	if err := ProbeEntry(Entry{EntryPoint: "no-such-cmd-xyz-123"}); err == nil {
		t.Fatal("ProbeEntry(missing cmd) should fail")
	}
	if err := ProbeEntry(Entry{EntryPoint: "/nonexistent/path/tool"}); err == nil {
		t.Fatal("ProbeEntry(missing file) should fail")
	}
	if err := ProbeEntry(Entry{EntryPoint: ""}); err == nil {
		t.Fatal("ProbeEntry(empty) should fail")
	}
}

// TestProbeEntryByCallType 是第 19 步第 5 批的探针分流：
// doc/sop/tool/mcp/data 不探针（知识条目，描述性 entry_point 也通过）；
// service 只查脚本文件在位；cli 的 "python3 -c ..." 跑真实 import 检查。
func TestProbeEntryByCallType(t *testing.T) {
	// 知识条目：entry_point 是描述文字也不探针。
	for _, ct := range []string{"doc", "sop", "tool", "mcp", "data"} {
		if err := ProbeEntry(Entry{CallType: ct, EntryPoint: "SKILL.md: steel-will-long-term"}); err != nil {
			t.Fatalf("ProbeEntry(%s, descriptive): %v", ct, err)
		}
		if err := ProbeEntry(Entry{CallType: ct, EntryPoint: "stdio scrapling-mcp server"}); err != nil {
			t.Fatalf("ProbeEntry(%s, mcp text): %v", ct, err)
		}
	}
	// service：文件在就通过，不在就失败（不查 PATH）。
	tmp := t.TempDir()
	script := filepath.Join(tmp, "x.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ProbeEntry(Entry{CallType: "service", EntryPoint: script}); err != nil {
		t.Fatalf("ProbeEntry(service, existing file): %v", err)
	}
	if err := ProbeEntry(Entry{CallType: "service", EntryPoint: filepath.Join(tmp, "nope.sh")}); err == nil {
		t.Fatal("ProbeEntry(service, missing file) should fail")
	}
	// service：文件在但不可执行也要失败（执行器直接 exec，没 +x 跑不起来）。
	noexec := filepath.Join(tmp, "noexec.sh")
	if err := os.WriteFile(noexec, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ProbeEntry(Entry{CallType: "service", EntryPoint: noexec}); err == nil {
		t.Fatal("ProbeEntry(service, non-executable file) should fail")
	}
	// service 的 entry_point 是命令名也不查 PATH：只当文件查。
	if err := ProbeEntry(Entry{CallType: "service", EntryPoint: "echo"}); err == nil {
		t.Fatal("ProbeEntry(service, bare command) should fail (file lookup only)")
	}
	// cli 的 python3 -c：真实 import 检查。
	if err := ProbeEntry(Entry{CallType: "cli", EntryPoint: "python3 -c import json"}); err != nil {
		t.Fatalf("ProbeEntry(python3 -c import json): %v", err)
	}
	if err := ProbeEntry(Entry{CallType: "cli", EntryPoint: "python3 -c import no_such_mod_xyz_123"}); err == nil {
		t.Fatal("ProbeEntry(python3 -c import missing) should fail")
	}
}

// TestL2OpsSingleFile 是 v1.7 重新分类后的 ops：34 条拆走 18 条
// （security 5、runtime 7、media 6）后剩 16 条；2026-11-23 第 4 阶段
// +dispatch-guard 后剩 17 条，一份 ops.md 放得下
// （3 行头 + 17 条 = 20 行），不再拆 ops-2.md；新三类各有单份 L2。
func TestL2OpsSingleFile(t *testing.T) {
	rg, err := Load(fixture(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	l2, err := rg.L2("ops")
	if err != nil {
		t.Fatalf("L2(ops): %v", err)
	}
	for _, want := range []string{"cap-ops-docker-init", "cap-ops-gh-token", "cap-ops-script-totp", "cap-ops-script-chrome-nosandbox"} {
		if !strings.Contains(l2, want) {
			t.Fatalf("L2(ops) missing %q", want)
		}
	}
	// 拆走的工具不再属于 ops。
	for _, gone := range []string{"cap-ops-ffmpeg", "cap-ops-tesseract", "cap-ops-bandit", "cap-ops-python"} {
		if strings.Contains(l2, gone) {
			t.Fatalf("L2(ops) should not contain moved tool %q", gone)
		}
	}
	if n := countLines(l2); n != 20 {
		t.Fatalf("L2(ops) lines = %d, want 20 (3+17)", n)
	}
	// 新三类各有单份 L2，且包含各自的工具。
	for cat, tool := range map[string]string{"security": "cap-ops-bandit", "runtime": "cap-ops-python", "media": "cap-ops-ffmpeg"} {
		l2cat, err := rg.L2(cat)
		if err != nil {
			t.Fatalf("L2(%s): %v", cat, err)
		}
		if !strings.Contains(l2cat, tool) {
			t.Fatalf("L2(%s) missing %q", cat, tool)
		}
	}
	l2seo, err := rg.L2("seo")
	if err != nil {
		t.Fatalf("L2(seo): %v", err)
	}
	if !strings.Contains(l2seo, "cap-seo-content-scorers") {
		t.Fatal("L2(seo) missing cap-seo-content-scorers")
	}
}

// 第 20 步：entry_point 内存覆盖层——先探针、通过才存；
// registry.json 文件本身永远不动。
func TestEntryPointOverride(t *testing.T) {
	dst := fixture(t)
	rg, err := Load(dst)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tool := "cap-design-hyperframes" // cli、available
	before, ok := rg.Entry(tool)
	if !ok {
		t.Fatalf("Entry(%s) missing", tool)
	}
	origEP := before.EntryPoint

	// 1. 新入口探针失败 → 拒绝，不存覆盖层。
	if err := rg.ApplyEntryPointFix(tool, "/nonexistent/xyz-123"); err == nil {
		t.Fatal("ApplyEntryPointFix bogus entry point: want error, got nil")
	}
	if got, _ := rg.Entry(tool); got.EntryPoint != origEP {
		t.Fatalf("bogus fix changed entry point to %q", got.EntryPoint)
	}
	if len(rg.EntryPointOverrides()) != 0 {
		t.Fatal("bogus fix left an override behind")
	}

	// 2. 新入口探针通过 → 存入覆盖层，Entry() 返回新入口。
	if err := rg.ApplyEntryPointFix(tool, "git"); err != nil {
		t.Fatalf("ApplyEntryPointFix git: %v", err)
	}
	if got, _ := rg.Entry(tool); got.EntryPoint != "git" {
		t.Fatalf("EntryPoint after fix = %q, want %q", got.EntryPoint, "git")
	}
	if ov := rg.EntryPointOverrides(); ov[tool] != "git" {
		t.Fatalf("EntryPointOverrides = %v, want %q", ov, "git")
	}

	// 3. registry.json 磁盘文件本身未被改动（仍是原入口）。
	raw, err := os.ReadFile(filepath.Join(dst, "registry", "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"entry_point": "`+origEP+`"`) {
		t.Fatalf("registry.json on disk changed: original entry_point %q gone", origEP)
	}

	// 4. 不存在的工具 / 不可用工具 / 空入口都拒绝。
	if err := rg.ApplyEntryPointFix("cap-no-such-tool", "git"); err == nil {
		t.Fatal("ApplyEntryPointFix missing tool: want error, got nil")
	}
	if err := rg.ApplyEntryPointFix("cap-fetch-bili", "git"); err == nil {
		t.Fatal("ApplyEntryPointFix unavailable tool: want error, got nil")
	}
	if err := rg.ApplyEntryPointFix(tool, "  "); err == nil {
		t.Fatal("ApplyEntryPointFix empty entry point: want error, got nil")
	}
}

// 第 20 步：UnmarkVerified 与 MarkVerified 对称——
// 重建时新坏的工具要把旧标记清掉，否则 NeedsProbe 永远认为它已验证。
func TestUnmarkVerified(t *testing.T) {
	rg, err := Load(fixture(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tool := "cap-ops-tesseract" // verified=false
	if !rg.NeedsProbe(tool) {
		t.Fatal("NeedsProbe before MarkVerified = false, want true")
	}
	rg.MarkVerified(tool)
	if rg.NeedsProbe(tool) {
		t.Fatal("NeedsProbe after MarkVerified = true, want false")
	}
	rg.UnmarkVerified(tool)
	if !rg.NeedsProbe(tool) {
		t.Fatal("NeedsProbe after UnmarkVerified = false, want true")
	}
}

// writeMiniRegistry 建一个最小总账数据目录：entries 只给 tool_id /
// entry_point / call_type / available，其余必填字段填占位值；
// 并给每条建一个空 L3 文件，满足 Load 的 1:1 校验。
// 用来测探针记录（Probe/ProbeFailed/ProbeAll），不依赖 step1 的
// 真实总账——真实总账的条目在测试机上有没有装好是不确定的。
type miniEntry struct {
	id    string
	ep    string
	ct    string
	avail bool
}

func writeMiniRegistry(t *testing.T, ents []miniEntry) string {
	t.Helper()
	dst := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dst, "registry"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dst, "index", "L3"), 0o755); err != nil {
		t.Fatal(err)
	}
	type mini struct {
		ToolID         string `json:"tool_id"`
		Category       string `json:"category"`
		Subcategory    string `json:"subcategory"`
		CallType       string `json:"call_type"`
		EntryPoint     string `json:"entry_point"`
		OutputContract string `json:"output_contract"`
		SourceFile     string `json:"source_file"`
		OneLiner       string `json:"one_liner"`
		Available      bool   `json:"available"`
	}
	ms := make([]mini, 0, len(ents))
	for _, e := range ents {
		ms = append(ms, mini{
			ToolID: e.id, Category: "ops", Subcategory: "test",
			CallType: e.ct, EntryPoint: e.ep,
			OutputContract: "text", SourceFile: "test.md",
			OneLiner: "test tool " + e.id, Available: e.avail,
		})
		if err := os.WriteFile(filepath.Join(dst, "index", "L3", e.id+".md"), []byte("# "+e.id+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := json.Marshal(map[string]any{
		"version": "test", "total": len(ms), "entries": ms,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "registry", "registry.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return dst
}

// 选项2：Probe 失败要记下来（probeFailed），通过要清掉；
// ProbeAll 只检可用可执行条目，返回已检/通过/失败数。
func TestProbeRecordsFailureAndProbeAll(t *testing.T) {
	dir := writeMiniRegistry(t, []miniEntry{
		{id: "cap-test-good", ep: "echo", ct: "cli", avail: true},
		{id: "cap-test-bad", ep: "no-such-cmd-xyz-123", ct: "cli", avail: true},
		{id: "cap-test-off", ep: "echo", ct: "cli", avail: false},
	})
	rg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// 不存在的工具：探针不记失败，只报找不到。
	if err := rg.Probe("cap-test-nope"); err == nil {
		t.Fatal("Probe(missing tool): want error, got nil")
	}
	if rg.ProbeFailed("cap-test-nope") {
		t.Fatal("ProbeFailed(missing tool) = true, want false")
	}
	// 好的：通过，不记失败。
	if err := rg.Probe("cap-test-good"); err != nil {
		t.Fatalf("Probe(good): %v", err)
	}
	if rg.ProbeFailed("cap-test-good") {
		t.Fatal("ProbeFailed(good) = true, want false")
	}
	if rg.NeedsProbe("cap-test-good") {
		t.Fatal("NeedsProbe(good) after Probe = true, want false")
	}
	// 坏的：失败，记下来。
	if err := rg.Probe("cap-test-bad"); err == nil {
		t.Fatal("Probe(bad): want error, got nil")
	}
	if !rg.ProbeFailed("cap-test-bad") {
		t.Fatal("ProbeFailed(bad) = false, want true")
	}
	if rg.ProbeFailureReason("cap-test-bad") == "" {
		t.Fatal("ProbeFailureReason(bad) empty, want a reason")
	}
	// ProbeAll：只检可用可执行（good、bad），off 不进。
	checked, ok, failed := rg.ProbeAll()
	if checked != 2 || ok != 1 || failed != 1 {
		t.Fatalf("ProbeAll = %d/%d/%d, want 2/1/1", checked, ok, failed)
	}
	// 入口修好后重探通过，失败记录清掉。
	if err := rg.ApplyEntryPointFix("cap-test-bad", "echo"); err != nil {
		t.Fatalf("ApplyEntryPointFix(bad -> echo): %v", err)
	}
	if err := rg.Probe("cap-test-bad"); err != nil {
		t.Fatalf("Probe(bad after fix): %v", err)
	}
	if rg.ProbeFailed("cap-test-bad") {
		t.Fatal("ProbeFailed(bad after fix) = true, want false")
	}
}

// TestSnapshotRestoreRollback 是快照回滚：第 23 步注册前快照 → 加一条
// 锻造工具 → RestoreSnapshot 回滚 → 条数与磁盘内容都回到快照状态。
func TestSnapshotRestoreRollback(t *testing.T) {
	dir := fixture(t)
	rg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if rg.Count() != 90 {
		t.Fatalf("fixture 应为 90 条，得到 %d", rg.Count())
	}
	snap := filepath.Join(t.TempDir(), "before.json")
	if err := rg.Snapshot(snap); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	forged := Entry{
		ToolID: "forge-snapshot-test", Category: "ops", Subcategory: "shell",
		CallType: "cli", EntryPoint: "/bin/true", OutputContract: "exit code",
		SourceFile: "forge:test", OneLiner: "A forged test tool for snapshot rollback checks.",
		Forged: true, Experimental: true, Risk: "low",
	}
	if err := rg.AddEntry(forged); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	if rg.Count() != 91 {
		t.Fatalf("加后应为 91 条，得到 %d", rg.Count())
	}
	n, err := rg.RestoreSnapshot(snap)
	if err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	if n != 90 || rg.Count() != 90 {
		t.Fatalf("回滚后应为 90 条，得到 restored=%d count=%d", n, rg.Count())
	}
	if _, ok := rg.Entry("forge-snapshot-test"); ok {
		t.Fatalf("回滚后锻造工具仍在注册表里")
	}
	// 磁盘上的 registry.json 也回到了 90 条。
	rg2, err := Load(dir)
	if err != nil {
		t.Fatalf("回滚后重载: %v", err)
	}
	if rg2.Count() != 90 {
		t.Fatalf("磁盘 registry.json 回滚后应为 90 条，得到 %d", rg2.Count())
	}
	// 锻造工具的 L3 文件也被清掉（否则下次 Load 会报孤儿 L3）。
	if _, err := os.Stat(filepath.Join(dir, "index", "L3", "forge-snapshot-test.md")); !os.IsNotExist(err) {
		t.Fatalf("回滚后锻造工具的 L3 文件应被删除")
	}
}
