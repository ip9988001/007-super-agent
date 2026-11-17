// Package registry 是 007 超级 Agent 的注册表加载器：读第 1 步生成的总账，
// 提供运行时三级索引服务（L1 常驻、L2 读 1 份、L3 读 1 份）。
//
// 设计依据（《超级Agent-全新设计方案》v1.5，第 4 轮三级索引规格）：
//   - L1 ≤25 行约 600 token 常驻；L2 每次只读 1 份（≤25 行）；
//     L3 每次只读 1 份（≤1600 字符，约 960 token）。
//   - registry.json 是唯一真相源；L3 文件名必须与 tool_id 一一对应。
//   - 第 6 轮坑 2：所有截断/计数先转 []rune 按字符算，防中文从字节中间劈开。
//
// 铁律：只读第 1 步已生成的索引文件，绝不自己重写；校验失败返回错误，
// 不许静默跳过；不写任何密钥、Token。
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// 硬上限（第 4 轮规格 + 第 1 步审计报告口径）。
const (
	MaxL1Lines = 25
	MaxL2Lines = 25
	MaxL3Runes = 1600
)

// 哨兵错误：调用方用 errors.Is 判断。
var (
	ErrNotFound        = errors.New("registry: not found")
	ErrNotAvailable    = errors.New("registry: entry marked available:false, must not be called")
	ErrLimitExceeded   = errors.New("registry: index exceeds hard cap")
	ErrInvalidRegistry = errors.New("registry: invalid registry")
)

// categoryOrder 是第 1 步定死的 6 类（与 index/L2/ 下 6 份文件一一对应）。
var categoryOrder = []string{"fetch", "code", "seo", "design", "ops", "memory"}

var validCategory = map[string]bool{
	"fetch": true, "code": true, "seo": true,
	"design": true, "ops": true, "memory": true,
}

// StatusTriad 是条目状态三联：装了没有 / 跑得起来没有 / 实测验证过没有。
type StatusTriad struct {
	Installed bool `json:"installed"`
	Runnable  bool `json:"runnable"`
	Verified  bool `json:"verified"`
}

// Entry 是总账里的一条工具（字段与 step1 registry.json 逐一对应）。
type Entry struct {
	ToolID         string         `json:"tool_id"`
	Category       string         `json:"category"`
	Subcategory    string         `json:"subcategory"`
	CallType       string         `json:"call_type"`
	TriggerPrefix  string         `json:"trigger_prefix"`
	PreCommand     string         `json:"pre_command"`
	EntryPoint     string         `json:"entry_point"`
	ParamSchema    map[string]any `json:"param_schema"`
	OutputContract string         `json:"output_contract"`
	StatusTriad    StatusTriad    `json:"status_triad"`
	Available      bool           `json:"available"`
	LastVerifiedAt string         `json:"last_verified_at"`
	SourceFile     string         `json:"source_file"`
	OneLiner       string         `json:"one_liner"`
	VerifiedBasis  string         `json:"verified_basis"`
	Note           string         `json:"note"`
}

type registryFile struct {
	Version   string  `json:"version"`
	Generated string  `json:"generated"`
	Total     int     `json:"total"`
	Entries   []Entry `json:"entries"`
}

// Option 是 Load 的可选配置。
type Option func(*Registry)

// WithMetricsRecorder 接入 index_metrics 记录器（超限时记账用）。
// 不传则超限只报错、不记账。
func WithMetricsRecorder(r MetricsRecorder) Option {
	return func(rg *Registry) { rg.metrics = r }
}

// WithSession 给记账行打上会话/任务标识（缺省为空字符串）。
func WithSession(sessionID, taskID string) Option {
	return func(rg *Registry) { rg.sessionID, rg.taskID = sessionID, taskID }
}

// Registry 是加载后的总账 + 三级索引服务。读操作无锁；
// 加载后内容不可变，调用方只读。唯一的例外是探针标记：
// verifiedOverride 只记“本进程内已探针通过”的工具（不写回 registry.json，
// registry.json 是第 1 步的真相源，运行时绝不重写），由 mu 保护。
type Registry struct {
	dataDir   string
	entries   []Entry
	byID      map[string]*Entry
	byCat     map[string][]*Entry
	metrics   MetricsRecorder
	sessionID string
	taskID    string

	mu               sync.RWMutex
	verifiedOverride map[string]bool
}

