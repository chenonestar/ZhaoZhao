package index

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenonestar/zhaozhao/internal/awake"
	"github.com/chenonestar/zhaozhao/internal/extract"
	"github.com/chenonestar/zhaozhao/internal/logx"
	"github.com/chenonestar/zhaozhao/internal/sysinfo"
	"github.com/chenonestar/zhaozhao/internal/worker"
)

// ocrState 是 OCR 进程数 × 线程数的当前搭配、本机的测速结果和最近的速度（见 ocrtune.go、ocrbench.go）。
type ocrState struct {
	mu    sync.Mutex
	plan  ocrPlan
	rate  rateWindow
	cores int          // 物理核心数
	pages atomic.Int64 // OCR 通道累计识别的页数

	machine   sysinfo.Machine
	key       string        // 本机连同 OCR 组件的指纹
	keyFor    string        // 算指纹时的 OCR 组件（组件变了要重新算、重新读测速结果）
	bench     *OCRBench     // 本机的测速结果；nil 表示还没测
	benchNote string        // 测速进度（界面显示）
	benchFail string        // 本次运行测速失败的原因：不再自动重试，下次启动再测
	benchAsk  bool          // 用户要求重新测速
	memCap    int           // 内存允许的 OCR 进程数（0 表示还没算）
	memDown   time.Duration // 内存允许的进程数上次减少的时间（awake.Now）
	waitScan  time.Duration // 开始等扫描完成再测速的时间（awake.Now；0 表示没在等）
	tail      int           // 队列收尾时每个文件的线程数（0 表示没有在收尾）
	tailSeen  int           // 连续几次看到队列已空、有进程空闲
	userBusy  bool          // 开了「用电脑时放慢」、用户正在用电脑：OCR 只用一半核心
	// 识别完的 PDF 的总大小、识别的总页数：估算队列里的 PDF 有多少页（剩余时间按页数算）
	pdfBytes, pdfPages int64
}

// ocrTuneEvery：多久调整一次。
var ocrTuneEvery = 30 * time.Second

// userActiveFor：设置里开了「用电脑时放慢」时，这么久之内用过键盘、鼠标就算用户正在用电脑，
// OCR 只用一半核心；离开这么久后再用全部核心。默认不开：OCR 进程本来就是低优先级，用户的程序优先；
// 0.2.4 默认开着，用户白天一直在用电脑，OCR 有 3.6 小时只用一半核心，建索引多花了约 1.9 小时。
const userActiveFor = 2 * time.Minute

// newOCRBenchRunner 返回实测一种搭配的函数（测试里换成模拟的）。
var newOCRBenchRunner = func(m *Manager, pool *worker.Pool, sample string) ocrBenchRunner {
	opts := m.extractOptions(m.Config(), true)
	opts.OCR = m.ocrCfg // 认字模型由测速指定
	return poolBench(pool, opts, sample)
}

// ocrModel 返回现在用的认字模型：设置里指定的（组件里有的话）；自动时用新模型的普通版（测速时它在这台电脑上
// 用不了的话用组件自带的）。量化版、Windows 自带的 OCR 只在设置里指定时用。
func (m *Manager) ocrModel() string {
	if want := m.Config().OCRModel; want != "" && m.ocrCfg.HasModel(want) {
		return want
	}
	m.ocr.mu.Lock()
	b := m.ocrBenchLocked()
	m.ocr.mu.Unlock()
	if b != nil && b.Model != "" && b.Model != extract.OCRModelV6Q && b.Model != extract.OCRModelWin && m.ocrCfg.HasModel(b.Model) {
		return b.Model
	}
	if ms := m.ocrCfg.Models(); len(ms) > 0 {
		return ms[0]
	}
	return extract.OCRModelV4
}

// ocrMachine 返回本机的处理器、核心数、内存。
func (m *Manager) ocrMachine() sysinfo.Machine {
	m.ocr.mu.Lock()
	defer m.ocr.mu.Unlock()
	if m.ocr.machine.Cores == 0 {
		m.ocr.machine = sysinfo.This()
	}
	return m.ocr.machine
}

func (m *Manager) physCores() int {
	m.ocr.mu.Lock()
	defer m.ocr.mu.Unlock()
	if m.ocr.cores == 0 {
		m.ocr.cores = sysinfo.PhysicalCores()
	}
	return m.ocr.cores
}

