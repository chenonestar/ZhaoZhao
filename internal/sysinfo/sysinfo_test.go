package sysinfo

import (
	"os"
	"runtime"
	"testing"
)

func TestThis(t *testing.T) {
	m := This()
	if m.CPU == "" || m.Cores < 1 || m.Logical < m.Cores {
		t.Fatalf("%+v", m)
	}
	total, avail := Memory()
	if runtime.GOOS == "windows" || runtime.GOOS == "linux" {
		if total == 0 || avail == 0 || avail > total || m.MemTotal != total {
			t.Fatalf("内存 %d / %d", avail, total)
		}
		if PeakMem(os.Getpid()) < 1<<20 {
			t.Fatalf("本进程的内存峰值 %d", PeakMem(os.Getpid()))
		}
	}
	if PeakMem(-1) != 0 {
		t.Fatal("不存在的进程应返回 0")
	}
}
