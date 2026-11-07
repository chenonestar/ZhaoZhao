package logx

import (
	"fmt"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// Async 是异步日志写入器：调用方只把一行追加到内存缓冲区（一次加锁和内存复制），
// 后台协程每隔一段时间或缓冲区较满时，把整块数据一次写入文件。
// 这样写日志不会因为磁盘慢、杀毒软件扫描日志文件而拖住索引；
// 缓冲区超过上限（磁盘卡住）时丢弃新行并计数，之后在日志里注明丢了多少行。
type Async struct {
	mu      sync.Mutex
	buf     []byte
	spare   []byte
	max     int
	dropped int64
	closed  bool

	w     io.Writer
	wmu   sync.Mutex // 串行化实际写入（后台刷新与 Flush 可能同时发生）
	kick  chan struct{}
	stop  chan struct{}
	done  chan struct{}
	every time.Duration
}

const (
	flushAt      = 64 << 10 // 缓冲区达到 64 KB 时立即唤醒写入协程
	defaultMax   = 8 << 20  // 最多缓存 8 MB（约 5 万行），超过即丢弃
	defaultEvery = 500 * time.Millisecond
)

// NewAsync 创建异步写入器并启动后台写入协程。
func NewAsync(w io.Writer) *Async {
	a := &Async{w: w, max: defaultMax, every: defaultEvery,
		kick: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	a.buf = make([]byte, 0, flushAt*2)
	go a.loop()
	return a
}

// Write 追加一行到缓冲区，不做磁盘操作。
func (a *Async) Write(p []byte) (int, error) {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		a.wmu.Lock()
		defer a.wmu.Unlock()
		return a.w.Write(p)
	}
	if len(a.buf)+len(p) > a.max {
		a.dropped++
		a.mu.Unlock()
		return len(p), nil
	}
	a.buf = append(a.buf, p...)
	full := len(a.buf) >= flushAt
	a.mu.Unlock()
	if full {
		select {
		case a.kick <- struct{}{}:
		default:
		}
	}
	return len(p), nil
}

func (a *Async) loop() {
	defer close(a.done)
	t := time.NewTicker(a.every)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-a.kick:
		case <-a.stop:
			a.Flush()
			return
		}
		a.Flush()
	}
}

// Flush 把缓冲区里的内容写入文件（同步）。导出诊断信息、退出前调用。
func (a *Async) Flush() {
	a.wmu.Lock()
	defer a.wmu.Unlock()
	a.mu.Lock()
	data, dropped := a.buf, a.dropped
	a.buf, a.spare = a.spare[:0], data // 交换两块缓冲区：写文件期间调用方继续往另一块追加
	a.dropped = 0
	a.mu.Unlock()
	if dropped > 0 {
		fmt.Fprintf(a.w, "%s 日志写入跟不上，丢弃了 %d 行\n", time.Now().Format("2006/01/02 15:04:05"), dropped)
	}
	if len(data) > 0 {
		_, _ = a.w.Write(data)
	}
}

// Close 写完剩余内容并停止后台协程。之后的写入直接落盘。
func (a *Async) Close() {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	a.closed = true
	a.mu.Unlock()
	close(a.stop)
	<-a.done
	a.Flush()
}

// ---- 级别 ----

var verbose atomic.Bool

// SetVerbose 切换「详细」日志。详细模式下记录每个文件的处理过程、子进程启动退出、定时状态快照等。
func SetVerbose(v bool) { verbose.Store(v) }

// Verbose 说明是否记录详细日志。参数需要额外计算时，先判断它再调用 Debugf。
func Verbose() bool { return verbose.Load() }

// Debugf 只在详细模式下记录。关闭时只有一次原子读取的开销，不做格式化。
func Debugf(format string, a ...any) {
	if verbose.Load() {
		log.Output(2, fmt.Sprintf(format, a...))
	}
}
