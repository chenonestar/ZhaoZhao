package index

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chenonestar/zhaozhao/internal/extract"
	"github.com/chenonestar/zhaozhao/internal/logx"
	"github.com/chenonestar/zhaozhao/internal/store"
)

// 后台整理索引库（F-BG-13）：
//   - 用已提取的文本训练压缩词典；把旧格式（0.1.x 的 zlib）或没用词典的文本重新压缩；
//     把内容相同的文件各存的一份文本合并成一份；
//   - 空闲时把 WAL 写回主文件并截短，把空闲页还给磁盘（增量回收模式的索引库）。
// 进度记在索引库里，退出后下次启动接着做；索引繁忙时放慢，不和提取抢资源。

// maintainDelay 是启动后开始整理前的等待（避开启动时的扫描高峰）；测试里调短。
var maintainDelay = 20 * time.Second

// recycleKeep：删除的文件的提取结果在回收区保留多久（期间出现内容相同的文件就直接复用）。
const recycleKeep = 3 * 24 * time.Hour

// dedupeEvery：空闲时每隔这么久再检查一次有没有内容相同却各存一份的文本
// （同时提取两个相同的文件时会各存一份）。
const dedupeEvery = 24 * time.Hour

type maintState struct {
	mu           sync.Mutex
	stage        string // 正在做的事，空表示没有
	recompressed int
	deduped      int
	saved        int64
	compacting   bool
	lastCompact  string
}

// Maintenance 返回后台整理的进度说明（没有在整理时为空）。
func (m *Manager) Maintenance() string {
	t := &m.maint
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stage == "" {
		return ""
	}
	s := t.stage
	var parts []string
	if t.recompressed > 0 {
		parts = append(parts, fmt.Sprintf("已重新压缩 %d 份文本", t.recompressed))
	}
	if t.deduped > 0 {
		parts = append(parts, fmt.Sprintf("%d 个内容相同的文件改为共用一份文本", t.deduped))
	}
	if t.saved > 1<<20 {
		parts = append(parts, "节省约 "+fmtBytes(t.saved))
	}
	if len(parts) > 0 {
		s += "：" + strings.Join(parts, "，")
	}
	return s
}

func (m *Manager) setMaint(stage string) {
	m.maint.mu.Lock()
	m.maint.stage = stage
	m.maint.mu.Unlock()
}

func (m *Manager) addMaint(st store.MigrateStats) {
	m.maint.mu.Lock()
	m.maint.recompressed += st.Recompressed
	m.maint.deduped += st.Deduped
	m.maint.saved += st.SavedBytes
	m.maint.mu.Unlock()
}

