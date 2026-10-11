package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/chenonestar/zhaozhao/internal/doc"
	"github.com/chenonestar/zhaozhao/internal/extract"
)

type proc struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader
}

// Pool 管理一组提取子进程。Exe 为空时在当前进程内执行（测试用）。
type Pool struct {
	Exe  string
	Args []string

	mu     sync.Mutex
	slots  chan *proc
	size   int
	closed bool
	seq    int64
	live   map[*proc]bool
}

// NewPool 创建 n 个槽位的进程池，子进程按需启动。
func NewPool(exe string, n int) *Pool {
	if n < 1 {
		n = 1
	}
	p := &Pool{Exe: exe, Args: []string{FlagName}, slots: make(chan *proc, n), size: n, live: map[*proc]bool{}}
	for i := 0; i < n; i++ {
		p.slots <- nil
	}
	return p
}

func (p *Pool) start() (*proc, error) {
	cmd := exec.Command(p.Exe, p.Args...)
	cmd.Stderr = os.Stderr
	prepare(cmd)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	lowerPriority(cmd)
	pr := &proc{cmd: cmd, in: in, out: bufio.NewReaderSize(out, 1<<20)}
	p.mu.Lock()
	p.live[pr] = true
	p.mu.Unlock()
	return pr, nil
}

func (p *Pool) kill(pr *proc) {
	if pr == nil {
		return
	}
	pr.in.Close()
	killTree(pr.cmd)
	pr.cmd.Wait()
	p.mu.Lock()
	delete(p.live, pr)
	p.mu.Unlock()
}

// Extract 在子进程里提取一个文件。timeout 到期、子进程崩溃都会返回失败结果。
func (p *Pool) Extract(ctx context.Context, path, ext string, opts extract.Options, timeout time.Duration) doc.Result {
	if p.Exe == "" {
		return extract.Extract(path, ext, opts)
	}
	var pr *proc
	select {
	case pr = <-p.slots:
	case <-ctx.Done():
		return doc.Result{Status: doc.StatusFailed, Err: "已取消"}
	}
	defer func() { p.slots <- pr }()
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	if pr == nil {
		var err error
		if pr, err = p.start(); err != nil {
			return doc.Result{Status: doc.StatusFailed, Err: "无法启动解析进程: " + err.Error()}
		}
	}
	p.mu.Lock()
	p.seq++
	id := p.seq
	p.mu.Unlock()

	line, _ := json.Marshal(Request{ID: id, Path: path, Ext: ext, Opts: opts})
	line = append(line, '\n')

	type reply struct {
		res doc.Result
		err error
	}
	ch := make(chan reply, 1)
	go func() {
		if _, err := pr.in.Write(line); err != nil {
			ch <- reply{err: err}
			return
		}
		for {
			b, err := pr.out.ReadBytes('\n')
			if err != nil {
				ch <- reply{err: err}
				return
			}
			var resp Response
			if json.Unmarshal(b, &resp) != nil {
				ch <- reply{err: fmt.Errorf("解析进程返回了无效数据")}
				return
			}
			if resp.ID != id {
				continue
			}
			ch <- reply{res: resp.Result}
			return
		}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		if r.err != nil {
			p.kill(pr)
			pr = nil
			return doc.Result{Status: doc.StatusFailed, Err: "解析进程崩溃: " + r.err.Error()}
		}
		return r.res
	case <-timer.C:
		p.kill(pr)
		pr = nil
		return doc.Result{Status: doc.StatusFailed, Err: "提取超时"}
	case <-ctx.Done():
		p.kill(pr)
		pr = nil
		return doc.Result{Status: doc.StatusFailed, Err: "已取消"}
	}
}

// Close 终止全部子进程。
func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	var ps []*proc
	for pr := range p.live {
		ps = append(ps, pr)
	}
	p.mu.Unlock()
	for _, pr := range ps {
		pr.in.Close()
		killTree(pr.cmd)
	}
}
