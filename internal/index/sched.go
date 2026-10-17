package index

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/chenonestar/zhaozhao/internal/awake"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenonestar/zhaozhao/internal/doc"
	"github.com/chenonestar/zhaozhao/internal/extract"
	"github.com/chenonestar/zhaozhao/internal/logx"
	"github.com/chenonestar/zhaozhao/internal/norm"
	"github.com/chenonestar/zhaozhao/internal/store"
	"github.com/chenonestar/zhaozhao/internal/volume"
	"github.com/chenonestar/zhaozhao/internal/worker"
)

const (
	hashMaxSize = 256 << 20
	// 网络盘上的文件先复制到本机临时目录再提取：网络盘读取名额只在复制期间占用，
	// 之后耗时的格式转换、OCR 不再占着名额，网络盘也只读一遍（复制时顺带算校验值）。
	// 超过这个大小的文件不复制，直接在网络盘上提取（期间一直占用读取名额）。
	netCopyMax = 512 << 20
)

var (
	// stallAfter：有任务在处理或等待处理，却这么久没有任何进展（文件完成、进度报告），判定为疑似卡住。
	stallAfter = 10 * time.Minute
	// watchEvery 是看门狗检查的间隔。
	watchEvery = 30 * time.Second
)

// job 是一个已派发的提取任务。
type job struct {
	f        store.File
	path     string
	class    int           // prioFast / prioLegacy / prioOCR：决定提取方式和时限
	lane     *lane         // 所在通道
	queued   time.Time     // 进入「正在处理」的时间：界面按它排序，顺序不跳动
	since    time.Duration // 开始当前处理的时间（awake.Now，不含电脑睡眠；等待时为派发时间）
	stage    string
	progress string
	sent     bool // 结果已交给写入协程：写入完成后才从「处理中」移除，避免同一文件被再次派发

	pagesDone, pagesTotal int         // OCR：已识别的页数、总页数（由进度消息得到，用于实测每分钟页数）
	ctl                   *worker.Ctl // OCR 识别期间不为 nil：可以调整之后识别用的线程数（队列收尾时）
}

// lane 是一条处理通道。普通文件、老格式转换、OCR 各一条，各有自己的派发器和提取协程，三条同时进行：
// 老格式和 OCR 不用等全部普通文件处理完才开始。每条通道里先处理最近修改的文件。
type lane struct {
	prio   int
	name   string
	jobs   chan *job
	kick   chan struct{}
	resize chan struct{} // 并发数变化时关闭并换新，唤醒空闲协程检查是否该退出

	// 以下由 schedState.mu 保护
	n, target, running int
	durs               []time.Duration // 最近完成的单个文件耗时，估算剩余时间用

	wantWork atomic.Bool // 派发器手里有可处理的文件
}

// schedState 是提取调度的运行时状态。
//
// 并发控制（不会死锁的关键）：
//   - 每条通道的提取协程数就是它的并发上限：派发器只在有协程空闲时才交出任务，交出的任务立即开始处理；
//   - 网络盘读取名额只在把文件复制到本机期间占用，持有它时不再等待任何别的资源。
//
// 任何协程都不会持有一种资源去等待另一种被别人反向持有的资源，因此不会出现循环等待。
type schedState struct {
	mu       sync.Mutex
	inflight map[int64]*job
	lanes    [4]*lane // 下标是 prioFast、prioLegacy、prioOCR
	write    chan *prepared
	net      *sem

	done         atomic.Int64
	doneByPri    [4]atomic.Int64
	startedAt    time.Time
	idle         atomic.Bool
	lastActivity atomic.Int64 // 最近一次进展的时间（awake.Now，不含电脑睡眠）
	stallLogged  time.Time
}

var lanePrios = []int{prioFast, prioLegacy, prioOCR}

func (s *schedState) eachLane(f func(l *lane)) {
	for _, p := range lanePrios {
		if l := s.lanes[p]; l != nil {
			f(l)
		}
	}
}

// prepared 是已提取完毕、等待写入数据库的结果。
type prepared struct {
	file     store.File
	res      doc.Result
	state    int
	text     *store.Text
	fts      string
	nameFTS  string
	reuse    int64           // 复用该文件的提取结果（内容相同）
	recycled *store.Recycled // 复用回收区里刚删除的文件的提取结果（文件被移动、改名）
	requeue  int             // 非 0 时保存结果后把文件重新排到这个队列（OCR、老格式转换）
	children []*preparedChild
}

type preparedChild struct {
	file    store.File
	state   int
	err     string
	text    *store.Text
	fts     string
	nameFTS string
	meta    string
	ocr     bool
}

