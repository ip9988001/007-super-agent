package memlock

import (
	"errors"
	"testing"
)

// TestTryLockExclusive：同一目录的锁是排他的，释放后能再抢；
// nil 锁 Unlock 不 panic。
func TestTryLockExclusive(t *testing.T) {
	dir := t.TempDir()
	l1, err := TryLock(dir)
	if err != nil {
		t.Fatalf("第一次 TryLock: %v", err)
	}
	if _, err := TryLock(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("锁被占用时应返回 ErrLocked，得到 %v", err)
	}
	l1.Unlock()
	l2, err := TryLock(dir)
	if err != nil {
		t.Fatalf("释放后 TryLock 应成功：%v", err)
	}
	l2.Unlock()
	var nilLock *Lock
	nilLock.Unlock()
}
