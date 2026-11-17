// Package config 是 007 超级 Agent 的环境配置加载（env.loader）。
//
// 设计依据（《超级Agent-全新设计方案》v1.5，第 7 轮）：
//   - 配置项 WORKER_MODEL / ADVISOR_MODEL / ADVISOR_MODE，缺失给默认值并记一条
//     日志，绝不因缺配置拒绝启动。
//   - 配置文件只读路径、不读内容进日志：日志里只许出现键名，绝不出现值
//     （密钥、Token 只认键名，值永远不进日志、不进账本）。
//   - 查找顺序：$SUPER_AGENT_ENV → /etc/super-agent/.env → ./.env。
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// AdvisorMode 的三档（第 7 轮）：STANDARD 审三处、ECONOMY 只审交活、OFF。
const (
	AdvisorStandard = "STANDARD"
	AdvisorEconomy  = "ECONOMY"
	AdvisorOff      = "OFF"
)

// Config 是全部环境配置。Defaults() 给出默认值；Load() 用 .env 覆盖。
type Config struct {
	WorkerModel  string
	AdvisorModel string
	AdvisorMode  string
	DataDir      string
	ListenAddr   string
	// MergeWindow：同 task_id 的信在这个窗口内合并（旧 waker 60 秒）。
	MergeWindow time.Duration
	// ForcedWake：首封等超过此时长强制处理，防饿死（旧 waker 180 秒）。
	ForcedWake time.Duration
	// Debounce：首封防抖（第 14 步 B-01）：无合并追加的孤信等满此时长
	// 即处理，不再干等 60 秒合并窗口；同 task_id 有追加信时仍等合并窗口。
	Debounce time.Duration
	// MaxPending：待处理队列上限，满了返回 503 并写水位告警。
	MaxPending int
	// Shadow：影子模式——收信、分诊照常走，执行只记“影子会跑什么”
	// 进 execution_ledger（状态 shadow），不真跑命令。由 SHADOW_MODE
	//（.env 或环境变量）或 --shadow 旗标打开。
	Shadow bool
	// EnvFile 是实际读到的配置文件路径（没读到为空）。
	EnvFile string
	// Missing 是走默认值补齐的键名（只有键名，没有值）。
	Missing []string
	// Warnings 是非致命提醒（只有键名/路径，没有值），如 .env 权限过松。
	Warnings []string
	// BackupEnabled：记忆自动备份开关（第 15 步），默认关；
	// 只在私密部署上显式打开（BACKUP_ENABLED=1）。
	BackupEnabled bool
	// BackupRepo：期望的私密仓库名 "owner/repo"，默认 DefaultRepo。
	BackupRepo string
	// BackupServerID：本机在 memories/ 下的子目录名；为空时取 hostname。
	BackupServerID string
	// BackupTokenKey：读 GitHub Token 的环境变量键名（只存键名）；
	// 默认 "GITHUB_API_TOKEN"，值在 .env（600 权限）里。
	BackupTokenKey string
	// BackupRepoDir：git 工作区（部署目录）；为空时取 DataDir 的父目录。
	BackupRepoDir string
}

// Defaults 返回带默认值的配置。
func Defaults() *Config {
	return &Config{
		WorkerModel:  "deepseek-flash",
		AdvisorModel: "deepseek-reasoner",
		AdvisorMode:  AdvisorStandard,
		DataDir:      "./data",
		ListenAddr:   "127.0.0.1:6262",
		MergeWindow:  60 * time.Second,
		ForcedWake:   180 * time.Second,
		Debounce:     3 * time.Second,
		MaxPending:   100,
		Shadow:       false,
		BackupEnabled: false,
		BackupRepo:    "ip9988001/super-agent-007",
		BackupTokenKey: "GITHUB_API_TOKEN",
	}
}

