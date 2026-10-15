package index

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenonestar/zhaozhao/internal/extract"
	"github.com/chenonestar/zhaozhao/internal/logx"
	"github.com/chenonestar/zhaozhao/internal/norm"
	"github.com/chenonestar/zhaozhao/internal/store"
	"github.com/chenonestar/zhaozhao/internal/volume"
	"github.com/chenonestar/zhaozhao/internal/worker"
)

// Notice 是需要提示用户的事件，例如接入了从未索引过的移动硬盘。
type Notice struct {
	ID      int64  `json:"id"`
	Kind    string `json:"kind"` // new-volume | volume-conflict | corrupt | info
	Message string `json:"message"`
	Volume  string `json:"volume,omitempty"`
	Path    string `json:"path,omitempty"`
	Time    int64  `json:"time"`
}

type volState struct {
	info    volume.Info // 在线时 Mount 为当前挂载点；离线时为最后一次的挂载点
	known   store.Volume
	online  bool
	syncing atomic.Bool
}

// rootRT 是索引根的运行时信息。
type rootRT struct {
	store.Root
	vol   *volState
	opts  RootOpts
	rules *Rules
}

type scanReq struct {
	rootID int64
	sub    string // 卷内相对路径，空表示整个索引根
	force  bool   // 忽略大小/时间对账，全部重新入队
}

// scanState 记录某个索引根的扫描进度；各字段会被扫描协程和状态查询并发读写。
type scanState struct {
	running  atomic.Bool
	count    int64        // 原子访问
	lastDone atomic.Int64 // 最近一次完整扫描完成的 Unix 时间
}

// Manager 管理索引的整个生命周期。
type Manager struct {
	DB      *store.DB
	Prov    volume.Provider
	Pool    *worker.Pool
	OCRPool *worker.Pool // OCR 专用进程池（并发数小，避免多个 OCR 引擎同时占用内存）；为空时使用 Pool
	// LegacyPool 是老格式转换专用进程池（每个子进程常驻一个 LibreOffice）；为空时使用 Pool
	LegacyPool *worker.Pool

	ltest   legacyTester // 老格式测试
	maint   maintState   // 后台整理索引库
	ocrCfg  extract.OCRConfig
	DataDir string

	cfgMu sync.RWMutex
	cfg   Config

	mu    sync.RWMutex
	vols  map[string]*volState
	roots map[int64]*rootRT

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	pausedAll atomic.Bool
	kickCh    chan struct{}

	scanMu   sync.Mutex
	scanQ    chan scanReq
	scanSet  map[scanReq]bool
	scanInfo map[int64]*scanState

	sched schedState

	watch watchState

	noticeMu  sync.Mutex
	notices   []Notice
	noticeSeq int64
	noticed   map[string]bool

	corrupt  atomic.Bool
	checking atomic.Bool // 后台完整性检查进行中

	ibMu       sync.Mutex // 卷的索引占用（后台计算的缓存）
	ibCache    map[string]int64
	ibAt       time.Time
	ibBusy     bool
	rebuilding atomic.Bool
	startedAt  time.Time
	baseline   atomic.Bool // 首次卷扫描完成后才通知「新卷」

	compMu       sync.Mutex
	components   []extract.ComponentStatus
	compChecking bool
}

// New 创建管理器。pool 为 nil 时在进程内提取（测试用）。
func New(db *store.DB, prov volume.Provider, pool *worker.Pool, dataDir string) *Manager {
	m := &Manager{DB: db, Prov: prov, Pool: pool, DataDir: dataDir,
		vols: map[string]*volState{}, roots: map[int64]*rootRT{},
		kickCh: make(chan struct{}, 1), scanQ: make(chan scanReq, 1024),
		scanSet: map[scanReq]bool{}, scanInfo: map[int64]*scanState{}}
	m.watch.watched = map[int64]recursiveWatcher{}
	m.watch.pending = map[string]pendingEvent{}
	if m.Pool == nil {
		m.Pool = worker.NewPool("", 1)
	}
	m.cfg = m.loadConfig()
	logx.SetVerbose(m.cfg.Verbose())
	m.ocrCfg = extract.FindOCR()
	return m
}

func (m *Manager) configPath() string { return filepath.Join(m.DataDir, "config.json") }

func (m *Manager) loadConfig() Config {
	c := DefaultConfig(m.DataDir)
	if b, err := os.ReadFile(m.configPath()); err == nil {
		c.Version = 0 // 文件里没有版本号的是旧版本保存的
		_ = json.Unmarshal(b, &c)
		c = c.migrate()
	}
	return c.normalized()
}

