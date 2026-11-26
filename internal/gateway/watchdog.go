// Package gateway 的外部看门狗（线 A）：独立于网关进程的超时兜底。
//
// 进程内的收尾兜底防不住进程崩溃、被系统杀掉或断网：网关进程死了，
// 它自己的水位扫荡（sweepStale）也不会跑，RUNNING 行就永远卡住，
// 用户永远等不到回信。外部看门狗是一个独立的一次性扫荡
// （cmd/gateway-watchdog，供 systemd 定时器每 5 分钟跑一次），
// 直接读 ledger.db：把超 deadline 未终结的 RUNNING 行置为 TIMEOUT，
// 并补发失败回信；若产物其实已落袋（reply.md 新鲜），则标 DONE。
package gateway

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"007-super-agent/internal/ledger"
	"007-super-agent/internal/sanitize"
)

// WatchdogSweep 是网关侧外部看门狗的一次扫荡：
//   - 列出超期未终结的 RUNNING 行（deadline 已过；无 deadline 的旧行
//     按 updated_ts + WatchdogDeadline 兜底）；
//   - 每行先验产物：artifacts/<taskID>/reply.md 存在、去空白后非空、
//     且 mtime 不早于该行入队时间（允许 2 秒容差，防文件系统精度与
//     时钟微调误判），说明任务其实做完了，只是网关没来得及确认 → 标 DONE；
//   - 否则写失败回信（reply.md 缺失或过期时才写，不覆盖新鲜产物），
//     行标 TIMEOUT，并在 dispatch_events 记一行 watchdog_timeout。
//
// 返回（置为 TIMEOUT 的行数，产物验收后标 DONE 的行数，错误）。
// dataDir 是数据目录（reply.md 落盘处）。本函数不依赖网关进程还活着，
// 只依赖 ledger.db 与产物目录；多实例并发跑时靠“只改 RUNNING 行”
// 的行级守卫保证同一行只被置位一次。
func WatchdogSweep(led *ledger.Ledger, dataDir string, now time.Time) (timedOut, recovered int, err error) {
	if led == nil {
		return 0, 0, fmt.Errorf("watchdog sweep: 账本为空")
	}
	rows, err := led.ListOverdueInbox(now, WatchdogDeadline)
	if err != nil {
		return 0, 0, fmt.Errorf("watchdog sweep: 列超期行失败: %w", err)
	}
	for _, r := range rows {
		if freshReplyExists(dataDir, r) {
			if merr := led.MarkInboxDone(r.ID); merr != nil {
				log.Printf("watchdog task=%s: 产物已落袋但标 DONE 失败: %v", r.TaskID, merr)
				continue
			}
			log.Printf("watchdog task=%s: 产物已落袋，标 DONE（网关漏确认）", r.TaskID)
			recovered++
			continue
		}
		if werr := writeTimeoutReply(dataDir, r, now); werr != nil {
			log.Printf("watchdog task=%s: 写超时回信失败: %v", r.TaskID, werr)
		}
		if merr := led.MarkInboxTimeout(r.ID); merr != nil {
			log.Printf("watchdog task=%s: 标 TIMEOUT 失败: %v", r.TaskID, merr)
			continue
		}
		if _, rerr := led.RecordDispatchEvent(ledger.DispatchEvent{
			TaskID: r.TaskID, Kind: ledger.DispatchEventWatchdogTimeout,
			Detail: fmt.Sprintf("任务超期未终结（deadline %s），看门狗置为 TIMEOUT 并补发失败回信", r.Deadline),
		}); rerr != nil {
			log.Printf("watchdog task=%s: 记超时事件失败: %v", r.TaskID, rerr)
		}
		log.Printf("watchdog task=%s: 超期未终结，标 TIMEOUT 并补发失败回信", r.TaskID)
		timedOut++
	}
	return timedOut, recovered, nil
}

