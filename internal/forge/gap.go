// Package forge 是第 23 步锻造核心（V1，复用优先）：缺口检测 → 复用优先
// 检索 → 评估分流（采用/包装 vs 自造）→ 蓝图 → 监工两道门 →
// staging 试造 → 注册（风险分级门禁）→ 试用期（熔断 + 修复复活）。
//
// 不做（第二段/第三段的事）：学习回路（第 24 步）、动机评分与白天
// 唤醒（第 25 步）、好奇心分支、Recall 元数据过滤。
package forge

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Gap 是缺口报告（规格 §2，存 data/forge/gaps/<gap-id>.json）。
type Gap struct {
	GapID     string         `json:"gap_id"`
	TaskID    string         `json:"task_id"`
	TaskText  string         `json:"task_text"` // 截断 ≤500 字
	Trigger   string         `json:"trigger"`   // no_tool / workaround / tool_unsupported
	Evidence  map[string]any `json:"evidence,omitempty"`
	Keywords  []string       `json:"keywords,omitempty"` // 显式搜索关键词（可空）
	CreatedTS string         `json:"created_ts"`
	Status    string         `json:"status,omitempty"` // pending / processing / done / deferred / rejected
}

// Store 是锻造的磁盘存储：缺口、蓝图、账本、留痕、快照、挂起、试用态
// 全部在 data/forge/ 下，不进 wiki、不向量化（规格 §11）。
type Store struct {
	DataDir string
}

func (s *Store) forgeDir() string      { return filepath.Join(s.DataDir, "forge") }
func (s *Store) GapsDir() string       { return filepath.Join(s.forgeDir(), "gaps") }
func (s *Store) BlueprintsDir() string { return filepath.Join(s.forgeDir(), "blueprints") }
func (s *Store) RetrievalDir() string  { return filepath.Join(s.forgeDir(), "retrieval") }
func (s *Store) StagingDir() string    { return filepath.Join(s.forgeDir(), "staging") }
func (s *Store) SnapshotsDir() string  { return filepath.Join(s.forgeDir(), "snapshots") }
func (s *Store) PendingDir() string    { return filepath.Join(s.forgeDir(), "pending") }
func (s *Store) TrialsDir() string     { return filepath.Join(s.forgeDir(), "trials") }
func (s *Store) LedgerPath() string    { return filepath.Join(s.forgeDir(), "ledger.jsonl") }

// EnsureDirs 建好锻造的全部目录（0600 文件、0700 目录）。
func (s *Store) EnsureDirs() error {
	for _, d := range []string{
		s.GapsDir(), s.BlueprintsDir(), filepath.Join(s.RetrievalDir(), "cache"),
		s.StagingDir(), s.SnapshotsDir(), s.PendingDir(), s.TrialsDir(),
	} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// NextGapID 生成下一个缺口号 gap-YYYY-MM-DD-NNN（当天自增）。
func (s *Store) NextGapID(date string) (string, error) {
	if err := s.EnsureDirs(); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(s.GapsDir())
	if err != nil {
		return "", err
	}
	n := 0
	prefix := "gap-" + date + "-"
	for _, de := range entries {
		if strings.HasPrefix(de.Name(), prefix) {
			n++
		}
	}
	return fmt.Sprintf("%s%03d", prefix, n+1), nil
}

// WriteGap 写一条缺口报告。去重：同一 task_text（前 200 字相同）7 天内
// 只记一次——命中时返回已有的 gap_id 与 (false, nil)，不写新文件。
// 返回 (gapID, isNew, err)。
func (s *Store) WriteGap(g Gap) (string, bool, error) {
	if err := s.EnsureDirs(); err != nil {
		return "", false, err
	}
	if g.TaskID == "" || g.TaskText == "" || g.Trigger == "" {
		return "", false, fmt.Errorf("forge gap: task_id/task_text/trigger 必填")
	}
	if len([]rune(g.TaskText)) > 500 {
		g.TaskText = string([]rune(g.TaskText)[:500])
	}
	if g.CreatedTS == "" {
		g.CreatedTS = time.Now().UTC().Format(time.RFC3339)
	}
	if g.Status == "" {
		g.Status = "pending"
	}
	// 去重：前 200 字相同且 7 天内。
	head := firstRunes(g.TaskText, 200)
	created, _ := time.Parse(time.RFC3339, g.CreatedTS)
	existing, err := s.ListGaps("")
	if err != nil {
		return "", false, err
	}
	for _, e := range existing {
		if firstRunes(e.TaskText, 200) != head {
			continue
		}
		ect, err := time.Parse(time.RFC3339, e.CreatedTS)
		if err != nil {
			continue
		}
		if created.Sub(ect) < 7*24*time.Hour && created.Sub(ect) > -7*24*time.Hour {
			return e.GapID, false, nil
		}
	}
	if g.GapID == "" {
		date := created.Format("2006-01-02")
		id, err := s.NextGapID(date)
		if err != nil {
			return "", false, err
		}
		g.GapID = id
	}
	b, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return "", false, err
	}
	if err := os.WriteFile(filepath.Join(s.GapsDir(), g.GapID+".json"), append(b, '\n'), 0o600); err != nil {
		return "", false, err
	}
	return g.GapID, true, nil
}