// Config 返回当前设置的副本。
func (m *Manager) Config() Config {
	m.cfgMu.RLock()
	defer m.cfgMu.RUnlock()
	return m.cfg.normalized()
}

// SetConfig 保存设置。繁简归一变化时在后台重建全文索引。
func (m *Manager) SetConfig(c Config) error {
	c = c.normalized()
	c.Version = configVersion // 保存的总是当前格式，下次启动不会再按旧版本升级
	old := m.Config()
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(m.configPath(), b, 0o600); err != nil {
		return err
	}
	m.cfgMu.Lock()
	m.cfg = c
	m.cfgMu.Unlock()
	logx.SetVerbose(c.Verbose())
	if d := configDiff(old, c); d != "" {
		m.logf("设置已修改：%s", d)
	}
	m.applyWorkers()
	m.refreshRules()
	if old.Simplify != c.Simplify {
		go m.rebuildFTS()
	}
	if old.LegacyLOOnly != c.LegacyLOOnly && m.ctx != nil {
		m.syncLegacyQueue(c.LegacyLOOnly)
	}
	// 排除规则按内容比较（只改了某条规则的内容、条数没变，也要重新扫描）
	if old.MaxTextMB != c.MaxTextMB || old.MaxOtherMB != c.MaxOtherMB || !slices.Equal(old.ExcludeDirNames, c.ExcludeDirNames) ||
		!slices.Equal(old.ExcludePrefixes, c.ExcludePrefixes) || !slices.Equal(old.ExcludeGlobs, c.ExcludeGlobs) ||
		!slices.Equal(old.ExcludeExts, c.ExcludeExts) || old.FollowLinks != c.FollowLinks || old.SkipCloud != c.SkipCloud ||
		old.SniffUnknown != c.SniffUnknown {
		m.RescanAll()
	}
	return nil
}

// Norm 返回当前归一化选项（搜索与入库必须一致）。
func (m *Manager) Norm() norm.Options { return m.Config().NormOptions() }

// Start 启动后台任务：先识别在线卷，再对账、监听、调度。
func (m *Manager) Start() error {
	m.ctx, m.cancel = context.WithCancel(context.Background())
	m.startedAt = time.Now()
	if err := m.loadState(); err != nil {
		return err
	}
	if n, err := m.DB.DeleteOrphans(); err == nil && n > 0 {
		m.logf("清理了 %d 条不属于任何索引根的残留记录", n)
	}
	m.syncLegacyQueue(m.Config().LegacyLOOnly)
	m.retryProtectedXls()
	m.refreshVolumes(true)
	m.initWatch()
	m.startScanners(2)
	m.startScheduler()
	m.wg.Add(1)
	go m.volumeLoop()
	m.wg.Add(1)
	go m.maintainLoop()
	// 启动对账：处理未运行期间发生的变化（F-BG-02）
	m.RescanAll()
	return nil
}

// syncLegacyQueue 让待处理的老格式文件排在与设置相符的队列：使用内置解析时排在普通队列
// （先用内置解析器读取，读不了的再排到老格式转换队列），只用 LibreOffice 时排在老格式转换队列。
// 从 0.1.x 升级时，以前转换失败（多为超时）的老格式文件也重新排队，用内置解析器再试一次。
func (m *Manager) syncLegacyQueue(loOnly bool) {
	want, prio := "native", prioFast
	if loOnly {
		want, prio = "lo", prioLegacy
	}
	cur, _ := m.DB.GetMeta("legacy_mode")
	if cur == want {
		return
	}
	moved, retried, err := m.DB.MoveLegacy(extract.LegacyExts(), prio, cur == "" && !loOnly)
	if err != nil {
		m.logf("调整老格式文件队列失败: %v", err)
		return
	}
	_ = m.DB.SetMeta("legacy_mode", want)
	if cur == "" && moved+retried == 0 {
		return // 新建的索引库
	}
	m.logf("老格式处理方式：%s；%d 个待处理的老格式文件已调整队列，%d 个以前失败的重新排队",
		map[bool]string{true: "只用 LibreOffice 转换", false: "内置解析优先，解析不了的再用 LibreOffice"}[loOnly], moved, retried)
	m.kick()
}

