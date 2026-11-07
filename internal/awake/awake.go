// Package awake 提供「电脑醒着的时间」：睡眠、休眠期间不计时。
//
// 电脑睡眠时所有程序都暂停。Windows 上 Go 的计时器和 time.Since 用的时钟把睡眠的时间也算进去，
// 醒来后正在处理的文件会被误判为超时、剩余时间被拉长、「可能卡住了」误报。超时判断、用时统计改用这里的时钟。
package awake

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

var (
	startWall = time.Now().Round(0) // 去掉单调时钟读数：墙上时间包含睡眠
	start     = now()
	lag       atomic.Int64 // 测试用：模拟睡眠
)

// Now 返回程序启动以来醒着的时间。
func Now() time.Duration { return now() - start - time.Duration(lag.Load()) }

// Since 返回从 t（Now 的返回值）到现在醒着的时间。
func Since(t time.Duration) time.Duration { return Now() - t }

// Slept 返回程序启动以来电脑睡眠（休眠）的总时长（墙上时间减去醒着的时间）。
func Slept() time.Duration {
	d := time.Now().Round(0).Sub(startWall) - Now()
	if d < 0 {
		return 0
	}
	return d
}

// SimulateSleep 只用于测试：让醒着的时间比实际少 d，就像刚睡了 d。
func SimulateSleep(d time.Duration) { lag.Add(int64(d)) }

// Timer 在醒着的时间过去 d 之后关闭 C。底层计时器按墙上时间先到期（墙上时间不会比醒着的时间短），
// 到期时若中间睡过，就按剩下的时间重新计时。
type Timer struct {
	C       <-chan struct{}
	c       chan struct{}
	mu      sync.Mutex
	end     time.Duration
	t       *time.Timer
	stopped bool
}

// NewTimer 创建计时器。不用时调用 Stop。
func NewTimer(d time.Duration) *Timer {
	tm := &Timer{c: make(chan struct{}), end: Now() + d}
	tm.C = tm.c
	tm.mu.Lock()
	tm.t = time.AfterFunc(d, tm.fire)
	tm.mu.Unlock()
	return tm
}

func (tm *Timer) fire() {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.stopped {
		return
	}
	if rem := tm.end - Now(); rem > 0 {
		tm.t.Reset(rem)
		return
	}
	tm.stopped = true
	close(tm.c)
}

// Stop 停止计时器。
func (tm *Timer) Stop() {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.stopped = true
	tm.t.Stop()
}

// Remaining 返回 deadline（Now 的值）之前还剩多少醒着的时间；用于标准计时器到期时判断是否真的超时。
func Remaining(deadline time.Duration) time.Duration { return deadline - Now() }

// WithTimeout 与 context.WithTimeout 相同，但只计醒着的时间；超时后 ctx.Err() 为 context.DeadlineExceeded。
func WithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	tm := NewTimer(d)
	go func() {
		select {
		case <-tm.C:
			cancel(context.DeadlineExceeded)
		case <-ctx.Done():
			tm.Stop()
		}
	}()
	return &deadlineCtx{ctx}, func() { cancel(context.Canceled) }
}

// deadlineCtx 让 Err() 在超时时返回 context.DeadlineExceeded（WithCancelCause 的 Err 总是 Canceled）。
type deadlineCtx struct{ context.Context }

func (c *deadlineCtx) Err() error {
	if c.Context.Err() == nil {
		return nil
	}
	return context.Cause(c.Context)
}
