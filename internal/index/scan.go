package index

import (
	"errors"
	"fmt"
	"github.com/chenonestar/zhaozhao/internal/volume"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"database/sql"
	"github.com/chenonestar/zhaozhao/internal/extract"
	"github.com/chenonestar/zhaozhao/internal/logx"
	"github.com/chenonestar/zhaozhao/internal/store"
)

// 提取优先级（数值越小越先处理）：文本/Office 新格式/PDF 文字层 → 老格式转换 → OCR。
const (
	prioFast   = 1
	prioLegacy = 2
	prioOCR    = 3
)

func (m *Manager) startScanners(n int) {
	for i := 0; i < n; i++ {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			for {
				select {
				case <-m.ctx.Done():
					return
				case req := <-m.scanQ:
					m.scanMu.Lock()
					delete(m.scanSet, req)
					m.scanMu.Unlock()
					m.runScan(req)
				}
			}
		}()
	}
}

func (m *Manager) enqueueScan(req scanReq) {
	m.scanMu.Lock()
	if m.scanSet[req] {
		m.scanMu.Unlock()
		return
	}
	m.scanSet[req] = true
	m.scanMu.Unlock()
	select {
	case m.scanQ <- req:
	default:
		m.scanMu.Lock()
		delete(m.scanSet, req)
		m.scanMu.Unlock()
	}
}

func (m *Manager) scanStateOf(id int64) *scanState {
	m.scanMu.Lock()
	defer m.scanMu.Unlock()
	s := m.scanInfo[id]
	if s == nil {
		s = &scanState{}
		m.scanInfo[id] = s
	}
	return s
}

// decide 根据规则决定一个文件的初始处理方式。
func (m *Manager) decide(rt *rootRT, cfg Config, abs, ext string, size int64, info os.FileInfo) (state int, msg string, prio int, kind string) {
	kind = extract.KindOf(ext)
	state, prio = store.StatePending, prioFast
	switch {
	case !rt.rules.ContentAllowed(ext):
		return store.StateMetaOnly, "按该索引根的设置不提取内容", prio, kind
	case size == 0:
		return store.StateMetaOnly, "空文件", prio, kind
	case size > cfg.sizeLimit(ext):
		return store.StateMetaOnly, fmt.Sprintf("文件超过 %d MB，仅索引文件名", cfg.sizeLimit(ext)>>20), prio, kind
	case cfg.SkipCloud && info != nil && isCloudPlaceholder(info):
		return store.StateMetaOnly, "云盘中仅在线的文件，未下载，已跳过", prio, kind
	}
	switch {
	case extract.IsImage(ext):
		if !m.ocrEnabled(rt) {
			why := "OCR 未启用，仅索引文件名"
			if !m.ocrAvailable() {
				why = "未安装 OCR 组件，仅索引文件名"
			}
			return store.StateMetaOnly, why, prioOCR, kind
		}
		// 本地盘上太小的图片（图标、缩略图）扫描时就排除，不占 OCR 队列。网络盘上不读（每个文件都要走一趟网络）；
		// TIFF 可能有多页，只看第一页不准
		if abs != "" && ext != "tif" && ext != "tiff" && !m.onNetwork(rt) && extract.ImageTooSmall(abs, m.ocrCfg) {
			return store.StateMetaOnly, extract.ErrTooSmall, prioOCR, kind
		}
		return state, "", prioOCR, kind
	case extract.IsLegacy(ext) && cfg.LegacyLOOnly:
		return state, "", prioLegacy, kind
	case extract.Supported(ext):
		return state, "", prio, kind
	}
	if _, known := knownNoHandler[ext]; known {
		return store.StateMetaOnly, "暂不支持提取该格式的内容", prio, kind
	}
	if cfg.SniffUnknown && abs != "" {
		if head, err := readHead(abs, 8192); err == nil && extract.LooksLikeText(head) {
			return state, "", prio, extract.KindText
		}
	}
	return store.StateMetaOnly, "非文本文件，仅索引文件名", prio, kind
}

var knownNoHandler = map[string]bool{"pst": true, "ost": true}

func readHead(p string, n int) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b := make([]byte, n)
	k, err := f.Read(b)
	if k > 0 {
		err = nil
	}
	return b[:k], err
}

// ocrAvailable 说明 OCR 组件是否已安装。
func (m *Manager) ocrAvailable() bool { return m.ocrCfg.Enabled() }

