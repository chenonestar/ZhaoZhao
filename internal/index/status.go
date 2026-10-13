package index

import (
	"sort"
	"sync/atomic"
	"time"

	"github.com/chenonestar/zhaozhao/internal/extract"
	"github.com/chenonestar/zhaozhao/internal/store"
)

// RootStatus 是单个索引根的进度（F-BG-09）。
type RootStatus struct {
	store.Root
	Path     string       `json:"path"`
	Label    string       `json:"label"`
	Kind     string       `json:"kind"`
	Online   bool         `json:"online"`
	Scanning bool         `json:"scanning"`
	Scanned  int64        `json:"scanned"`
	Counts   store.Counts `json:"counts"`
}

// VolumeStatus 是单个卷的信息（F-VOL-08）。
type VolumeStatus struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	Kind       string `json:"kind"`
	Online     bool   `json:"online"`
	Mount      string `json:"mount"`
	LastMount  string `json:"lastMount"`
	LastSeen   int64  `json:"lastSeen"`
	Files      int64  `json:"files"`
	IndexBytes int64  `json:"indexBytes"`
	Syncing    bool   `json:"syncing"`
	Capacity   int64  `json:"capacity"`
}

// CurrentJob 是一个正在处理的文件。
type CurrentJob struct {
	Path     string `json:"path"`
	Kind     string `json:"kind"`     // 普通 / 老格式 / OCR
	Stage    string `json:"stage"`    // 当前步骤，如「从网络盘读取」「OCR 识别」
	Progress string `json:"progress"` // 步骤内的进度，如「第 3/20 页」
	Seconds  int64  `json:"seconds"`  // 已处理的时间
}

// SlotStatus 是一类受限资源的使用情况。
type SlotStatus struct {
	Used int `json:"used"`
	Cap  int `json:"cap"`
}

// Status 是整体状态。
type Status struct {
	Paused      bool           `json:"paused"`
	Idle        bool           `json:"idle"`
	Workers     int            `json:"workers"`
	Active      int            `json:"active"`
	Current     []CurrentJob   `json:"current"`
	Totals      store.Counts   `json:"totals"`
	OCRQueue    int64          `json:"ocrQueue"`
	LegacyQueue int64          `json:"legacyQueue"`
	OCREtaSec   int64          `json:"ocrEtaSec"` // 0 表示还无法估算
	Legacy      SlotStatus     `json:"legacy"`
	OCR         SlotStatus     `json:"ocr"`
	Net         SlotStatus     `json:"net"`
	StalledSec  int64          `json:"stalledSec"` // >0：疑似卡住，这么多秒没有任何进展
	Rebuilding  bool           `json:"rebuilding"`
	Corrupt     bool           `json:"corrupt"`
	OnBattery   bool           `json:"onBattery"`
	Scanning    bool           `json:"scanning"`
	Processed   int64          `json:"processed"`
	Roots       []RootStatus   `json:"roots"`
	Volumes     []VolumeStatus `json:"volumes"`
	// Components 是外部组件（LibreOffice、OCR）的自检结果
	Components         []extract.ComponentStatus `json:"components"`
	ComponentsChecking bool                      `json:"componentsChecking"`
}

// currentJobs 返回正在处理的文件，处理得最久的在前。
func (m *Manager) currentJobs() []CurrentJob {
	s := &m.sched
	out := []CurrentJob{}
	if s.inflight == nil {
		return out
	}
	now := time.Now()
	s.mu.Lock()
	for _, j := range s.inflight {
		out = append(out, CurrentJob{Path: j.path, Kind: classNames[j.class], Stage: j.stage, Progress: j.progress,
			Seconds: int64(now.Sub(j.since).Seconds())})
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, k int) bool {
		if out[i].Seconds != out[k].Seconds {
			return out[i].Seconds > out[k].Seconds
		}
		return out[i].Path < out[k].Path
	})
	return out
}