func (m *Manager) sleep(d time.Duration) bool {
	select {
	case <-m.ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// throttle 在两批之间稍作停顿：索引繁忙时停得久一些。
func (m *Manager) throttle() bool {
	if m.sched.idle.Load() {
		return m.sleep(5 * time.Millisecond)
	}
	return m.sleep(300 * time.Millisecond)
}

func (m *Manager) maintainLoop() {
	defer m.wg.Done()
	if !m.sleep(maintainDelay) {
		return
	}
	for {
		if n, err := m.DB.PurgeRecycled(time.Now().Add(-recycleKeep)); err == nil && n > 0 {
			logx.Debugf("回收区：删除 %d 份过期的文本", n)
		}
		m.recheckPDFs()
		m.migrateTexts()
		if m.sched.idle.Load() {
			m.mergeFTS()
		}
		if m.sched.idle.Load() {
			m.housekeeping()
		}
		if !m.sleep(time.Minute) {
			return
		}
	}
}

// migrateTexts 训练词典、合并重复文本、重新压缩旧格式文本。每一步都可以中途退出，下次接着做。
func (m *Manager) migrateTexts() {
	db := m.DB
	if !db.HasDict() {
		if ok, err := db.TrainDict(); err != nil {
			m.logf("训练压缩词典失败: %v", err)
		} else if ok {
			m.logf("已用本机已提取的文本训练出压缩词典，之后的文本都用它压缩")
		}
	}
	defer m.setMaint("")
	start := time.Now()
	var total store.MigrateStats
	add := func(st store.MigrateStats) {
		m.addMaint(st)
		total.Recompressed += st.Recompressed
		total.Deduped += st.Deduped
		total.SavedBytes += st.SavedBytes
	}

	last, _ := db.GetMeta("texts_deduped_at")
	t, _ := strconv.ParseInt(last, 10, 64)
	if t == 0 || (time.Since(time.Unix(t, 0)) > dedupeEvery && m.sched.idle.Load()) {
		m.setMaint("正在整理索引库")
		for m.ctx.Err() == nil {
			g, st, err := db.DedupeBatch(200)
			if err != nil {
				m.logf("合并重复文本失败: %v", err)
				return
			}
			add(st)
			if g == 0 {
				_ = db.SetMeta("texts_deduped_at", strconv.FormatInt(time.Now().Unix(), 10))
				break
			}
			if !m.throttle() {
				return
			}
		}
	}

	if db.HasDict() {
		if done, _ := db.GetMeta("texts_recompressed"); done != "1" {
			m.setMaint("正在整理索引库（升级为新的压缩格式）")
			v, _ := db.GetMeta("recompress_after")
			after, _ := strconv.ParseInt(v, 10, 64)
			for m.ctx.Err() == nil {
				next, st, err := db.RecompressBatch(after, 100)
				if err != nil {
					m.logf("重新压缩文本失败: %v", err)
					return
				}
				add(st)
				if next == 0 {
					_ = db.SetMeta("texts_recompressed", "1")
					break
				}
				after = next
				_ = db.SetMeta("recompress_after", strconv.FormatInt(after, 10))
				if !m.throttle() {
					return
				}
			}
		}
	}
	if total.Recompressed+total.Deduped > 0 {
		sp := db.Space()
		m.logf("索引库整理完成：重新压缩 %d 份文本，%d 个内容相同的文件改为共用一份文本，节省约 %s，用时 %s；目前可回收 %s%s",
			total.Recompressed, total.Deduped, fmtBytes(total.SavedBytes), fmtDuration(time.Since(start)), fmtBytes(sp.FreeBytes),
			map[bool]string{true: "（空闲时自动归还磁盘）", false: "（可在「索引」页点「压缩索引库」归还磁盘）"}[sp.Incremental])
	}
}

// recheckPDFs：0.2.1 起，扫描页上叠了少量文字（页码、电子章）的页、文字层是乱码的页也会 OCR。
// 之前已提取的 PDF 里可能有这两种情况，后台复查一次，把可疑的重新排队：
//   - 文字极少（每 KB 不到 1 个字）的较大 PDF：多半是扫描件，只取到了叠加的少量文字；
//   - 提取出的文字是乱码。
//
// 重新提取完成前，原来的文字照样能搜到。
func (m *Manager) recheckPDFs() {
	db := m.DB
	if v, _ := db.GetMeta("pdf_recheck"); v == "done" {
		return
	}
	if !m.ocrAvailable() || !m.Config().OCREnabled {
		return // 不能 OCR 时重新提取也没有用：等开了 OCR 再复查
	}
	v, _ := db.GetMeta("pdf_recheck")
	after, _ := strconv.ParseInt(v, 10, 64)
	total := 0
	for m.ctx.Err() == nil {
		batch, err := db.DonePDFs(after, 300)
		if err != nil {
			m.logf("复查 PDF 失败: %v", err)
			return
		}
		if len(batch) == 0 {
			_ = db.SetMeta("pdf_recheck", "done")
			if total > 0 {
				m.logf("复查以前提取的 PDF：%d 个可能是扫描件或文字层是乱码，已重新排队（会做 OCR）", total)
			}
			return
		}
		var ids []int64
		for _, p := range batch {
			suspect := p.Size > 64<<10 && int64(p.TextLen)*1000 < p.Size
			if !suspect {
				if t, err := db.GetText(p.ID); err == nil && t != nil && extract.GarbledText(t.Text) {
					suspect = true
				}
			}
			if suspect {
				ids = append(ids, p.ID)
			}
		}
		if len(ids) > 0 {
			if err := db.Requeue(ids, prioFast); err != nil {
				m.logf("复查 PDF 失败: %v", err)
				return
			}
			total += len(ids)
			m.kick()
		}
		after = batch[len(batch)-1].ID
		_ = db.SetMeta("pdf_recheck", strconv.FormatInt(after, 10))
		if !m.throttle() {
			return
		}
	}
}

// 全文索引的段（store/fts.go）：FTS5 写入时会自动合并，平时段数不多，但已删除、已重新提取的文件在旧段里
// 留下的条目要等合并到那个段才清掉。空闲时把所有段合成一个：清掉这些残留，搜索也快一些（实测快约一成）。
// 要把整个全文索引重写一遍，所以只在值得时做（ftsNeedsMerge），并且至少隔一天。
const (
	ftsMergeEvery = 24 * time.Hour
	ftsMergeStep  = 1000 // 每步写入的页数（每页约 4 KB）：一步不到一秒，提取结果的写入只需稍等
)

// ftsMergeMin：全文索引不到这么多页（约 10 MB）时不必整理；测试里调小。
var ftsMergeMin int64 = 2560

// ftsNeedsMerge：最大的段以外的数据占一成以上，或者已删除的残留条目占 5% 以上。
func ftsNeedsMerge(fi store.FTSInfo) bool {
	if fi.Pages < ftsMergeMin {
		return false
	}
	return (fi.Pages-fi.Largest)*10 >= fi.Pages || fi.Deleted*20 >= fi.Entries
}

// mergeFTS 空闲时把全文索引合并成一个段：分小步做，索引忙起来就停下，下次空闲接着做（进度在索引库里）。
func (m *Manager) mergeFTS() {
	db := m.DB
	// 进行中的整理：「开始前索引库已用的字节数 要合并的页数」
	state, _ := db.GetMeta("fts_merge")
	var usedBefore, total int64
	resumed := state != ""
	if resumed {
		_, _ = fmt.Sscan(state, &usedBefore, &total)
	} else {
		if v, _ := db.GetMeta("fts_merged_at"); v != "" {
			if t, _ := strconv.ParseInt(v, 10, 64); time.Since(time.Unix(t, 0)) < ftsMergeEvery {
				return
			}
		}
		fi, err := db.FTSInfo()
		if err != nil {
			logx.Debugf("读取全文索引结构失败: %v", err)
			return
		}
		if !ftsNeedsMerge(fi) {
			return
		}
		_ = db.Checkpoint()
		sp := db.Space()
		usedBefore, total = sp.DBBytes-sp.FreeBytes, fi.Pages
		_ = db.SetMeta("fts_merge", fmt.Sprint(usedBefore, total))
		m.logf("开始整理全文索引：%d 个段、约 %s，其中已删除的残留条目 %d 个；空闲时分步合并成一个段",
			fi.Segments, fmtBytes(fi.Pages*4096), fi.Deleted)
	}
	defer m.setMaint("")
	done := func() {
		_ = db.SetMeta("fts_merge", "")
		_ = db.SetMeta("fts_merged_at", strconv.FormatInt(time.Now().Unix(), 10))
	}
	start := time.Now()
	for first := !resumed; m.ctx.Err() == nil && m.sched.idle.Load(); first = false {
		if fi, err := db.FTSInfo(); err == nil && total > 0 {
			m.setMaint(fmt.Sprintf("正在整理全文索引（%d%%）", min(99, max(0, 100-fi.Merging*100/total))))
		}
		more, err := db.MergeFTS(ftsMergeStep, first)
		if err != nil {
			m.logf("整理全文索引失败: %v", err)
			done() // 一天后再试
			return
		}
		if !more {
			done()
			_ = db.Checkpoint()
			sp := db.Space()
			msg := "全文索引整理完成"
			if fi, err := db.FTSInfo(); err == nil {
				msg += fmt.Sprintf("：%d 个段、约 %s", fi.Segments, fmtBytes(fi.Pages*4096))
			}
			if !resumed {
				msg += "，用时 " + fmtDuration(time.Since(start))
			}
			if saved := usedBefore - (sp.DBBytes - sp.FreeBytes); saved > 1<<20 {
				msg += "，索引库小了约 " + fmtBytes(saved)
			}
			m.logf("%s", msg)
			return
		}
		if _, _, err := db.CheckpointIfLarge(store.WALLimit, time.Second); err != nil {
			logx.Debugf("WAL 检查点失败: %v", err)
		}
		if !m.sleep(20 * time.Millisecond) {
			return
		}
	}
}

// housekeeping 空闲时：WAL 写回主文件并截短；增量回收模式下把空闲页还给磁盘。
func (m *Manager) housekeeping() {
	db := m.DB
	sp := db.Space()
	if sp.WALBytes > 0 {
		if err := db.Checkpoint(); err != nil {
			logx.Debugf("WAL 检查点失败: %v", err)
		}
	}
	if !sp.Incremental || sp.FreeBytes < 32<<20 {
		return
	}
	start := sp.FreeBytes
	for m.ctx.Err() == nil && m.sched.idle.Load() {
		if err := db.IncrementalVacuum(4096); err != nil {
			m.logf("归还磁盘空间失败: %v", err)
			return
		}
		sp = db.Space()
		if sp.FreeBytes == 0 || !m.sleep(50*time.Millisecond) {
			break
		}
	}
	_ = db.Checkpoint()
	if freed := start - sp.FreeBytes; freed > 0 {
		m.logf("空闲时归还了 %s 磁盘空间", fmtBytes(freed))
	}
}

// StartCompact 在后台压缩索引库（VACUUM）：把空闲空间全部还给磁盘，并改为增量回收模式。
// 期间可以搜索，新的提取结果等压缩完成后再写入。
func (m *Manager) StartCompact() error {
	t := &m.maint
	t.mu.Lock()
	if t.compacting {
		t.mu.Unlock()
		return fmt.Errorf("正在压缩索引库")
	}
	t.compacting = true
	t.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		before := m.DB.Space()
		m.logf("开始压缩索引库：%s，其中空闲 %s", fmtBytes(before.DBBytes+before.WALBytes), fmtBytes(before.FreeBytes))
		start := time.Now()
		err := m.DB.Vacuum()
		after := m.DB.Space()
		var msg string
		if err != nil {
			msg = "压缩索引库失败：" + err.Error()
			if strings.Contains(strings.ToLower(err.Error()), "full") {
				msg += "（磁盘空间不足：压缩时需要与索引库差不多大的临时空间）"
			}
		} else {
			msg = fmt.Sprintf("压缩完成：%s → %s，用时 %s", fmtBytes(before.DBBytes+before.WALBytes), fmtBytes(after.DBBytes+after.WALBytes), fmtDuration(time.Since(start)))
		}
		m.logf("%s", msg)
		t.mu.Lock()
		t.compacting = false
		t.lastCompact = msg
		t.mu.Unlock()
	}()
	return nil
}

// SpaceStatus 是界面显示的空间信息。
type SpaceStatus struct {
	store.SpaceInfo
	Compacting  bool   `json:"compacting"`
	LastCompact string `json:"lastCompact,omitempty"`
}

// Space 返回索引库空间概况（很快）。
func (m *Manager) Space() SpaceStatus {
	s := SpaceStatus{SpaceInfo: m.DB.Space()}
	m.maint.mu.Lock()
	s.Compacting, s.LastCompact = m.maint.compacting, m.maint.lastCompact
	m.maint.mu.Unlock()
	return s
}

// SpaceDetail 是详细的空间统计，大文本带上文件路径。
type SpaceDetail struct {
	*store.SpaceStats
	BiggestPaths []string `json:"biggestPaths"`
}

// SpaceStats 统计索引库空间（读遍整个索引库，较慢）。
func (m *Manager) SpaceStats() (*SpaceDetail, error) {
	return m.SpaceStatsContext(context.Background())
}

// SpaceStatsContext 同 SpaceStats，ctx 结束时中止。
func (m *Manager) SpaceStatsContext(ctx context.Context) (*SpaceDetail, error) {
	st, err := m.DB.StatsContext(ctx)
	if err != nil {
		return nil, err
	}
	d := &SpaceDetail{SpaceStats: st}
	for _, b := range st.Biggest {
		p := ""
		if f, err := m.DB.GetFile(b.FileID); err == nil && f != nil {
			p, _ = m.AbsPath(f.VolumeID, f.RelPath)
			if p == "" {
				p = f.RelPath
			}
			if f.Inner != "" {
				p += " › " + f.Inner
			}
		}
		d.BiggestPaths = append(d.BiggestPaths, p)
	}
	return d, nil
}

// SpaceText 把空间统计写成文字（诊断信息用）。
func SpaceText(d *SpaceDetail) string {
	var b strings.Builder
	fmt.Fprintf(&b, "索引库 %s，WAL %s，空闲 %s（%s）\n", fmtBytes(d.DBBytes), fmtBytes(d.WALBytes), fmtBytes(d.FreeBytes),
		map[bool]string{true: "增量回收模式", false: "需手动压缩才能归还"}[d.Incremental])
	for _, p := range d.Parts {
		fmt.Fprintf(&b, "  %s：%s\n", p.Name, fmtBytes(p.Bytes))
	}
	if f := d.FTS; f != nil {
		fmt.Fprintf(&b, "全文索引：%d 个段，最大的段占 %d%%，已删除的残留条目 %d 个（共 %d 个）\n",
			f.Segments, f.Largest*100/max(1, f.Pages), f.Deleted, f.Entries)
	}
	fmt.Fprintf(&b, "提取文本 %d 份，%d 个文件引用（%d 个与其他文件共用），共 %d 字；旧格式（待重新压缩）%d 份\n",
		d.Texts, d.TextFiles, d.TextFiles-d.Texts, d.RawChars, d.Legacy)
	b.WriteString("各扩展名的提取文本（压缩后）：\n")
	for _, x := range d.ByExt {
		fmt.Fprintf(&b, "  %s：%d 份，%s\n", x.Name, x.Count, fmtBytes(x.Bytes))
	}
	b.WriteString("最大的提取文本：\n")
	for i, x := range d.Biggest {
		p := ""
		if i < len(d.BiggestPaths) {
			p = d.BiggestPaths[i]
		}
		fmt.Fprintf(&b, "  %s：%s，%d 字，%d 个文件共用\n", logx.P(p), fmtBytes(x.Bytes), x.Chars, x.Sharers)
	}
	return b.String()
}
