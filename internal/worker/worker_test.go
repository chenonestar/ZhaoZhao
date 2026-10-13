package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
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
	if v := os.Getenv("ZZ_PROGRESS_WORKER"); v != "" {
		progressWorker(v)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// progressWorker 模拟一个慢任务：每隔 gap 报告一次进度，共 n 次，然后返回结果。
// 格式 "n,gap"，例如 "5,100ms"。还会往标准错误写一行，检验日志转存。
func progressWorker(spec string) {
	parts := strings.Split(spec, ",")
	n, _ := strconv.Atoi(parts[0])
	gap, _ := time.ParseDuration(parts[1])
	fmt.Fprintln(os.Stderr, "子进程日志 hello-from-worker")
	sc := bufio.NewScanner(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	for sc.Scan() {
		var req Request
		json.Unmarshal(sc.Bytes(), &req)
		for i := 1; i <= n; i++ {
			time.Sleep(gap)
			enc.Encode(Response{ID: req.ID, Progress: fmt.Sprintf("第 %d/%d 页", i, n)})
		}
		enc.Encode(Response{ID: req.ID, Result: doc.Result{Status: doc.StatusOK, Text: "done"}})
	}
}

func fakePool(t *testing.T, n int, spec string) *Pool {
	t.Helper()
	t.Setenv("ZZ_PROGRESS_WORKER", spec)
	exe, _ := os.Executable()
	p := NewPool(exe, n)
	p.Args = []string{"-test.run=^$"}
	t.Cleanup(p.Close)
	return p
}

// 有进度时按「没有进展」计时：总时长超过 Idle 也不算超时；停止报告进度才超时。
func TestProgressExtendsTimeout(t *testing.T) {
	var logBuf strings.Builder
	var mu sync.Mutex
	log.SetOutput(writerFunc(func(b []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return logBuf.Write(b) }))
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	p := fakePool(t, 1, "8,100ms") // 共 0.8 秒，每 0.1 秒一次进度
	var got []string
	r := p.Run(context.Background(), "x", "pdf", extract.DefaultOptions(), Timeouts{Idle: 400 * time.Millisecond, Total: 10 * time.Second},
		func(s string) { got = append(got, s) })
	if r.Status != doc.StatusOK || len(got) != 8 || got[7] != "第 8/8 页" {
		t.Fatalf("%+v %v", r, got)
	}
	mu.Lock()
	ls := logBuf.String()
	mu.Unlock()
	if !strings.Contains(ls, "hello-from-worker") || !strings.Contains(ls, "[解析进程 ") {
		t.Fatalf("子进程的标准错误应转存到日志: %q", ls)
	}

	p2 := fakePool(t, 1, "2,1s") // 每 1 秒才有一次进度，超过 Idle
	r = p2.Run(context.Background(), "x", "pdf", extract.DefaultOptions(), Timeouts{Idle: 300 * time.Millisecond, Total: 10 * time.Second}, nil)
	if r.Status != doc.StatusFailed || !strings.Contains(r.Err, "没有任何进展") {
		t.Fatalf("%+v", r)
	}
	p3 := fakePool(t, 1, "20,100ms") // 一直有进度，但超过总时限
	r = p3.Run(context.Background(), "x", "pdf", extract.DefaultOptions(), Timeouts{Idle: 500 * time.Millisecond, Total: 700 * time.Millisecond}, nil)
	if r.Status != doc.StatusFailed || !strings.Contains(r.Err, "超过") {
		t.Fatalf("%+v", r)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

// 进程池：优先复用最近用过的子进程；数量不超过上限；闲置太久的子进程自动结束；上限可调。
func TestPoolReuseIdleAndResize(t *testing.T) {
	p := realPool(t, 4)
	d := t.TempDir()
	f := filepath.Join(d, "a.txt")
	os.WriteFile(f, []byte("复用 kakapo"), 0o644)
	for i := 0; i < 10; i++ { // 串行：始终复用同一个子进程（旧版会轮流启动全部 4 个）
		if r := p.Extract(context.Background(), f, "txt", extract.DefaultOptions(), 10*time.Second); r.Status != doc.StatusOK {
			t.Fatalf("%+v", r)
		}
	}
	if _, live, _ := p.Stats(); live != 1 {
		t.Fatalf("串行处理只需要 1 个子进程，实际 %d 个", live)
	}
	// 并发 8 个、上限 4：最多 4 个子进程
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.Extract(context.Background(), f, "txt", extract.DefaultOptions(), 10*time.Second)
		}()
	}
	wg.Wait()
	if _, live, _ := p.Stats(); live > 4 {
		t.Fatalf("子进程数超过上限: %d", live)
	}
	p.SetSize(1)
	if _, live, size := p.Stats(); live > 1 || size != 1 {
		t.Fatalf("缩小上限后应结束多余的空闲子进程: live=%d size=%d", live, size)
	}
	// 闲置回收
	p.mu.Lock()
	p.IdleTimeout = time.Millisecond
	for _, pr := range p.idle {
		pr.lastUsed = time.Now().Add(-time.Hour)
	}
	p.mu.Unlock()
	p.reapIdle()
	if _, live, _ := p.Stats(); live != 0 {
		t.Fatalf("闲置子进程应被结束: %d", live)
	}
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
	shell, crashArgs, hangArgs := "/bin/sh", []string{"-c", "exit 3"}, []string{"-c", "sleep 30"}
	if runtime.GOOS == "windows" {
		shell, crashArgs, hangArgs = "cmd.exe", []string{"/c", "exit 3"}, []string{"/c", "ping -n 30 127.0.0.1 >nul"}
	}
	p := NewPool(shell, 1)
	t.Cleanup(p.Close)

	p.Args = crashArgs // 子进程一启动就崩溃
	r := p.Extract(context.Background(), f, "txt", extract.DefaultOptions(), 5*time.Second)
	if r.Status != doc.StatusFailed || !strings.Contains(r.Err, "崩溃") {
		t.Fatalf("崩溃应记为失败: %+v", r)
	}

	p.Args = hangArgs // 子进程卡死
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
