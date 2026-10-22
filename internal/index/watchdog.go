package index

import (
	"fmt"
	"math"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync/atomic"
	"time"

	"github.com/chenonestar/zhaozhao/internal/awake"
	"github.com/chenonestar/zhaozhao/internal/logx"
	"github.com/chenonestar/zhaozhao/internal/worker"
)

// cpuMeter 由子进程累计用掉的 CPU 时间，算出最近一分钟它们实际平均占用几个逻辑处理器，写进状态快照：
// RapidOCR 设了线程数，实际占用可能更多（用户截图里设 3 线程的 RapidOCR 占了 66% 的 CPU）。只有 Windows 量得出来。
type cpuMeter struct {
	at, cpu time.Duration // 上次采样的时间（awake）和累计 CPU 时间；只在 watchdog 里用
	use     atomic.Uint64 // 最近一次算出的逻辑处理器数（float64 的位）；0 表示没有
}

// sample 每分钟由 watchdog 调用一次。
func (c *cpuMeter) sample() {
	cpu, ok := worker.ChildrenCPU()
	now := awake.Now()
	if !ok {
		return
	}
	if c.at > 0 && now-c.at >= 10*time.Second {
		c.use.Store(math.Float64bits(float64(cpu-c.cpu) / float64(now-c.at)))
	}
	c.at, c.cpu = now, cpu
}

// last 返回最近一分钟子进程实际平均占用的逻辑处理器数；量不出来或几乎为 0 时 ok 为 false。
func (c *cpuMeter) last() (float64, bool) {
	v := math.Float64frombits(c.use.Load())
	return v, v >= 0.05
}

// watchdog 定时检查调度状态：
//   - 详细日志模式下，有任务时每分钟记录一次状态快照（各类名额、队列、正在处理的文件和步骤）；
//   - 有任务却超过 stallAfter 没有任何进展时，判定为疑似卡住，在日志里写明状态和每个线程停在哪里
//     （之后每 30 分钟再记一次，直到恢复），便于从日志直接定位问题。
func (m *Manager) watchdog() {
	defer m.wg.Done()
	t := time.NewTicker(watchEvery)
	defer t.Stop()
	s := &m.sched
	var lastBeat, lastCPU time.Time
	slept := awake.Slept()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
		}
		// 电脑睡眠、休眠期间所有程序都暂停：醒来后在日志里记一笔，便于对照进度
		if d := awake.Slept() - slept; d >= time.Minute {
			slept += d
			m.logf("电脑刚从睡眠中恢复（睡了约 %s）；睡眠期间索引暂停，时限和剩余时间不计睡眠的时间", fmtDuration(d))
		}
		if d := m.stalled(); d > 0 {
			if s.stallLogged.IsZero() || time.Since(s.stallLogged) >= 30*time.Minute {
				s.stallLogged = time.Now()
				m.logf("疑似卡住：已经 %s 没有任何文件完成或报告进度。\n%s\n各线程当前位置：\n%s", fmtDuration(d), m.snapshot(), goroutineDump(64<<10))
			}
		} else if !s.stallLogged.IsZero() {
			s.stallLogged = time.Time{}
			m.logf("已恢复：重新有文件完成")
		}
		if s.idle.Load() {
			m.maybeCheckIntegrity()
		}
		if time.Since(lastCPU) >= time.Minute {
			lastCPU = time.Now()
			m.cpu.sample()
		}
		if logx.Verbose() && !s.idle.Load() && time.Since(lastBeat) >= time.Minute {
			lastBeat = time.Now()
			logx.Debugf("状态：%s", m.snapshot())
		}
	}
}

// Snapshot 返回调度状态的文字描述（导出诊断信息用）。
func (m *Manager) Snapshot() string {
	if m.sched.net == nil {
		return "索引服务未启动"
	}
	return m.snapshot()
}

