// Package memlock 是精炼（04:00）与自进化（04:30）共用的目录级文件锁
// （Gemini 审阅线 B 第二阶段第五节隐蔽冲突三）：两者只隔 30 分钟，
// 精炼一旦超时会与自进化并发改记忆目录。选文件锁而不是只记耗时告警，
// 因为告警只能事后发现撞车，锁能事先拦住撞车。
//
// flock 是内核级排他锁：同一进程内两个 goroutine 各自 open 同一文件
// 拿到的也是不同文件描述符，照样互斥；进程崩溃/退出时内核自动释放，
// 不留 stale 锁文件，比 O_EXCL 占位文件安全。拿不到锁的一方直接跳过
// 本次窗口（60 秒 tick + 日期标记，下个 tick 再试），不排队、不阻塞。
// 锁文件放在数据目录下，与 refine_last 等标记文件同目录。
package memlock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrLocked 表示记忆写锁正被另一方持有——不是错误，是“本次跳过”。
var ErrLocked = errors.New("memlock: 记忆写锁正被占用")

// LockFile 是锁文件路径。
func LockFile(dataDir string) string {
	return filepath.Join(dataDir, "memory-write.lock")
}

// Lock 是一把已持有的记忆写锁；用完必须 Unlock。
type Lock struct {
	f *os.File
}

// TryLock 非阻塞地抢记忆写锁。抢到返回锁；已被占用返回 ErrLocked；
// 其他失败（建目录/建文件/flock 异常）返回包装后的错误。
func TryLock(dataDir string) (*Lock, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("memlock mkdir %s: %w", dataDir, err)
	}
	f, err := os.OpenFile(LockFile(dataDir), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("memlock open %s: %w", LockFile(dataDir), err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("memlock flock %s: %w", LockFile(dataDir), err)
	}
	return &Lock{f: f}, nil
}

// Unlock 释放写锁（先解 flock 再关文件；关文件本身也会释放）。
func (l *Lock) Unlock() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
}