func (m *Manager) startScheduler() {
	s := &m.sched
	cfg := m.Config()
	s.inflight = map[int64]*job{}
	s.write = make(chan *prepared, 256)
	s.net = newSem(cfg.netConc())
	for _, p := range lanePrios {
		s.lanes[p] = &lane{prio: p, name: classNames[p], jobs: make(chan *job), kick: make(chan struct{}, 1), resize: make(chan struct{})}
	}
	s.startedAt = time.Now()
	s.lastActivity.Store(int64(awake.Now()))
	m.applyWorkers()
	m.wg.Add(2)
	go m.writer()
	go m.watchdog()
	s.eachLane(func(l *lane) {
		m.wg.Add(1)
		go m.laneDispatcher(l)
	})
}

// laneSizes 返回三条通道各自的并发数。OCR 的进程数由 ocrPlanNow 决定（自动时实测选择）。
func (m *Manager) laneSizes(c Config) map[int]int {
	return map[int]int{prioFast: c.workers(), prioLegacy: c.legacyWorkers(), prioOCR: m.ocrPlanNow().procs}
}

// applyWorkers 按当前设置调整各通道并发数、进程池大小和网络盘读取名额（设置修改后立即生效，不用重启）。
func (m *Manager) applyWorkers() {
	s := &m.sched
	if s.lanes[prioFast] == nil {
		return
	}
	cfg := m.Config()
	sizes := m.laneSizes(cfg)
	s.net.SetCap(cfg.netConc())
	// 每条通道用自己的进程池；没有单独的池（测试）时共用主池
	main := sizes[prioFast]
	if m.LegacyPool != nil {
		m.LegacyPool.SetSize(sizes[prioLegacy])
	} else {
		main += sizes[prioLegacy]
	}
	if m.OCRPool != nil {
		m.OCRPool.SetSize(sizes[prioOCR])
	} else {
		main += sizes[prioOCR]
	}
	m.Pool.SetSize(main)
	s.mu.Lock()
	s.eachLane(func(l *lane) {
		l.target = sizes[l.prio]
		for l.n < l.target && m.ctx.Err() == nil {
			l.n++
			m.wg.Add(1)
			go m.laneWorker(l)
		}
		close(l.resize)
		l.resize = make(chan struct{})
	})
	s.mu.Unlock()
	logx.Debugf("并发设置：普通文件 %d，老格式转换 %d，OCR %s，网络盘读取 %d（三类同时进行）", sizes[prioFast], sizes[prioLegacy], m.ocrPlanNow(), cfg.netConc())
	m.kick()
}

func (m *Manager) offlineVolumes() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []string
	for id, v := range m.vols {
		if !v.online {
			out = append(out, id)
		}
	}
	return out
}

func (m *Manager) inflightIDs() []int64 {
	s := &m.sched
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]int64, 0, len(s.inflight))
	for id := range s.inflight {
		ids = append(ids, id)
	}
	return ids
}

// classOf 决定文件的提取方式：OCR、老格式转换（LibreOffice），或普通提取。
// 老格式文件先在普通通道用内置解析器读取，处理不了的才排到老格式转换队列（prio=prioLegacy）。
func classOf(f store.File, loOnly bool) int {
	switch {
	case f.Prio == prioOCR:
		return prioOCR
	case f.Prio == prioLegacy, loOnly && extract.IsLegacy(f.Ext):
		return prioLegacy
	}
	return prioFast
}

// updateIdle 在没有任何通道有活、也没有文件在处理时标记空闲。
func (m *Manager) updateIdle() {
	s := &m.sched
	busy := false
	s.eachLane(func(l *lane) { busy = busy || l.wantWork.Load() })
	s.mu.Lock()
	busy = busy || len(s.inflight) > 0
	s.mu.Unlock()
	s.idle.Store(!busy)
}

