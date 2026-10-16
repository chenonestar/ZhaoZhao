package index

import (
	"github.com/chenonestar/zhaozhao/internal/awake"
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
	// 待处理里老格式转换、OCR 各有多少
	LegacyPending int64 `json:"legacyPending"`
	OCRPending    int64 `json:"ocrPending"`
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
	ID       int64  `json:"id"`
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
	Paused      bool         `json:"paused"`
	Idle        bool         `json:"idle"`
	Workers     int          `json:"workers"`
	Active      int          `json:"active"`
	Current     []CurrentJob `json:"current"`
	Totals      store.Counts `json:"totals"`
	OCRQueue    int64        `json:"ocrQueue"`
	LegacyQueue int64        `json:"legacyQueue"`
	FastQueue   int64        `json:"fastQueue"`
	// 各通道的预计剩余时间（秒）：按最近单个文件的平均耗时和并发数估算；0 表示还无法估算
	OCREtaSec    int64          `json:"ocrEtaSec"`
	LegacyEtaSec int64          `json:"legacyEtaSec"`
	FastEtaSec   int64          `json:"fastEtaSec"`
	Legacy       SlotStatus     `json:"legacy"`
	OCR          SlotStatus     `json:"ocr"`
	Net          SlotStatus     `json:"net"`
	StalledSec   int64          `json:"stalledSec"` // >0：疑似卡住，这么多秒没有任何进展
	Rebuilding   bool           `json:"rebuilding"`
	Corrupt      bool           `json:"corrupt"`
	OnBattery    bool           `json:"onBattery"`
	Scanning     bool           `json:"scanning"`
	Processed    int64          `json:"processed"`
	Roots        []RootStatus   `json:"roots"`
	Volumes      []VolumeStatus `json:"volumes"`
	// Components 是外部组件（LibreOffice、OCR）的自检结果
	Components         []extract.ComponentStatus `json:"components"`
	ComponentsChecking bool                      `json:"componentsChecking"`
	// Maintenance 是后台整理索引库的进度（没有在整理时为空）；Space 是索引库占用空间
	Maintenance string      `json:"maintenance,omitempty"`
	Space       SpaceStatus `json:"space"`
}

// currentJobs 返回正在处理的文件，按进入列表的先后排序：已有的文件位置不变，新的排在后面（界面不跳动）。
func (m *Manager) currentJobs() []CurrentJob {
	s := &m.sched
	out := []CurrentJob{}
	if s.inflight == nil {
		return out
	}
	type item struct {
		CurrentJob
		queued time.Time
	}
	var items []item
	s.mu.Lock()
	for _, j := range s.inflight {
		items = append(items, item{CurrentJob{ID: j.f.ID, Path: j.path, Kind: classNames[j.class], Stage: j.stage, Progress: j.progress,
			Seconds: int64(awake.Since(j.since).Seconds())}, j.queued})
	}
	s.mu.Unlock()
	sort.Slice(items, func(i, k int) bool {
		if !items[i].queued.Equal(items[k].queued) {
			return items[i].queued.Before(items[k].queued)
		}
		return items[i].ID < items[k].ID
	})
	for _, it := range items {
		out = append(out, it.CurrentJob)
	}
	return out
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
	s.eachLane(func(l *lane) { busy = busy || l.wantWork.Load() })
	if !busy {
		return 0
	}
	d := awake.Since(time.Duration(s.lastActivity.Load())) // 电脑睡眠的时间不算「没有进展」
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
	st.Maintenance = m.Maintenance()
	st.Space = m.Space()
	if s.net != nil {
		fast := m.laneStat(prioFast)
		st.Active, st.Workers = fast.Used, fast.Cap
		st.Legacy, st.OCR = m.laneStat(prioLegacy), m.laneStat(prioOCR)
		st.Net.Used, st.Net.Cap = s.net.Stats()
		st.Processed = s.done.Load()
	}
	// 一次分组查询得到全部统计（只读覆盖索引）：合计、每个索引根、每个卷都由它汇总
	stats, _ := m.DB.StatsByRoot(prioLegacy, prioOCR)
	rootVol := map[int64]string{}
	for _, x := range stats {
		st.Totals = addCounts(st.Totals, x.Counts)
		st.OCRQueue += x.OCRPending
		st.LegacyQueue += x.LegacyPending
	}
	st.FastQueue = st.Totals.Pending - st.LegacyQueue - st.OCRQueue
	st.FastEtaSec = m.laneEta(prioFast, st.FastQueue)
	st.LegacyEtaSec = m.laneEta(prioLegacy, st.LegacyQueue)
	st.OCREtaSec = m.laneEta(prioOCR, st.OCRQueue)
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
		if x := stats[r.ID]; x != nil {
			rs.Counts, rs.LegacyPending, rs.OCRPending = x.Counts, x.LegacyPending, x.OCRPending
		}
		rootVol[r.ID] = r.VolumeID
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
		for id, x := range stats {
			if rootVol[id] == v.ID {
				vs.Files += x.Total
			}
		}
		vs.IndexBytes = m.indexBytes(v.ID)
		st.Volumes = append(st.Volumes, vs)
	}
	st.Components, st.ComponentsChecking = m.Components()
	st.Idle = s.idle.Load() && !st.Scanning && st.Totals.Pending == 0
	return st
}

func addCounts(a, b store.Counts) store.Counts {
	a.Total += b.Total
	a.Pending += b.Pending
	a.Done += b.Done
	a.Failed += b.Failed
	a.Encrypted += b.Encrypted
	a.MetaOnly += b.MetaOnly
	a.OCR += b.OCR
	return a
}

// indexBytes 返回卷的索引占用（提取文本的大小）。要读遍文本表，所以在后台计算、最多每分钟更新一次。
func (m *Manager) indexBytes(volID string) int64 {
	m.ibMu.Lock()
	defer m.ibMu.Unlock()
	if time.Since(m.ibAt) > time.Minute && !m.ibBusy {
		m.ibBusy = true
		go func() {
			vols, _ := m.DB.ListVolumes()
			res := map[string]int64{}
			for _, v := range vols {
				res[v.ID], _ = m.DB.VolumeIndexBytes(v.ID)
			}
			m.ibMu.Lock()
			m.ibCache, m.ibAt, m.ibBusy = res, time.Now(), false
			m.ibMu.Unlock()
		}()
	}
	return m.ibCache[volID]
}
