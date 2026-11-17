// 每日精炼：把前一天的 L1 日志调模型提炼成 Wiki 条目。
//
// 对应旧系统的 l1_watcher.py 精炼链路（每天 04:00 调模型把 L1 提炼成
// wiki），但修掉旧系统两个毛病：
//   - L1 写入不再靠模型自觉：每条信由框架在 handler 里逐条记 L1
//     （见 cmd/super-agent 的 handle），漏记的事实事后补不回来；
//   - 精炼只走 memory.WriteWiki 单一入口，非法词条名拒绝并记日志，
//     不直接落盘。
//
// 铁律：模型不可用（refine 为 nil）时跳过并记一条日志，不报错、
// 不重试、不阻塞主程序；写入失败返回错误，不许静默吞掉。
package watchdog

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"007-super-agent/internal/ledger"
	"007-super-agent/internal/memory"
)

// RefineFunc 把一天的 L1 日志文本提炼成 Wiki 条目；调用方接模型客户端
// （llm.Client.RefineL1）。测试里用桩，不真调 API。
// taskID 是精炼任务号（refine-YYYY-MM-DD），供模型费用按任务归属记账。
type RefineFunc func(ctx context.Context, taskID, l1Text string) ([]memory.WikiEntry, error)

// RefineResult 是一次精炼的结果。
type RefineResult struct {
	Date    string // 精炼的 L1 日期（YYYY-MM-DD）
	Lines   int    // 读到的 L1 行数
	Written int    // 成功写入 Wiki 的条目数
	Failed  int    // 写入失败的条目数（非法词条名等，原因已记日志）
	Skipped bool   // 无模型、无日志时跳过（不是失败）
}

// refineTaskID 是精炼在 execution_ledger 里的任务号。
func refineTaskID(date string) string { return "refine-" + date }

// RefineDaily 精炼指定日期的 L1 日志：读日志 → 调 refine 提炼 →
// 每条经 memory.WriteWiki 单一入口写入。结果写进 execution_ledger
// （ToolID refine-daily）。refine 为 nil 或当天无 L1 日志时跳过，
// 不报错、不记账。模型调用失败返回错误，并记一行 failure 回执。
func (w *Watchdog) RefineDaily(ctx context.Context, mem *memory.Store, date string, refine RefineFunc) (RefineResult, error) {
	res := RefineResult{Date: date}
	if mem == nil {
		return res, fmt.Errorf("watchdog refine: 记忆 Store 为空")
	}
	if refine == nil {
		res.Skipped = true
		log.Printf("refine %s: 无模型调用，跳过", date)
		return res, nil
	}
	text, err := mem.ReadL1(date)
	if err != nil {
		if os.IsNotExist(err) || strings.Contains(err.Error(), "no such file") {
			res.Skipped = true
			log.Printf("refine %s: 当天无 L1 日志，跳过", date)
			return res, nil
		}
		return res, err
	}
	res.Lines = strings.Count(text, "\n")
	entries, err := refine(ctx, refineTaskID(date), text)
	if err != nil {
		if w.led != nil {
			_, _ = w.led.RecordExecution(ledger.ExecutionRecord{
				TaskID: refineTaskID(date), ToolID: "refine-daily",
				Command: "refine " + date, ExitCode: 1, Status: "failure",
				ErrHead: err.Error(), StdoutTail: fmt.Sprintf("lines=%d", res.Lines),
			})
		}
		return res, fmt.Errorf("watchdog refine %s: %w", date, err)
	}
	for _, e := range entries {
		if err := mem.WriteWiki(e); err != nil {
			res.Failed++
			log.Printf("refine %s: 词条 %q 写入失败: %v", date, e.Title, err)
			continue
		}
		res.Written++
	}
	if w.led != nil {
		_, lerr := w.led.RecordExecution(ledger.ExecutionRecord{
			TaskID: refineTaskID(date), ToolID: "refine-daily",
			Command: "refine " + date, ExitCode: 0, Status: "success",
			StdoutTail: fmt.Sprintf("lines=%d written=%d failed=%d", res.Lines, res.Written, res.Failed),
		})
		if lerr != nil {
			return res, fmt.Errorf("watchdog refine %s: 记账失败: %w", date, lerr)
		}
	}
	log.Printf("refine %s: lines=%d written=%d failed=%d", date, res.Lines, res.Written, res.Failed)
	return res, nil
}

// refineMarkerFile 是“当天已跑过精炼”的标记文件（放数据目录，内容为日期）。
func refineMarkerFile(dataDir string) string {
	return filepath.Join(dataDir, "refine_last")
}

// beijingNow 取北京时间；系统无 Asia/Shanghai 时区数据时用固定 +8 区兜底。
func beijingNow() time.Time {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		loc = time.FixedZone("CST", 8*3600)
	}
	return time.Now().In(loc)
}

// shouldRefine 判断现在是否该跑精炼：北京时间 04:00–04:15，
// 且标记文件里的日期不是今天（当天未跑过）。
func shouldRefine(now time.Time, marker string) bool {
	if now.Hour() != 4 || now.Minute() >= 15 {
		return false
	}
	return marker != now.Format("2006-01-02")
}

// RefineLoop 常驻轻量循环：每 60 秒检查一次，北京时间 04:00–04:15 且
// 当天未跑过（标记文件）才跑一次 RefineDaily（精炼前一天的 L1）。
// 跑完才写当天标记；ctx 取消时退出。模型不可用时每天只记一条日志。
func (w *Watchdog) RefineLoop(ctx context.Context, mem *memory.Store, dataDir string, refine RefineFunc) {
	tick := time.NewTicker(60 * time.Second)
	defer tick.Stop()
	loggedNoModel := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if refine == nil {
			if !loggedNoModel {
				log.Printf("refine loop: 无模型调用，每日精炼跳过")
				loggedNoModel = true
			}
			continue
		}
		now := beijingNow()
		marker := ""
		if b, err := os.ReadFile(refineMarkerFile(dataDir)); err == nil {
			marker = strings.TrimSpace(string(b))
		}
		if !shouldRefine(now, marker) {
			continue
		}
		yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")
		if _, err := w.RefineDaily(ctx, mem, yesterday, refine); err != nil {
			log.Printf("refine loop: %v", err)
			continue
		}
		if err := os.WriteFile(refineMarkerFile(dataDir), []byte(now.Format("2006-01-02")+"\n"), 0o600); err != nil {
			log.Printf("refine loop: 写标记文件失败: %v（下个 60 秒会重跑一次）", err)
		}
		// 第 15 步：精炼成功后调记忆备份钩子（失败只记日志，不阻塞循环）。
		w.afterRefine(ctx, yesterday)
	}
}