// retryProtectedXls：0.2.0 把设了「保护工作簿」的 .xls（用默认密码加密，打开不需要密码）误记为「已加密」，
// 0.2.1 起能读出内容。升级后把这些文件重新排队一次；真正有打开密码的会再次记为已加密。
func (m *Manager) retryProtectedXls() {
	if v, _ := m.DB.GetMeta("xls_default_password"); v == "1" {
		return
	}
	prio := prioFast
	if m.Config().LegacyLOOnly {
		prio = prioLegacy
	}
	n, err := m.DB.RequeueEncrypted([]string{"xls", "xlt", "et", "ett"}, prio)
	if err != nil {
		m.logf("重新排队已加密的 .xls 失败: %v", err)
		return
	}
	_ = m.DB.SetMeta("xls_default_password", "1")
	if n > 0 {
		m.logf("%d 个标记为「已加密」的 Excel 文件重新排队：设了「保护工作簿」的文件现在能读出内容", n)
	}
}

// Stop 停止全部后台任务。
func (m *Manager) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	m.stopWatch()
	m.wg.Wait()
	m.Pool.Close()
	if m.OCRPool != nil {
		m.OCRPool.Close()
	}
	if m.LegacyPool != nil {
		m.LegacyPool.Close()
	}
}

func (m *Manager) loadState() error {
	vs, err := m.DB.ListVolumes()
	if err != nil {
		return err
	}
	m.mu.Lock()
	for _, v := range vs {
		st := &volState{known: v}
		st.info = volume.Info{ID: v.ID, Label: v.Label, Serial: v.Serial, GUID: v.GUID, Capacity: v.Capacity, Kind: v.Kind, Mount: v.LastMount}
		m.vols[v.ID] = st
	}
	m.mu.Unlock()
	return m.reloadRoots()
}

func (m *Manager) reloadRoots() error {
	rs, err := m.DB.ListRoots()
	if err != nil {
		return err
	}
	cfg := m.Config()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.roots = map[int64]*rootRT{}
	for _, r := range rs {
		rt := &rootRT{Root: r, vol: m.vols[r.VolumeID], opts: parseRootOpts(r.Opts)}
		rt.rules = newRules(cfg, rt.opts)
		m.roots[r.ID] = rt
	}
	return nil
}

func (m *Manager) refreshRules() {
	cfg := m.Config()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, rt := range m.roots {
		rt.rules = newRules(cfg, rt.opts)
	}
}

func (m *Manager) getRoot(id int64) *rootRT {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.roots[id]
}

// kick 唤醒各通道的派发器（有新文件、有名额空出来了）。
func (m *Manager) kick() {
	m.sched.eachLane(func(l *lane) {
		select {
		case l.kick <- struct{}{}:
		default:
		}
	})
}

// ---- 路径 ----

func (m *Manager) volOf(id string) *volState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.vols[id]
}

// volSnapshot 在锁内取出卷信息的副本，避免与卷监控协程并发读写。
func (m *Manager) volSnapshot(id string) (info volume.Info, known store.Volume, online, ok bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v := m.vols[id]
	if v == nil {
		return volume.Info{}, store.Volume{}, false, false
	}
	return v.info, v.known, v.online, true
}

func joinMount(mount, rel string) string {
	if rel == "" {
		return mount
	}
	return filepath.Join(mount, filepath.FromSlash(rel))
}

// AbsPath 把「卷 + 相对路径」换成当前真实路径；卷离线时 online 为 false。
func (m *Manager) AbsPath(volID, rel string) (string, bool) {
	v := m.volOf(volID)
	if v == nil {
		return "", false
	}
	m.mu.RLock()
	mount, on := v.info.Mount, v.online
	m.mu.RUnlock()
	return joinMount(mount, rel), on
}

