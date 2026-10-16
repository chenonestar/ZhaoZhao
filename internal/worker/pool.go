package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/chenonestar/zhaozhao/internal/awake"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenonestar/zhaozhao/internal/doc"
	"github.com/chenonestar/zhaozhao/internal/extract"
	"github.com/chenonestar/zhaozhao/internal/logx"
)

type proc struct {
	cmd      *exec.Cmd
	in       io.WriteCloser
	out      *bufio.Reader
	pid      int
	lastUsed time.Time
	jobs     int

	waitOnce sync.Once
	waitErr  error
	waited   chan struct{}
}

// done 返回子进程退出时关闭的通道（只调用一次 cmd.Wait）。
func (pr *proc) done() <-chan struct{} {
	pr.waitOnce.Do(func() {
		pr.waited = make(chan struct{})
		go func() { pr.waitErr = pr.cmd.Wait(); close(pr.waited) }()
	})
	return pr.waited
}

// exited 等待子进程退出，最多 d。
func (pr *proc) exited(d time.Duration) bool {
	if d < 0 {
		d = 0
	}
	select {
	case <-pr.done():
		return true
	case <-time.After(d):
		return false
	}
}

// Pool 管理一组提取子进程。Exe 为空时在当前进程内执行（测试用）。
//
// 子进程按需启动；用完的子进程放回空闲栈，下次优先复用最近用过的（已经加载过组件、更快），
// 闲置超过 IdleTimeout 的子进程自动结束。同时运行的子进程数不超过 size，size 可在运行中调整。
type Pool struct {
	Exe         string
	Args        []string
	Name        string        // 日志里的名称，如「解析」「OCR」
	IdleTimeout time.Duration // 闲置多久后结束子进程

	mu     sync.Mutex
	size   int
	busy   int
	idle   []*proc // 栈：末尾是最近用过的
	live   map[*proc]bool
	wake   chan struct{}
	closed bool
	stop   chan struct{}
	seq    atomic.Int64
}

// label 是日志里子进程的称呼，如「解析进程」「OCR 进程」。
func (p *Pool) label() string {
	for _, r := range p.Name {
		if r >= 0x80 {
			return p.Name + "进程"
		}
	}
	return p.Name + " 进程"
}

// Timeouts 是单个文件的时限：Idle 是「没有进展」的时限（子进程每报告一次进度就重新计时），
// Total 是总时限。不报告进度的提取两者取同一个值。
type Timeouts struct {
	Idle, Total time.Duration
}

// NewPool 创建最多 n 个子进程的进程池。
func NewPool(exe string, n int) *Pool {
	if n < 1 {
		n = 1
	}
	p := &Pool{Exe: exe, Args: []string{FlagName}, Name: "解析", IdleTimeout: 3 * time.Minute,
		size: n, live: map[*proc]bool{}, wake: make(chan struct{}), stop: make(chan struct{})}
	if exe != "" {
		go p.reaper()
	}
	return p
}

// SetSize 调整同时运行的子进程上限。
func (p *Pool) SetSize(n int) {
	if n < 1 {
		n = 1
	}
	p.mu.Lock()
	p.size = n
	var extra []*proc
	for len(p.idle) > 0 && len(p.idle)+p.busy > n {
		extra = append(extra, p.idle[0])
		p.idle = p.idle[1:]
	}
	p.broadcast()
	p.mu.Unlock()
	for _, pr := range extra {
		p.kill(pr)
	}
}

// Stats 返回正在使用的子进程数、已启动的子进程数和上限。
func (p *Pool) Stats() (busy, live, size int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.busy, len(p.live), p.size
}

func (p *Pool) broadcast() {
	close(p.wake)
	p.wake = make(chan struct{})
}

func (p *Pool) acquire(ctx context.Context) (*proc, bool) {
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, false
		}
		if p.busy < p.size {
			p.busy++
			var pr *proc
			if n := len(p.idle); n > 0 {
				pr = p.idle[n-1]
				p.idle = p.idle[:n-1]
			}
			p.mu.Unlock()
			return pr, true
		}
		w := p.wake
		p.mu.Unlock()
		select {
		case <-w:
		case <-ctx.Done():
			return nil, false
		}
	}
}