// ocrBudget 返回 OCR 的线程预算：全部物理核心；half（设置里开了「用电脑时放慢」、用户正在用电脑）时用一半。
// 实测超线程更快时用全部逻辑处理器（见 autoPlanLocked）。
//
// 普通文件、老格式还在处理时 OCR 也用全部核心：OCR 进程是最低优先级，解析、转换进程先用 CPU，OCR 只用剩下的。
// 0.2.5 以前这时只给 OCR 一半核心，可普通文件阶段多半在等磁盘、等写入，CPU 大多空着：某位用户的普通文件
// 处理了 83 分钟，这期间 OCR 每分钟只识别 16 页（只剩 OCR 时每分钟 60 多页）。
func ocrBudget(cores int, half bool) int {
	if !half {
		return cores
	}
	return min(cores, max(2, cores/2))
}

// ocrBenchLocked 返回本机的测速结果（第一次调用或 OCR 组件变了时从索引库读出）。调用时持有 m.ocr.mu。
func (m *Manager) ocrBenchLocked() *OCRBench {
	comp := m.ocrCfg.Command + "\x00" + strings.Join(m.ocrCfg.Args, "\x00")
	if m.ocr.key == "" || m.ocr.keyFor != comp {
		if m.ocr.machine.Cores == 0 {
			m.ocr.machine = sysinfo.This()
		}
		m.ocr.key, m.ocr.keyFor = ocrMachineKey(m.ocr.machine, m.ocrCfg), comp
		m.ocr.bench = m.loadOCRBench(m.ocr.key)
	}
	return m.ocr.bench
}

// ocrLive 返回 OCR 进程池现有的进程数（它们占着的内存在换算内存上限时要加回来）。
func (m *Manager) ocrLive() int {
	if m.OCRPool == nil {
		return 0
	}
	_, live, _ := m.OCRPool.Stats()
	return live
}

// autoPlanLocked 返回自动时的搭配和线程预算：按本机测速结果估算各种搭配的速度，选最快的（还没测时每个进程
// 1 线程），进程数受内存限制。half 见 ocrBudget。调用时持有 m.ocr.mu。
func (m *Manager) autoPlanLocked(cores int, half bool, live int) (ocrPlan, int) {
	b := m.ocrBenchLocked()
	slot := uint64(ocrSlotMemDefault)
	if bp, ok := b.best(); ok && bp.SlotMem > 0 {
		slot = bp.SlotMem * 5 / 4 // 留些余量：大页面、大 PDF 占得更多
	}
	total, avail := sysinfo.Memory()
	m.ocr.memCap = m.memCapLocked(nextMemCap(m.ocr.memCap, ocrMemSlots(total, avail, slot, live)), awake.Now())
	budget := ocrBudget(cores, half)
	phys, smt := b.curves()
	if !half && b.smtFaster() {
		return smt.bestPlan(b.Logical, m.ocr.memCap), b.Logical
	}
	return phys.bestPlan(budget, m.ocr.memCap), budget
}

// memCapLocked 由 nextMemCap 的结果 next 定出内存允许的进程数：减少后 ocrMemUpDelay 之内不增加。调用时持有 m.ocr.mu。
func (m *Manager) memCapLocked(next int, now time.Duration) int {
	cur := m.ocr.memCap
	switch {
	case cur > 0 && next < cur:
		m.ocr.memDown = now
	case cur > 0 && next > cur && m.ocr.memDown > 0 && now-m.ocr.memDown < ocrMemUpDelay:
		return cur
	}
	return next
}

// ocrPlanNow 返回当前搭配；还没调整过时按设置给出初值。
func (m *Manager) ocrPlanNow() ocrPlan {
	cores := m.physCores()
	cfg := m.Config()
	live := m.ocrLive()
	m.ocr.mu.Lock()
	defer m.ocr.mu.Unlock()
	if m.ocr.plan.procs == 0 {
		if cfg.OCRWorkers > 0 {
			m.ocr.plan = ocrManualPlan(cfg.OCRWorkers, cores)
		} else {
			m.ocr.plan, _ = m.autoPlanLocked(cores, slowForUser(cfg), live)
		}
	}
	return m.ocr.plan
}

