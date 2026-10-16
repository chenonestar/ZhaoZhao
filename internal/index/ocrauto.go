package index

import (
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenonestar/zhaozhao/internal/awake"
	"github.com/chenonestar/zhaozhao/internal/logx"
)

// ocrState 是 OCR 进程数 × 线程数的当前搭配和实测数据（见 ocrtune.go）。
type ocrState struct {
	mu         sync.Mutex
	plan       ocrPlan
	tuner      *ocrTuner
	rate       rateWindow
	note       string        // 正在比较搭配时给界面的说明
	cores      int           // 物理核心数
	othersIdle time.Duration // 普通文件、老格式开始空闲的时间（awake）；0 表示忙
	pages      atomic.Int64  // OCR 通道累计识别的页数
}

// ocrTuneEvery：多久调整一次。
var ocrTuneEvery = 30 * time.Second

// othersIdleFor：普通文件、老格式空闲这么久后，OCR 才用全部核心（避免偶尔来一个文件就来回切换）。
const othersIdleFor = 2 * time.Minute

func (m *Manager) physCores() int {
	m.ocr.mu.Lock()
	defer m.ocr.mu.Unlock()
	if m.ocr.cores == 0 {
		m.ocr.cores = physicalCores()
	}
	return m.ocr.cores
}

// ocrPlanNow 返回当前搭配；还没调整过时按设置给出初值。
func (m *Manager) ocrPlanNow() ocrPlan {
	cores := m.physCores()
	cfg := m.Config()
	m.ocr.mu.Lock()
	defer m.ocr.mu.Unlock()
	if m.ocr.plan.procs == 0 {
		if cfg.OCRWorkers > 0 {
			m.ocr.plan = ocrManualPlan(cfg.OCRWorkers, cores)
		} else {
			m.ocr.plan = ocrDefaultPlan(max(2, cores/2))
		}
	}
	return m.ocr.plan
}

// resetOCRPlan：OCR 并发设置改了，重新开始。
func (m *Manager) resetOCRPlan() {
	m.ocr.mu.Lock()
	m.ocr.plan, m.ocr.tuner, m.ocr.note = ocrPlan{}, nil, ""
	m.ocr.mu.Unlock()
}

// othersBusy：普通文件或老格式转换还有文件在处理或排队。
func (m *Manager) othersBusy() bool {
	s := &m.sched
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range []int{prioFast, prioLegacy} {
		if l := s.lanes[p]; l != nil && (l.running > 0 || l.wantWork.Load()) {
			return true
		}
	}
	return false
}

// ocrSaturated：OCR 的进程都在忙，而且还有文件排队（这时的每分钟页数才能比较不同的搭配）。
func (m *Manager) ocrSaturated() bool {
	s := &m.sched
	l := s.lanes[prioOCR]
	if l == nil {
		return false
	}
	s.mu.Lock()
	full := l.target > 0 && l.running >= l.target
	s.mu.Unlock()
	return full && l.wantWork.Load()
}

func (m *Manager) ocrTuneLoop() {
	defer m.wg.Done()
	t := time.NewTicker(ocrTuneEvery)
	defer t.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
		}
		m.ocrTuneStep()
	}
}

func (m *Manager) ocrTuneStep() {
	cfg := m.Config()
	if !m.ocrAvailable() || !cfg.OCREnabled {
		return
	}
	cores := m.physCores()
	now := awake.Now()
	pages := m.ocr.pages.Load()
	busy := m.othersBusy()
	saturated := m.ocrSaturated() && !m.pausedAll.Load()

	m.ocr.mu.Lock()
	m.ocr.rate.add(now, pages)
	switch {
	case busy:
		m.ocr.othersIdle = 0
	case m.ocr.othersIdle == 0:
		m.ocr.othersIdle = now
	}
	budget := max(2, cores/2)
	if !busy && now-m.ocr.othersIdle >= othersIdleFor {
		budget = cores
	}
	var next ocrPlan
	var note string
	if cfg.OCRWorkers > 0 {
		next = ocrManualPlan(cfg.OCRWorkers, cores)
		m.ocr.note = ""
	} else {
		if m.ocr.tuner == nil {
			m.ocr.tuner = newOCRTuner()
		}
		next, _, note = m.ocr.tuner.step(budget, saturated, pages, now)
		m.ocr.note = m.ocr.tuner.describe()
	}
	changed := next != m.ocr.plan
	m.ocr.plan = next
	m.ocr.mu.Unlock()

	if note != "" {
		m.logf("%s", note)
	}
	if changed {
		logx.Debugf("OCR 改用 %s（线程预算 %d，物理核心 %d）", next, budget, cores)
		m.applyWorkers()
	}
}

// ocrPageRe 匹配 OCR 逐页的进度（extract 包里写的「第 i/n 页」：开始识别第 i 页，前 i-1 页已完成）。
var ocrPageRe = regexp.MustCompile(`^第 (\d+)/(\d+) 页$`)

// countOCRProgress 由进度消息累计已识别的页数。调用时持有 sched.mu。
func (m *Manager) countOCRProgress(j *job, p string) {
	mm := ocrPageRe.FindStringSubmatch(p)
	if mm == nil {
		return
	}
	i, _ := strconv.Atoi(mm[1])
	n, _ := strconv.Atoi(mm[2])
	if d := i - 1 - j.pagesDone; d > 0 {
		m.ocr.pages.Add(int64(d))
		j.pagesDone = i - 1
	}
	j.pagesTotal = n
}

// finishOCRPages：OCR 文件处理完，补上最后一页（单张图片算一页）。
func (m *Manager) finishOCRPages(j *job) {
	m.sched.mu.Lock()
	rest := 1
	if j.pagesTotal > 0 {
		rest = j.pagesTotal - j.pagesDone
	}
	j.pagesDone += rest
	m.sched.mu.Unlock()
	if rest > 0 {
		m.ocr.pages.Add(int64(rest))
	}
}

// ocrStatus 返回界面显示的每个进程线程数、最近每分钟页数和调整说明。
func (m *Manager) ocrStatus() (threads int, perMin float64, note string) {
	p := m.ocrPlanNow()
	m.ocr.mu.Lock()
	defer m.ocr.mu.Unlock()
	return p.threads, m.ocr.rate.perMin(), m.ocr.note
}