// laneDispatcher 为一条通道取出待处理的文件（最近修改的在前），交给该通道空闲的提取协程。
func (m *Manager) laneDispatcher(l *lane) {
	defer m.wg.Done()
	s := &m.sched
	for {
		if m.ctx.Err() != nil {
			return
		}
		if m.pausedAll.Load() {
			l.wantWork.Store(false)
			s.idle.Store(false)
			m.waitLane(l, time.Second)
			continue
		}
		if l.prio != prioFast && m.Config().PauseOnBattery && onBattery() {
			l.wantWork.Store(false) // 使用电池时暂停 OCR 和格式转换（F-BG-06）
			m.updateIdle()
			m.waitLane(l, 5*time.Second)
			continue
		}
		if l.prio != prioOCR && m.ocrBenching.Load() {
			s.idle.Store(false) // 正在测试 OCR 速度：暂停派发（约 1 分钟）
			m.waitLane(l, time.Second)
			continue
		}
		if l.prio == prioOCR && m.ocrBenchAsked() {
			m.ocrBench() // 用户要求重新测速：没有要识别的文件也测
			continue
		}
		files, err := m.DB.NextPending(8, []int{l.prio}, m.offlineVolumes(), m.inflightIDs(), l.prio == prioOCR)
		if err != nil {
			m.logf("读取待处理队列失败: %v", err)
			m.waitLane(l, 2*time.Second)
			continue
		}
		if len(files) == 0 {
			l.wantWork.Store(false)
			m.updateIdle()
			m.waitLane(l, 3*time.Second)
			continue
		}
		if l.prio == prioOCR && m.ocrBenchDue() {
			m.ocrBench() // 本机还没测过 OCR 速度：先测，再按结果开进程
			continue
		}
		l.wantWork.Store(true)
		s.idle.Store(false)
		loOnly := m.Config().LegacyLOOnly
		for _, f := range files {
			if l.prio != prioOCR && m.ocrBenching.Load() {
				break // 开始测试 OCR 速度了：剩下的等测完再派发
			}
			now := time.Now()
			j := &job{f: f, class: classOf(f, loOnly), lane: l, stage: "等待空闲的提取线程", queued: now, since: awake.Now()}
			j.path, _ = m.AbsPath(f.VolumeID, f.RelPath)
			s.mu.Lock()
			s.inflight[f.ID] = j
			s.mu.Unlock()
			select {
			case l.jobs <- j:
			case <-m.ctx.Done():
				return
			}
		}
	}
}

func (m *Manager) waitLane(l *lane, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-l.kick:
	case <-t.C:
	case <-m.ctx.Done():
	}
}

func (m *Manager) laneWorker(l *lane) {
	defer m.wg.Done()
	s := &m.sched
	for {
		s.mu.Lock()
		if l.n > l.target {
			l.n--
			s.mu.Unlock()
			return
		}
		rs := l.resize
		s.mu.Unlock()
		var j *job
		select {
		case j = <-l.jobs:
		case <-rs:
			continue
		case <-m.ctx.Done():
			return
		}
		s.mu.Lock()
		l.running++
		j.since = awake.Now()
		s.mu.Unlock()
		m.activity()
		m.runJob(j)
		d := awake.Since(j.since) // 不含电脑睡眠的时间，剩余时间的估算才不会被拉长
		s.mu.Lock()
		l.running--
		if j.sent {
			l.durs = append(l.durs, d)
			if len(l.durs) > 50 {
				l.durs = l.durs[len(l.durs)-50:]
			}
		} else {
			delete(s.inflight, j.f.ID)
		}
		s.mu.Unlock()
		m.kick()
		throttleForUser(m.ctx)
	}
}