// resetOCRPlan：OCR 并发设置改了，重新开始。
func (m *Manager) resetOCRPlan() {
	m.ocr.mu.Lock()
	m.ocr.plan, m.ocr.memCap, m.ocr.tailSeen = ocrPlan{}, 0, 0
	m.ocr.mu.Unlock()
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

// ocrTuneStep 按线程预算（其他通道忙不忙）、测速结果和可用内存调整搭配，并处理队列收尾。
func (m *Manager) ocrTuneStep() {
	cfg := m.Config()
	if !m.ocrAvailable() || !cfg.OCREnabled || m.ocrBenching.Load() {
		return
	}
	cores := m.physCores()
	now := awake.Now()
	pages := m.ocr.pages.Load()
	live := m.ocrLive()
	userBusy := slowForUser(cfg)

	m.ocr.mu.Lock()
	m.ocr.rate.add(now, pages)
	var next ocrPlan
	var budget int
	if cfg.OCRWorkers > 0 {
		next = ocrManualPlan(cfg.OCRWorkers, cores)
		budget = next.procs * next.threads
		userBusy = false // 手动设的不自动调整
	} else {
		next, budget = m.autoPlanLocked(cores, userBusy, live)
	}
	changed := next != m.ocr.plan
	userChanged := userBusy != m.ocr.userBusy
	m.ocr.plan, m.ocr.userBusy = next, userBusy
	memCap := m.ocr.memCap
	m.ocr.mu.Unlock()

	if userChanged {
		logx.Debugf("%s", map[bool]string{true: "用户正在用电脑：OCR 只用一半核心", false: "用户离开了：OCR 恢复用全部核心"}[userBusy])
	}
	if changed {
		logx.Debugf("OCR 改用 %s（线程预算 %d，物理核心 %d，内存允许 %d 个进程）", next, budget, cores, memCap)
		m.applyWorkers()
	}
	m.ocrTail(next, budget)
}

// ocrTail：OCR 队列已经空了、正在识别的文件比进程少时（多半是排在最后的大文件），把空出来的线程平分给
// 它们，从下一页起生效；队列里又有文件时恢复。连续两次（约 1 分钟）都这样才调整，免得队列偶尔见底时来回切换。
// 不在收尾时，正在识别的文件用的线程比现在的搭配多（预算刚减半：用户开始用电脑、来了普通文件），也从下一页起
// 减到现在的线程数，不等这个文件识别完。
func (m *Manager) ocrTail(p ocrPlan, budget int) {
	s := &m.sched
	l := s.lanes[prioOCR]
	if l == nil {
		return
	}
	type running struct {
		ctl     *worker.Ctl
		path    string
		threads int // 开始识别时的线程数
	}
	var rs []running
	s.mu.Lock()
	for _, j := range s.inflight {
		if j.ctl != nil {
			rs = append(rs, running{j.ctl, j.path, j.ocrThreads})
		}
	}
	s.mu.Unlock()
	m.ocr.mu.Lock()
	if !l.wantWork.Load() && len(rs) > 0 && len(rs) < p.procs {
		m.ocr.tailSeen++
	} else {
		m.ocr.tailSeen = 0
	}
	t := 0
	if m.ocr.tailSeen >= 2 {
		t = tailThreads(p, budget, len(rs))
	}
	m.ocr.tail = t
	m.ocr.mu.Unlock()
	for _, r := range rs {
		want := t
		if want == 0 && r.threads > p.threads {
			want = p.threads
		}
		if r.ctl.OCRThreads() != want {
			switch {
			case t > 0:
				logx.Debugf("OCR 队列已空，%s 剩下的页改用 %d 线程", logx.P(r.path), t)
			case want > 0:
				logx.Debugf("OCR 线程预算减少，%s 剩下的页改用 %d 线程", logx.P(r.path), want)
			}
			r.ctl.SetOCRThreads(want)
		}
	}
}

// ocrTailOff：又有 OCR 文件开始处理，收尾时多分的线程立即收回（不等下一次调整）。
func (m *Manager) ocrTailOff() {
	m.ocr.mu.Lock()
	on := m.ocr.tail > 0
	m.ocr.tail, m.ocr.tailSeen = 0, 0
	m.ocr.mu.Unlock()
	if !on {
		return
	}
	s := &m.sched
	var ctls []*worker.Ctl
	s.mu.Lock()
	for _, j := range s.inflight {
		if j.ctl != nil {
			ctls = append(ctls, j.ctl)
		}
	}
	s.mu.Unlock()
	for _, c := range ctls {
		c.SetOCRThreads(0)
	}
}

// ---- 测速 ----

// ocrBenchDue：OCR 设为自动、本机还没测过速（或用户要求重新测）时，OCR 通道先测速再派发文件。
func (m *Manager) ocrBenchDue() bool {
	if !m.ocrAuto() {
		return false
	}
	m.ocr.mu.Lock()
	defer m.ocr.mu.Unlock()
	return m.ocr.benchAsk || (m.ocrBenchLocked() == nil && m.ocr.benchFail == "")
}

// ocrBenchScanWait：本机还没测过 OCR 速度时，等扫描完成再测（扫描读盘、占 CPU，测出来不准），最多等这么久。
const ocrBenchScanWait = 10 * time.Minute

const benchWaitNote = "等扫描完成后测试本机 OCR 速度，再开始识别"

// ocrBenchWait：测速前还要等扫描完成。0.2.7 测速时扫描还在进行，同一台电脑两次测速的结果差得很多。
func (m *Manager) ocrBenchWait() bool {
	scanning := m.scanning()
	now := awake.Now()
	m.ocr.mu.Lock()
	defer m.ocr.mu.Unlock()
	if !scanning || (m.ocr.waitScan > 0 && now-m.ocr.waitScan > ocrBenchScanWait) {
		if m.ocr.benchNote == benchWaitNote {
			m.ocr.benchNote = ""
		}
		return false
	}
	if m.ocr.waitScan == 0 {
		m.ocr.waitScan = now
	}
	m.ocr.benchNote = benchWaitNote
	return true
}

// scanning：有索引根或文件夹正在扫描（或排队等扫描）。
func (m *Manager) scanning() bool {
	m.scanMu.Lock()
	defer m.scanMu.Unlock()
	return len(m.scanRun) > 0 || len(m.scanSet) > 0
}

// ocrBenchAsked：用户要求了重新测速（没有要识别的文件时也测）。
func (m *Manager) ocrBenchAsked() bool {
	if !m.ocrAuto() {
		return false
	}
	m.ocr.mu.Lock()
	defer m.ocr.mu.Unlock()
	return m.ocr.benchAsk
}

// ocrAuto：OCR 可用、已开启，进程数设为自动。
func (m *Manager) ocrAuto() bool {
	cfg := m.Config()
	return cfg.OCRWorkers == 0 && cfg.OCREnabled && m.ocrAvailable()
}

func (m *Manager) setBenchNote(s string) {
	m.ocr.mu.Lock()
	m.ocr.benchNote = s
	m.ocr.mu.Unlock()
}

// sleepCtx 等待 d；程序退出时返回 false。
func (m *Manager) sleepCtx(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-m.ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// ocrBench 在 OCR 通道的派发协程里测速：正在识别的文件先做完；测速期间普通文件、老格式也暂停
// （手头的做完，最多等 1 分钟），CPU 不被别的任务占用，结果才准。约 3～5 分钟。
func (m *Manager) ocrBench() {
	if !m.runOCRBench() || m.ctx.Err() != nil {
		return
	}
	m.ocrTuneStep() // 按测速结果换搭配
}

func (m *Manager) runOCRBench() bool {
	s := &m.sched
	running := func(p int) int {
		s.mu.Lock()
		defer s.mu.Unlock()
		if l := s.lanes[p]; l != nil {
			return l.running
		}
		return 0
	}
	if running(prioOCR) > 0 {
		m.setBenchNote("正在识别的文件做完后测试本机 OCR 速度")
		for running(prioOCR) > 0 {
			if !m.sleepCtx(time.Second) {
				return false
			}
		}
	}
	m.ocrBenching.Store(true)
	s.idle.Store(false)
	defer func() {
		m.ocrBenching.Store(false)
		m.setBenchNote("")
		m.kick()
	}()
	m.setBenchNote("正在测试本机 OCR 速度，其他文件暂停约 3～5 分钟")
	for end := awake.Now() + time.Minute; (running(prioFast) > 0 || running(prioLegacy) > 0) && awake.Now() < end; {
		if !m.sleepCtx(time.Second) {
			return false
		}
	}

	cores := m.physCores()
	m.ocr.mu.Lock()
	m.ocr.benchAsk = false
	m.ocrBenchLocked()
	key, mc := m.ocr.key, m.ocr.machine
	m.ocr.mu.Unlock()
	// 普通文件、老格式的空闲进程也结束，放出它们占着的内存。可用内存按至少总内存的一半算：测速时手头的大文件
	// 可能还在解析（最多等 1 分钟），0.2.8 某次测速时可用内存只够 2 个 OCR 进程，常用的 6×1、3×2 都没测到，
	// 之后内存够 11 个进程时只能按推算的开
	for _, p := range []*worker.Pool{m.Pool, m.LegacyPool, m.OCRPool} {
		if p != nil {
			p.CloseIdle()
		}
	}
	total, avail := sysinfo.Memory()
	avail = max(avail, total/2)
	cands := ocrBenchCandidates(cores, mc.Logical, nextMemCap(0, ocrMemSlots(total, avail, ocrSlotMemDefault, m.ocrLive())))
	fail := func(why string) bool {
		m.ocr.mu.Lock()
		m.ocr.benchFail = why
		m.ocr.mu.Unlock()
		m.logf("OCR 测速失败（%s），先按每个进程 1 线程处理，下次启动时再测", why)
		return false
	}
	sample, cleanup, err := writeOCRSample()
	if err != nil {
		return fail(err.Error())
	}
	defer cleanup()
	pool := m.OCRPool
	if pool == nil {
		pool = m.Pool
	}
	m.logf("测试本机 OCR 速度：比较 %d 种搭配（%s），交替测两轮，普通文件暂停约 3～5 分钟", len(cands), planList(cands))
	t0 := awake.Now()
	cpu := sysinfo.NewCPUMonitor(worker.ChildrenCPU)
	b := runOCRBench(m.ctx, cores, cands, m.ocrCfg.Models(), newOCRBenchRunner(m, pool, sample), func(i, n int, what string) {
		m.setBenchNote(fmt.Sprintf("正在测试本机 OCR 速度（第 %d/%d 项：%s），其他文件暂停约 3～5 分钟", i+1, n, what))
		m.activity()
	})
	if m.ctx.Err() != nil {
		return false
	}
	b.Machine, b.CPU, b.Cores, b.Logical, b.MemGB = key, mc.CPU, mc.Cores, mc.Logical, float64(mc.MemTotal)/(1<<30)
	b.CPUNote = cpuNote(cpu.Read())
	if _, ok := b.best(); !ok {
		return fail(b.Err)
	}
	if err := m.saveOCRBench(key, b); err != nil {
		m.logf("保存 OCR 测速结果失败: %v", err)
	}
	m.ocr.mu.Lock()
	m.ocr.bench, m.ocr.benchFail, m.ocr.memCap, m.ocr.memDown = b, "", 0, 0
	m.ocr.mu.Unlock()
	m.logf("本机 OCR 测速（%s，%d 个物理核心，用时 %d 秒）：%s", mc.CPU, mc.Cores, int(awake.Since(t0).Seconds()), b.Summary())
	m.recheckOCRModel() // 测速后认字模型可能变了（新模型在这台电脑上用不了）：自检结果写明实际用的模型
	return true
}

func benchPlans(b *OCRBench) []ocrPlan {
	var out []ocrPlan
	for _, p := range b.Plans {
		out = append(out, p.plan())
	}
	return out
}

func planList(ps []ocrPlan) string {
	var parts []string
	for _, p := range ps {
		parts = append(parts, fmt.Sprintf("%d×%d", p.procs, p.threads))
	}
	return strings.Join(parts, "、")
}

// OCRBenchInfo 是设置页显示的本机 OCR 测速情况。
type OCRBenchInfo struct {
	Bench   *OCRBench `json:"bench"`          // 本机的测速结果；nil 表示还没测
	Running bool      `json:"running"`        // 正在测速
	Asked   bool      `json:"asked"`          // 已要求重新测速，等正在识别的文件做完
	Note    string    `json:"note,omitempty"` // 测速进度或失败的原因
	Manual  bool      `json:"manual"`         // 手动设了 OCR 进程数：不测速
	OCR     bool      `json:"ocr"`            // OCR 组件可用且已开启
	Plan    string    `json:"plan"`           // 现在的搭配
	MemCap  int       `json:"memCap"`         // 内存允许的进程数
	Cores   int       `json:"cores"`          // 本机物理核心数
	Model   string    `json:"model"`          // 现在用的认字模型
	Models  []string  `json:"models"`         // 组件里有的认字模型
	// ReOCR 是用别的认字模型识别过的文件数（可以用现在的模型重新识别），其中 ReOCRUnknown 个是 0.2.8 以前
	// 识别的（没记下用的哪个模型）
	ReOCR        int64  `json:"reocr"`
	ReOCRUnknown int64  `json:"reocrUnknown"`
	SMTNote      string `json:"smtNote,omitempty"`  // 为什么用或不用超线程
	PlanNote     string `json:"planNote,omitempty"` // 现在的搭配没有直接测过时的说明
}

// OCRBenchInfo 返回本机的 OCR 测速情况。
func (m *Manager) OCRBenchInfo() OCRBenchInfo {
	cfg := m.Config()
	plan := m.ocrPlanNow()
	cores := m.physCores()
	model := m.ocrModel()
	reocr, unknown, _ := m.DB.CountOCRDone(prioOCR, model)
	m.ocr.mu.Lock()
	defer m.ocr.mu.Unlock()
	info := OCRBenchInfo{Bench: m.ocrBenchLocked(), Running: m.ocrBenching.Load(), Asked: m.ocr.benchAsk,
		Note: m.ocr.benchNote, Manual: cfg.OCRWorkers > 0, OCR: m.ocrAvailable() && cfg.OCREnabled,
		Plan: plan.String(), MemCap: m.ocr.memCap, Cores: cores, Model: model, Models: m.ocrCfg.Models(), ReOCR: reocr, ReOCRUnknown: unknown}
	if info.Note == "" && m.ocr.benchFail != "" {
		info.Note = "测速失败：" + m.ocr.benchFail
	}
	if b := info.Bench; b != nil && !info.Manual {
		if b.Logical > b.Budget {
			info.SMTNote = strings.Trim(smtNote(b), "（）")
		}
		measured := false
		for _, p := range b.Plans {
			measured = measured || (p.Err == "" && p.plan() == plan)
		}
		if !measured && len(b.Plans) > 0 {
			info.PlanNote = fmt.Sprintf("%s 没有直接测过：是按实测的每个线程的速度推算的（测速时比较的是 %s；现在内存允许的进程数、"+
				"可用的线程数和测速时不同，就从实测结果推算最快的搭配）", plan, planList(benchPlans(b)))
		}
	}
	return info
}

// RebenchOCR 要求重新测速：OCR 通道把正在识别的文件做完后立即测（没有要识别的文件也测）。
func (m *Manager) RebenchOCR() error {
	cfg := m.Config()
	switch {
	case !m.ocrAvailable() || !cfg.OCREnabled:
		return errors.New("OCR 组件没有安装或 OCR 已关闭")
	case cfg.OCRWorkers > 0:
		return errors.New("OCR 进程数是手动设置的，不测速；改成 0（自动）后才会测速")
	}
	m.ocr.mu.Lock()
	m.ocr.benchAsk, m.ocr.benchFail = true, ""
	m.ocr.mu.Unlock()
	m.kick()
	return nil
}

// ReOCR 用现在的认字模型重新识别用别的模型识别过的文件（换了模型后可选；0.2.8 以前识别的也算），返回排队的文件数。
// 旧文字保留到重新识别完成时替换，期间照样能搜到。
func (m *Manager) ReOCR() (int64, error) {
	if !m.ocrAvailable() || !m.Config().OCREnabled {
		return 0, errors.New("OCR 组件没有安装或 OCR 已关闭")
	}
	model := m.ocrModel()
	n, err := m.DB.ResetOCR(prioOCR, model)
	if err != nil {
		return 0, err
	}
	m.logf("用 %s 重新识别用别的模型识别过的 %d 个文件（旧文字在重新识别完成前照样能搜到）", extract.OCRModelName(model), n)
	m.kick()
	return n, nil
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

// finishOCRPages：OCR 文件处理完，补上最后一页（单张图片算一页），返回这个文件识别了几页。
func (m *Manager) finishOCRPages(j *job) int {
	m.sched.mu.Lock()
	rest := 1
	if j.pagesTotal > 0 {
		rest = j.pagesTotal - j.pagesDone
	}
	j.pagesDone += rest
	pages := j.pagesDone
	m.sched.mu.Unlock()
	if j.f.Ext == "pdf" && pages > 0 {
		m.ocr.mu.Lock()
		m.ocr.pdfBytes += j.f.Size
		m.ocr.pdfPages += int64(pages)
		m.ocr.mu.Unlock()
	}
	if rest > 0 {
		m.ocr.pages.Add(int64(rest))
	}
	return pages
}

// ocrEtaMinPages：识别完这么多页 PDF 后才按「每页多大」估算没记下页数的 PDF 有多少页。
const ocrEtaMinPages = 20

// ocrEta 按页数估算 OCR 还要多久（秒）：(正在识别的文件剩下的页数 + 队列里的页数) ÷ 最近每分钟页数。
// 队列里的 PDF 用普通文件阶段记下的页数（没有文字层的页）；没记下的（0.2.8 以前排进队列的）按「已识别的
// PDF 平均每页多大」由文件大小估算，图片算一页。
// 0.2.7 全部按大小估算，可大文件每页也大（某位用户不到 500 KB 的 PDF 平均每页 106 KB，10 MB 以上的每页
// 1.8 MB），刚开始时小文件先做，估出的剩余时间比实际多一倍多。
func (m *Manager) ocrEta(queue int64) int64 {
	m.ocr.mu.Lock()
	perMin := m.ocr.rate.perMin()
	var bpp float64
	if m.ocr.pdfPages >= ocrEtaMinPages {
		bpp = float64(m.ocr.pdfBytes) / float64(m.ocr.pdfPages)
	}
	m.ocr.mu.Unlock()
	if perMin <= 0 {
		return m.laneEta(prioOCR, queue)
	}
	q, err := m.DB.OCRQueueSize(prioOCR)
	if err != nil || (q.PDFBytes > 0 && bpp <= 0) {
		return m.laneEta(prioOCR, queue)
	}
	pages := float64(q.Pages) + float64(q.Others)
	if q.PDFBytes > 0 {
		pages += float64(q.PDFBytes) / bpp
	}
	type running struct {
		id               int64
		pdf              bool
		size             int64
		pagesTotal, done int
	}
	var rs []running
	var ids []int64
	s := &m.sched
	s.mu.Lock()
	for _, j := range s.inflight {
		if j.lane == nil || j.lane.prio != prioOCR {
			continue
		}
		rs = append(rs, running{j.f.ID, j.f.Ext == "pdf", j.f.Size, j.pagesTotal, j.pagesDone})
		ids = append(ids, j.f.ID)
	}
	s.mu.Unlock()
	known, _ := m.DB.OCRPagesOf(ids)
	for _, r := range rs {
		// 正在识别的文件在索引库里还是待处理，上面按整个文件算进去了：换成剩下的页数
		full := 1.0
		switch {
		case known[r.id] > 0:
			full = float64(known[r.id])
		case r.pdf && bpp > 0:
			full = float64(r.size) / bpp
		}
		pages -= full
		if r.pagesTotal > 0 {
			pages += float64(r.pagesTotal - r.done)
		} else {
			pages += max(1, full)
		}
	}
	pages = max(0, pages)
	return int64(pages / perMin * 60)
}

// ocrStatus 返回界面显示的每个进程线程数、最近每分钟页数和说明（测速进度、队列收尾）。
func (m *Manager) ocrStatus() (threads int, perMin float64, note string) {
	p := m.ocrPlanNow()
	m.ocr.mu.Lock()
	defer m.ocr.mu.Unlock()
	note = m.ocr.benchNote
	switch {
	case note != "":
	case m.ocr.tail > 0:
		note = fmt.Sprintf("队列已空，正在识别的文件每个改用 %d 线程", m.ocr.tail)
	case m.ocr.userBusy:
		note = "正在用电脑，OCR 只用一半核心"
	}
	return p.threads, m.ocr.rate.perMin(), note
}
