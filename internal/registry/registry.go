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
// 2026-11-18 重新分类 v1.7：ops 拆成 security/runtime/media/ops，
// 大类从 6 个变 9 个；L2 下新增 security.md、runtime.md、media.md。
var categoryOrder = []string{"fetch", "code", "seo", "design", "security", "runtime", "media", "ops", "memory"}

var validCategory = map[string]bool{
	"fetch": true, "code": true, "seo": true,
	"design": true, "security": true, "runtime": true,
	"media": true, "ops": true, "memory": true,
}

// StatusTriad 是条目状态三联：装了没有 / 跑得起来没有 / 实测验证过没有。
type StatusTriad struct {
	Installed bool `json:"installed"`
	Runnable  bool `json:"runnable"`
	Verified  bool `json:"verified"`
}

// Entry 是总账里的一条工具（字段与 step1 registry.json 逐一对应）。
// 第 23 步锻造标记（omitempty：旧条目没有这些字段，Load 不受影响）：
// Forged 表示由锻造流程注册；Experimental 表示仍在试用期；
// BlueprintID 是来源蓝图；Risk 是风险分级 low/medium/high；
// PrivateCLIOnly 表示 GPL/AGPL 口径 B 的独立 CLI 工具，公开打包须跳过。
// Hidden 表示默认隐藏（第 16 项）：未过试用期 / 未验证的锻造工具先藏起来，
// 不进任何工具清单、不可派单；验证通过后把 hidden 改回 false 即可放行。
// FastPath 表示该工具确实零 CLI 参数且原始输出可直接作回执，TOOL 派单才走
// runTool 快捷路径；默认 false（安全默认走 runAgent），防空参裸调。
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
	Forged         bool           `json:"forged,omitempty"`
	Experimental   bool           `json:"experimental,omitempty"`
	BlueprintID    string         `json:"blueprint_id,omitempty"`
	Risk           string         `json:"risk,omitempty"`
	PrivateCLIOnly bool           `json:"private_cli_only,omitempty"`
	Hidden         bool           `json:"hidden,omitempty"`
	FastPath       bool           `json:"fast_path,omitempty"`
}

type registryFile struct {
	Version       string            `json:"version"`
	Generated     string            `json:"generated"`
	Total         int               `json:"total"`
	Entries       []Entry           `json:"entries"`
	Categories    []CategoryMeta    `json:"categories"`
	Subcategories []SubcategoryMeta `json:"subcategories"`
}

