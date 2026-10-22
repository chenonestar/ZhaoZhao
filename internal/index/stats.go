package index

import (
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/chenonestar/zhaozhao/internal/awake"
)

// 阶段用时统计：建索引期间每 statsEvery 在日志里写一行，说明这段时间普通文件的时间花在哪一步（读文件和查重、
// 提取、准备写入）、写入线程有多忙、等写入等了多久、OCR 识别了多少页。导出的诊断信息里有它，就能看出慢在哪。
var statsEvery = 10 * time.Minute

type stageStats struct {
	mu    sync.Mutex
	c     stageCounts
	since time.Duration // 这段统计开始的时间（awake，不含电脑睡眠）
	pages int64         // 这段统计开始时 OCR 累计识别的页数
}

type stageCounts struct {
	files, reused          int           // 普通文件处理完的个数、其中复用已有结果的（内容相同、移动改名）
	read, extract, prepare time.Duration // 普通文件各阶段的用时之和：读取与查重（含计算校验值）、提取、准备写入（分词、压缩）
	written                int           // 写入的文件数（所有通道）
	chars                  int64         // 写入的文字（字数）
	queued                 time.Duration // 交给写入协程后等到开始写的时间之和
	writeBusy, bulkBusy    time.Duration // 写入协程在事务里的时间、其中批量写入的
	paused                 time.Duration // 用户在用电脑时让出的时间之和（各处理线程相加）
}

func (s *stageStats) addJob(read, extract, prepare time.Duration, reused bool) {
	s.mu.Lock()
	s.c.files++
	if reused {
		s.c.reused++
	}
	s.c.read += read
	s.c.extract += extract
	s.c.prepare += prepare
	s.mu.Unlock()
}

// addBatch 记下写入的一批：事务用时、是否批量写入，以及每个文件交给写入协程后等了多久。
func (s *stageStats) addBatch(batch []*prepared, start time.Time, dur time.Duration, bulk bool) {
	var chars int64
	var queued time.Duration
	for _, p := range batch {
		if p.text != nil {
			chars += int64(utf8.RuneCountInString(p.text.Text))
		}
		for _, c := range p.children {
			if c.text != nil {
				chars += int64(utf8.RuneCountInString(c.text.Text))
			}
		}
		if !p.sent.IsZero() {
			queued += start.Sub(p.sent)
		}
	}
	s.mu.Lock()
	s.c.written += len(batch)
	s.c.chars += chars
	s.c.queued += queued
	s.c.writeBusy += dur
	if bulk {
		s.c.bulkBusy += dur
	}
	s.mu.Unlock()
}

func (s *stageStats) addPause(d time.Duration) {
	s.mu.Lock()
	s.c.paused += d
	s.mu.Unlock()
}

// take 取出这段统计的汇总（没有任何进展时返回空字符串），并开始下一段。
func (s *stageStats) take(now time.Duration, pages int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	span, ocr := now-s.since, pages-s.pages
	old := s.c
	s.c, s.since, s.pages = stageCounts{}, now, pages
	if span <= 0 || (old.files == 0 && old.written == 0 && ocr == 0) {
		return ""
	}
	var parts []string
	if old.files > 0 {
		n := time.Duration(old.files)
		parts = append(parts, fmt.Sprintf("普通文件 %d 个（复用已有结果 %d 个），平均每个：读取与查重 %s、提取 %s、准备写入 %s",
			old.files, old.reused, secs(old.read/n), secs(old.extract/n), secs(old.prepare/n)))
	}
	if old.written > 0 {
		w := fmt.Sprintf("写入 %d 个文件、%s字，写入线程忙 %.0f%%", old.written, wanZi(old.chars),
			100*old.writeBusy.Seconds()/span.Seconds())
		if old.bulkBusy > 0 {
			w += fmt.Sprintf("（其中批量写入 %.0f%%）", 100*old.bulkBusy.Seconds()/span.Seconds())
		}
		if old.writeBusy > 0 {
			w += fmt.Sprintf("，每秒写入 %s字", wanZi(int64(float64(old.chars)/old.writeBusy.Seconds())))
		}
		w += "，平均等写入 " + secs(old.queued/time.Duration(old.written))
		parts = append(parts, w)
	}
	if ocr > 0 {
		parts = append(parts, fmt.Sprintf("OCR %d 页（每分钟 %.0f 页）", ocr, float64(ocr)/span.Minutes()))
	}
	if old.paused > 0 {
		parts = append(parts, "用电脑时让出 "+secs(old.paused))
	}
	return fmt.Sprintf("最近 %.0f 分钟：%s", span.Minutes(), strings.Join(parts, "；"))
}

func secs(d time.Duration) string {
	if d < 10*time.Second {
		return fmt.Sprintf("%.2f 秒", d.Seconds())
	}
	return fmt.Sprintf("%.0f 秒", d.Seconds())
}

// wanZi 把字数写成「1234 」「12.3 万」这样（后面接「字」）。
func wanZi(n int64) string {
	if n < 10000 {
		return fmt.Sprintf("%d ", n)
	}
	return fmt.Sprintf("%.1f 万", float64(n)/1e4)
}

// statsLoop 每 statsEvery 把阶段用时写进日志（这段时间什么都没做就不写）。
func (m *Manager) statsLoop() {
	defer m.wg.Done()
	m.stats.mu.Lock()
	m.stats.since, m.stats.pages = awake.Now(), m.ocr.pages.Load()
	m.stats.mu.Unlock()
	t := time.NewTicker(statsEvery)
	defer t.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
		}
		if s := m.stats.take(awake.Now(), m.ocr.pages.Load()); s != "" {
			m.logf("%s", s)
		}
	}
}
