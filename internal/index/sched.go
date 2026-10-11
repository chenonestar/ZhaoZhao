package index

import (
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenonestar/zhaozhao/internal/doc"
	"github.com/chenonestar/zhaozhao/internal/extract"
	"github.com/chenonestar/zhaozhao/internal/norm"
	"github.com/chenonestar/zhaozhao/internal/store"
	"github.com/chenonestar/zhaozhao/internal/volume"
)

const hashMaxSize = 256 << 20

// schedState 是提取调度的运行时状态。
type schedState struct {
	mu       sync.Mutex
	cond     *sync.Cond
	limit    int // 当前允许的并发数
	running  int
	inflight map[int64]string // 处理中的文件 id → 路径
	jobs     chan store.File
	write    chan *prepared
	legacy   chan struct{}
	ocr      chan struct{}
	net      chan struct{}

	done      atomic.Int64
	doneByPri [4]atomic.Int64
	startedAt time.Time
	idle      atomic.Bool
}

// prepared 是已提取完毕、等待写入数据库的结果。
type prepared struct {
	file     store.File
	res      doc.Result
	state    int
	text     *store.Text
	fts      string
	nameFTS  string
	reuse    int64 // 复用该文件的提取结果（内容相同）
	requeue  bool  // 先保存文字层结果，再排到 OCR 队列
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
	s.cond = sync.NewCond(&s.mu)
	s.inflight = map[int64]string{}
	s.jobs = make(chan store.File)
	s.write = make(chan *prepared, 256)
	cfg := m.Config()
	s.limit = cfg.workers()
	lw := cfg.LegacyWorkers
	if lw < 1 {
		lw = 1
	}
	s.legacy = make(chan struct{}, lw)
	ow := cfg.OCRWorkers
	if ow < 1 {
		ow = 1
	}
	s.ocr = make(chan struct{}, ow)
	nc := cfg.NetworkReadConc
	if nc < 1 {
		nc = 2
	}
	s.net = make(chan struct{}, nc)
	s.startedAt = time.Now()

	max := runtime.NumCPU()
	if s.limit > max {
		max = s.limit
	}
	for i := 0; i < max; i++ {
		m.wg.Add(1)
		go m.worker()
	}
	m.wg.Add(2)
	go m.dispatcher()
	go m.writer()
}

// applyWorkers 在设置变化后调整并发数。
func (m *Manager) applyWorkers() {
	s := &m.sched
	if s.cond == nil {
		return
	}
	s.mu.Lock()
	s.limit = m.Config().workers()
	s.mu.Unlock()
	s.cond.Broadcast()
}

func (m *Manager) acquire() bool {
	s := &m.sched
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.running >= s.limit {
		if m.ctx.Err() != nil {
			return false
		}
		s.cond.Wait()
	}
	s.running++
	return true
}

func (m *Manager) release() {
	s := &m.sched
	s.mu.Lock()
	s.running--
	s.mu.Unlock()
	s.cond.Broadcast()
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

func (m *Manager) dispatcher() {
	defer m.wg.Done()
	s := &m.sched
	defer func() {
		// 唤醒阻塞的 worker 以便退出
		s.cond.Broadcast()
	}()
	for {
		if m.ctx.Err() != nil {
			return
		}
		if m.pausedAll.Load() {
			s.idle.Store(false)
			m.waitKick(time.Second)
			continue
		}
		maxPrio := prioOCR
		if m.Config().PauseOnBattery && onBattery() {
			maxPrio = prioFast
		}
		batch := 2*m.Config().workers() + 2
		files, err := m.DB.NextPending(batch, maxPrio, m.offlineVolumes(), m.inflightIDs())
		if err != nil {
			m.logf("读取待处理队列失败: %v", err)
			m.waitKick(2 * time.Second)
			continue
		}
		if len(files) == 0 {
			s.idle.Store(len(m.inflightIDs()) == 0)
			m.waitKick(3 * time.Second)
			continue
		}
		s.idle.Store(false)
		for _, f := range files {
			p, _ := m.AbsPath(f.VolumeID, f.RelPath)
			s.mu.Lock()
			s.inflight[f.ID] = p
			s.mu.Unlock()
			select {
			case s.jobs <- f:
			case <-m.ctx.Done():
				return
			}
		}
	}
}

func (m *Manager) waitKick(d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-m.kickCh:
	case <-t.C:
	case <-m.ctx.Done():
	}
}