// ocrEta 按最近的完成速度估算 OCR 队列还要多久：只看最近 1 小时内完成的（最多 30 个），
// 处理慢下来或停住时估算会跟着变长，而不是沿用开始时的速度。不足 2 个时无法估算，返回 0。
func (m *Manager) ocrEta(queue int64) int64 {
	s := &m.sched
	if queue <= 0 {
		return 0
	}
	now := time.Now()
	s.mu.Lock()
	var recent []time.Time
	for _, t := range s.ocrDone {
		if now.Sub(t) <= time.Hour {
			recent = append(recent, t)
		}
	}
	s.mu.Unlock()
	if len(recent) < 2 {
		return 0
	}
	span := now.Sub(recent[0]).Seconds() // 到现在为止：停住时速度自然下降
	if span <= 0 {
		return 0
	}
	rate := float64(len(recent)-1) / span
	return int64(float64(queue) / rate)
}

// stalled 返回疑似卡住的时长：有文件在处理或等待处理，却超过 stallAfter 没有任何进展。
func (m *Manager) stalled() time.Duration {
	s := &m.sched
	if m.pausedAll.Load() || s.inflight == nil {
		return 0
	}
	s.mu.Lock()
	busy := len(s.inflight) > 0
	s.mu.Unlock()
	if !busy && !s.wantWork.Load() {
		return 0
	}
	d := time.Since(time.Unix(0, s.lastActivity.Load()))
	if d < stallAfter {
		return 0
	}
	return d
}

// Status 汇总当前状态。
func (m *Manager) Status() Status {
	s := &m.sched
	st := Status{Paused: m.pausedAll.Load(), Rebuilding: m.rebuilding.Load(), Corrupt: m.corrupt.Load(),
		Workers: m.Config().workers(), OnBattery: onBattery(), Roots: []RootStatus{}, Volumes: []VolumeStatus{}}
	st.Current = m.currentJobs()
	if s.legacy != nil {
		s.mu.Lock()
		st.Active = s.running
		s.mu.Unlock()
		st.Legacy.Used, st.Legacy.Cap = s.legacy.Stats()
		st.OCR.Used, st.OCR.Cap = s.ocr.Stats()
		st.Net.Used, st.Net.Cap = s.net.Stats()
		st.Processed = s.done.Load()
	}
	st.Totals, _ = m.DB.AllCounts()
	st.OCRQueue, _ = m.DB.PendingByPrio(prioOCR)
	st.LegacyQueue, _ = m.DB.PendingByPrio(prioLegacy)
	st.OCREtaSec = m.ocrEta(st.OCRQueue)
	st.StalledSec = int64(m.stalled().Seconds())

	roots, _ := m.DB.ListRoots()
	volSync := map[string]bool{}
	for _, r := range roots {
		rs := RootStatus{Root: r}
		if rt := m.getRoot(r.ID); rt != nil && rt.vol != nil {
			m.mu.RLock()
			rs.Online, rs.Kind, rs.Label = rt.vol.online, rt.vol.info.Kind, rt.vol.info.Label
			m.mu.RUnlock()
		}
		rs.Path = m.rootDisplay(r)
		ss := m.scanStateOf(r.ID)
		rs.Scanning, rs.Scanned = ss.running.Load(), atomic.LoadInt64(&ss.count)
		if rs.Scanning {
			st.Scanning = true
			volSync[r.VolumeID] = true
		}
		rs.Counts, _ = m.DB.RootCounts(r.ID)
		st.Roots = append(st.Roots, rs)
	}
	vols, _ := m.DB.ListVolumes()
	for _, v := range vols {
		vs := VolumeStatus{ID: v.ID, Label: v.Label, Kind: v.Kind, LastMount: v.LastMount, LastSeen: v.LastSeen, Capacity: v.Capacity, Syncing: volSync[v.ID]}
		if o := m.volOf(v.ID); o != nil {
			m.mu.RLock()
			vs.Online, vs.Mount, vs.Label = o.online, o.info.Mount, o.info.Label
			if vs.Label == "" {
				vs.Label = v.Label
			}
			m.mu.RUnlock()
		}
		c, _ := m.DB.VolumeCounts(v.ID)
		vs.Files = c.Total
		vs.IndexBytes, _ = m.DB.VolumeIndexBytes(v.ID)
		st.Volumes = append(st.Volumes, vs)
	}
	st.Components, st.ComponentsChecking = m.Components()
	st.Idle = s.idle.Load() && !st.Scanning && st.Totals.Pending == 0
	return st
}