// put 归还槽位；pr 为 nil 表示子进程已结束。
func (p *Pool) put(pr *proc) {
	p.mu.Lock()
	p.busy--
	var kill *proc
	if pr != nil {
		if p.closed || len(p.idle)+p.busy >= p.size {
			kill = pr
		} else {
			pr.lastUsed = time.Now()
			p.idle = append(p.idle, pr)
		}
	}
	p.broadcast()
	p.mu.Unlock()
	if kill != nil {
		p.kill(kill)
	}
}

// reaper 结束闲置太久的子进程，避免长期占用内存。
func (p *Pool) reaper() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
		}
		p.reapIdle()
	}
}

func (p *Pool) reapIdle() {
	p.mu.Lock()
	var old []*proc
	for len(p.idle) > 0 && time.Since(p.idle[0].lastUsed) > p.IdleTimeout {
		old = append(old, p.idle[0])
		p.idle = p.idle[1:]
	}
	p.mu.Unlock()
	for _, pr := range old {
		logx.Debugf("%s %d 闲置超过 %v，结束（共处理 %d 个文件）", p.label(), pr.pid, p.IdleTimeout, pr.jobs)
		p.retire(pr, 15*time.Second)
	}
}

func (p *Pool) start() (*proc, error) {
	cmd := exec.Command(p.Exe, p.Args...)
	prepare(cmd)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	ll := &lineLogger{}
	cmd.Stderr = ll
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	lowerPriority(cmd)
	pr := &proc{cmd: cmd, in: in, out: bufio.NewReaderSize(out, 1<<20), pid: cmd.Process.Pid}
	ll.setPrefix(fmt.Sprintf("[%s %d] ", p.label(), pr.pid))
	p.mu.Lock()
	p.live[pr] = true
	n := len(p.live)
	p.mu.Unlock()
	logx.Debugf("启动%s %d（现有 %d 个）", p.label(), pr.pid, n)
	return pr, nil
}

// kill 结束子进程并返回它的退出状态。
func (p *Pool) kill(pr *proc) error {
	if pr == nil {
		return nil
	}
	pr.in.Close()
	killTree(pr.cmd)
	<-pr.done()
	err := pr.waitErr
	p.mu.Lock()
	delete(p.live, pr)
	p.mu.Unlock()
	return err
}

// retire 让闲置的子进程正常退出：关闭它的标准输入，子进程会结束常驻的 LibreOffice、OCR 进程并删除
// 临时配置目录；grace 内没有退出才强制结束。
func (p *Pool) retire(pr *proc, grace time.Duration) {
	pr.in.Close()
	if !pr.exited(grace) {
		killTree(pr.cmd)
		<-pr.done()
	}
	p.mu.Lock()
	delete(p.live, pr)
	p.mu.Unlock()
}

// Extract 在子进程里提取一个文件（不报告进度，timeout 为总时限）。
func (p *Pool) Extract(ctx context.Context, path, ext string, opts extract.Options, timeout time.Duration) doc.Result {
	return p.Run(ctx, path, ext, opts, Timeouts{Idle: timeout, Total: timeout}, nil)
}