func (m *Manager) worker() {
	defer m.wg.Done()
	s := &m.sched
	for {
		var f store.File
		select {
		case f = <-s.jobs:
		case <-m.ctx.Done():
			return
		}
		if !m.acquire() {
			return
		}
		m.process(f)
		m.release()
		s.mu.Lock()
		delete(s.inflight, f.ID)
		s.mu.Unlock()
		throttleForUser(m.ctx)
	}
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

func (m *Manager) extractOptions(cfg Config, withOCR bool) extract.Options {
	o := extract.DefaultOptions()
	if withOCR {
		o.OCR = m.ocrCfg
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

// process 提取一个文件并把结果交给写入协程。
func (m *Manager) process(f store.File) {
	s := &m.sched
	abs, online := m.AbsPath(f.VolumeID, f.RelPath)
	if !online {
		return // 卷离线：保持待处理，接入后继续
	}
	info, _, _, _ := m.volSnapshot(f.VolumeID)
	isNet := info.Kind == volume.KindNetwork
	if isNet {
		select {
		case s.net <- struct{}{}:
			defer func() { <-s.net }()
		case <-m.ctx.Done():
			return
		}
	}
	if extract.IsLegacy(f.Ext) {
		if !m.takeSlot(s.legacy) {
			return
		}
		defer func() { <-s.legacy }()
	}
	useOCR := f.Prio == prioOCR
	if useOCR {
		if !m.takeSlot(s.ocr) {
			return
		}
		defer func() { <-s.ocr }()
	}
	fi, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			_ = m.DB.DeleteFiles([]int64{f.ID})
			return
		}
		m.send(&prepared{file: f, res: doc.Result{Status: doc.StatusFailed, Err: "无权限访问或无法读取: " + err.Error()}})
		return
	}
	f.Size, f.MTime = fi.Size(), fi.ModTime().Unix()
	cfg := m.Config()

	hash := ""
	if !extract.IsContainer(f.Ext) {
		hash = hashFile(abs, f.Size)
		if hash != "" {
			if dup, _ := m.DB.FindDuplicate(hash, f.Size, f.ID); dup != 0 {
				if p := m.prepareReuse(f, dup, hash); p != nil {
					m.send(p)
					return
				}
			}
		}
	}
	opts := m.extractOptions(cfg, useOCR)
	pool, timeout := m.Pool, opts.Timeout+15*time.Second
	if extract.IsContainer(f.Ext) {
		timeout = 20 * time.Minute // 压缩包和邮箱可能很大
	}
	if useOCR {
		// OCR 按页计时（每页的超时在 OCR 组件内处理），整份文件给更长的总时限
		timeout = 45 * time.Minute
		if m.OCRPool != nil {
			pool = m.OCRPool
		}
	}
	res := pool.Extract(m.ctx, abs, f.Ext, opts, timeout)
	if m.ctx.Err() != nil {
		return
	}
	if res.Hash == "" {
		res.Hash = hash
	}
	m.send(m.prepare(f, res))
}

// takeSlot 等待一个受限资源（老格式转换、OCR）的名额。等待期间先让出并发令牌，
// 避免排队的慢任务占住令牌、拖住快任务。
func (m *Manager) takeSlot(ch chan struct{}) bool {
	select {
	case ch <- struct{}{}:
		return true
	default:
	}
	m.release()
	defer m.acquire()
	select {
	case ch <- struct{}{}:
		return true
	case <-m.ctx.Done():
		return false
	}
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

func ftsOf(o norm.Options, text string) string {
	return norm.FTSText(norm.Tokenize(o.Runes(text)))
}

func (m *Manager) prepare(f store.File, res doc.Result) *prepared {
	o := m.Norm()
	p := &prepared{file: f, res: res, state: stateOf(res), nameFTS: ftsOf(o, f.Name)}
	if res.NeedOCR && f.Prio != prioOCR && m.ocrEnabled(m.getRoot(f.RootID)) {
		p.requeue = true // 有页面没有文字层：先保存已有文字，再排到 OCR 队列
	}
	if res.Status == doc.StatusOK || len(res.Children) > 0 {
		p.text = &store.Text{Text: res.Text, LineBased: res.LineBased, Locs: res.Locs}
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
			pc.fts = ftsOf(o, c.Text)
		}
		p.children = append(p.children, pc)
	}
	return p
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

func (m *Manager) writer() {
	defer m.wg.Done()
	s := &m.sched
	for {
		var first *prepared
		select {
		case first = <-s.write:
		case <-m.ctx.Done():
			return
		}
		batch := []*prepared{first}
		timeout := time.After(150 * time.Millisecond)
	collect:
		for len(batch) < 64 {
			select {
			case p := <-s.write:
				batch = append(batch, p)
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
					m.logf("写入 %s 失败: %v", p.file.Name, err)
				}
			}
		}
		for _, p := range batch {
			s.done.Add(1)
			if p.file.Prio >= 1 && p.file.Prio <= 3 {
				s.doneByPri[p.file.Prio].Add(1)
			}
		}
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
		return nil
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
	if err := store.PutResult(tx, f.ID, p.state, p.res.Err, p.res.Hash, p.res.OCR, p.text, p.fts, p.nameFTS); err != nil {
		return err
	}
	if p.requeue {
		if _, err := tx.Exec(`UPDATE files SET state=?, err='', prio=? WHERE id=?`, store.StatePending, prioOCR, f.ID); err != nil {
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
		if err := store.UpsertFile(tx, &cf, c.nameFTS); err != nil {
			return err
		}
		if err := store.PutResult(tx, cf.ID, c.state, c.err, "", c.ocr, c.text, c.fts, c.nameFTS); err != nil {
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
