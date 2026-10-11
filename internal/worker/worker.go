// Package worker 在独立子进程中运行文件提取：单个文件导致子进程崩溃或卡死时，
// 自动重启子进程并把该文件记为失败，主程序不受影响。
package worker

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/chenonestar/zhaozhao/internal/doc"
	"github.com/chenonestar/zhaozhao/internal/extract"
)

// Request 是发给子进程的任务。
type Request struct {
	ID   int64           `json:"id"`
	Path string          `json:"path"`
	Ext  string          `json:"ext"`
	Opts extract.Options `json:"opts"`
}

// Response 是子进程的返回。
type Response struct {
	ID     int64      `json:"id"`
	Result doc.Result `json:"result"`
}

// FlagName 是主程序用来进入子进程模式的命令行参数。
const FlagName = "--extract-worker"

// Serve 是子进程入口：从标准输入逐行读取任务，把结果逐行写到标准输出。
// 库代码误写到标准输出的内容会被重定向到标准错误，避免破坏协议。
func Serve() {
	out := os.Stdout
	os.Stdout = os.Stderr
	w := bufio.NewWriterSize(out, 1<<20)
	r := bufio.NewReaderSize(os.Stdin, 1<<20)
	enc := json.NewEncoder(w)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if err != io.EOF {
				fmt.Fprintln(os.Stderr, "worker read:", err)
			}
			return
		}
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}
		res := extract.Extract(req.Path, req.Ext, req.Opts)
		if err := enc.Encode(Response{ID: req.ID, Result: res}); err != nil {
			return
		}
		w.Flush()
	}
}

// DefaultTimeout 是兜底的单文件超时。
const DefaultTimeout = 90 * time.Second
