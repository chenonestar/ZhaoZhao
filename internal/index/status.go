package index

import (
	"sort"
	"sync/atomic"
	"time"

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

// Status 是整体状态。
type Status struct {
	Paused      bool           `json:"paused"`
	Idle        bool           `json:"idle"`
	Workers     int            `json:"workers"`
	Active      int            `json:"active"`
	Current     []string       `json:"current"`
	Totals      store.Counts   `json:"totals"`
	OCRQueue    int64          `json:"ocrQueue"`
	LegacyQueue int64          `json:"legacyQueue"`
	OCREtaSec   int64          `json:"ocrEtaSec"`
	Rebuilding  bool           `json:"rebuilding"`
	Corrupt     bool           `json:"corrupt"`
	OnBattery   bool           `json:"onBattery"`
	Scanning    bool           `json:"scanning"`
	Processed   int64          `json:"processed"`
	Roots       []RootStatus   `json:"roots"`
	Volumes     []VolumeStatus `json:"volumes"`
}

// Status 汇总当前状态。
func (m *Manager) Status() Status {
	s := &m.sched
	st := Status{Paused: m.pausedAll.Load(), Rebuilding: m.rebuilding.Load(), Corrupt: m.corrupt.Load(),
		Workers: m.Config().workers(), OnBattery: onBattery(), Roots: []RootStatus{}, Volumes: []VolumeStatus{}, Current: []string{}}
	if s.cond != nil {
		s.mu.Lock()
		st.Active = s.running
		for id, p := range s.inflight {
			_ = id
			st.Current = append(st.Current, p)
		}
		s.mu.Unlock()
		sort.Strings(st.Current)
		if len(st.Current) > 8 {
			st.Current = st.Current[:8]
		}
		st.Processed = s.done.Load()
	}
	st.Totals, _ = m.DB.AllCounts()
	st.OCRQueue, _ = m.DB.PendingByPrio(prioOCR)
	st.LegacyQueue, _ = m.DB.PendingByPrio(prioLegacy)
	if st.OCRQueue > 0 {
		if n := s.doneByPri[prioOCR].Load(); n > 0 {
			secs := m.sched.sinceStart()
			if secs > 0 {
				st.OCREtaSec = int64(float64(st.OCRQueue) / (float64(n) / secs))
			}
		}
	}

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
	st.Idle = s.idle.Load() && !st.Scanning && st.Totals.Pending == 0
	return st
}

func (s *schedState) sinceStart() float64 {
	if s.startedAt.IsZero() {
		return 0
	}
	return timeSince(s.startedAt)
}

func timeSince(t time.Time) float64 { return time.Since(t).Seconds() }