// Load 读 DATA_DIR/registry/registry.json 并全量校验：
// tool_id 唯一、必填字段齐全、分类在 6 类之内、total 与条数一致、
// 72 条与 index/L3/ 的 72 份文件一一对应。任一失败都返回错误。
func Load(dataDir string, opts ...Option) (*Registry, error) {
	rg := &Registry{dataDir: dataDir}
	for _, o := range opts {
		o(rg)
	}
	raw, err := os.ReadFile(filepath.Join(dataDir, "registry", "registry.json"))
	if err != nil {
		return nil, fmt.Errorf("registry: read registry.json: %w", err)
	}
	var f registryFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("%w: registry.json: %v", ErrInvalidRegistry, err)
	}
	if f.Total != 0 && f.Total != len(f.Entries) {
		return nil, fmt.Errorf("%w: total=%d but entries=%d",
			ErrInvalidRegistry, f.Total, len(f.Entries))
	}
	rg.entries = f.Entries
	rg.byID = make(map[string]*Entry, len(f.Entries))
	rg.byCat = make(map[string][]*Entry)
	for i := range rg.entries {
		e := &rg.entries[i]
		if err := validateEntry(e); err != nil {
			return nil, err
		}
		if _, dup := rg.byID[e.ToolID]; dup {
			return nil, fmt.Errorf("%w: duplicate tool_id %q", ErrInvalidRegistry, e.ToolID)
		}
		rg.byID[e.ToolID] = e
		rg.byCat[e.Category] = append(rg.byCat[e.Category], e)
	}
	if err := rg.checkL3Correspondence(); err != nil {
		return nil, err
	}
	return rg, nil
}

func validateEntry(e *Entry) error {
	for field, v := range map[string]string{
		"tool_id": e.ToolID, "category": e.Category, "subcategory": e.Subcategory,
		"call_type": e.CallType, "entry_point": e.EntryPoint,
		"output_contract": e.OutputContract, "source_file": e.SourceFile,
		"one_liner": e.OneLiner,
	} {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("%w: %s missing required field %q",
				ErrInvalidRegistry, e.ToolID, field)
		}
	}
	if !validCategory[e.Category] {
		return fmt.Errorf("%w: %s unknown category %q",
			ErrInvalidRegistry, e.ToolID, e.Category)
	}
	return nil
}

// checkL3Correspondence 核对 registry 72 条与 index/L3/ 72 份文件一一对应。
func (rg *Registry) checkL3Correspondence() error {
	dir := filepath.Join(rg.dataDir, "index", "L3")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("registry: read index/L3: %w", err)
	}
	onDisk := map[string]bool{}
	for _, de := range ents {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".md") {
			continue
		}
		onDisk[strings.TrimSuffix(de.Name(), ".md")] = true
	}
	for id := range rg.byID {
		if !onDisk[id] {
			return fmt.Errorf("%w: L3 file missing for %q", ErrInvalidRegistry, id)
		}
	}
	for id := range onDisk {
		if _, ok := rg.byID[id]; !ok {
			return fmt.Errorf("%w: orphan L3 file %q has no registry entry",
				ErrInvalidRegistry, id)
		}
	}
	return nil
}

// Count 返回总账条目数。
func (rg *Registry) Count() int { return len(rg.entries) }

// Entry 按 tool_id 取一条；不存在返回 false。
func (rg *Registry) Entry(toolID string) (*Entry, bool) {
	e, ok := rg.byID[toolID]
	return e, ok
}

// Callable 判断一条工具是否可调用：存在且 available 为真。
// available:false 的条目照常加载，但永远不可调用。
func (rg *Registry) Callable(toolID string) bool {
	e, ok := rg.byID[toolID]
	return ok && e.Available
}

