package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWarnL1Targets：过期 L1 目标告警，当前目标不告警，缺失目标告警。
func TestWarnL1Targets(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "l1"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 过期目标：文件名日期 2026-11-01，mtime 也拨到 10 天前。
	oldPath := filepath.Join(dir, "l1", "2026-11-01.log")
	if err := os.WriteFile(oldPath, []byte("ctxread 旧记录\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-10 * 24 * time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	// 当前目标：今天日期，mtime 新鲜。
	today := time.Now().Format("2006-01-02")
	if err := os.WriteFile(filepath.Join(dir, "l1", today+".log"), []byte("ctxread 新记录\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	es := evalSet{
		Positive: []evalItem{
			{ID: "X-old", Target: "l1/2026-11-01.log"},
			{ID: "X-now", Target: "l1/" + today + ".log"},
			{ID: "X-missing", Target: "l1/2026-11-02.log"},
			{ID: "X-wiki", Target: "wiki/entities/x"},
		},
	}
	warns := warnL1Targets(dir, es, nil)
	joined := strings.Join(warns, "\n")
	if !strings.Contains(joined, "2026-11-01.log") {
		t.Fatalf("过期目标应告警，得到 %v", warns)
	}
	if !strings.Contains(joined, "2026-11-02.log") {
		t.Fatalf("缺失目标应告警，得到 %v", warns)
	}
	if strings.Contains(joined, today+".log") {
		t.Fatalf("当前目标不应告警，得到 %v", warns)
	}
	if strings.Contains(joined, "wiki/entities/x") {
		t.Fatalf("非 l1 目标不应告警，得到 %v", warns)
	}
	if len(warns) < 3 {
		t.Fatalf("至少 3 条告警（过期日期、过期 mtime、缺失），得到 %d 条：%v", len(warns), warns)
	}
}

// TestStageFixtures：目标缺失时从夹具补入，现盘文件在时不覆盖，夹具缺失报错。
func TestStageFixtures(t *testing.T) {
	memDir := t.TempDir()
	fixDir := t.TempDir()
	content := "2026-11-24T10:00:00+08:00 task verify-20261124-ctxread-001 route ERROR front_error\n"
	if err := os.WriteFile(filepath.Join(fixDir, "l1-2026-11-24.log"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	es := evalSet{Positive: []evalItem{
		{ID: "X05", Target: "l1/2026-11-24.log", Fixture: "l1-2026-11-24.log"},
		{ID: "X06", Target: "l1/2026-11-24.log", Fixture: "l1-2026-11-24.log"},
	}}
	staged, err := stageFixtures(memDir, fixDir, es)
	if err != nil {
		t.Fatalf("补夹具不应报错：%v", err)
	}
	if len(staged) != 1 || staged[0] != "l1/2026-11-24.log" {
		t.Fatalf("X05/X06 同一目标只应补一次，得到 %v", staged)
	}
	got, err := os.ReadFile(filepath.Join(memDir, "l1", "2026-11-24.log"))
	if err != nil {
		t.Fatalf("补入后目标应存在：%v", err)
	}
	if string(got) != content {
		t.Fatalf("补入内容应与夹具一致，得到 %q", got)
	}
	// 再跑一次：已存在，不重复补、不覆盖。
	if _, err := stageFixtures(memDir, fixDir, es); err != nil {
		t.Fatalf("二次补夹具不应报错：%v", err)
	}
	// 现盘文件在时不覆盖：先写不同内容，再跑一次。
	if err := os.WriteFile(filepath.Join(memDir, "l1", "2026-11-24.log"), []byte("live 内容\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	staged, err = stageFixtures(memDir, fixDir, es)
	if err != nil {
		t.Fatalf("现盘在时补夹具不应报错：%v", err)
	}
	if len(staged) != 0 {
		t.Fatalf("现盘在时不应再补，得到 %v", staged)
	}
	got, _ = os.ReadFile(filepath.Join(memDir, "l1", "2026-11-24.log"))
	if string(got) != "live 内容\n" {
		t.Fatalf("现盘内容不应被夹具覆盖，得到 %q", got)
	}
	// 夹具文件缺失时报错。
	if _, err := stageFixtures(t.TempDir(), fixDir, evalSet{Positive: []evalItem{
		{ID: "X-missing", Target: "l1/2026-11-25.log", Fixture: "不存在.log"},
	}}); err == nil {
		t.Fatal("夹具缺失应报错")
	}
}

// TestWarnL1TargetsStaged：已由夹具固化的目标不再告警过期。
func TestWarnL1TargetsStaged(t *testing.T) {
	dir := t.TempDir()
	es := evalSet{Positive: []evalItem{
		{ID: "X05", Target: "l1/2026-11-24.log"},
	}}
	warns := warnL1Targets(dir, es, nil)
	if len(warns) == 0 {
		t.Fatal("未固化且缺失的目标应告警")
	}
	warns = warnL1Targets(dir, es, map[string]bool{"l1/2026-11-24.log": true})
	if len(warns) != 0 {
		t.Fatalf("已固化的目标不应告警，得到 %v", warns)
	}
}