// laneEta 估算一条通道把 queue 个文件处理完还要多久：最近单个文件的平均耗时 × 数量 ÷ 并发数。
// 还没有处理过文件时无法估算，返回 0。
func (m *Manager) laneEta(p int, queue int64) int64 {
	s := &m.sched
	l := s.lanes[p]
	if l == nil || queue <= 0 {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(l.durs) < 3 || l.target < 1 {
		return 0
	}
	var sum time.Duration
	for _, d := range l.durs {
		sum += d
	}
	avg := sum.Seconds() / float64(len(l.durs))
	return int64(avg * float64(queue) / float64(l.target))
}

func (m *Manager) laneStat(p int) SlotStatus {
	s := &m.sched
	l := s.lanes[p]
	if l == nil {
		return SlotStatus{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return SlotStatus{Used: l.running, Cap: l.target}
}

func (m *Manager) activity() {
	m.sched.lastActivity.Store(int64(awake.Now()))
	if !m.keepingAwake.Load() {
		m.pokeAwake()
	}
}

func (m *Manager) setStage(j *job, stage string) {
	m.sched.mu.Lock()
	j.stage, j.progress = stage, ""
	m.sched.mu.Unlock()
}

func (m *Manager) setProgress(j *job, p string) {
	m.sched.mu.Lock()
	j.progress = p
	if j.class == prioOCR {
		m.countOCRProgress(j, p)
	}
	m.sched.mu.Unlock()
	m.activity()
}

func hashFile(p string, size int64) string {
	if size > hashMaxSize {
		return ""
	}
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha1.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ctxReader 在每次读取前检查是否已取消（退出程序时尽快停止复制）。
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// progressWriter 每复制 8 MB 报告一次进度（大文件复制较久，不能被误判为卡住）。
type progressWriter struct {
	n, last int64
	report  func(int64)
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.n += int64(len(b))
	if p.report != nil && p.n-p.last >= 8<<20 {
		p.last = p.n
		p.report(p.n)
	}
	return len(b), nil
}

// copyLocal 把网络盘上的文件复制到本机临时目录，复制时顺带计算校验值（wantHash）。
// 返回本机副本路径；调用方负责删除其所在目录。
func copyLocal(ctx context.Context, src string, size int64, wantHash bool, report func(int64)) (string, string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", "", err
	}
	defer in.Close()
	dir, err := os.MkdirTemp("", "zhaozhao-net-")
	if err != nil {
		return "", "", err
	}
	dst := filepath.Join(dir, filepath.Base(src))
	out, err := os.Create(dst)
	if err != nil {
		os.RemoveAll(dir)
		return "", "", err
	}
	h := sha1.New()
	ws := []io.Writer{out, &progressWriter{report: report}}
	hash := wantHash && size <= hashMaxSize
	if hash {
		ws = append(ws, h)
	}
	w := io.MultiWriter(ws...)
	_, err = io.CopyBuffer(w, ctxReader{ctx, in}, make([]byte, 1<<20))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.RemoveAll(dir)
		return "", "", err
	}
	sum := ""
	if hash {
		sum = hex.EncodeToString(h.Sum(nil))
	}
	return dst, sum, nil
}

func (m *Manager) extractOptions(cfg Config, withOCR bool) extract.Options {
	o := extract.DefaultOptions()
	if withOCR {
		o.OCR = m.ocrCfg
		o.OCR.Threads = m.ocrPlanNow().threads // 每个 OCR 进程的线程数：总数不超过 CPU 核心数
	}
	o.MaxTextBytes = int64(cfg.MaxTextMB) << 20
	o.SniffUnknown = cfg.SniffUnknown
	if cfg.TimeoutSec > 0 {
		o.Timeout = time.Duration(cfg.TimeoutSec) * time.Second
	}
	if cfg.ContainerMaxDepth > 0 {
		o.MaxDepth = cfg.ContainerMaxDepth
	}
	if cfg.ArchiveMaxMB > 0 {
		o.ArchiveMaxBytes = int64(cfg.ArchiveMaxMB) << 20
	}
	if cfg.ArchiveMaxRatio > 0 {
		o.ArchiveMaxRatio = cfg.ArchiveMaxRatio
	}
	return o
}

var classNames = map[int]string{prioFast: "普通", prioLegacy: "老格式", prioOCR: "OCR"}

var statusNames = map[string]string{doc.StatusOK: "成功", doc.StatusFailed: "失败", doc.StatusEncrypted: "已加密",
	doc.StatusNoContent: "没有文字内容", doc.StatusSkipped: "跳过"}

// KindName 返回磁盘类型的中文名称。
func KindName(kind string) string {
	switch kind {
	case volume.KindLocal:
		return "本地盘"
	case volume.KindRemovable:
		return "移动盘"
	case volume.KindNetwork:
		return "网络盘"
	}
	return kind
}

// runJob 提取一个文件并把结果交给写入协程。
func (m *Manager) runJob(j *job) {
	f := j.f
	s := &m.sched
	abs, online := m.AbsPath(f.VolumeID, f.RelPath)
	if !online {
		logx.Debugf("暂缓 %s：所在磁盘离线，接入后继续", logx.P(abs))
		return // 卷离线：保持待处理，接入后继续
	}
	s.mu.Lock() // 状态页在锁里读 j.path
	j.path = abs
	s.mu.Unlock()
	info, _, _, _ := m.volSnapshot(f.VolumeID)
	isNet := info.Kind == volume.KindNetwork
	start := time.Now()
	var netWait, readDur time.Duration

	m.setStage(j, "读取文件信息")
	fi, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			logx.Debugf("文件已不存在，移除记录 %s", logx.P(abs))
			_ = m.DB.DeleteFiles([]int64{f.ID})
			return
		}
		res := doc.Result{Status: doc.StatusFailed, Err: "无权限访问或无法读取: " + err.Error()}
		m.logJob(j, res, start, 0, 0)
		m.sendJob(j, &prepared{file: f, res: res})
		return
	}
	if size, mtime := fi.Size(), fi.ModTime().Unix(); size != f.Size || mtime != f.MTime {
		// 与扫描时记录的不一致（网络盘的目录缓存、扫描时文件还在复制等）：以现在读到的为准。
		// 否则写入时会把结果当成「提取期间文件又被修改」丢弃，文件永远停在待处理、反复提取。
		logx.Debugf("%s 的大小/修改时间与扫描记录不同（扫描 %d/%d，现在 %d/%d），以现在为准", logx.P(abs), f.Size, f.MTime, size, mtime)
		if err := m.DB.SetSig(f.ID, size, mtime); err != nil {
			m.logf("更新 %s 的文件信息失败: %v", logx.P(abs), err)
		}
		f.Size, f.MTime = size, mtime
		j.f = f
	}
	cfg := m.Config()
	container := extract.IsContainer(f.Ext)
	src, hash := abs, ""
	switch {
	case isNet && f.Size <= netCopyMax:
		m.setStage(j, "等待读取网络盘")
		t0 := time.Now()
		if !s.net.Acquire(m.ctx) {
			return
		}
		netWait = time.Since(t0)
		m.setStage(j, "从网络盘读取")
		t1 := time.Now()
		local, sum, err := copyLocal(m.ctx, abs, f.Size, !container, func(n int64) {
			m.setProgress(j, fmt.Sprintf("已读取 %s / %s", fmtBytes(n), fmtBytes(f.Size)))
		})
		s.net.Release()
		readDur = time.Since(t1)
		if m.ctx.Err() != nil {
			return
		}
		if err != nil {
			res := doc.Result{Status: doc.StatusFailed, Err: "无法从网络盘读取: " + err.Error()}
			m.logJob(j, res, start, netWait, readDur)
			m.sendJob(j, m.prepare(f, res))
			return
		}
		defer os.RemoveAll(filepath.Dir(local))
		src, hash = local, sum
	default:
		if isNet {
			// 太大的文件不复制：直接在网络盘上提取，期间一直占用读取名额
			m.setStage(j, "等待读取网络盘")
			t0 := time.Now()
			if !s.net.Acquire(m.ctx) {
				return
			}
			netWait = time.Since(t0)
			defer s.net.Release()
		}
		if !container {
			m.setStage(j, "计算校验值")
			hash = hashFile(abs, f.Size)
		}
	}
	if hash != "" {
		if dup, _ := m.DB.FindDuplicate(hash, f.Size, f.ID); dup != 0 {
			if p := m.prepareReuse(f, dup, hash); p != nil {
				logx.Debugf("内容与已索引的文件相同，复用结果 %s", logx.P(abs))
				m.sendJob(j, p)
				return
			}
		}
		// 刚删除的文件里有内容相同的（文件被移动、改名）：复用它的提取结果，不必重新提取、重新 OCR
		if r, _ := m.DB.FindRecycled(hash, f.Size); r != nil {
			if p := m.prepareRecycled(f, r, hash); p != nil {
				logx.Debugf("内容与刚删除的文件相同（移动或改名），复用结果 %s", logx.P(abs))
				m.sendJob(j, p)
				return
			}
		}
	}

	useOCR := j.class == prioOCR
	opts := m.extractOptions(cfg, useOCR)
	opts.LegacyMode = extract.LegacyNative // 内置解析不了的老格式文件返回 NeedConvert，排到老格式转换队列
	pool := m.Pool
	t := worker.Timeouts{Total: opts.Timeout + 15*time.Second}
	stage := "提取内容"
	switch {
	case useOCR:
		// OCR 逐页报告进度：10 分钟没有任何进展才算超时，整份文件最多 6 小时
		stage = "OCR 识别"
		t = worker.Timeouts{Idle: 10 * time.Minute, Total: 6 * time.Hour}
		if m.OCRPool != nil {
			pool = m.OCRPool
		}
	case j.class == prioLegacy:
		stage = "格式转换"
		opts.LegacyMode = extract.LegacyLO
		t.Total = extract.LegacyTimeout(opts.Timeout) + 30*time.Second
		if m.LegacyPool != nil {
			pool = m.LegacyPool
		}
	case container:
		// 压缩包、邮箱逐项报告进度
		stage = "解开压缩包/邮件"
		t = worker.Timeouts{Idle: 10 * time.Minute, Total: time.Hour}
	}
	m.setStage(j, stage)
	var ctl *worker.Ctl
	if useOCR {
		m.ocrTailOff()
		ctl = &worker.Ctl{}
		s.mu.Lock()
		j.ctl = ctl
		s.mu.Unlock()
	}
	res := pool.RunCtl(m.ctx, src, f.Ext, opts, t, func(p string) { m.setProgress(j, p) }, ctl)
	if useOCR {
		s.mu.Lock()
		j.ctl = nil
		s.mu.Unlock()
	}
	if m.ctx.Err() != nil {
		return
	}
	if useOCR {
		m.finishOCRPages(j)
	}
	if src != abs && res.Err != "" {
		res.Err = strings.ReplaceAll(res.Err, src, abs) // 错误信息里不出现临时副本的路径
	}
	if res.Hash == "" {
		res.Hash = hash
	}
	m.logJob(j, res, start, netWait, readDur)
	m.sendJob(j, m.prepare(f, res))
}

