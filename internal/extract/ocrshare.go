package extract

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/chenonestar/zhaozhao/internal/doc"
	"github.com/chenonestar/zhaozhao/internal/logx"
)

// 大 PDF 按页分给多个 OCR 进程。
//
// 以前一个几百页的扫描 PDF 只由一个进程从头认到尾。OCR 队列见底时只剩几个大文件在认，别的进程都空着：某位用户
// Z 盘最后 28 分钟每分钟只有 16～57 页（平时 80 多页）。0.2.4 起这时把空出来的线程加给大文件，可一个进程多开
// 线程远不如多开进程（实测 1 个进程 × 6 线程每分钟 27 页，6 个进程 × 1 线程 35 页）。
//
// 现在主程序给每个要 OCR 的 PDF 准备一个共享目录（Options.OCRShare）：识别它的进程（主力）从第一页往后认，
// 认每一页前先在目录里占下这一页（建一个占位文件，建不了说明别人占了）；队列见底时主程序让空闲的进程当帮手
// （Options.OCRHelper），从最后一页往前认同一个文件，每认完一页把文字写进目录。主力认完自己占的页，再收齐
// 帮手认的页，拼成整份文件的结果。帮手中途退出（又有文件要认了、出错、被结束）留下的页，主力自己补上。
//
// 目录里的文件：pages（要 OCR 的页码，主力写）、c<页码>（占位）、r<页码>（帮手认出的文字）、
// stop（主程序要帮手停下）、idle（现在没有帮手在运行，主程序维护）。

type pageShare struct{ dir string }

func (s pageShare) on() bool { return s.dir != "" }

func (s pageShare) path(name string) string { return filepath.Join(s.dir, name) }

func (s pageShare) exists(name string) bool {
	_, err := os.Stat(s.path(name))
	return err == nil
}