// freshReplyExists 判断某任务的产物是否已落袋：reply.md 存在、
// 去空白后非空、且修改时间不早于该行入队时间（允许 2 秒容差，
// 防文件系统时间精度与时钟微调造成的误判）。入队时间解析失败时
// 按不新鲜处理（宁可误判超时，不可把旧产物当成新任务的）。
// 只含空白字符的文件不算产物（P0 修复：TrimSpace 验收）。
func freshReplyExists(dataDir string, r ledger.InboxRow) bool {
	p := filepath.Join(dataDir, "artifacts", r.TaskID, "reply.md")
	b, err := os.ReadFile(p)
	if err != nil || len(strings.TrimSpace(string(b))) == 0 {
		return false
	}
	fi, err := os.Stat(p)
	if err != nil {
		return false
	}
	enqueued, err := time.Parse(time.RFC3339, r.TS)
	if err != nil {
		return false
	}
	// mtime 允许 2 秒容差：文件系统时间精度与时钟微调可能让新鲜
	// 产物的 mtime 比入队时间戳早 1~2 秒。
	return !fi.ModTime().Before(enqueued.Add(-2 * time.Second))
}

// writeFailureReply 原子排他写一封失败回信（P0 修复）：
//   - 先验产物：已有新鲜产物时不动；
//   - 已有过期旧文件时先删掉（它不是本次任务的真实回信），再排他创建；
//   - 用 O_CREATE|O_EXCL 打开：检查与创建之间的竞态里若真实回信刚落盘，
//     创建失败即放弃，绝不覆盖。
//
// dataDir 为空时不写。回信只写人话原因，不写密钥、不贴日志。
func writeFailureReply(dataDir string, r ledger.InboxRow, reason string) error {
	if strings.TrimSpace(dataDir) == "" {
		return nil
	}
	if freshReplyExists(dataDir, r) {
		return nil // 新鲜产物，不覆盖
	}
	artDir := filepath.Join(dataDir, "artifacts", r.TaskID)
	if err := os.MkdirAll(artDir, 0o700); err != nil {
		return fmt.Errorf("建产物目录失败: %w", err)
	}
	p := filepath.Join(artDir, "reply.md")
	if _, err := os.Stat(p); err == nil {
		// 旧文件已确认不新鲜（上一步已验），删掉再排他创建。
		if err := os.Remove(p); err != nil {
			return fmt.Errorf("清过期旧回信失败: %w", err)
		}
	}
	text := fmt.Sprintf("任务未能完成。\n\n任务：%s\n原因：%s\n时间：%s\n",
		r.TaskID, reason, time.Now().UTC().Format(time.RFC3339))
	// 出站硬扫描（线 B 修复轮第 3 棒）：失败回信也是 reply.md，
	// 与其他落盘点走同一道门。
	text, n, blocked := sanitize.ScrubOutbound(text)
	if n > 0 || blocked {
		log.Printf("watchdog task=%s 出站脱敏：掩码 %d 处，整封拦截=%v", r.TaskID, n, blocked)
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			// 检查与创建之间真实回信刚落盘：放弃，绝不覆盖。
			return nil
		}
		return fmt.Errorf("排他写失败回信失败: %w", err)
	}
	if _, err := f.Write([]byte(text)); err != nil {
		f.Close()
		os.Remove(p)
		return fmt.Errorf("写失败回信失败: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(p)
		return fmt.Errorf("关失败回信失败: %w", err)
	}
	return nil
}

// writeTimeoutReply 写超时失败回信（reply.md 缺失或过期时才写，
// 不覆盖新鲜产物）。回信只写人话原因，不写密钥、不贴日志。
func writeTimeoutReply(dataDir string, r ledger.InboxRow, now time.Time) error {
	deadline := r.Deadline
	if strings.TrimSpace(deadline) == "" {
		deadline = "未设（旧行，按取走后 1800 秒兜底）"
	}
	reason := fmt.Sprintf("超过终结期限（deadline %s）仍未收到终态回执，网关看门狗已将任务置为 TIMEOUT。\n执行进程可能已崩溃、被系统杀掉或断网；如需重试，请重新派单。", deadline)
	return writeFailureReply(dataDir, r, reason)
}