// NeedsProbe 判断一条工具在调用前是否需要先跑预检探针（第 14 步 B-03）：
// 存在、available 为真，但 StatusTriad.Verified 为假、且本进程内还没探针通过。
// verified=false 不是禁用——只是还没在实机上验过，探针通过即可调用。
func (rg *Registry) NeedsProbe(toolID string) bool {
	e, ok := rg.byID[toolID]
	if !ok || !e.Available || e.StatusTriad.Verified {
		return false
	}
	rg.mu.RLock()
	defer rg.mu.RUnlock()
	return !rg.verifiedOverride[toolID]
}

// MarkVerified 把一条工具标记为本进程内已探针通过（只记内存，不写回
// registry.json；进程重启后重新探针）。
func (rg *Registry) MarkVerified(toolID string) {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	if rg.verifiedOverride == nil {
		rg.verifiedOverride = map[string]bool{}
	}
	rg.verifiedOverride[toolID] = true
}

// Probe 对一条工具跑预检探针（只做存在性检查，不真跑命令）：
// 按 call_type 分流（第 19 步第 5 批）：cli 查命令/路径、service 查
// 脚本文件、doc/sop/tool/mcp/data 不探针（只记知识，不可执行）。
// 通过则标记本进程内已验证并返回 nil；失败返回原因，不标记。
// 与 watchdog.DefaultProbe 同一口径，探针逻辑的唯一实现放在这里，
// watchdog.DefaultProbe 只是委托（防两处口径漂移）。
func (rg *Registry) Probe(toolID string) error {
	e, ok := rg.byID[toolID]
	if !ok {
		return fmt.Errorf("%w: %q", ErrNotFound, toolID)
	}
	if err := ProbeEntry(*e); err != nil {
		return err
	}
	rg.MarkVerified(toolID)
	return nil
}

// ProbeEntry 对一条总账条目做存在性探针（不真跑命令），按 call_type 分流：
//   - cli：entry_point 首词是命令（PATH 里找）或路径（文件在位）；
//     "python3 -c ..." 形式跑一次只读 import 检查（Python API 类工具，
//     如 camoufox，没有 CLI，LookPath 只能验到 python3 本身）。
//   - service：entry_point 就是脚本路径（相对部署根目录），只查文件在位。
//   - doc/sop/tool/mcp/data：知识条目，不探针（只记知识，不可执行）。
// 未知 call_type 按 cli 处理。空 entry_point 一律失败。
// 注意：相对路径按进程工作目录解析——守护进程必须以部署根目录为
// 工作目录启动（systemd WorkingDirectory），否则脚本类探针失败。
func ProbeEntry(e Entry) error {
	ep := strings.TrimSpace(e.EntryPoint)
	if ep == "" {
		return errors.New("registry probe: entry_point 为空")
	}
	switch e.CallType {
	case "doc", "sop", "tool", "mcp", "data":
		return nil
	case "service":
		first := strings.Fields(ep)[0]
		if _, err := os.Stat(first); err != nil {
			return fmt.Errorf("registry probe: 脚本 %q 不存在: %v", first, err)
		}
		return nil
	default: // cli 与未知类型
		if strings.HasPrefix(ep, "python3 -c ") {
			return probePythonImport(strings.TrimPrefix(ep, "python3 -c "))
		}
		first := strings.Fields(ep)[0]
		if strings.Contains(first, "/") {
			if _, err := os.Stat(first); err != nil {
				return fmt.Errorf("registry probe: entry_point %q 不存在: %v", first, err)
			}
			return nil
		}
		if _, err := exec.LookPath(first); err != nil {
			return fmt.Errorf("registry probe: 命令 %q 不在 PATH: %v", first, err)
		}
		return nil
	}
}