// CategoryMeta 是一级大类的元数据（v1.7 重新分类新增）：
// id 是 slug，name 是展示名，description 是 200-400 词的英文说明，
// 懒加载第一级只显示其首句摘要，展开后才显示全文。
type CategoryMeta struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// SubcategoryMeta 是二级子类的元数据（v1.7 新增）：
// category 是所属大类，id 是子类 slug，description 是 100-200 词的英文说明。
type SubcategoryMeta struct {
	Category    string `json:"category"`
	ID          string `json:"id"`
	Description string `json:"description"`
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
// verifiedOverride 只记“本进程内已探针通过”的工具，probeFailed 只记
// “本进程内已探针失败”的工具（tool_id → 失败原因），都不写回
// registry.json，registry.json 是第 1 步的真相源，运行时绝不重写，
// 由 mu 保护。第 20 步：entryPointOverride 是同一口径的第二层覆盖——
// 自进化自动应用的 entry_point 修正只活在内存里，重启即失效，
// 永久修正仍须由人写进 registry.json；存入前必须先过探针。
// 第 23 步例外：锻造注册/隔离会改写 registry.json（AddEntry /
// RemoveEntry / RestoreSnapshot），因为锻造工具本来就是运行时新生
// 的条目；改写前一律先做版本快照（见 Snapshot）。
type Registry struct {
	dataDir   string
	entries   []Entry
	byID      map[string]*Entry
	byCat     map[string][]*Entry
	catMeta   []CategoryMeta
	subMeta   []SubcategoryMeta
	version   string // registry.json 的 version 字段（Save 时原样写回）
	generated string // registry.json 的 generated 字段（Save 时原样写回）
	metrics   MetricsRecorder
	sessionID string
	taskID    string

	mu                 sync.RWMutex
	verifiedOverride   map[string]bool
	probeFailed        map[string]string
	entryPointOverride map[string]string

	// fullIndexOnce/fullIndexTokensVal：全量索引基线（懒加载省下的 token
	// 相对它算），只算一次、缓存起来。见 fullIndexTokens。
	fullIndexOnce      sync.Once
	fullIndexTokensVal int
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
	rg.catMeta = f.Categories
	rg.subMeta = f.Subcategories
	rg.version = f.Version
	rg.generated = f.Generated
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

// AllEntries 返回全部条目的指针切片（只读；第 23 步锻造的第零层
// 检查用）。返回的是内部指针，调用方只读字段，不许改写。
func (rg *Registry) AllEntries() []*Entry {
	rg.mu.RLock()
	defer rg.mu.RUnlock()
	out := make([]*Entry, 0, len(rg.entries))
	for i := range rg.entries {
		out = append(out, &rg.entries[i])
	}
	return out
}

// SetExperimental 改一条锻造工具的 experimental 标记并落盘
// （第 23 步试用期转正时去掉标记用）。非锻造条目拒绝。
func (rg *Registry) SetExperimental(toolID string, experimental bool) error {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	e, ok := rg.byID[toolID]
	if !ok {
		return fmt.Errorf("registry: 没有工具 %q", toolID)
	}
	if !e.Forged {
		return fmt.Errorf("registry: %q 不是锻造工具", toolID)
	}
	e.Experimental = experimental
	return rg.persist()
}

// Entry 按 tool_id 取一条；不存在返回 false。
// 返回的是拷贝（带上内存覆盖层的 entry_point）：调用方只读字段，
// 改返回值的字段不会影响总账；真正的修正只走 ApplyEntryPointFix。
func (rg *Registry) Entry(toolID string) (*Entry, bool) {
	e, ok := rg.byID[toolID]
	if !ok {
		return nil, false
	}
	cp := *e
	rg.mu.RLock()
	if ep, ok := rg.entryPointOverride[toolID]; ok {
		cp.EntryPoint = ep
	}
	rg.mu.RUnlock()
	return &cp, true
}

// effectiveEntryPoint 取一条工具当前生效的 entry_point（覆盖层优先）。
func (rg *Registry) effectiveEntryPoint(toolID string) (string, bool) {
	e, ok := rg.byID[toolID]
	if !ok {
		return "", false
	}
	rg.mu.RLock()
	defer rg.mu.RUnlock()
	if ep, ok := rg.entryPointOverride[toolID]; ok {
		return ep, true
	}
	return e.EntryPoint, true
}

// Callable 判断一条工具是否可调用：存在且 available 为真。
// available:false 的条目照常加载，但永远不可调用。
// Executable 是一条工具能不能被真正执行：只有 cli（直接跑命令）与
// service（跑脚本文件）两类能进执行层；doc/sop/tool/mcp/data 是知识
// 条目，只能读、不能跑。2026-11-18 ping0 真机测试：规划器挑了
// call_type=tool 的 cap-memory-recall-memory（entry_point
// "recall_memory" 根本不存在），执行器启动即失败，整个 15 步计划
// 0 步完成。执行层只认可执行工具。
func (e *Entry) Executable() bool {
	return e.CallType == "cli" || e.CallType == "service"
}

// Callable 是一条工具能不能被派单执行：在册、可用、可执行、且未默认隐藏。
// 只看 available 不够：知识条目也标 available:true，但跑不起来。
// Hidden 的工具（未过试用期的锻造工具）一律不可派单，直到验证通过改回 false。
func (rg *Registry) Callable(toolID string) bool {
	e, ok := rg.byID[toolID]
	return ok && e.Available && e.Executable() && !e.Hidden
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

// UnmarkVerified 撤掉一条工具的本进程内已验证标记（第 20 步每周重建用：
// 新坏的工具要把之前探针通过留下的标记清掉，否则 NeedsProbe 永远
// 认为它已验证、再也不探针）。只动内存，不写回 registry.json。
func (rg *Registry) UnmarkVerified(toolID string) {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	delete(rg.verifiedOverride, toolID)
}

// ApplyEntryPointFix 应用一条 entry_point 修正到内存覆盖层（第 20 步
// 自进化自动应用档唯一可改注册表元数据的操作）：先对新入口跑探针，
// 通过才存入覆盖层；探针失败返回错误、不存。工具不存在、不可用、
// 新入口为空都拒绝。registry.json 文件本身永远不动。
func (rg *Registry) ApplyEntryPointFix(toolID, newEntryPoint string) error {
	e, ok := rg.byID[toolID]
	if !ok {
		return fmt.Errorf("%w: %q", ErrNotFound, toolID)
	}
	if !e.Available {
		return fmt.Errorf("%w: tool %q", ErrNotAvailable, toolID)
	}
	newEntryPoint = strings.TrimSpace(newEntryPoint)
	if newEntryPoint == "" {
		return errors.New("registry apply entry_point fix: 新入口为空")
	}
	probeCopy := *e
	probeCopy.EntryPoint = newEntryPoint
	if err := ProbeEntry(probeCopy); err != nil {
		return fmt.Errorf("registry apply entry_point fix %q: 新入口探针失败: %w", toolID, err)
	}
	rg.mu.Lock()
	defer rg.mu.Unlock()
	if rg.entryPointOverride == nil {
		rg.entryPointOverride = map[string]string{}
	}
	rg.entryPointOverride[toolID] = newEntryPoint
	return nil
}

// EntryPointOverrides 返回当前内存覆盖层的快照拷贝（重建报告与核验用）。
func (rg *Registry) EntryPointOverrides() map[string]string {
	rg.mu.RLock()
	defer rg.mu.RUnlock()
	out := make(map[string]string, len(rg.entryPointOverride))
	for k, v := range rg.entryPointOverride {
		out[k] = v
	}
	return out
}

// Probe 对一条工具跑预检探针（只做存在性检查，不真跑命令）：
// 按 call_type 分流（第 19 步第 5 批）：cli 查命令/路径、service 查
// 脚本文件、doc/sop/tool/mcp/data 不探针（只记知识，不可执行）。
// 通过则标记本进程内已验证并返回 nil；失败返回原因，并记一条
// probeFailed（调用方据此把该条从规划器清单里拿掉，模型没机会
// 再挑一个不存在的工具）；下一次探针通过会清掉这条失败记录。
// 与 watchdog.DefaultProbe 同一口径，探针逻辑的唯一实现放在这里，
// watchdog.DefaultProbe 只是委托（防两处口径漂移）。
func (rg *Registry) Probe(toolID string) error {
	e, ok := rg.byID[toolID]
	if !ok {
		return fmt.Errorf("%w: %q", ErrNotFound, toolID)
	}
	// 探针跑生效的入口（内存覆盖层优先），否则自进化修好的路径
	// 永远探不到、也永远标不上已验证。
	ep, _ := rg.effectiveEntryPoint(toolID)
	probeCopy := *e
	probeCopy.EntryPoint = ep
	if err := ProbeEntry(probeCopy); err != nil {
		rg.mu.Lock()
		if rg.probeFailed == nil {
			rg.probeFailed = map[string]string{}
		}
		rg.probeFailed[toolID] = err.Error()
		delete(rg.verifiedOverride, toolID)
		rg.mu.Unlock()
		return err
	}
	rg.mu.Lock()
	delete(rg.probeFailed, toolID)
	rg.mu.Unlock()
	rg.MarkVerified(toolID)
	return nil
}

// ProbeFailed 判断一条工具是否在本进程内已有探针失败记录。
// 没探过的不算失败——探针没跑 ≠ 坏，清单里照常列它；只有探过且
// 失败的才从规划器清单里拿掉。
func (rg *Registry) ProbeFailed(toolID string) bool {
	rg.mu.RLock()
	defer rg.mu.RUnlock()
	_, ok := rg.probeFailed[toolID]
	return ok
}

// ProbeFailureReason 取一条工具的探针失败原因（没失败返回空串）。
func (rg *Registry) ProbeFailureReason(toolID string) string {
	rg.mu.RLock()
	defer rg.mu.RUnlock()
	return rg.probeFailed[toolID]
}

// ProbeAll 对全部可用可执行条目跑一遍探针（只做存在性检查，不真跑
// 命令），结果记进本进程覆盖层：通过的标已验证，失败的记 probeFailed。
// 启动时调一次，让规划器清单一开始就是干净的；重建时也可复用。
// 返回（已检数，通过数，失败数）。
func (rg *Registry) ProbeAll() (checked, ok, failed int) {
	for _, cat := range categoryOrder {
		for _, e := range rg.byCat[cat] {
			if !e.Available || !e.Executable() {
				continue
			}
			checked++
			if err := rg.Probe(e.ToolID); err != nil {
				failed++
			} else {
				ok++
			}
		}
	}
	return checked, ok, failed
}

// ---- 第 23 步锻造：注册 / 隔离 / 快照（改写 registry.json 的唯一入口） ----

// registryPath 返回 registry.json 的路径。
func (rg *Registry) registryPath() string {
	return filepath.Join(rg.dataDir, "registry", "registry.json")
}

// persist 把内存里的总账写回 registry.json（0600），并写 L3 文件。
// 调用方须已持有 rg.mu 写锁；只在 AddEntry / RemoveEntry /
// RestoreSnapshot 内部调。
func (rg *Registry) persist() error {
	f := registryFile{
		Version: rg.version, Generated: rg.generated,
		Total: len(rg.entries), Entries: rg.entries,
		Categories: rg.catMeta, Subcategories: rg.subMeta,
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("registry persist: %w", err)
	}
	if err := os.WriteFile(rg.registryPath(), append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("registry persist: %w", err)
	}
	return nil
}

// l3Content 给一条锻造工具生成最小 L3 文件（≤1600 字符，供 Load 的
// L3 对应校验与人查阅）。非锻造条目不走这里。
func l3Content(e Entry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n## 一句话\n%s\n\n", e.ToolID, e.OneLiner)
	fmt.Fprintf(&b, "## 怎么调\n入口：`%s`\n\n", e.EntryPoint)
	fmt.Fprintf(&b, "## 来源\n锻造注册（蓝图 %s，风险 %s）。\n\n", e.BlueprintID, e.Risk)
	fmt.Fprintf(&b, "## 验收\n退出码 0 为成功；失败返回 code 与 stderr 头 600 字。\n")
	if e.PrivateCLIOnly {
		b.WriteString("\n> private-cli-only：GPL/AGPL 独立 CLI 工具，公开打包自动跳过。\n")
	}
	return b.String()
}

// AddEntry 注册一条新工具（第 23 步锻造用）：内存 + registry.json +
// L3 文件三处一起落盘。tool_id 重复、分类非法、必填字段缺失、风险
// 分级非法都拒绝。调用前须先做版本快照（见 Snapshot）。
// 注意：切片用“建新切片替换”的方式更新，读侧持旧切片不受影响；
// 锻造只在夜间窗口跑，网关串行，冲突窗口极小。
func (rg *Registry) AddEntry(e Entry) error {
	if err := validateEntry(&e); err != nil {
		return err
	}
	if e.Risk != "" && e.Risk != "low" && e.Risk != "medium" && e.Risk != "high" {
		return fmt.Errorf("%w: %s 非法 risk %q", ErrInvalidRegistry, e.ToolID, e.Risk)
	}
	rg.mu.Lock()
	defer rg.mu.Unlock()
	if _, dup := rg.byID[e.ToolID]; dup {
		return fmt.Errorf("%w: duplicate tool_id %q", ErrInvalidRegistry, e.ToolID)
	}
	// 先写 L3 文件（失败则内存不动）；再更新内存；最后落盘 registry.json。
	l3Path := filepath.Join(rg.dataDir, "index", "L3", e.ToolID+".md")
	content := l3Content(e)
	if n := len([]rune(content)); n > MaxL3Runes {
		return fmt.Errorf("%w: L3 %s has %d runes, cap %d", ErrLimitExceeded, e.ToolID, n, MaxL3Runes)
	}
	if err := os.WriteFile(l3Path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("registry add %q: 写 L3 失败: %w", e.ToolID, err)
	}
	newEntries := make([]Entry, len(rg.entries)+1)
	copy(newEntries, rg.entries)
	newEntries[len(rg.entries)] = e
	rg.entries = newEntries
	rg.byID[e.ToolID] = &rg.entries[len(rg.entries)-1]
	newCat := make([]*Entry, 0, len(rg.byCat[e.Category])+1)
	newCat = append(newCat, rg.byCat[e.Category]...)
	newCat = append(newCat, &rg.entries[len(rg.entries)-1])
	rg.byCat[e.Category] = newCat
	if err := rg.persist(); err != nil {
		// 落盘失败：回滚内存与 L3（尽力，不掩盖原错）。
		delete(rg.byID, e.ToolID)
		rg.entries = rg.entries[:len(rg.entries)-1]
		rg.byCat[e.Category] = rg.byCat[e.Category][:len(rg.byCat[e.Category])-1]
		_ = os.Remove(l3Path)
		return err
	}
	return nil
}

// RemoveEntry 从注册表摘掉一条工具（第 23 步隔离用）：内存 +
// registry.json + L3 文件三处一起清掉。不存在的 tool_id 返回
// ErrNotFound。调用前应已做版本快照。
func (rg *Registry) RemoveEntry(toolID string) error {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	e, ok := rg.byID[toolID]
	if !ok {
		return fmt.Errorf("%w: %q", ErrNotFound, toolID)
	}
	cat := e.Category
	newEntries := make([]Entry, 0, len(rg.entries)-1)
	for i := range rg.entries {
		if rg.entries[i].ToolID != toolID {
			newEntries = append(newEntries, rg.entries[i])
		}
	}
	rg.entries = newEntries
	delete(rg.byID, toolID)
	// byID 指针全部重建（entries 切片换了，旧指针失效）。
	rg.byID = make(map[string]*Entry, len(rg.entries))
	for i := range rg.entries {
		rg.byID[rg.entries[i].ToolID] = &rg.entries[i]
	}
	newCat := make([]*Entry, 0, len(rg.byCat[cat]))
	for _, ce := range rg.byCat[cat] {
		if ce.ToolID != toolID {
			if ne, ok := rg.byID[ce.ToolID]; ok {
				newCat = append(newCat, ne)
			}
		}
	}
	rg.byCat[cat] = newCat
	delete(rg.verifiedOverride, toolID)
	delete(rg.probeFailed, toolID)
	delete(rg.entryPointOverride, toolID)
	if err := rg.persist(); err != nil {
		return err
	}
	l3Path := filepath.Join(rg.dataDir, "index", "L3", toolID+".md")
	if err := os.Remove(l3Path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("registry remove %q: 删 L3 失败: %w", toolID, err)
	}
	return nil
}

// Snapshot 把当前 registry.json 原样复制到 path（第 23 步注册前必做，
// 隔离回滚用）。目录不存在时创建。
func (rg *Registry) Snapshot(path string) error {
	raw, err := os.ReadFile(rg.registryPath())
	if err != nil {
		return fmt.Errorf("registry snapshot: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("registry snapshot: %w", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("registry snapshot: %w", err)
	}
	return nil
}

// RestoreSnapshot 用快照文件恢复注册表（第 23 步隔离回滚的兜底）：
// 快照非法时拒绝、不动内存。恢复后删掉快照里没有的工具的 L3 文件，
// 再把恢复后的状态落盘。成功返回恢复的条目数。
func (rg *Registry) RestoreSnapshot(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("registry restore snapshot: %w", err)
	}
	var f registryFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return 0, fmt.Errorf("%w: snapshot %s: %v", ErrInvalidRegistry, path, err)
	}
	if f.Total != 0 && f.Total != len(f.Entries) {
		return 0, fmt.Errorf("%w: snapshot total=%d but entries=%d",
			ErrInvalidRegistry, f.Total, len(f.Entries))
	}
	rg.mu.Lock()
	defer rg.mu.Unlock()
	oldIDs := map[string]bool{}
	for id := range rg.byID {
		oldIDs[id] = true
	}
	for i := range f.Entries {
		if err := validateEntry(&f.Entries[i]); err != nil {
			return 0, err
		}
	}
	rg.entries = f.Entries
	rg.version = f.Version
	rg.generated = f.Generated
	rg.catMeta = f.Categories
	rg.subMeta = f.Subcategories
	rg.byID = make(map[string]*Entry, len(f.Entries))
	rg.byCat = make(map[string][]*Entry)
	for i := range rg.entries {
		e := &rg.entries[i]
		rg.byID[e.ToolID] = e
		rg.byCat[e.Category] = append(rg.byCat[e.Category], e)
	}
	// 删掉快照里没有的工具的 L3 文件（多余的 L3 会让下次 Load 报错）。
	for id := range oldIDs {
		if _, ok := rg.byID[id]; !ok {
			_ = os.Remove(filepath.Join(rg.dataDir, "index", "L3", id+".md"))
			delete(rg.verifiedOverride, id)
			delete(rg.probeFailed, id)
			delete(rg.entryPointOverride, id)
		}
	}
	if err := rg.persist(); err != nil {
		return 0, err
	}
	return len(rg.entries), nil
}

// PublicEntries 返回可进公开打包的条目（第 23 步 GPL/AGPL 口径 B 的
// 硬控制）：带 private_cli_only 标记的工具一律排除。公开库打包流程
// 必须用它取清单，不许直接读全部条目。
func (rg *Registry) PublicEntries() []*Entry {
	rg.mu.RLock()
	defer rg.mu.RUnlock()
	var out []*Entry
	for i := range rg.entries {
		if rg.entries[i].PrivateCLIOnly {
			continue
		}
		cp := rg.entries[i]
		out = append(out, &cp)
	}
	return out
}

//   - cli：entry_point 首词是命令（PATH 里找）或路径（文件在位）；
//     "python3 -c ..." 形式跑一次只读 import 检查（Python API 类工具，
//     如 camoufox，没有 CLI，LookPath 只能验到 python3 本身）。
//   - service：entry_point 就是脚本路径（相对部署根目录），查文件在位
//     且可执行（缺 +x 也失败——执行器直接 exec，没 +x 跑不起来）。
//
// ProbeEntry 对一条总账条目做存在性探针（不真跑命令），按 call_type 分流：
//   - cli：entry_point 首词是命令（PATH 里找）或路径（文件在位）；
//     "python3 -c ..." 形式跑一次只读 import 检查（Python API 类工具，
//     如 camoufox，没有 CLI，LookPath 只能验到 python3 本身）。
//   - service：entry_point 就是脚本路径（相对部署根目录），查文件在位
//     且可执行（缺 +x 也失败——执行器直接 exec，没 +x 跑不起来）。
//   - doc/sop/tool/mcp/data：知识条目，不探针（只记知识，不可执行）。
//
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
		info, err := os.Stat(first)
		if err != nil {
			return fmt.Errorf("registry probe: 脚本 %q 不存在: %v", first, err)
		}
		// 服务条目是直接跑的脚本：文件必须可执行，否则执行器 exec 会报
		// 权限错误。2026-11-18 核查：totp.js/link-selftest.cjs 只查在位
		// 就放过，实际跑不起来——在位不够，还得能跑。
		if info.Mode()&0111 == 0 {
			return fmt.Errorf("registry probe: 脚本 %q 不可执行（缺 +x）", first)
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

// SetSession 给记账行换上当前会话/任务标识（前置与执行共用一个 Registry
// 时，每单开始前调一次；没接记录器时无操作）。
func (rg *Registry) SetSession(sessionID, taskID string) {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	rg.sessionID, rg.taskID = sessionID, taskID
}

// fullIndexTokens 是全量索引基线：把注册表里全部大类说明、子类说明、
// 工具 one-liner 与 tool_id 全摊开要注入的 token 估算（EstimateTokens
// 口径）。懒加载每次只注入其中一小片，省下的数 = 基线 − 本次注入。
// 保守口径：未计 L3 工具全文（不在注册表里），故省下的数是下限。
// 只算一次，缓存起来。
func (rg *Registry) fullIndexTokens() int {
	rg.fullIndexOnce.Do(func() {
		total := 0
		for _, c := range rg.catMeta {
			total += EstimateTokens(c.ID + c.Name + c.Description)
		}
		for _, s := range rg.subMeta {
			total += EstimateTokens(s.ID + s.Description)
		}
		for _, e := range rg.entries {
			total += EstimateTokens(e.ToolID + e.OneLiner)
		}
		rg.fullIndexTokensVal = total
	})
	return rg.fullIndexTokensVal
}

// RecordIndexLoad 把一次懒加载读索引记进 index_metrics（第 6 项）：
// level 是 L1/L2/L3，toolID 是 L3 时的子类（L1/L2 传空串），
// tokensInjected 是本次实际注入的 token 估算。
// TokensSavedVsFull = 全量基线 − 本次注入（不小于 0），Hit 记 true。
// 没接记录器时静默跳过（生产 main.go 负责接 LedgerFileRecorder）。
func (rg *Registry) RecordIndexLoad(level, toolID string, tokensInjected int) {
	if rg.metrics == nil {
		return
	}
	saved := rg.fullIndexTokens() - tokensInjected
	if saved < 0 {
		saved = 0
	}
	rg.mu.RLock()
	sid, tid := rg.sessionID, rg.taskID
	rg.mu.RUnlock()
	_ = rg.metrics.RecordIndexMetric(IndexMetric{
		SessionID: sid, TaskID: tid,
		Level: level, ToolID: toolID, Hit: true,
		TokensInjected: tokensInjected, TokensSavedVsFull: saved,
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

// CategoryMetas 返回一级大类元数据（按 categoryOrder 顺序）。
// 注册表里没有 categories 字段时（旧版），返回只有 id 的空描述条目。
func (rg *Registry) CategoryMetas() []CategoryMeta {
	if len(rg.catMeta) > 0 {
		return rg.catMeta
	}
	out := make([]CategoryMeta, 0, len(categoryOrder))
	for _, id := range categoryOrder {
		out = append(out, CategoryMeta{ID: id, Name: id})
	}
	return out
}

// CategoryDescription 取某大类的全文说明；没有返回空串。
func (rg *Registry) CategoryDescription(id string) string {
	for _, c := range rg.catMeta {
		if c.ID == id {
			return c.Description
		}
	}
	return ""
}

// SubcategoriesOf 返回某大类下的子类元数据（注册表里的顺序）。
func (rg *Registry) SubcategoriesOf(category string) []SubcategoryMeta {
	var out []SubcategoryMeta
	for _, s := range rg.subMeta {
		if s.Category == category {
			out = append(out, s)
		}
	}
	return out
}

// SubcategoryDescription 取某子类的全文说明；没有返回空串。
func (rg *Registry) SubcategoryDescription(category, id string) string {
	for _, s := range rg.subMeta {
		if s.Category == category && s.ID == id {
			return s.Description
		}
	}
	return ""
}

// FirstSentence 取一段描述的首句（懒加载上一级只显示首句摘要）：
// 找到第一个句号后空格就断，没有就截到 120 字符。
func FirstSentence(s string) string {
	if i := strings.Index(s, ". "); i >= 0 && i < 200 {
		return s[:i+1]
	}
	r := []rune(s)
	if len(r) > 120 {
		return string(r[:120]) + "…"
	}
	return s
}
