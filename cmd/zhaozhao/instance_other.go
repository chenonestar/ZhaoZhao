//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
)

var instanceLock *os.File

// acquireInstanceLock 用文件锁保证同一个数据目录只运行一个找找（进程结束时系统自动释放）。
func acquireInstanceLock(dataDir string) bool {
	f, err := os.OpenFile(filepath.Join(dataDir, "zhaozhao.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return true
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return false
	}
	instanceLock = f
	return true
}
