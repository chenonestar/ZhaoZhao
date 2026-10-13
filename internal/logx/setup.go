package logx

import (
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
)

// Files 是当前日志文件的位置，导出诊断信息时使用。
type Files struct {
	Log   string // zhaozhao.log（滚动文件为 .1 … .N）
	Keep  int
	Crash string // 程序崩溃时 Go 运行时写出的崩溃报告
}

// Logger 是程序的日志：滚动文件 + 异步缓冲。
type Logger struct {
	Files
	rot   *Rotating
	async *Async
}

// Setup 打开日志并接管标准库 log 的输出：单个文件 10 MB，保留 5 个旧文件。
// 同时把运行时崩溃报告（panic、致命错误）写到 crash.log：图形界面程序没有控制台，否则这些信息会丢失。
func Setup(dataDir string) (*Logger, error) {
	l := &Logger{Files: Files{Log: filepath.Join(dataDir, "zhaozhao.log"), Keep: 5, Crash: filepath.Join(dataDir, "crash.log")}}
	rot, err := New(l.Log, 10<<20, l.Keep)
	if err != nil {
		return nil, err
	}
	l.rot = rot
	l.async = NewAsync(rot)
	log.SetOutput(l.async)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if f, err := os.OpenFile(l.Crash, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		_ = debug.SetCrashOutput(f, debug.CrashOptions{})
		f.Close() // SetCrashOutput 自己复制了句柄
	}
	return l, nil
}

// Flush 立即把缓冲的日志写入文件。
func (l *Logger) Flush() {
	if l != nil && l.async != nil {
		l.async.Flush()
	}
}

// Close 写完缓冲区并关闭文件。
func (l *Logger) Close() {
	if l == nil {
		return
	}
	l.async.Close()
	log.SetOutput(os.Stderr)
	l.rot.Close()
	_ = debug.SetCrashOutput(nil, debug.CrashOptions{}) // 释放 crash.log（Windows 上打开的文件不能删除）
}

// AllLogs 返回现有的日志文件（最新的在前）。
func (f Files) AllLogs() []string {
	out := []string{f.Log}
	for i := 1; i <= f.Keep; i++ {
		out = append(out, f.Log+"."+itoa(i))
	}
	out = append(out, f.Crash)
	var exist []string
	for _, p := range out {
		if st, err := os.Stat(p); err == nil && st.Size() > 0 {
			exist = append(exist, p)
		}
	}
	return exist
}

func itoa(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return itoa(i/10) + string(rune('0'+i%10))
}