// logJob 记录一个文件的处理结果：失败总是记录，成功只在详细日志里记录。
func (m *Manager) logJob(j *job, res doc.Result, start time.Time, netWait, readDur time.Duration) {
	if res.NeedConvert {
		logx.Debugf("%s：%s，排到老格式转换队列（LibreOffice）", logx.P(j.path), res.Err)
		return
	}
	failed := res.Status == doc.StatusFailed
	if !failed && !logx.Verbose() {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s [%s] 结果 %s，大小 %s，用时 %.1fs", map[bool]string{true: "失败", false: "完成"}[failed],
		logx.P(j.path), classNames[j.class], statusNames[res.Status], fmtBytes(j.f.Size), time.Since(start).Seconds())
	if netWait > 0 || readDur > 0 {
		fmt.Fprintf(&b, "（等待网络盘 %.1fs，读取 %.1fs）", netWait.Seconds(), readDur.Seconds())
	}
	m.sched.mu.Lock()
	if j.progress != "" {
		fmt.Fprintf(&b, "，最后进度 %s", j.progress)
	}
	m.sched.mu.Unlock()
	if res.Err != "" {
		fmt.Fprintf(&b, "，%s", res.Err)
	}
	if failed {
		m.logf("%s", b.String())
	} else {
		logx.Debugf("%s", b.String())
	}
}

func fmtBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// textSize 估算写入这个结果要处理的文字量（字节）。
func (p *prepared) textSize() int {
	n := len(p.fts)
	if p.text != nil {
		n += len(p.text.Text)
	}
	for _, c := range p.children {
		n += len(c.fts)
		if c.text != nil {
			n += len(c.text.Text)
		}
	}
	return n
}

// sendJob 把任务结果交给写入协程。写入完成后才把文件从「处理中」移除。
func (m *Manager) sendJob(j *job, p *prepared) {
	m.setStage(j, "写入索引")
	j.sent = true
	m.send(p)
}

func (m *Manager) send(p *prepared) {
	select {
	case m.sched.write <- p:
	case <-m.ctx.Done():
	}
}

func stateOf(r doc.Result) int {
	switch r.Status {
	case doc.StatusOK:
		return store.StateDone
	case doc.StatusEncrypted:
		return store.StateEncrypted
	case doc.StatusFailed:
		return store.StateFailed
	}
	return store.StateMetaOnly
}

func ftsOf(o norm.Options, text string) string { return o.FTS(text) }

func (m *Manager) prepare(f store.File, res doc.Result) *prepared {
	o := m.Norm()
	p := &prepared{file: f, res: res, state: stateOf(res), nameFTS: ftsOf(o, f.Name)}
	if res.NeedOCR && f.Prio != prioOCR && m.ocrEnabled(m.getRoot(f.RootID)) {
		p.requeue = prioOCR // 有页面没有文字层：先保存已有文字，再排到 OCR 队列
	}
	if res.NeedConvert && f.Prio != prioLegacy {
		p.requeue = prioLegacy // 内置解析器处理不了的老格式文件：排到老格式转换队列
		return p
	}
	if res.Status == doc.StatusOK || len(res.Children) > 0 {
		p.text = &store.Text{Text: res.Text, LineBased: res.LineBased, Locs: res.Locs}
		m.DB.Encode(p.text) // 在提取线程里压缩（可并行），写入线程只管写
		p.fts = ftsOf(o, res.Text)
		if len(res.Children) > 0 && (res.Status == doc.StatusNoContent || res.Status == doc.StatusSkipped) {
			p.state = store.StateDone
		}
	}
	for _, c := range res.Children {
		name := c.Inner
		if i := strings.LastIndex(name, " › "); i >= 0 {
			name = name[i+len(" › "):]
		}
		name = path.Base(strings.ReplaceAll(name, `\`, "/"))
		ext := extract.Ext(name)
		cf := store.File{RootID: f.RootID, VolumeID: f.VolumeID, RelPath: f.RelPath, Inner: c.Inner, Name: name, Ext: ext,
			Kind: extract.KindOf(ext), Size: c.Size, MTime: c.MTime, Prio: 1}
		if c.Meta != "" {
			cf.Kind = extract.KindMail
		}
		pc := &preparedChild{file: cf, state: stateOf(c.Result), err: c.Err, meta: c.Meta, ocr: c.OCR, nameFTS: ftsOf(o, name)}
		if cf.MTime == 0 {
			cf.MTime = f.MTime
			pc.file.MTime = f.MTime
		}
		if c.Status == doc.StatusOK {
			pc.text = &store.Text{Text: c.Text, LineBased: c.LineBased, Locs: c.Locs}
			m.DB.Encode(pc.text)
			pc.fts = ftsOf(o, c.Text)
		}
		p.children = append(p.children, pc)
	}
	return p
}

// prepareRecycled 复用回收区里的文本（文件被移动、改名，内容没变）。
func (m *Manager) prepareRecycled(f store.File, r *store.Recycled, hash string) *prepared {
	t, err := m.DB.GetTextByID(r.TextID)
	if err != nil || t == nil {
		return nil
	}
	o := m.Norm()
	return &prepared{file: f, state: store.StateDone, recycled: r, text: t, fts: ftsOf(o, t.Text), nameFTS: ftsOf(o, f.Name),
		res: doc.Result{Status: doc.StatusOK, Hash: hash, OCR: r.OCR}}
}

// prepareReuse 复用内容相同文件的提取结果，不再重新解析（F-BG-03）。
func (m *Manager) prepareReuse(f store.File, src int64, hash string) *prepared {
	t, err := m.DB.GetText(src)
	if err != nil || t == nil {
		return nil
	}
	o := m.Norm()
	srcFile, _ := m.DB.GetFile(src)
	p := &prepared{file: f, state: store.StateDone, reuse: src, text: t, fts: ftsOf(o, t.Text), nameFTS: ftsOf(o, f.Name),
		res: doc.Result{Status: doc.StatusOK, Hash: hash}}
	if srcFile != nil {
		p.res.OCR = srcFile.OCR
	}
	return p
}

// walCheckEvery：写入期间每隔这么久看一次 WAL 有没有超过上限。
const walCheckEvery = 10 * time.Second

func (m *Manager) writer() {
	defer m.wg.Done()
	s := &m.sched
	var walChecked time.Time
	for {
		var first *prepared
		select {
		case first = <-s.write:
		case <-m.ctx.Done():
			return
		}
		// 一批最多 64 个文件、约 8 MB 文字：事务短，其他写操作（加入索引根、扫描）最多等一秒左右
		batch := []*prepared{first}
		size := first.textSize()
		timeout := time.After(150 * time.Millisecond)
	collect:
		for len(batch) < 64 && size < 8<<20 {
			select {
			case p := <-s.write:
				batch = append(batch, p)
				size += p.textSize()
			case <-timeout:
				break collect
			case <-m.ctx.Done():
				break collect
			}
		}
		err := m.DB.Tx(func(tx *sql.Tx) error {
			for _, p := range batch {
				if err := m.writeOne(tx, p); err != nil {
					return fmt.Errorf("%s: %w", p.file.Name, err)
				}
			}
			return nil
		})
		if err != nil {
			m.logf("批量写入失败，改为逐个写入: %v", err)
			for _, p := range batch {
				if err := m.DB.Tx(func(tx *sql.Tx) error { return m.writeOne(tx, p) }); err != nil {
					m.logf("写入 %s 失败: %v", logx.P(p.file.RelPath), err)
				}
			}
		}
		s.mu.Lock()
		for _, p := range batch {
			delete(s.inflight, p.file.ID) // 已写入：之后才允许再次派发同一文件
			s.done.Add(1)
			if p.file.Prio >= 1 && p.file.Prio <= 3 {
				s.doneByPri[p.file.Prio].Add(1)
			}
		}
		s.mu.Unlock()
		m.activity()
		if time.Since(walChecked) >= walCheckEvery {
			walChecked = time.Now()
			m.checkpointWAL()
		}
	}
}

// checkpointWAL 在 WAL 超过上限时主动写回主文件并截短（一直有人在搜索时自动检查点追不上）。
func (m *Manager) checkpointWAL() {
	t0 := time.Now()
	did, busy, err := m.DB.CheckpointIfLarge(store.WALLimit, 3*time.Second)
	switch {
	case err != nil:
		logx.Debugf("WAL 检查点失败: %v", err)
	case busy:
		logx.Debugf("WAL 超过 %s，有搜索没读完，稍后再写回", fmtBytes(store.WALLimit))
	case did:
		logx.Debugf("WAL 超过 %s，已写回主文件并截短，用时 %v", fmtBytes(store.WALLimit), time.Since(t0).Round(time.Millisecond))
	}
}

func (m *Manager) writeOne(tx *sql.Tx, p *prepared) error {
	f := p.file
	// 文件在提取期间被删除或再次修改时丢弃本次结果，避免写入过期内容
	var size, mtime int64
	if err := tx.QueryRow(`SELECT size,mtime FROM files WHERE id=?`, f.ID).Scan(&size, &mtime); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	if size != f.Size || mtime != f.MTime {
		logx.Debugf("丢弃 %s 的提取结果：提取期间文件又被修改（将重新提取）", logx.P(f.RelPath))
		return nil
	}
	if p.recycled != nil {
		err := store.AttachText(tx, f.ID, p.recycled, p.res.Hash, p.fts, p.nameFTS)
		if err == nil {
			return nil
		}
		logx.Debugf("复用回收区的文本失败（%v），重新提取 %s", err, logx.P(f.RelPath))
		_, err = tx.Exec(`UPDATE files SET state=0 WHERE id=?`, f.ID)
		return err
	}
	if p.reuse != 0 {
		if err := store.CopyResult(tx, p.reuse, f.ID, p.fts, p.nameFTS); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE files SET hash=? WHERE id=?`, p.res.Hash, f.ID)
		return err
	}
	if len(p.children) > 0 || extract.IsContainer(f.Ext) {
		if err := store.ReplaceChildren(tx, f.VolumeID, f.RelPath); err != nil {
			return err
		}
	}
	if err := m.DB.PutResult(tx, f.ID, p.state, p.res.Err, p.res.Hash, p.res.OCR, p.text, p.fts, p.nameFTS); err != nil {
		return err
	}
	if p.requeue != 0 {
		if _, err := tx.Exec(`UPDATE files SET state=?, err='', prio=? WHERE id=?`, store.StatePending, p.requeue, f.ID); err != nil {
			return err
		}
	}
	if p.res.Meta != "" {
		if _, err := tx.Exec(`UPDATE files SET meta=?, kind='mail' WHERE id=?`, p.res.Meta, f.ID); err != nil {
			return err
		}
	}
	for _, c := range p.children {
		cf := c.file
		cf.State = store.StatePending
		if err := store.UpsertFile(tx, &cf, c.nameFTS); errors.Is(err, store.ErrRootGone) {
			return nil
		} else if err != nil {
			return err
		}
		if err := m.DB.PutResult(tx, cf.ID, c.state, c.err, "", c.ocr, c.text, c.fts, c.nameFTS); err != nil {
			return err
		}
		if c.meta != "" {
			if _, err := tx.Exec(`UPDATE files SET meta=? WHERE id=?`, c.meta, cf.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// rebuildFTS 清空并重建全文索引（繁简归一设置变化后需要）。旧索引在重建期间不可靠，
// 状态面板会显示「正在重建」。
func (m *Manager) rebuildFTS() {
	if !m.rebuilding.CompareAndSwap(false, true) {
		return
	}
	defer m.rebuilding.Store(false)
	if err := m.DB.ClearFTS(); err != nil {
		m.logf("重建全文索引失败: %v", err)
		return
	}
	o := m.Norm()
	var last int64
	for m.ctx.Err() == nil {
		rows, err := m.DB.R.Query(`SELECT id,name FROM files WHERE id>? ORDER BY id LIMIT 200`, last)
		if err != nil {
			m.logf("重建全文索引失败: %v", err)
			return
		}
		type item struct {
			id   int64
			name string
		}
		var items []item
		for rows.Next() {
			var it item
			if rows.Scan(&it.id, &it.name) == nil {
				items = append(items, it)
			}
		}
		rows.Close()
		if len(items) == 0 {
			return
		}
		last = items[len(items)-1].id
		type row struct {
			id          int64
			content, nm string
		}
		var out []row
		for _, it := range items {
			c := ""
			if t, _ := m.DB.GetText(it.id); t != nil {
				c = ftsOf(o, t.Text)
			}
			out = append(out, row{it.id, c, ftsOf(o, it.name)})
		}
		_ = m.DB.Tx(func(tx *sql.Tx) error {
			for _, r := range out {
				if _, err := tx.Exec(`INSERT INTO fts(rowid,c,n) VALUES(?,?,?)`, r.id, r.content, r.nm); err != nil {
					return err
				}
			}
			return nil
		})
	}
}

var _ = json.Marshal
