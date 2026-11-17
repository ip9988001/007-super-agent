package registry

import (
	"database/sql"
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

// 1. 加载 73 条（第 11 步加了 cap-design-hyperframes，design 7→8）。
func TestLoad73Entries(t *testing.T) {
	rg, err := Load(fixture(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := rg.Count(); got != 73 {
		t.Fatalf("Count = %d, want 73", got)
	}
	if got := len(Categories()); got != 6 {
		t.Fatalf("Categories = %d, want 6", got)
	}
	want := map[string]int{"fetch": 13, "code": 7, "seo": 7, "design": 8, "ops": 22, "memory": 16}
	for cat, n := range want {
		if got := len(rg.EntriesByCategory(cat)); got != n {
			t.Errorf("category %s = %d entries, want %d", cat, got, n)
		}
	}
}

// 2. L1 返回 6 类。
func TestL1SixCategories(t *testing.T) {
	rg, err := Load(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	l1, err := rg.L1()
	if err != nil {
		t.Fatalf("L1: %v", err)
	}
	for _, cat := range []string{"取数与抓取", "代码与审计", "SEO与内容", "设计与UI", "系统与运维", "记忆与知识"} {
		if !strings.Contains(l1, cat) {
			t.Errorf("L1 missing category %q", cat)
		}
	}
	if n := countLines(l1); n > MaxL1Lines {
		t.Errorf("L1 has %d lines, cap %d", n, MaxL1Lines)
	}
}

// 3. L2 读一份（ops 正好 25 行，顶格必须过）。
func TestL2OneCategory(t *testing.T) {
	rg, err := Load(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	l2, err := rg.L2("ops")
	if err != nil {
		t.Fatalf("L2(ops): %v", err)
	}
	if n := countLines(l2); n != 25 {
		t.Errorf("L2(ops) = %d lines, want 25", n)
	}
	if !strings.Contains(l2, "cap-ops-ffmpeg") {
		t.Error("L2(ops) missing cap-ops-ffmpeg")
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
	if !rg.NeedsProbe("cap-design-hyperframes") {
		t.Fatal("hyperframes verified=false, want NeedsProbe=true")
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
	rg.MarkVerified("cap-design-hyperframes")
	if rg.NeedsProbe("cap-design-hyperframes") {
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