// Run 在子进程里提取一个文件。超时、子进程崩溃都会返回失败结果并结束该子进程。
// onProgress 收到子进程报告的进度（可为 nil）。
func (p *Pool) Run(ctx context.Context, path, ext string, opts extract.Options, t Timeouts, onProgress func(string)) doc.Result {
	if t.Total <= 0 {
		t.Total = DefaultTimeout
	}
	if t.Idle <= 0 || t.Idle > t.Total {
		t.Idle = t.Total
	}
	opts.Debug = logx.Verbose()
	if p.Exe == "" {
		if onProgress != nil {
			opts.SetProgress(onProgress)
		}
		return extract.Extract(path, ext, opts)
	}
	pr, ok := p.acquire(ctx)
	if !ok {
		return doc.Result{Status: doc.StatusFailed, Err: "已取消"}
	}
	if pr == nil {
		var err error
		if pr, err = p.start(); err != nil {
			p.put(nil)
			return doc.Result{Status: doc.StatusFailed, Err: "无法启动解析进程: " + err.Error()}
		}
	}
	pr.jobs++
	id := p.seq.Add(1)

	line, _ := json.Marshal(Request{ID: id, Path: path, Ext: ext, Opts: opts})
	line = append(line, '\n')

	type reply struct {
		progress string
		res      doc.Result
		err      error
	}
	ch := make(chan reply, 8)
	done := make(chan struct{})
	defer close(done)
	cur := pr
	go func() {
		emit := func(r reply) bool {
			select {
			case ch <- r:
				return true
			case <-done:
				return false
			}
		}
		if _, err := cur.in.Write(line); err != nil {
			emit(reply{err: err})
			return
		}
		for {
			b, err := cur.out.ReadBytes('\n')
			if err != nil {
				emit(reply{err: err})
				return
			}
			var resp Response
			if json.Unmarshal(b, &resp) != nil {
				emit(reply{err: fmt.Errorf("解析进程返回了无效数据")})
				return
			}
			if resp.ID != id {
				continue
			}
			if resp.Progress != "" {
				if !emit(reply{progress: resp.Progress}) {
					return
				}
				continue
			}
			emit(reply{res: resp.Result})
			return
		}
	}()

	// 时限只计醒着的时间：电脑睡眠时计时器照走，醒来后到期的先看实际过了多久，没到就重新计时
	idleEnd, totalEnd := awake.Now()+t.Idle, awake.Now()+t.Total
	idle := time.NewTimer(t.Idle)
	defer idle.Stop()
	total := time.NewTimer(t.Total)
	defer total.Stop()
	fail := func(msg string) doc.Result {
		err := p.kill(pr)
		p.put(nil)
		why := ""
		if err != nil {
			why = "（" + extract.DescribeExit(err, "") + "）"
		}
		log.Printf("%s %d：%s，已结束该进程%s；文件 %s", p.label(), pr.pid, msg, why, logx.P(path))
		return doc.Result{Status: doc.StatusFailed, Err: msg}
	}
	for {
		select {
		case r := <-ch:
			if r.progress != "" {
				if onProgress != nil {
					onProgress(r.progress)
				}
				if !idle.Stop() {
					select {
					case <-idle.C:
					default:
					}
				}
				idle.Reset(t.Idle)
				idleEnd = awake.Now() + t.Idle
				continue
			}
			if r.err != nil {
				return fail("解析进程崩溃: " + r.err.Error())
			}
			p.put(pr)
			return r.res
		case <-idle.C:
			if rem := awake.Remaining(idleEnd); rem > 0 {
				idle.Reset(rem)
				continue
			}
			if t.Idle < t.Total {
				return fail(fmt.Sprintf("提取超时（%s 内没有任何进展）", fmtDur(t.Idle)))
			}
			return fail("提取超时")
		case <-total.C:
			if rem := awake.Remaining(totalEnd); rem > 0 {
				total.Reset(rem)
				continue
			}
			return fail(fmt.Sprintf("提取超时（超过 %s）", fmtDur(t.Total)))
		case <-ctx.Done():
			p.kill(pr)
			p.put(nil)
			return doc.Result{Status: doc.StatusFailed, Err: "已取消"}
		}
	}
}

func fmtDur(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		return fmt.Sprintf("%d 分钟", int(d/time.Minute))
	}
	return fmt.Sprintf("%d 秒", int(d/time.Second))
}

// Close 终止全部子进程。
func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	close(p.stop)
	var ps []*proc
	for pr := range p.live {
		ps = append(ps, pr)
	}
	p.idle = nil
	p.broadcast()
	p.mu.Unlock()
	// 先关闭标准输入让子进程自己收尾（结束常驻的 LibreOffice、删除临时配置目录），最多等 3 秒
	for _, pr := range ps {
		pr.in.Close()
	}
	deadline := time.Now().Add(3 * time.Second)
	for _, pr := range ps {
		if !pr.exited(time.Until(deadline)) {
			killTree(pr.cmd)
		}
	}
}

// lineLogger 把子进程的标准错误逐行写进日志（加上进程号前缀）。
type lineLogger struct {
	mu     sync.Mutex
	prefix string
	buf    []byte
}

func (l *lineLogger) setPrefix(s string) {
	l.mu.Lock()
	l.prefix = s
	l.mu.Unlock()
}

func (l *lineLogger) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, b...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		if line := strings.TrimRight(string(l.buf[:i]), "\r"); line != "" {
			log.Print(l.prefix + line)
		}
		l.buf = l.buf[i+1:]
	}
	if len(l.buf) > 64<<10 { // 没有换行的超长输出：截断写出
		log.Print(l.prefix + string(l.buf[:4096]) + "…")
		l.buf = l.buf[:0]
	}
	return len(b), nil
}