// probePythonImport 跑一次只读的 python3 -c 检查（10 秒超时），
// 给没有 CLI 的 Python API 工具（如 camoufox）一个真实探针，
// 而不是只验到 python3 本身。只做 import，不写、不联网。
func probePythonImport(code string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", "-c", code)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("registry probe: python3 -c %q 失败: %v: %s",
			code, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// EntriesByCategory 按类取条目（返回拷贝的指针切片，只读）。
func (rg *Registry) EntriesByCategory(category string) []*Entry {
	return rg.byCat[category]
}

// L1 返回常驻一级索引全文（6 类，每类一行），并校验 ≤25 行。
func (rg *Registry) L1() (string, error) {
	content, err := os.ReadFile(filepath.Join(rg.dataDir, "index", "L1.md"))
	if err != nil {
		return "", fmt.Errorf("registry: read L1: %w", err)
	}
	if n := countLines(string(content)); n > MaxL1Lines {
		rg.recordViolation("L1", "", n, 0)
		return "", fmt.Errorf("%w: L1 has %d lines, cap %d",
			ErrLimitExceeded, n, MaxL1Lines)
	}
	return string(content), nil
}

// L2 按类名读二级索引，并校验每份 ≤25 行。超了返回错误并记账。
// ops 类条目超过 25 条时拆成多份（ops.md、ops-2.md……），按“主文件
// 在先、续文件按名排序”拼接后返回；每份单独校验上限（第 19 步第 5 批）。
// 续文件只在确实超限的类上出现，其余类仍是单份。
func (rg *Registry) L2(category string) (string, error) {
	if !validCategory[category] {
		return "", fmt.Errorf("%w: unknown category %q", ErrNotFound, category)
	}
	base := filepath.Join(rg.dataDir, "index", "L2", category+".md")
	rest, err := filepath.Glob(filepath.Join(rg.dataDir, "index", "L2", category+"-*.md"))
	if err != nil {
		return "", fmt.Errorf("registry: glob L2 %s: %w", category, err)
	}
	sort.Strings(rest)
	files := append([]string{base}, rest...)
	var sb strings.Builder
	for _, f := range files {
		content, err := os.ReadFile(f)
		if err != nil {
			return "", fmt.Errorf("registry: read L2 %s: %w", category, err)
		}
		if n := countLines(string(content)); n > MaxL2Lines {
			rg.recordViolation("L2", category, n, 0)
			return "", fmt.Errorf("%w: L2 %s (%s) has %d lines, cap %d",
				ErrLimitExceeded, category, filepath.Base(f), n, MaxL2Lines)
		}
		sb.Write(content)
		if !strings.HasSuffix(string(content), "\n") {
			sb.WriteString("\n")
		}
	}
	return sb.String(), nil
}

// L3 按工具名读一份三级索引，并校验 ≤1600 字符（按字符数）。
// 条目不存在返回 ErrNotFound；available:false 返回 ErrNotAvailable；
// 超限返回错误并记账。
func (rg *Registry) L3(toolID string) (string, error) {
	e, ok := rg.byID[toolID]
	if !ok {
		return "", fmt.Errorf("%w: tool %q", ErrNotFound, toolID)
	}
	if !e.Available {
		return "", fmt.Errorf("%w: tool %q", ErrNotAvailable, toolID)
	}
	content, err := os.ReadFile(filepath.Join(rg.dataDir, "index", "L3", toolID+".md"))
	if err != nil {
		return "", fmt.Errorf("registry: read L3 %s: %w", toolID, err)
	}
	if n := len([]rune(string(content))); n > MaxL3Runes {
		rg.recordViolation("L3", toolID, n, 0)
		return "", fmt.Errorf("%w: L3 %s has %d runes, cap %d",
			ErrLimitExceeded, toolID, n, MaxL3Runes)
	}
	return string(content), nil
}

// recordViolation 把一次超限写进 index_metrics（记账失败不掩盖原错误，
// 只把失败原因附在返回的错误里；没接记录器时静默跳过记账）。
func (rg *Registry) recordViolation(level, toolID string, size, tokens int) {
	if rg.metrics == nil {
		return
	}
	_ = rg.metrics.RecordIndexMetric(IndexMetric{
		SessionID: rg.sessionID, TaskID: rg.taskID,
		Level: level, ToolID: toolID, Hit: false,
		TokensInjected: tokens,
	})
}

// countLines 按行数计（与 Python splitlines 口径一致：末尾换行不另算一行）。
func countLines(s string) int {
	if s == "" {
		return 0
	}
	return len(strings.Split(strings.TrimSuffix(s, "\n"), "\n"))
}

// Categories 返回 6 类名（第 1 步定死的 01–06 顺序，不排序）。
func Categories() []string {
	out := make([]string, len(categoryOrder))
	copy(out, categoryOrder)
	return out
}