// Load 按查找顺序读 .env，用文件里的值覆盖默认值。
// 缺失的键走默认值并记进 Missing（只记键名）；.env 权限不是 0600 时记一条 Warning。
func Load() (*Config, error) {
	cfg := Defaults()
	path := ""
	cands := []string{os.Getenv("SUPER_AGENT_ENV"), "/etc/super-agent/.env", "./.env"}
	for _, cand := range cands {
		if cand == "" {
			continue
		}
		if _, err := os.Stat(cand); err == nil {
			path = cand
			break
		}
	}
	if path == "" {
		cfg.Missing = []string{"WORKER_MODEL", "ADVISOR_MODEL", "ADVISOR_MODE", "DATA_DIR", "LISTEN_ADDR"}
		return cfg, nil
	}
	cfg.EnvFile = path
	if st, err := os.Stat(path); err == nil && st.Mode().Perm()&0077 != 0 {
		cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("env file %s permissions too open (%04o), expected 0600", path, st.Mode().Perm()))
	}
	kv, err := parseEnvFile(path)
	if err != nil {
		return nil, fmt.Errorf("config read %s: %w", path, err)
	}
	set := func(key string, dst *string) {
		if v, ok := kv[key]; ok {
			*dst = v
		} else {
			cfg.Missing = append(cfg.Missing, key)
		}
	}
	set("WORKER_MODEL", &cfg.WorkerModel)
	set("ADVISOR_MODEL", &cfg.AdvisorModel)
	set("ADVISOR_MODE", &cfg.AdvisorMode)
	set("DATA_DIR", &cfg.DataDir)
	set("LISTEN_ADDR", &cfg.ListenAddr)
	if v, ok := kv["MERGE_WINDOW_S"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.MergeWindow = time.Duration(n) * time.Second
		}
	}
	if v, ok := kv["FORCED_WAKE_S"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.ForcedWake = time.Duration(n) * time.Second
		}
	}
	if v, ok := kv["DEBOUNCE_S"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Debounce = time.Duration(n) * time.Second
		}
	}
	if v, ok := kv["MAX_PENDING"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.MaxPending = n
		}
	}
	switch cfg.AdvisorMode {
	case AdvisorStandard, AdvisorEconomy, AdvisorOff:
	default:
		cfg.Missing = append(cfg.Missing, "ADVISOR_MODE(invalid, fallback STANDARD)")
		cfg.AdvisorMode = AdvisorStandard
	}
	// SHADOW_MODE：.env 里的值先行，环境变量再覆盖（部署时不用改文件就能开影子）。
	if v, ok := kv["SHADOW_MODE"]; ok {
		if b, ok := parseBoolFlag(v); ok {
			cfg.Shadow = b
		}
	}
	if v := os.Getenv("SHADOW_MODE"); v != "" {
		if b, ok := parseBoolFlag(v); ok {
			cfg.Shadow = b
		}
	}
	// 记忆自动备份（第 15 步）：只读开关与仓库名、服务器 id、Token 键名、
	// 工作区路径；Token 值本身只在 backup 包里读环境变量，永不进日志。
	if v, ok := kv["BACKUP_ENABLED"]; ok {
		if b, ok := parseBoolFlag(v); ok {
			cfg.BackupEnabled = b
		}
	}
	if v := os.Getenv("BACKUP_ENABLED"); v != "" {
		if b, ok := parseBoolFlag(v); ok {
			cfg.BackupEnabled = b
		}
	}
	if v, ok := kv["BACKUP_REPO"]; ok && strings.TrimSpace(v) != "" {
		cfg.BackupRepo = strings.TrimSpace(v)
	}
	if v, ok := kv["BACKUP_SERVER_ID"]; ok {
		cfg.BackupServerID = strings.TrimSpace(v)
	}
	if v, ok := kv["BACKUP_TOKEN_KEY"]; ok && strings.TrimSpace(v) != "" {
		cfg.BackupTokenKey = strings.TrimSpace(v)
	}
	if v, ok := kv["BACKUP_REPO_DIR"]; ok {
		cfg.BackupRepoDir = strings.TrimSpace(v)
	}
	return cfg, nil
}

// parseBoolFlag 认 1/true/yes/on 为真，0/false/no/off 为假，其余忽略（走默认值）。
func parseBoolFlag(v string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	}
	return false, false
}

// parseEnvFile 读 KEY=VALUE 文件，跳过空行与 # 注释，值两侧引号剥掉。
// 只返回键值映射，调用方只许把键名写进日志。
func parseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && v[0] == v[len(v)-1] && (v[0] == '"' || v[0] == '\'') {
			v = v[1 : len(v)-1]
		}
		if k != "" {
			out[k] = v
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ResolveDataDir 返回绝对化后的数据目录（不存在不创建，由调用方建）。
func (c *Config) ResolveDataDir() string {
	if filepath.IsAbs(c.DataDir) {
		return c.DataDir
	}
	abs, err := filepath.Abs(c.DataDir)
	if err != nil {
		return c.DataDir
	}
	return abs
}