// Display 实现 search.Resolver。
func (m *Manager) Display(f *store.File) (string, bool, string) {
	v := m.volOf(f.VolumeID)
	if v == nil {
		return filepath.FromSlash(f.RelPath), false, f.VolumeID
	}
	m.mu.RLock()
	mount, on, label := v.info.Mount, v.online, v.info.Label
	m.mu.RUnlock()
	p := joinMount(mount, f.RelPath)
	if f.Inner != "" {
		inner := f.Inner
		if filepath.Separator == '\\' {
			inner = strings.ReplaceAll(inner, "/", `\`)
		}
		p += " › " + inner
	}
	return p, on, label
}

// OpenTarget 返回打开文件所需的真实路径。容器内的文件需要调用方先解出到临时目录。
func (m *Manager) OpenTarget(f *store.File) (path string, err error) {
	p, on := m.AbsPath(f.VolumeID, f.RelPath)
	if !on {
		info, known, _, ok := m.volSnapshot(f.VolumeID)
		if !ok {
			return "", fmt.Errorf("文件所在的磁盘已不在索引中")
		}
		return "", &OfflineError{Label: info.Label, LastMount: known.LastMount, LastSeen: known.LastSeen}
	}
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("文件不存在或无法访问：%v", err)
	}
	return p, nil
}

// OfflineError 说明需要接入哪块盘才能打开文件（F-VOL-04）。
type OfflineError struct {
	Label     string
	LastMount string
	LastSeen  int64
}

func (e *OfflineError) Error() string {
	t := "未知"
	if e.LastSeen > 0 {
		t = time.Unix(e.LastSeen, 0).Format("2006-01-02 15:04")
	}
	return fmt.Sprintf("文件所在的磁盘当前未接入：请接入卷标为「%s」的磁盘（上次盘符 %s，最后在线时间 %s）", e.Label, strings.TrimRight(e.LastMount, `\/`), t)
}

// ---- 索引根 ----

func nestedIn(a, b string) bool { // a 是否位于 b 之内（含相等），都是卷内相对路径
	return b == "" || a == b || strings.HasPrefix(a, b+"/")
}

// AddRoot 添加索引根并开始建立索引。
func (m *Manager) AddRoot(path string) (*store.Root, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("无法访问该目录：%v", err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%s 不是目录", path)
	}
	info, rel, err := m.Prov.Detect(path)
	if err != nil {
		return nil, err
	}
	roots, _ := m.DB.ListRoots()
	for _, r := range roots {
		if r.VolumeID != info.ID {
			continue
		}
		if nestedIn(rel, r.RelPath) {
			return nil, fmt.Errorf("该目录已包含在索引根「%s」中", m.rootDisplay(r))
		}
		if nestedIn(r.RelPath, rel) {
			return nil, fmt.Errorf("该目录包含已有的索引根「%s」，请先移除它再添加上层目录", m.rootDisplay(r))
		}
	}
	if err := m.DB.UpsertVolume(store.Volume{ID: info.ID, Label: info.Label, Serial: info.Serial, GUID: info.GUID,
		Capacity: info.Capacity, Kind: info.Kind, LastMount: info.Mount, LastSeen: time.Now().Unix()}); err != nil {
		return nil, err
	}
	m.mu.Lock()
	vs := m.vols[info.ID]
	if vs == nil {
		vs = &volState{}
		m.vols[info.ID] = vs
	}
	vs.info, vs.online = info, true
	vs.known = store.Volume{ID: info.ID, Label: info.Label, LastMount: info.Mount, LastSeen: time.Now().Unix(), Kind: info.Kind}
	m.mu.Unlock()
	id, err := m.DB.AddRoot(info.ID, rel)
	if err != nil {
		return nil, err
	}
	if err := m.reloadRoots(); err != nil {
		return nil, err
	}
	m.saveRootsBackup()
	rt := m.getRoot(id)
	m.watchRoot(rt)
	m.enqueueScan(scanReq{rootID: id})
	r := rt.Root
	return &r, nil
}

func (m *Manager) rootDisplay(r store.Root) string {
	p, _ := m.AbsPath(r.VolumeID, r.RelPath)
	if p == "" {
		return r.RelPath
	}
	return p
}

// RemoveRoot 删除索引根及其全部索引数据。
func (m *Manager) RemoveRoot(id int64) error {
	if rt := m.getRoot(id); rt != nil {
		m.unwatchRoot(rt)
	}
	if err := m.DB.RemoveRoot(id); err != nil {
		return err
	}
	_ = m.reloadRoots() // 正在进行的扫描会在下一次检查时发现索引根已删除并停止
	m.saveRootsBackup()
	m.kick()
	return nil
}

func (m *Manager) PauseRoot(id int64, paused bool) error {
	if err := m.DB.SetRootPaused(id, paused); err != nil {
		return err
	}
	_ = m.reloadRoots()
	if !paused {
		m.enqueueScan(scanReq{rootID: id})
		m.kick()
	}
	return nil
}

func (m *Manager) SetRootOpts(id int64, o RootOpts) error {
	b, _ := json.Marshal(o)
	if err := m.DB.SetRootOpts(id, b); err != nil {
		return err
	}
	_ = m.reloadRoots()
	m.saveRootsBackup()
	m.enqueueScan(scanReq{rootID: id})
	return nil
}

// PauseAll / ResumeAll 暂停或恢复全部后台任务（F-BG-07）。
func (m *Manager) PauseAll()      { m.pausedAll.Store(true) }
func (m *Manager) ResumeAll()     { m.pausedAll.Store(false); m.kick() }
func (m *Manager) IsPaused() bool { return m.pausedAll.Load() }

// RescanAll 对全部索引根做增量对账。
func (m *Manager) RescanAll() {
	m.mu.RLock()
	var ids []int64
	for id, rt := range m.roots {
		if !rt.Paused {
			ids = append(ids, id)
		}
	}
	m.mu.RUnlock()
	for _, id := range ids {
		m.enqueueScan(scanReq{rootID: id})
	}
}

func (m *Manager) RescanRoot(id int64) { m.enqueueScan(scanReq{rootID: id}) }

// RebuildRoot 重新提取某个索引根的全部内容；id 为 0 表示全部（F-BG-12）。
func (m *Manager) RebuildRoot(id int64) error {
	if err := m.DB.ResetContent(id); err != nil {
		return err
	}
	if id == 0 {
		m.RescanAll()
	} else {
		m.enqueueScan(scanReq{rootID: id, force: true})
	}
	m.kick()
	return nil
}

// RetryFailed 重试失败的文件。
func (m *Manager) RetryFailed(ids []int64) error {
	if err := m.DB.RetryFailed(ids); err != nil {
		return err
	}
	m.kick()
	return nil
}

// DeleteVolume 删除某个卷的全部索引（F-VOL-08）。
func (m *Manager) DeleteVolume(id string) error {
	m.mu.RLock()
	for _, rt := range m.roots {
		if rt.VolumeID == id {
			m.mu.RUnlock()
			m.unwatchRoot(rt)
			m.mu.RLock()
		}
	}
	m.mu.RUnlock()
	if err := m.DB.RemoveVolume(id); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.vols, id)
	m.mu.Unlock()
	_ = m.reloadRoots()
	m.saveRootsBackup()
	return nil
}

// ---- 通知 ----

func (m *Manager) addNotice(n Notice) {
	m.noticeMu.Lock()
	defer m.noticeMu.Unlock()
	m.noticeSeq++
	n.ID, n.Time = m.noticeSeq, time.Now().Unix()
	m.notices = append(m.notices, n)
	if len(m.notices) > 50 {
		m.notices = m.notices[len(m.notices)-50:]
	}
}

// Notices 返回 id 之后的新通知。
func (m *Manager) Notices(after int64) []Notice {
	m.noticeMu.Lock()
	defer m.noticeMu.Unlock()
	var out []Notice
	for _, n := range m.notices {
		if n.ID > after {
			out = append(out, n)
		}
	}
	return out
}

// DismissNotice 不再提示某个新卷（用户选择了不加入）。
func (m *Manager) logf(format string, a ...any) { log.Printf(format, a...) }

// ---- 索引根备份：索引库损坏重建后可恢复索引根列表 ----

type rootBackup struct {
	Roots []struct {
		Volume store.Volume `json:"volume"`
		Rel    string       `json:"rel"`
		Opts   RootOpts     `json:"opts"`
	} `json:"roots"`
}

func (m *Manager) saveRootsBackup() {
	var b rootBackup
	rs, _ := m.DB.ListRoots()
	for _, r := range rs {
		v, _ := m.DB.GetVolume(r.VolumeID)
		if v == nil {
			continue
		}
		b.Roots = append(b.Roots, struct {
			Volume store.Volume `json:"volume"`
			Rel    string       `json:"rel"`
			Opts   RootOpts     `json:"opts"`
		}{*v, r.RelPath, parseRootOpts(r.Opts)})
	}
	data, _ := json.MarshalIndent(b, "", "  ")
	_ = os.WriteFile(filepath.Join(m.DataDir, "roots.json"), data, 0o600)
}

// RestoreRoots 从备份恢复索引根（索引库被重建之后调用），返回恢复的数量。
func RestoreRoots(db *store.DB, dataDir string) int {
	data, err := os.ReadFile(filepath.Join(dataDir, "roots.json"))
	if err != nil {
		return 0
	}
	var b rootBackup
	if json.Unmarshal(data, &b) != nil {
		return 0
	}
	n := 0
	for _, r := range b.Roots {
		if db.UpsertVolume(r.Volume) != nil {
			continue
		}
		id, err := db.AddRoot(r.Volume.ID, r.Rel)
		if err != nil {
			continue
		}
		o, _ := json.Marshal(r.Opts)
		_ = db.SetRootOpts(id, o)
		n++
	}
	return n
}

var _ = runtime.GOOS

// RefreshVolumes 立即重新识别在线卷（收到系统的设备变化通知时调用）。
func (m *Manager) RefreshVolumes() { m.refreshVolumes(false) }