// ocrEnabled 说明某个索引根是否做 OCR：总开关、组件可用、索引根没有单独关闭。
func (m *Manager) ocrEnabled(rt *rootRT) bool {
	if !m.Config().OCREnabled || !m.ocrAvailable() {
		return false
	}
	return rt == nil || rt.opts.OCR == nil || *rt.opts.OCR
}

func (m *Manager) nameFTS(name string) string {
	return m.Norm().FTS(name)
}

func relJoin(rel, name string) string {
	if rel == "" {
		return name
	}
	return rel + "/" + name
}

// runScan 遍历索引根，与数据库对账：新增和变化的文件入队，消失的文件从索引删除。
func (m *Manager) runScan(req scanReq) {
	rt := m.getRoot(req.rootID)
	if rt == nil || rt.Paused || rt.vol == nil {
		return
	}
	m.mu.RLock()
	online, mount, kind := rt.vol.online, rt.vol.info.Mount, rt.vol.info.Kind
	m.mu.RUnlock()
	if !online {
		return
	}
	startRel := rt.RelPath
	if req.sub != "" {
		startRel = req.sub
	}
	startAbs := joinMount(mount, startRel)
	st := m.scanStateOf(rt.ID)
	if req.sub == "" {
		st.running.Store(true)
		atomic.StoreInt64(&st.count, 0)
	}
	cfg := m.Config()
	sigs, err := m.DB.RootSigs(rt.ID)
	if err != nil {
		m.logf("读取索引失败: %v", err)
		return
	}
	scanStart := time.Now()
	logx.Debugf("开始扫描 %s（%s，已索引 %d 个文件）", logx.P(startAbs), KindName(kind), len(sigs))
	changed := 0
	seen := make(map[string]struct{}, len(sigs))
	rootGone := false
	var batch []*store.File
	flush := func() {
		if len(batch) == 0 {
			return
		}
		err := m.DB.Tx(func(tx *sql.Tx) error {
			for _, f := range batch {
				if err := store.UpsertFile(tx, f, m.nameFTS(f.Name)); err != nil {
					return err
				}
			}
			return nil
		})
		if errors.Is(err, store.ErrRootGone) {
			rootGone = true
		} else if err != nil {
			m.logf("写入索引失败: %v", err)
		}
		batch = batch[:0]
		m.kick()
	}
	aborted := false
	visit := func(abs, rel string, info fs.FileInfo) {
		name := info.Name()
		ext := extract.Ext(name)
		seen[rel] = struct{}{}
		atomic.AddInt64(&st.count, 1)
		size, mtime := info.Size(), info.ModTime().Unix()
		if sig, ok := sigs[rel]; ok && !req.force && sig.Size == size && sig.MTime == mtime {
			// 未变化。只处理「设置放宽后原先被跳过的文件」：扫描时按规则跳过、从没提取过的。
			// 提取过但没有文字（没有文字内容、图片太小等）的不再动，否则每次扫描都会重新处理一遍。
			if sig.State == store.StateMetaOnly && !sig.Indexed && extract.Supported(ext) {
				state, msg, prio, kind := m.decide(rt, cfg, abs, ext, size, info)
				if state == store.StatePending {
					batch = append(batch, &store.File{RootID: rt.ID, VolumeID: rt.VolumeID, RelPath: rel, Name: name, Ext: ext, Kind: kind,
						Size: size, MTime: mtime, State: state, Err: msg, Prio: prio})
				}
			}
			return
		}
		state, msg, prio, kind := m.decide(rt, cfg, abs, ext, size, info)
		changed++
		batch = append(batch, &store.File{RootID: rt.ID, VolumeID: rt.VolumeID, RelPath: rel, Name: name, Ext: ext, Kind: kind,
			Size: size, MTime: mtime, State: state, Err: msg, Prio: prio})
		if len(batch) >= 500 {
			flush()
		}
	}
	follow := cfg.FollowLinks
	if rt.opts.FollowLinks != nil {
		follow = *rt.opts.FollowLinks
	}
	w := &walker{m: m, rt: rt, follow: follow, visited: map[string]bool{}, visit: visit, abort: func() bool {
		if m.ctx.Err() != nil || rootGone {
			return true
		}
		// 索引根被删除或暂停：立即停止，不再写入
		if cur := m.getRoot(rt.ID); cur == nil || cur.Paused {
			aborted = true
			return true
		}
		if m.volOf(rt.VolumeID) == nil || !m.volOnlineNow(rt.VolumeID) {
			aborted = true
			return true
		}
		return false
	}}
	if fi, err := os.Stat(startAbs); err != nil {
		if os.IsNotExist(err) && req.sub != "" {
			// 子路径已不存在：删除其下全部条目
			_, _ = m.DB.DeleteSubtree(rt.VolumeID, startRel)
		} else {
			m.logf("无法访问索引根 %s: %v", logx.P(startAbs), err)
		}
		st.running.Store(false)
		return
	} else if !fi.IsDir() {
		visit(startAbs, startRel, fi)
	} else {
		w.walk(startAbs, startRel)
	}
	flush()
	if w.aborted() || aborted || rootGone || m.ctx.Err() != nil {
		if req.sub == "" {
			st.running.Store(false)
		}
		logx.Debugf("扫描中止 %s（索引根被移除或暂停、磁盘离线、程序退出），已看到 %d 个文件", logx.P(startAbs), len(seen))
		return // 中途卷被拔出或程序退出：不做删除对账
	}
	// 对账：数据库里有、磁盘上没有的文件
	var gone []int64
	for rel, sig := range sigs {
		if _, ok := seen[rel]; ok {
			continue
		}
		if startRel == "" || rel == startRel || len(rel) > len(startRel) && rel[:len(startRel)] == startRel && rel[len(startRel)] == '/' {
			gone = append(gone, sig.ID)
		}
	}
	for i := 0; i < len(gone); i += 500 {
		j := i + 500
		if j > len(gone) {
			j = len(gone)
		}
		if err := m.DB.DeleteFiles(gone[i:j]); err != nil {
			m.logf("删除失效索引失败: %v", err)
		}
	}
	if req.sub == "" {
		now := time.Now().Unix()
		_ = m.DB.SetRootScanned(rt.ID, now)
		st.lastDone.Store(now)
		st.running.Store(false)
		m.logf("扫描完成 %s：%d 个文件，新增或变化 %d 个，已删除 %d 个，用时 %s", logx.P(startAbs), len(seen), changed, len(gone), fmtDuration(time.Since(scanStart)))
	} else if changed > 0 || len(gone) > 0 {
		logx.Debugf("扫描完成 %s：新增或变化 %d 个，已删除 %d 个", logx.P(startAbs), changed, len(gone))
	}
	m.kick()
}