// claim 占下第 pg 页；别人已经占了时返回 false。目录出了问题（被删了）时主力照常认（mine 为真），帮手不认。
func (s pageShare) claim(pg int, mine bool) bool {
	f, err := os.OpenFile(s.path("c"+strconv.Itoa(pg)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return mine && !errors.Is(err, fs.ErrExist)
	}
	f.Close()
	return true
}

// writeFile 先写临时文件再改名：读的一方要么看不到，要么看到完整的内容。
func (s pageShare) writeFile(name string, b []byte) error {
	tmp := s.path(name + ".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(name))
}

// sharedPage 是帮手认出的一页：文字，或者没法认的原因。
type sharedPage struct {
	Text string `json:"t,omitempty"`
	Skip string `json:"s,omitempty"`
}

func (s pageShare) put(pg int, r sharedPage) error {
	b, _ := json.Marshal(r)
	return s.writeFile("r"+strconv.Itoa(pg), b)
}

func (s pageShare) get(pg int) (sharedPage, bool) {
	var r sharedPage
	b, err := os.ReadFile(s.path("r" + strconv.Itoa(pg)))
	if err != nil || json.Unmarshal(b, &r) != nil {
		return r, false
	}
	return r, true
}

// results 返回帮手已经认完的页数。
func (s pageShare) results() int {
	ents, _ := os.ReadDir(s.dir)
	n := 0
	for _, e := range ents {
		if name := e.Name(); strings.HasPrefix(name, "r") && !strings.HasSuffix(name, ".tmp") {
			n++
		}
	}
	return n
}

func (s pageShare) writePages(pages []int) {
	b, _ := json.Marshal(pages)
	_ = s.writeFile("pages", b)
}

func (s pageShare) readPages() []int {
	var pages []int
	b, err := os.ReadFile(s.path("pages"))
	if err == nil {
		_ = json.Unmarshal(b, &pages)
	}
	return pages
}

// shareWaitMax：主力等帮手认某一页最多等多久（帮手正常时一页不过几秒到几十秒），超时就自己认。
var shareWaitMax = 5 * time.Minute

// collectShared 收齐帮手认的页（pending 里的），收到的写进 res；帮手都已退出还没有结果的页返回，由主力自己认。
// 等待期间按已完成的页数报告进度（主程序据此不判超时、算速度）。
func collectShared(s pageShare, pending []int, res pdfOCRResult, progress func()) []int {
	waitStart := time.Now()
	for {
		var left []int
		for _, pg := range pending {
			r, ok := s.get(pg)
			if !ok {
				left = append(left, pg)
				continue
			}
			if r.Skip != "" {
				res.skipped[r.Skip]++
			} else if r.Text != "" {
				res.texts[pg] = r.Text
			}
		}
		if len(left) < len(pending) {
			waitStart = time.Now()
			progress()
		}
		pending = left
		if len(pending) == 0 {
			return nil
		}
		// 没有帮手在运行了（idle），或者等太久：剩下的页自己认。先再看一次，免得帮手刚写完结果就退出
		if s.exists("idle") || time.Since(waitStart) > shareWaitMax {
			var still []int
			for _, pg := range pending {
				if r, ok := s.get(pg); ok {
					if r.Skip != "" {
						res.skipped[r.Skip]++
					} else if r.Text != "" {
						res.texts[pg] = r.Text
					}
					continue
				}
				still = append(still, pg)
			}
			return still
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// helpPDF 当帮手：从最后一页往前认主力还没占的页，认完一页写一页，主程序要求停下（stop）时认完手头的就停。
func helpPDF(p string, opt Options) doc.Result {
	s := pageShare{opt.OCRShare}
	pages := s.readPages()
	if !s.on() || len(pages) == 0 || !opt.OCR.Enabled() {
		return doc.Result{Status: doc.StatusOK}
	}
	if _, err := PDFiumPath(); err != nil {
		return doc.Result{Status: doc.StatusOK}
	}
	pdfiumMu.Lock()
	defer pdfiumMu.Unlock()
	d, err := openPDFium(p)
	if err != nil {
		return doc.Result{Status: doc.StatusFailed, Err: err.Error()}
	}
	defer d.close()
	tmp, err := os.MkdirTemp("", "zhaozhao-pdfocr-")
	if err != nil {
		return doc.Result{Status: doc.StatusFailed, Err: err.Error()}
	}
	defer os.RemoveAll(tmp)
	rev := make([]int, len(pages))
	for i, pg := range pages {
		rev[len(pages)-1-i] = pg
	}
	ahead, stop := renderAhead(d, rev, tmp, func(pg int) bool { return !s.exists("stop") && s.claim(pg, false) })
	defer stop()
	big := newBigOCR(opt.OCR)
	defer big.close()
	done := 0
	for r := range ahead {
		if r.err != nil {
			return doc.Result{Status: doc.StatusFailed, Err: r.err.Error()}
		}
		opt.Progress("帮忙识别第 %d 页", r.pg)
		var sp sharedPage
		switch {
		case r.skip != "":
			sp.Skip = r.skip
		case r.path != "":
			var text string
			text, _, err = recognizePage(opt.OCR, big, r.path, r.size)
			os.Remove(r.path)
			switch {
			case errors.Is(err, errOCRTimeout):
				sp.Skip = "识别超时"
			case err != nil:
				return doc.Result{Status: doc.StatusFailed, Err: err.Error()}
			default:
				sp.Text = text
				logx.Debugf("OCR %s 第 %d 页（帮手）识别出 %d 字", logx.P(p), r.pg, len([]rune(text)))
			}
		}
		if err := s.put(r.pg, sp); err != nil {
			return doc.Result{Status: doc.StatusFailed, Err: err.Error()}
		}
		done++
	}
	return doc.Result{Status: doc.StatusOK, OCRPages: done}
}

// resultsIf 返回帮手已经认完的页数（没有共享目录时为 0）。
func (s pageShare) resultsIf() int {
	if !s.on() {
		return 0
	}
	return s.results()
}
