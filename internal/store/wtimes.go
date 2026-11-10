package store

import (
	"sync/atomic"
	"time"
)

// WriteTimes 是写入提取结果时各步累计的用时（纳秒），写入分项计时用：建索引期间写入线程九成以上时间在忙，
// 要知道时间花在全文索引、文本、文件信息里的哪一步。只由写入线程累加，统计时读出并清零。
type WriteTimes struct {
	FTS   atomic.Int64 // 删除旧的、写入新的全文索引条目
	Text  atomic.Int64 // 写入提取文本
	Files atomic.Int64 // 更新文件信息
	Dup   atomic.Int64 // 写入时按内容查重
	Check atomic.Int64 // 写入前确认文件在提取期间没被修改、删除
	// 内容和已索引的文件相同、共用文本的文件个数和字数（只写全文索引和文件信息，不再存文本）
	DupFiles, DupChars atomic.Int64
}

// Times 是本程序写入的累计用时。
var Times WriteTimes

// WriteSplit 是一段时间里各步的用时。
type WriteSplit struct {
	FTS, Text, Files, Dup, Check time.Duration
	DupFiles, DupChars           int64
}

// Take 取出累计的用时并清零。
func (w *WriteTimes) Take() WriteSplit {
	return WriteSplit{time.Duration(w.FTS.Swap(0)), time.Duration(w.Text.Swap(0)), time.Duration(w.Files.Swap(0)),
		time.Duration(w.Dup.Swap(0)), time.Duration(w.Check.Swap(0)), w.DupFiles.Swap(0), w.DupChars.Swap(0)}
}

// Add 把另一段的用时加进来。
func (s *WriteSplit) Add(o WriteSplit) {
	s.FTS += o.FTS
	s.Text += o.Text
	s.Files += o.Files
	s.Dup += o.Dup
	s.Check += o.Check
	s.DupFiles += o.DupFiles
	s.DupChars += o.DupChars
}

// since 把从 t 到现在的用时加到 c 上。
func since(c *atomic.Int64, t time.Time) { c.Add(int64(time.Since(t))) }
