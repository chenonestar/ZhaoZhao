package index

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"database/sql"
	"github.com/chenonestar/zhaozhao/internal/extract"
	"github.com/chenonestar/zhaozhao/internal/store"
	"github.com/chenonestar/zhaozhao/internal/volume"
)

// recursiveWatcher 递归监听一个目录树，把变化的路径送到 Events。
type recursiveWatcher interface {
	Events() <-chan string
	// Overflow 在系统事件缓冲区溢出（丢事件）时触发，调用方应整体重新对账。
	Overflow() <-chan struct{}
	Close()
}

type watchState struct {
	mu      sync.Mutex
	watched map[int64]recursiveWatcher
	pending map[string]pendingEvent
}

type pendingEvent struct {
	rootID int64
	at     time.Time
}

const debounce = 700 * time.Millisecond

func (m *Manager) initWatch() {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		t := time.NewTicker(300 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-m.ctx.Done():
				return
			case <-t.C:
				m.flushEvents()
			}
		}
	}()
	m.mu.RLock()
	var rts []*rootRT
	for _, rt := range m.roots {
		rts = append(rts, rt)
	}
	m.mu.RUnlock()
	for _, rt := range rts {
		m.watchRoot(rt)
	}
}

func (m *Manager) stopWatch() {
	m.watch.mu.Lock()
	defer m.watch.mu.Unlock()
	for id, w := range m.watch.watched {
		w.Close()
		delete(m.watch.watched, id)
	}
}

// watchesRemovable：可移动盘上持有目录监听句柄会妨碍「安全弹出」，默认不监听，改用定时对账。
func (m *Manager) watchesRemovable() bool { return false }

func (m *Manager) watchRoot(rt *rootRT) {
	if rt == nil || rt.Paused || rt.vol == nil {
		return
	}
	m.mu.RLock()
	online, kind, mount := rt.vol.online, rt.vol.info.Kind, rt.vol.info.Mount
	m.mu.RUnlock()
	if !online || kind == volume.KindNetwork || (kind == volume.KindRemovable && !m.watchesRemovable()) {
		return
	}
	m.watch.mu.Lock()
	if _, ok := m.watch.watched[rt.ID]; ok {
		m.watch.mu.Unlock()
		return
	}
	m.watch.mu.Unlock()
	abs := joinMount(mount, rt.RelPath)
	w, err := newRecursiveWatcher(abs, rt.rules.SkipDir)
	if err != nil {
		m.logf("无法监听 %s（将依靠定时对账）: %v", abs, err)
		return
	}
	m.watch.mu.Lock()
	m.watch.watched[rt.ID] = w
	m.watch.mu.Unlock()
	rootID := rt.ID
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		for {
			select {
			case <-m.ctx.Done():
				return
			case p, ok := <-w.Events():
				if !ok {
					return
				}
				m.watch.mu.Lock()
				m.watch.pending[p] = pendingEvent{rootID: rootID, at: time.Now()}
				m.watch.mu.Unlock()
			case _, ok := <-w.Overflow():
				if !ok {
					return
				}
				m.logf("文件变化事件溢出，重新对账索引根 %d", rootID)
				m.enqueueScan(scanReq{rootID: rootID})
			}
		}
	}()
}

func (m *Manager) unwatchRoot(rt *rootRT) {
	if rt == nil {
		return
	}
	m.watch.mu.Lock()
	w := m.watch.watched[rt.ID]
	delete(m.watch.watched, rt.ID)
	m.watch.mu.Unlock()
	if w != nil {
		w.Close()
	}
}

func (m *Manager) flushEvents() {
	if m.pausedAll.Load() {
		return
	}
	now := time.Now()
	type item struct {
		path string
		ev   pendingEvent
	}
	var due []item
	m.watch.mu.Lock()
	for p, ev := range m.watch.pending {
		if now.Sub(ev.at) >= debounce {
			due = append(due, item{p, ev})
			delete(m.watch.pending, p)
		}
	}
	m.watch.mu.Unlock()
	for _, it := range due {
		if rt := m.getRoot(it.ev.rootID); rt != nil && !rt.Paused {
			m.handlePath(rt, it.path)
		}
	}
}

// relInVolume 把真实路径换成卷内相对路径；不在索引根之内返回 false。
func (m *Manager) relInVolume(rt *rootRT, abs string) (string, bool) {
	m.mu.RLock()
	mount := rt.vol.info.Mount
	m.mu.RUnlock()
	base := joinMount(mount, rt.RelPath)
	r, err := filepath.Rel(base, abs)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", false
	}
	if r == "." {
		return rt.RelPath, true
	}
	return relJoin(rt.RelPath, filepath.ToSlash(r)), true
}

// pathAllowed 检查路径的每一级目录是否被排除规则挡住。
func (m *Manager) pathAllowed(rt *rootRT, abs, rel string) bool {
	m.mu.RLock()
	mount := rt.vol.info.Mount
	m.mu.RUnlock()
	base := joinMount(mount, rt.RelPath)
	r, err := filepath.Rel(base, abs)
	if err != nil || r == "." {
		return err == nil
	}
	parts := strings.Split(r, string(filepath.Separator))
	cur := base
	for i, p := range parts {
		cur = filepath.Join(cur, p)
		if i < len(parts)-1 {
			if rt.rules.SkipDir(cur, p) {
				return false
			}
		} else if rt.rules.SkipDir(cur, p) || rt.rules.SkipFile(cur, p, extract.Ext(p)) {
			// 最后一级无法区分文件和目录：两种规则都挡才算排除
			if fi, err := os.Lstat(cur); err == nil && fi.IsDir() {
				return !rt.rules.SkipDir(cur, p)
			}
			return !rt.rules.SkipFile(cur, p, extract.Ext(p))
		}
	}
	return true
}

// handlePath 处理一个变化的路径：新增、修改、删除、目录改名。
func (m *Manager) handlePath(rt *rootRT, abs string) {
	rel, ok := m.relInVolume(rt, abs)
	if !ok || !m.pathAllowed(rt, abs, rel) {
		return
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			if n, _ := m.DB.DeleteSubtree(rt.VolumeID, rel); n > 0 {
				m.kick()
			}
		}
		return
	}
	if fi.IsDir() {
		if rel != rt.RelPath {
			m.enqueueScan(scanReq{rootID: rt.ID, sub: rel})
		}
		return
	}
	if !fi.Mode().IsRegular() {
		return
	}
	cfg := m.Config()
	ext := extract.Ext(fi.Name())
	if sig, ok := m.DB.GetSig(rt.VolumeID, rel); ok && sig.Size == fi.Size() && sig.MTime == fi.ModTime().Unix() {
		return
	}
	state, msg, prio, kind := m.decide(rt, cfg, abs, ext, fi.Size(), fi)
	f := &store.File{RootID: rt.ID, VolumeID: rt.VolumeID, RelPath: rel, Name: fi.Name(), Ext: ext, Kind: kind,
		Size: fi.Size(), MTime: fi.ModTime().Unix(), State: state, Err: msg, Prio: prio}
	err = m.DB.Tx(func(tx *sql.Tx) error { return store.UpsertFile(tx, f, m.nameFTS(f.Name)) })
	if errors.Is(err, store.ErrRootGone) {
		return
	}
	if err != nil {
		m.logf("写入索引失败: %v", err)
		return
	}
	m.kick()
}
