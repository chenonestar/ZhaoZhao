package index

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/chenonestar/zhaozhao/internal/store"
	"github.com/chenonestar/zhaozhao/internal/volume"
)

const volumePoll = 5 * time.Second

func (m *Manager) volumeLoop() {
	defer m.wg.Done()
	t := time.NewTicker(volumePoll)
	defer t.Stop()
	tick := 0
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
		}
		tick++
		m.refreshVolumes(false)
		m.periodicScans()
		if tick%720 == 0 { // 约每小时
			m.autoDeleteStale()
		}
	}
}

// probeNetwork 检查未映射盘符的 UNC 共享是否可达；用超时避免卡住。
func probeNetwork(unc string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ch := make(chan bool, 1)
	go func() {
		_, err := os.Stat(unc + `\`)
		ch <- err == nil
	}()
	select {
	case ok := <-ch:
		return ok
	case <-ctx.Done():
		return false
	}
}

// refreshVolumes 对比当前在线卷与已知卷，处理接入、拔出、盘符变化。
func (m *Manager) refreshVolumes(initial bool) {
	live := map[string]volume.Info{}
	for _, v := range m.Prov.List() {
		if _, dup := live[v.ID]; dup {
			m.noticeOnce("conflict:"+v.ID, Notice{Kind: "volume-conflict", Volume: v.ID,
				Message: "发现两块磁盘的卷 ID 相同（可能是克隆盘）：" + v.Label + "。不会自动合并，请只接入其中一块。"})
			continue
		}
		live[v.ID] = v
	}
	// 未映射盘符的网络共享
	m.mu.RLock()
	var probe []*volState
	for id, vs := range m.vols {
		if _, ok := live[id]; !ok && strings.HasPrefix(id, "unc:") && vs.known.Label != "" {
			probe = append(probe, vs)
		}
	}
	m.mu.RUnlock()
	for _, vs := range probe {
		if probeNetwork(vs.known.Label) {
			live[vs.known.ID] = volume.Info{ID: vs.known.ID, Label: vs.known.Label, Kind: volume.KindNetwork, Mount: vs.known.Label + `\`}
		}
	}

	type change struct {
		id   string
		info volume.Info
		on   bool
	}
	var changes []change
	now := time.Now().Unix()
	m.mu.Lock()
	for id, info := range live {
		vs, ok := m.vols[id]
		if !ok {
			vs = &volState{}
			m.vols[id] = vs
		}
		if !vs.online || vs.info.Mount != info.Mount {
			changes = append(changes, change{id, info, true})
		}
		vs.info, vs.online = info, true
	}
	for id, vs := range m.vols {
		if _, ok := live[id]; !ok && vs.online {
			vs.online = false
			changes = append(changes, change{id: id, on: false})
		}
	}
	m.mu.Unlock()

	for _, c := range changes {
		known, _ := m.DB.GetVolume(c.id)
		hasRoots := m.volumeHasRoots(c.id)
		if c.on {
			if known != nil {
				_ = m.DB.UpsertVolume(storeVolume(c.info, now))
				m.mu.Lock()
				m.vols[c.id].known = storeVolume(c.info, now)
				m.mu.Unlock()
			} else if !initial && m.baseline.Load() && !hasRoots {
				m.noticeOnce("new:"+c.id, Notice{Kind: "new-volume", Volume: c.id,
					Message: "检测到新接入的磁盘「" + c.info.Label + "」（" + c.info.Mount + "），是否加入索引？"})
			}
			if hasRoots {
				m.volumeOnline(c.id)
			}
		} else if hasRoots {
			m.volumeOffline(c.id)
		}
	}
	if initial {
		m.baseline.Store(true)
	}
}

func (m *Manager) noticeOnce(key string, n Notice) {
	m.noticeMu.Lock()
	if m.noticed == nil {
		m.noticed = map[string]bool{}
	}
	if m.noticed[key] {
		m.noticeMu.Unlock()
		return
	}
	m.noticed[key] = true
	m.noticeMu.Unlock()
	m.addNotice(n)
}

func storeVolume(i volume.Info, now int64) store.Volume {
	return store.Volume{ID: i.ID, Label: i.Label, Serial: i.Serial, GUID: i.GUID, Capacity: i.Capacity, Kind: i.Kind, LastMount: i.Mount, LastSeen: now}
}

func (m *Manager) volumeHasRoots(id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, rt := range m.roots {
		if rt.VolumeID == id {
			return true
		}
	}
	return false
}

func (m *Manager) rootsOf(volID string) []*rootRT {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*rootRT
	for _, rt := range m.roots {
		if rt.VolumeID == volID {
			out = append(out, rt)
		}
	}
	return out
}

// volumeOnline：卷接入或盘符变化。启动监听并后台增量对账（F-VOL-05）。
func (m *Manager) volumeOnline(id string) {
	m.logf("卷已接入: %s", id)
	for _, rt := range m.rootsOf(id) {
		m.unwatchRoot(rt)
		m.watchRoot(rt)
		if !rt.Paused {
			m.enqueueScan(scanReq{rootID: rt.ID})
		}
	}
	m.kick()
}

// volumeOffline：卷被拔出。释放监听句柄，保留全部索引（F-VOL-03、F-VOL-07）。
func (m *Manager) volumeOffline(id string) {
	m.logf("卷已离线: %s", id)
	for _, rt := range m.rootsOf(id) {
		m.unwatchRoot(rt)
	}
}

// periodicScans 对不能实时监听的卷（网络盘、可移动盘）做定时增量扫描（F-NET-02）。
func (m *Manager) periodicScans() {
	cfg := m.Config()
	m.mu.RLock()
	var due []int64
	now := time.Now().Unix()
	for id, rt := range m.roots {
		if rt.Paused || rt.vol == nil || !rt.vol.online {
			continue
		}
		var every int64
		switch rt.vol.info.Kind {
		case volume.KindNetwork:
			every = int64(cfg.NetworkScanMin) * 60
		case volume.KindRemovable:
			if !m.watchesRemovable() {
				every = 10 * 60
			}
		}
		if every > 0 && now-rt.LastScan >= every {
			due = append(due, id)
		}
	}
	m.mu.RUnlock()
	for _, id := range due {
		m.enqueueScan(scanReq{rootID: id})
	}
}

func (m *Manager) autoDeleteStale() {
	days := m.Config().AutoDeleteDays
	if days <= 0 {
		return
	}
	cut := time.Now().Add(-time.Duration(days) * 24 * time.Hour).Unix()
	vs, _ := m.DB.ListVolumes()
	for _, v := range vs {
		if o := m.volOf(v.ID); o != nil && !o.online && v.LastSeen > 0 && v.LastSeen < cut {
			m.logf("超过 %d 天未接入，删除卷索引: %s", days, v.Label)
			_ = m.DeleteVolume(v.ID)
		}
	}
}
