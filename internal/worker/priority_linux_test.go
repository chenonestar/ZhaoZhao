//go:build linux

package worker

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/chenonestar/zhaozhao/internal/extract"
)

// 解析进程「低于正常」（nice 10），OCR 进程用最低优先级（nice 19）：普通文件先用 CPU，OCR 只用剩下的。
func TestPoolPriority(t *testing.T) {
	t.Setenv("ZZ_BURN_WORKER", "10ms")
	exe, _ := os.Executable()
	for _, lowest := range []bool{false, true} {
		p := NewPool(exe, 1)
		p.Args = []string{"-test.run=^$"}
		p.Lowest = lowest
		p.Run(context.Background(), exe, "txt", extract.DefaultOptions(), Timeouts{Total: time.Minute}, nil)
		p.mu.Lock()
		var pid int
		for pr := range p.live {
			pid = pr.pid
		}
		p.mu.Unlock()
		// Linux 的 getpriority 系统调用返回 20-nice
		v, err := syscall.Getpriority(syscall.PRIO_PROCESS, pid)
		p.Close()
		if err != nil {
			t.Fatal(err)
		}
		want := 10
		if lowest {
			want = 19
		}
		if nice := 20 - v; nice != want {
			t.Fatalf("Lowest=%v：子进程 nice 应为 %d，实际 %d", lowest, want, nice)
		}
	}
}
