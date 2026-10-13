package logx

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type slowWriter struct {
	mu    sync.Mutex
	b     bytes.Buffer
	delay time.Duration
	block chan struct{}
}

func (s *slowWriter) Write(p []byte) (int, error) {
	if s.block != nil {
		<-s.block
	}
	time.Sleep(s.delay)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *slowWriter) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestAsyncKeepsOrderAndFlushes(t *testing.T) {
	w := &slowWriter{delay: 5 * time.Millisecond}
	a := NewAsync(w)
	for i := 0; i < 1000; i++ {
		fmt.Fprintf(a, "line %d\n", i)
	}
	a.Close()
	lines := strings.Split(strings.TrimSpace(w.String()), "\n")
	if len(lines) != 1000 || lines[0] != "line 0" || lines[999] != "line 999" {
		t.Fatalf("got %d lines: %q … %q", len(lines), lines[0], lines[len(lines)-1])
	}
}

// 磁盘卡住时写日志不能阻塞调用方：超出上限的行被丢弃并计数。
func TestAsyncNeverBlocksWhenDiskStalls(t *testing.T) {
	w := &slowWriter{block: make(chan struct{})}
	a := NewAsync(w)
	a.max = 1 << 10
	start := time.Now()
	for i := 0; i < 10000; i++ {
		fmt.Fprintf(a, "line %d\n", i)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("磁盘卡住时写日志被阻塞了 %v", d)
	}
	close(w.block)
	a.Close()
	if !strings.Contains(w.String(), "丢弃了") {
		t.Fatal("应在日志里注明丢弃的行数")
	}
}

func TestSetupWritesThroughStdLog(t *testing.T) {
	dir := t.TempDir()
	l, err := Setup(dir)
	if err != nil {
		t.Fatal(err)
	}
	log.Printf("hello %s", P(`Z:\资料\合同.pdf`))
	SetVerbose(false)
	Debugf("hidden")
	SetVerbose(true)
	Debugf("shown")
	l.Flush()
	b, _ := os.ReadFile(filepath.Join(dir, "zhaozhao.log"))
	l.Close()
	s := string(b)
	if !strings.Contains(s, "hello «Z:\\资料\\合同.pdf»") || strings.Contains(s, "hidden") || !strings.Contains(s, "shown") {
		t.Fatalf("%q", s)
	}
	if got := l.AllLogs(); len(got) != 1 {
		t.Fatalf("AllLogs: %v", got)
	}
}

func TestAnonymize(t *testing.T) {
	in := "完成 «Z:\\共享资料\\合同 2024\\采购合同.pdf» 用时 3s\n" +
		"stat Z:\\共享资料\\合同 2024\\采购合同.pdf: 拒绝访问。\n" +
		"压缩包 «\\\\nas\\docs\\归档.zip › 内部\\a.txt»\n" +
		"数据目录 E:\\ZhaoZhao\\data\n" +
		"URL http://127.0.0.1:17891/ 不变，版本 0.1.3"
	out := Anonymize(in)
	for _, leak := range []string{"共享资料", "合同 2024", "采购合同", "归档", "内部", "ZhaoZhao"} {
		if strings.Contains(out, leak) {
			t.Fatalf("泄露了 %q:\n%s", leak, out)
		}
	}
	for _, keep := range []string{"Z:\\", ".pdf» 用时 3s", ": 拒绝访问。", "\\\\", ".zip › ", "http://127.0.0.1:17891/", "0.1.3"} {
		if !strings.Contains(out, keep) {
			t.Fatalf("不应改动 %q:\n%s", keep, out)
		}
	}
	// 同一路径无论是否标记都得到同一个代号
	a := strings.Split(strings.Split(out, "\n")[0], "«")[1]
	a = strings.Split(a, "»")[0]
	if !strings.Contains(strings.Split(out, "\n")[1], a) {
		t.Fatalf("同一路径代号不一致:\n%s", out)
	}
}

// ---- 性能 ----

// 每条日志在调用方的开销（含 log 包的时间戳格式化）。
func BenchmarkLogLine(b *testing.B) {
	a := NewAsync(io.Discard)
	defer a.Close()
	lg := log.New(a, "", log.LstdFlags|log.Lmicroseconds)
	path := P(`Z:\共享资料\合同\2024年采购合同（终稿）.pdf`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lg.Printf("完成 %s [done] 大小 %d 等待 %.1fs 读取 %.1fs 提取 %.1fs", path, 123456, 0.2, 0.5, 1.3)
	}
}

// 详细日志关闭时 Debugf 的开销。
func BenchmarkDebugfOff(b *testing.B) {
	SetVerbose(false)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		Debugf("完成 %s 用时 %.1fs", "x", 1.5)
	}
}

// 多个协程同时写日志（模拟 6 个提取协程）。
func BenchmarkLogLineParallel(b *testing.B) {
	a := NewAsync(io.Discard)
	defer a.Close()
	lg := log.New(a, "", log.LstdFlags|log.Lmicroseconds)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			lg.Printf("完成 %s [done] 用时 %.1fs", `«Z:\共享资料\合同\a.pdf»`, 1.3)
		}
	})
}

// 对照：旧做法，每条日志直接写文件（一次系统调用）。
func BenchmarkLogLineSyncFile(b *testing.B) {
	r, err := New(filepath.Join(b.TempDir(), "a.log"), 1<<40, 1)
	if err != nil {
		b.Fatal(err)
	}
	defer r.Close()
	lg := log.New(r, "", log.LstdFlags|log.Lmicroseconds)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		lg.Printf("完成 %s [done] 用时 %.1fs", `«Z:\共享资料\合同\a.pdf»`, 1.3)
	}
}

// 对照：异步写入真实文件，包括后台写盘。
func BenchmarkLogLineAsyncFile(b *testing.B) {
	r, err := New(filepath.Join(b.TempDir(), "a.log"), 1<<40, 1)
	if err != nil {
		b.Fatal(err)
	}
	a := NewAsync(r)
	lg := log.New(a, "", log.LstdFlags|log.Lmicroseconds)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		lg.Printf("完成 %s [done] 用时 %.1fs", `«Z:\共享资料\合同\a.pdf»`, 1.3)
	}
	a.Close()
	r.Close()
}
