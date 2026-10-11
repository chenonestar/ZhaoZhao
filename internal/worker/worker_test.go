package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenonestar/zhaozhao/internal/doc"
	"github.com/chenonestar/zhaozhao/internal/extract"
)

// 测试二进制自己充当解析子进程，走和生产环境完全相同的管道协议。
func TestMain(m *testing.M) {
	if os.Getenv("ZZ_AS_WORKER") == "1" {
		Serve()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func realPool(t *testing.T, n int) *Pool {
	t.Helper()
	t.Setenv("ZZ_AS_WORKER", "1")
	exe, _ := os.Executable()
	p := NewPool(exe, n)
	p.Args = []string{"-test.run=^$"}
	t.Cleanup(p.Close)
	return p
}

func TestWorkerProtocol(t *testing.T) {
	p := realPool(t, 2)
	d := t.TempDir()
	f := filepath.Join(d, "a.txt")
	os.WriteFile(f, []byte("子进程提取的内容 ptarmigan"), 0o644)
	for i := 0; i < 3; i++ { // 同一个子进程处理多个请求
		r := p.Extract(context.Background(), f, "txt", extract.DefaultOptions(), 10*time.Second)
		if r.Status != doc.StatusOK || !strings.Contains(r.Text, "ptarmigan") {
			t.Fatalf("%+v", r)
		}
	}
	// 并发
	errs := make(chan string, 8)
	for i := 0; i < 8; i++ {
		go func() {
			r := p.Extract(context.Background(), f, "txt", extract.DefaultOptions(), 10*time.Second)
			if r.Status != doc.StatusOK {
				errs <- r.Err
				return
			}
			errs <- ""
		}()
	}
	for i := 0; i < 8; i++ {
		if e := <-errs; e != "" {
			t.Fatal(e)
		}
	}
	// 解析器内部的 panic 被子进程捕获，记为失败而不是拖垮子进程
	bad := filepath.Join(d, "bad.docx")
	os.WriteFile(bad, []byte("not a zip"), 0o644)
	if r := p.Extract(context.Background(), bad, "docx", extract.DefaultOptions(), 10*time.Second); r.Status != doc.StatusFailed {
		t.Fatalf("%+v", r)
	}
	if r := p.Extract(context.Background(), f, "txt", extract.DefaultOptions(), 10*time.Second); r.Status != doc.StatusOK {
		t.Fatal("失败之后子进程应仍然可用")
	}
}

func TestCrashAndHangAreIsolated(t *testing.T) {
	d := t.TempDir()
	f := filepath.Join(d, "a.txt")
	os.WriteFile(f, []byte("恢复之后 pika"), 0o644)
	p := NewPool("/bin/sh", 1)
	t.Cleanup(p.Close)

	p.Args = []string{"-c", "exit 3"} // 子进程一启动就崩溃
	r := p.Extract(context.Background(), f, "txt", extract.DefaultOptions(), 5*time.Second)
	if r.Status != doc.StatusFailed || !strings.Contains(r.Err, "崩溃") {
		t.Fatalf("崩溃应记为失败: %+v", r)
	}

	p.Args = []string{"-c", "sleep 30"} // 子进程卡死
	start := time.Now()
	r = p.Extract(context.Background(), f, "txt", extract.DefaultOptions(), 600*time.Millisecond)
	if r.Status != doc.StatusFailed || !strings.Contains(r.Err, "超时") || time.Since(start) > 3*time.Second {
		t.Fatalf("卡死应在超时后记为失败: %+v (%v)", r, time.Since(start))
	}

	// 恢复：同一个池换成正常的子进程后继续工作
	t.Setenv("ZZ_AS_WORKER", "1")
	exe, _ := os.Executable()
	p.Exe, p.Args = exe, []string{"-test.run=^$"}
	r = p.Extract(context.Background(), f, "txt", extract.DefaultOptions(), 10*time.Second)
	if r.Status != doc.StatusOK || !strings.Contains(r.Text, "pika") {
		t.Fatalf("重启后应恢复: %+v", r)
	}

	// 取消：不会一直阻塞
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r := p.Extract(ctx, f, "txt", extract.DefaultOptions(), 5*time.Second); r.Status != doc.StatusFailed {
		t.Fatalf("%+v", r)
	}
}
