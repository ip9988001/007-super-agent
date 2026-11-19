package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeEnv(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		t.Fatalf("write env: %v", err)
	}
	return p
}

func TestLoadOverridesAndDefaults(t *testing.T) {
	p := writeEnv(t, "# 注释行\nWORKER_MODEL=deepseek-chat\nADVISOR_MODE=ECONOMY\nSECRET_KEY=do-not-log-this-value\n")
	t.Setenv("SUPER_AGENT_ENV", p)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WorkerModel != "deepseek-chat" {
		t.Fatalf("WorkerModel = %q", cfg.WorkerModel)
	}
	if cfg.AdvisorMode != AdvisorEconomy {
		t.Fatalf("AdvisorMode = %q", cfg.AdvisorMode)
	}
	// 缺失的键走默认值，且 Missing 里只有键名。
	if cfg.AdvisorModel != "deepseek-reasoner" {
		t.Fatalf("AdvisorModel default = %q", cfg.AdvisorModel)
	}
	for _, m := range cfg.Missing {
		if strings.Contains(m, "do-not-log-this-value") {
			t.Fatalf("secret value leaked into Missing: %q", m)
		}
	}
	found := false
	for _, m := range cfg.Missing {
		if m == "ADVISOR_MODEL" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Missing should contain ADVISOR_MODEL, got %v", cfg.Missing)
	}
}

func TestLoadNoFileAllDefaults(t *testing.T) {
	t.Setenv("SUPER_AGENT_ENV", filepath.Join(t.TempDir(), "does-not-exist.env"))
	// /etc/super-agent/.env 与 ./.env 都不存在时（测试工作目录无 .env），全走默认值。
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WorkerModel != "deepseek-flash" || cfg.AdvisorMode != AdvisorStandard {
		t.Fatalf("defaults wrong: %+v", cfg)
	}
	if len(cfg.Missing) == 0 {
		t.Fatalf("Missing should list defaulted keys")
	}
}

func TestLoadInvalidAdvisorModeFallsBack(t *testing.T) {
	p := writeEnv(t, "ADVISOR_MODE=BOGUS\n")
	t.Setenv("SUPER_AGENT_ENV", p)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AdvisorMode != AdvisorStandard {
		t.Fatalf("AdvisorMode = %q, want STANDARD fallback", cfg.AdvisorMode)
	}
}

func TestLoadWarnsOnLoosePerms(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(p, []byte("WORKER_MODEL=x\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("SUPER_AGENT_ENV", p)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Warnings) == 0 {
		t.Fatalf("expected permission warning, got none")
	}
}

func TestShadowMode(t *testing.T) {
	p := writeEnv(t, "SHADOW_MODE=true\n")
	t.Setenv("SUPER_AGENT_ENV", p)
	t.Setenv("SHADOW_MODE", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Shadow {
		t.Fatalf("Shadow = false, want true from file")
	}
	// 环境变量覆盖文件里的值。
	t.Setenv("SHADOW_MODE", "false")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Shadow {
		t.Fatalf("Shadow = true, want false (env var overrides file)")
	}
}

func TestForgeDailyBudgetMilli(t *testing.T) {
	p := writeEnv(t, "FORGE_DAILY_BUDGET_MILLI=3000\n")
	t.Setenv("SUPER_AGENT_ENV", p)
	t.Setenv("FORGE_DAILY_BUDGET_MILLI", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ForgeDailyBudgetMilli != 3000 {
		t.Fatalf("ForgeDailyBudgetMilli = %d, want 3000 from file", cfg.ForgeDailyBudgetMilli)
	}
	// 环境变量覆盖文件里的值。
	t.Setenv("FORGE_DAILY_BUDGET_MILLI", "800")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ForgeDailyBudgetMilli != 800 {
		t.Fatalf("ForgeDailyBudgetMilli = %d, want 800 (env var overrides file)", cfg.ForgeDailyBudgetMilli)
	}
	// 非法值不生效，走默认值 500。
	p = writeEnv(t, "FORGE_DAILY_BUDGET_MILLI=abc\n")
	t.Setenv("SUPER_AGENT_ENV", p)
	t.Setenv("FORGE_DAILY_BUDGET_MILLI", "")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ForgeDailyBudgetMilli != 500 {
		t.Fatalf("ForgeDailyBudgetMilli = %d, want 500 default on bad value", cfg.ForgeDailyBudgetMilli)
	}
}