func (m *Manager) volOnlineNow(id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v := m.vols[id]
	return v != nil && v.online
}

// walker 遍历目录树，应用排除规则，处理符号链接和权限错误。
type walker struct {
	m       *Manager
	rt      *rootRT
	follow  bool
	visited map[string]bool
	visit   func(abs, rel string, info fs.FileInfo)
	abort   func() bool
	n       int
	stop    bool
}

func (w *walker) aborted() bool { return w.stop }

func (w *walker) walk(abs, rel string) {
	if w.stop {
		return
	}
	if w.follow {
		if real, err := filepath.EvalSymlinks(abs); err == nil {
			if w.visited[real] {
				return
			}
			w.visited[real] = true
		}
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		w.m.logf("跳过无法读取的目录 %s: %v", logx.P(abs), err) // F-IDX-09：记录日志，不弹窗
		return
	}
	for _, e := range entries {
		w.n++
		if w.n%256 == 0 && w.abort() {
			w.stop = true
			return
		}
		name := e.Name()
		cabs := filepath.Join(abs, name)
		crel := relJoin(rel, name)
		typ := e.Type()
		isDir := e.IsDir()
		if typ&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
			if !w.follow {
				continue
			}
			fi, err := os.Stat(cabs)
			if err != nil {
				continue
			}
			isDir = fi.IsDir()
		}
		if isDir {
			if w.rt.rules.SkipDir(cabs, name) {
				continue
			}
			w.walk(cabs, crel)
			if w.stop {
				return
			}
			continue
		}
		if !typ.IsRegular() && typ&(fs.ModeSymlink|fs.ModeIrregular) == 0 {
			continue
		}
		ext := extract.Ext(name)
		if w.rt.rules.SkipFile(cabs, name, ext) {
			continue
		}
		var info fs.FileInfo
		if typ&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
			info, err = os.Stat(cabs)
		} else {
			info, err = e.Info()
		}
		if err != nil {
			w.m.logf("跳过无法访问的文件 %s: %v", logx.P(cabs), err)
			continue
		}
		w.visit(cabs, crel, info)
	}
}

// onNetwork：索引根在网络盘上。
func (m *Manager) onNetwork(rt *rootRT) bool {
	if rt == nil {
		return false
	}
	info, _, _, _ := m.volSnapshot(rt.VolumeID)
	return info.Kind == volume.KindNetwork
}
