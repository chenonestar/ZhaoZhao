package index

import (
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/chenonestar/zhaozhao/internal/doc"
	"github.com/chenonestar/zhaozhao/internal/extract"
	"github.com/chenonestar/zhaozhao/internal/logx"
	"github.com/chenonestar/zhaozhao/internal/worker"
)

// 大 PDF 按页分给多个 OCR 进程（extract/ocrshare.go）：每个要 OCR 的 PDF 有一个共享目录；OCR 队列见底、有进程
// 空着时，让空着的进程当帮手，从最后一页往前认还在识别的大文件。又有文件要认时让帮手停下（认完手头这页）。

// ocrHelpMinLeft：剩下这么多页以上才找帮手；每个帮手至少分到 ocrHelpPerHelper 页（帮手启动要加载模型、
// 打开文件，页太少不值得）。
const (
	ocrHelpMinLeft   = 8
	ocrHelpPerHelper = 4
)

// jobShare 是一个正在 OCR 的 PDF 的共享目录和帮手。
type jobShare struct {
	dir  string
	src  string          // 要识别的文件（网络盘上的已复制到本地）
	opts extract.Options // 主力用的选项（帮手照抄，再标上帮手）

	mu      sync.Mutex
	helpers int  // 正在运行的帮手
	closed  bool // 主力已经认完：不再加帮手
	wg      sync.WaitGroup
}

// newJobShare 给要 OCR 的 PDF 建共享目录（建不了时返回 nil，照常一个进程认）。
func newJobShare(src string, opts extract.Options) *jobShare {
	dir, err := os.MkdirTemp("", "zhaozhao-ocrshare-")
	if err != nil {
		return nil
	}
	_ = os.WriteFile(filepath.Join(dir, "idle"), nil, 0o600) // 还没有帮手
	return &jobShare{dir: dir, src: src, opts: opts}
}

// ready：主力已经写下要认哪些页（用 PDFium 渲染的才有，帮手才帮得上）。
func (s *jobShare) ready() bool {
	_, err := os.Stat(filepath.Join(s.dir, "pages"))
	return err == nil
}

// stopHelpers 让帮手认完手头这页就停。
func (s *jobShare) stopHelpers() {
	_ = os.WriteFile(filepath.Join(s.dir, "stop"), nil, 0o600)
}

// finish：主力认完了。不再加帮手，让在运行的停下、等它们退出，删掉共享目录。
func (s *jobShare) finish() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.stopHelpers()
	s.wg.Wait()
	os.RemoveAll(s.dir)
}

// startHelper 加一个帮手（用 OCR 进程池里空着的进程）。主力已经认完时返回 false。
func (m *Manager) startHelper(s *jobShare, pool *worker.Pool, path string) bool {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return false
	}
	s.helpers++
	if s.helpers == 1 {
		os.Remove(filepath.Join(s.dir, "idle"))
		os.Remove(filepath.Join(s.dir, "stop"))
	}
	s.wg.Add(1)
	s.mu.Unlock()
	m.ocrHelpers.Add(1)
	go func() {
		defer func() {
			s.mu.Lock()
			s.helpers--
			if s.helpers == 0 {
				_ = os.WriteFile(filepath.Join(s.dir, "idle"), nil, 0o600)
			}
			s.mu.Unlock()
			m.ocrHelpers.Add(-1)
			s.wg.Done()
		}()
		o := s.opts
		o.OCRHelper = true
		t0 := time.Now()
		// 帮手每认一页报告一次进度：10 分钟没有进展才算卡住
		res := pool.Run(m.ctx, s.src, "pdf", o, worker.Timeouts{Idle: 10 * time.Minute, Total: 6 * time.Hour}, func(string) {})
		if res.Status == doc.StatusFailed {
			logx.Debugf("帮忙识别 %s 出错（剩下的页由识别它的进程补上）：%s", logx.P(path), res.Err)
			return
		}
		logx.Debugf("帮忙识别 %s：认了 %d 页，用时 %.0f 秒", logx.P(path), res.OCRPages, time.Since(t0).Seconds())
	}()
	return true
}

// ocrHelp：OCR 队列已空、有 idle 个进程空着时，给剩下页数多的 PDF 加帮手（每个帮手至少分到 ocrHelpPerHelper 页），
// 返回加了几个。
func (m *Manager) ocrHelp(idle int) int {
	pool := m.OCRPool
	if pool == nil {
		pool = m.Pool // 和 runJob 一样：没有单独的 OCR 进程池时用普通的
	}
	if idle <= 0 || pool == nil {
		return 0
	}
	type cand struct {
		share   *jobShare
		path    string
		left    int
		helpers int
	}
	var cs []*cand
	s := &m.sched
	s.mu.Lock()
	for _, j := range s.inflight {
		if j.share == nil || j.pagesTotal == 0 {
			continue
		}
		if left := j.pagesTotal - j.pagesDone; left >= ocrHelpMinLeft {
			cs = append(cs, &cand{share: j.share, path: j.path, left: left})
		}
	}
	s.mu.Unlock()
	for _, c := range cs {
		c.share.mu.Lock()
		c.helpers = c.share.helpers
		c.share.mu.Unlock()
	}
	added := 0
	for added < idle {
		// 每次给「每个进程平均要认的页」最多的文件加一个
		sort.Slice(cs, func(a, b int) bool { return cs[a].left*(cs[b].helpers+1) > cs[b].left*(cs[a].helpers+1) })
		if len(cs) == 0 || cs[0].left/(cs[0].helpers+2) < ocrHelpPerHelper || !cs[0].share.ready() {
			break
		}
		c := cs[0]
		if !m.startHelper(c.share, pool, c.path) {
			c.left = 0
			continue
		}
		c.helpers++
		added++
		logx.Debugf("OCR 队列已空，%s 还剩 %d 页：加一个进程从后往前帮忙识别（现在 %d 个帮手）", logx.P(c.path), c.left, c.helpers)
	}
	return added
}

// stopOCRHelpers：又有文件要 OCR 了，帮手认完手头这页就停，把进程让出来。
func (m *Manager) stopOCRHelpers() {
	s := &m.sched
	s.mu.Lock()
	var shares []*jobShare
	for _, j := range s.inflight {
		if j.share != nil {
			shares = append(shares, j.share)
		}
	}
	s.mu.Unlock()
	for _, sh := range shares {
		sh.mu.Lock()
		n := sh.helpers
		sh.mu.Unlock()
		if n > 0 {
			sh.stopHelpers()
		}
	}
}
