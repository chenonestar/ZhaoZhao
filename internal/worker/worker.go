// Package worker 在独立子进程中运行文件提取：单个文件导致子进程崩溃或卡死时，
// 自动重启子进程并把该文件记为失败，主程序不受影响。
package worker

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenonestar/zhaozhao/internal/doc"
	"github.com/chenonestar/zhaozhao/internal/extract"
	"github.com/chenonestar/zhaozhao/internal/logx"
	"github.com/chenonestar/zhaozhao/internal/sysinfo"
)

// Request 是发给子进程的任务。
type Request struct {
	ID   int64           `json:"id"`
	Path string          `json:"path"`
	Ext  string          `json:"ext"`
	Opts extract.Options `json:"opts"`
}

// Control 是任务进行中发给子进程的消息：把任务 Job 之后识别用的 OCR 线程数改为 OCRThreads
// （0 表示按任务参数）。任务已经结束时子进程忽略它。
type Control struct {
	Job        int64 `json:"ctlJob"`
	OCRThreads int   `json:"ocrThreads"`
}

// Response 是子进程的返回。Progress 非空时是进度消息，任务还没有结束。
type Response struct {
	ID       int64      `json:"id"`
	Progress string     `json:"progress,omitempty"`
	Result   doc.Result `json:"result"`
}

// FlagName 是主程序用来进入子进程模式的命令行参数。
const FlagName = "--extract-worker"

// Serve 是子进程入口：从标准输入逐行读取任务，把结果逐行写到标准输出。
// 日志写到标准错误，由主程序加上进程号转存到日志文件。
// 库代码误写到标准输出的内容会被重定向到标准错误，避免破坏协议。
func Serve() {
	// 主程序退出（标准输入关闭）时，结束 OCR 子进程并删除 LibreOffice 配置目录
	defer extract.CleanupWorker()
	out := os.Stdout
	os.Stdout = os.Stderr
	log.SetOutput(os.Stderr)
	log.SetFlags(0) // 时间由主程序加
	w := bufio.NewWriterSize(out, 1<<20)
	r := bufio.NewReaderSize(os.Stdin, 1<<20)
	enc := json.NewEncoder(w)
	var mu sync.Mutex
	send := func(resp Response) error {
		mu.Lock()
		defer mu.Unlock()
		if err := enc.Encode(resp); err != nil {
			return err
		}
		return w.Flush()
	}
	// 单独的协程读标准输入：任务进行中也能收到控制消息。任务按顺序交给主循环（主程序收到上一个结果后
	// 才会发下一个任务，所以读到新任务时没有任务在进行）。
	reqs := make(chan Request)
	var cur atomic.Int64
	go func() {
		defer close(reqs)
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				if err != io.EOF {
					fmt.Fprintln(os.Stderr, "读取任务失败:", err)
				}
				return
			}
			var c Control
			if json.Unmarshal(line, &c) == nil && c.Job != 0 {
				if c.Job == cur.Load() {
					extract.SetOCRThreads(c.OCRThreads)
				}
				continue
			}
			var req Request
			if err := json.Unmarshal(line, &req); err != nil {
				continue
			}
			cur.Store(req.ID)
			extract.SetOCRThreads(0)
			reqs <- req
		}
	}()
	for req := range reqs {
		logx.SetVerbose(req.Opts.Debug)
		var last time.Time
		id := req.ID
		req.Opts.SetProgress(func(msg string) {
			// 进度消息限流：最多每 0.3 秒一条
			if now := time.Now(); now.Sub(last) >= 300*time.Millisecond {
				last = now
				_ = send(Response{ID: id, Progress: msg})
			}
		})
		res := extract.Extract(req.Path, req.Ext, req.Opts)
		if req.Opts.ReportMem {
			res.Mem = sysinfo.PeakMem(os.Getpid()) + extract.OCRPeakMem()
		}
		if err := send(Response{ID: req.ID, Result: res}); err != nil {
			return
		}
	}
}

// DefaultTimeout 是兜底的单文件超时。
const DefaultTimeout = 90 * time.Second