// snapshot 返回调度状态的文字描述。
func (m *Manager) snapshot() string {
	s := &m.sched
	var b strings.Builder
	f, l, o := m.laneStat(prioFast), m.laneStat(prioLegacy), m.laneStat(prioOCR)
	nu, nc := s.net.Stats()
	fmt.Fprintf(&b, "普通文件 %d/%d，老格式转换 %d/%d，OCR %d/%d，网络盘读取 %d/%d", f.Used, f.Cap, l.Used, l.Cap, o.Used, o.Cap, nu, nc)
	if m.Pool != nil {
		busy, live, size := m.Pool.Stats()
		fmt.Fprintf(&b, "，解析进程 %d 个在用/%d 个已启动/上限 %d", busy, live, size)
	}
	if m.LegacyPool != nil {
		busy, live, size := m.LegacyPool.Stats()
		fmt.Fprintf(&b, "，转换进程 %d/%d/%d", busy, live, size)
	}
	if m.OCRPool != nil {
		busy, live, size := m.OCRPool.Stats()
		fmt.Fprintf(&b, "，OCR 进程 %d/%d/%d", busy, live, size)
	}
	c, _ := m.DB.AllCounts()
	oq, _ := m.DB.PendingByPrio(prioOCR)
	lq, _ := m.DB.PendingByPrio(prioLegacy)
	fmt.Fprintf(&b, "；待处理 %d（老格式 %d，OCR %d），已提取 %d，失败 %d", c.Pending, lq, oq, c.Done, c.Failed)
	if m.pausedAll.Load() {
		b.WriteString("；已暂停")
	}
	if u, ok := m.cpu.last(); ok {
		fmt.Fprintf(&b, "；子进程最近一分钟实际占用约 %.1f 个逻辑处理器（共 %d 个）", u, runtime.NumCPU())
	}
	jobs := m.currentJobs()
	for i, j := range jobs {
		if i == 20 {
			fmt.Fprintf(&b, "\n  …… 还有 %d 个", len(jobs)-20)
			break
		}
		fmt.Fprintf(&b, "\n  [%s] %s：%s", j.Kind, logx.P(j.Path), j.Stage)
		if j.Progress != "" {
			fmt.Fprintf(&b, " %s", j.Progress)
		}
		fmt.Fprintf(&b, "，已 %s", fmtDuration(time.Duration(j.Seconds)*time.Second))
	}
	return b.String()
}

// goroutineDump 返回所有协程的调用栈（相同的合并显示），最多 max 字节。
func goroutineDump(max int) string {
	var b strings.Builder
	_ = pprof.Lookup("goroutine").WriteTo(&b, 1)
	s := b.String()
	if len(s) > max {
		s = s[:max] + "\n……（已截断）"
	}
	return s
}

func fmtDuration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%d 小时 %d 分", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%d 分 %d 秒", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%d 秒", int(d.Seconds()))
}

// integrityEvery 是后台完整性检查的间隔。
const integrityEvery = 7 * 24 * time.Hour

// maybeCheckIntegrity 空闲时在后台做完整性检查（距上次超过 7 天）。启动时不再检查，
// 避免索引库大了以后启动变慢；上次异常退出时由启动流程检查。
func (m *Manager) maybeCheckIntegrity() {
	if !m.checking.CompareAndSwap(false, true) {
		return
	}
	last, _ := m.DB.GetMeta("last_integrity_check")
	var t int64
	fmt.Sscan(last, &t)
	if time.Since(time.Unix(t, 0)) < integrityEvery {
		m.checking.Store(false)
		return
	}
	go func() {
		defer m.checking.Store(false)
		start := time.Now()
		err := m.DB.Check()
		if m.ctx.Err() != nil {
			return
		}
		m.logf("后台检查索引库完整性：%v，用时 %s", map[bool]string{true: "正常", false: fmt.Sprint(err)}[err == nil], fmtDuration(time.Since(start)))
		if err != nil {
			m.corrupt.Store(true)
			m.addNotice(Notice{Kind: "corrupt", Message: err.Error() + "。请在设置中重建索引。"})
			return
		}
		_ = m.DB.SetMeta("last_integrity_check", fmt.Sprint(time.Now().Unix()))
	}()
}