// ReadGap 读一条缺口报告。
func (s *Store) ReadGap(gapID string) (Gap, error) {
	var g Gap
	b, err := os.ReadFile(filepath.Join(s.GapsDir(), gapID+".json"))
	if err != nil {
		return g, err
	}
	if err := json.Unmarshal(b, &g); err != nil {
		return g, err
	}
	return g, nil
}

// ListGaps 列缺口报告（created_ts 先后）。status 为空时列全部。
func (s *Store) ListGaps(status string) ([]Gap, error) {
	entries, err := os.ReadDir(s.GapsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Gap
	for _, de := range entries {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".json") {
			continue
		}
		g, err := s.ReadGap(strings.TrimSuffix(de.Name(), ".json"))
		if err != nil {
			continue
		}
		if status != "" && g.Status != status {
			continue
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedTS < out[j].CreatedTS })
	return out, nil
}

// UpdateGapStatus 改一条缺口的状态（pending / processing / done /
// deferred / rejected）。
func (s *Store) UpdateGapStatus(gapID, status string) error {
	g, err := s.ReadGap(gapID)
	if err != nil {
		return err
	}
	g.Status = status
	b, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.GapsDir(), gapID+".json"), append(b, '\n'), 0o600)
}

func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// ---- 锻造账本（规格 §9） ----

// Event 是锻造账本的一行（data/forge/ledger.jsonl，只追加不修改）。
type Event struct {
	TS            string `json:"ts"`
	Event         string `json:"event"`
	GapID         string `json:"gap_id,omitempty"`
	BlueprintID   string `json:"blueprint_id,omitempty"`
	Input         string `json:"input,omitempty"`
	Output        string `json:"output,omitempty"`
	Result        string `json:"result"`                    // ok / fail / halt / rejected
	SelfCoreCalls int    `json:"self_core_calls,omitempty"` // search_deferred_rate_limit 事件必填
}

// 锻造账本事件名。
const (
	EvGapReported             = "gap_reported"
	EvReuseSearchDone         = "reuse_search_done"
	EvAdopted                 = "adopted"
	EvWrapped                 = "wrapped"
	EvSelfBuild               = "self_build"
	EvSearchExhaustedNoMatch  = "search_exhausted_no_match"
	EvSearchDeferredRateLimit = "search_deferred_rate_limit"
	EvRateLimited             = "rate_limited"
	EvBlueprintWritten        = "blueprint_written"
	EvAdvisorBlueprintHalt    = "advisor_blueprint_halt"
	EvAdvisorBlueprintPass    = "advisor_blueprint_pass"
	EvTestPass                = "test_pass"
	EvTestFail                = "test_fail"
	EvAdvisorTestHalt         = "advisor_test_halt"
	EvAdvisorTestPass         = "advisor_test_pass"
	EvRegistered              = "registered"
	EvQuarantined             = "quarantined"
	EvRevived                 = "revived"
	EvDeprecated              = "deprecated"
	EvPausedBudget            = "paused_budget"
)

// AppendEvent 追加一行锻造账本事件。
func (s *Store) AppendEvent(e Event) error {
	if err := s.EnsureDirs(); err != nil {
		return err
	}
	if e.TS == "" {
		e.TS = time.Now().UTC().Format(time.RFC3339)
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.LedgerPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// ReadEvents 读全部锻造账本事件（按文件顺序）。
func (s *Store) ReadEvents() ([]Event, error) {
	b, err := os.ReadFile(s.LedgerPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Event
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// CountDeferred14d 统计近 14 天内检索器自身导致的顺延次数
// （§2A.7 升级触发线用）：事件为 search_deferred_rate_limit 且
// self_core_calls ≥40 的才算“检索器自身导致”。
func (s *Store) CountDeferred14d(now time.Time) (int, error) {
	events, err := s.ReadEvents()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range events {
		if e.Event != EvSearchDeferredRateLimit || e.SelfCoreCalls < 40 {
			continue
		}
		t, err := time.Parse(time.RFC3339, e.TS)
		if err != nil {
			continue
		}
		if now.Sub(t) <= 14*24*time.Hour && now.Sub(t) >= 0 {
			n++
		}
	}
	return n, nil
}
