package index

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/chenonestar/zhaozhao/internal/doc"
	"github.com/chenonestar/zhaozhao/internal/extract"
	"github.com/chenonestar/zhaozhao/internal/logx"
	"github.com/chenonestar/zhaozhao/internal/worker"
)

// OCR 引擎对比：从 OCR 识别过的文件里随机抽样，用各个认字模型（PP-OCRv6 普通版、量化版、PP-OCRv4、Windows
// 自带的 OCR）各认一遍，和普通版比较认出的字、每页用时。测速用的样张是程序生成的，字迹清楚，各模型都认得
// 差不多（v4 也有 100%）；真实的扫描件上差别大得多（0.2.7 某位用户两万多页扫描件上，量化版比普通版少认 0.4%
// 的字，173 页少了一成以上），所以用用户自己的文件比。每个 PDF 只认前几个扫描页，各模型都用 1 个线程。

// ocrTestPages：每个 PDF 最多认几页。
const ocrTestPages = 5

// OCRTestModel 是一个认字模型在全部抽样文件上的结果。
type OCRTestModel struct {
	Name   string  `json:"name"`
	Files  int     `json:"files"`  // 认成功的文件数
	Failed int     `json:"failed"` // 失败的文件数
	Pages  int     `json:"pages"`
	Chars  int     `json:"chars"`  // 认出的字数（不计空白）
	MsPage float64 `json:"msPage"` // 每页平均用时（毫秒，1 个线程）
	// Agree 是和基准模型认出的字相同的比例（%，按编辑距离逐个文件算，按字数加权平均）；CharsVsBase 是认出的字数
	// 比基准模型多（正）或少（负）百分之几（两个都认成功的文件）
	Agree       float64 `json:"agree"`
	CharsVsBase float64 `json:"charsVsBase"`
	LowFiles    int     `json:"lowFiles"` // 和基准相同的不到 90% 的文件数
	// 不看先后顺序的比较（%，按相邻两字算，整份文字，按字数加权平均）：Cover 是基准认出的内容它认到了多少，
	// CoverBack 是它认出的内容基准也有多少。全文索引就是按相邻两字建的，Cover 差不多就是「基准能搜到的它也能搜到」
	// 的比例。0.2.8 的「相同的字」按先后顺序比，Windows OCR 读表格的顺序不同，比出来只有一半，分不清是顺序不同还是认错。
	Cover     float64 `json:"cover"`
	CoverBack float64 `json:"coverBack"`
	Garbled   int     `json:"garbled"` // 疑似认出乱码的文件数
}

// OCRTestFile 是一个抽样文件在各个模型上的结果。
type OCRTestFile struct {
	ID    int64             `json:"id"`
	Path  string            `json:"path"`
	Ext   string            `json:"ext"`
	Size  int64             `json:"size"`
	Pages int               `json:"pages"`
	Items []OCRTestFileItem `json:"items"`
}

// OCRTestFileItem 是一个文件用一个模型认的结果。Sample 是开头的一段文字（只在界面和诊断信息里另存的文件中显示）。
type OCRTestFileItem struct {
	Model     string  `json:"model"`
	Chars     int     `json:"chars"`
	Ms        int64   `json:"ms"`
	Agree     float64 `json:"agree"`
	Cover     float64 `json:"cover"`
	CoverBack float64 `json:"coverBack"`
	Garbled   bool    `json:"garbled,omitempty"`
	Err       string  `json:"err,omitempty"`
	Sample    string  `json:"sample,omitempty"`
}

// OCRTestReport 是一次 OCR 引擎对比的结果。
type OCRTestReport struct {
	StartedAt  time.Time      `json:"startedAt"`
	FinishedAt time.Time      `json:"finishedAt"`
	Sampled    int            `json:"sampled"`
	Canceled   bool           `json:"canceled,omitempty"`
	Base       string         `json:"base"` // 基准模型（PP-OCRv6 普通版，没有时用第一个能用的）
	Models     []OCRTestModel `json:"models"`
	Files      []OCRTestFile  `json:"files"`
}

// OCRTestState 是 OCR 引擎对比的进度和最近一次结果。
type OCRTestState struct {
	Running bool           `json:"running"`
	Done    int            `json:"done"`
	Total   int            `json:"total"`
	Note    string         `json:"note,omitempty"`
	Report  *OCRTestReport `json:"report,omitempty"`
}

type ocrTester struct {
	mu     sync.Mutex
	state  OCRTestState
	cancel context.CancelFunc
	loaded bool
}

func (m *Manager) ocrTestPath() string { return filepath.Join(m.DataDir, "ocr-test.json") }

// OCRTest 返回 OCR 引擎对比的进度和最近一次结果。
func (m *Manager) OCRTest() OCRTestState {
	t := &m.otest
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.loaded {
		t.loaded = true
		if b, err := os.ReadFile(m.ocrTestPath()); err == nil {
			var r OCRTestReport
			if json.Unmarshal(b, &r) == nil {
				t.state.Report = &r
			}
		}
	}
	return t.state
}

// CancelOCRTest 停止正在进行的 OCR 引擎对比（已完成的部分照样出报告）。
func (m *Manager) CancelOCRTest() {
	t := &m.otest
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cancel != nil {
		t.cancel()
	}
}

type ocrPick struct {
	id       int64
	vol, rel string
	ext      string
	size     int64
}

// StartOCRTest 在后台开始 OCR 引擎对比：抽 n 个 OCR 识别出了文字的文件。
func (m *Manager) StartOCRTest(n int) error {
	if n <= 0 {
		n = 20
	}
	if !m.ocrAvailable() || !m.Config().OCREnabled {
		return fmt.Errorf("OCR 组件没有安装或 OCR 已关闭")
	}
	models := m.ocrCfg.Models()
	if len(models) < 2 {
		return fmt.Errorf("这台电脑上只有一个认字模型（%s），没有可比的", extract.OCRModelName(models[0]))
	}
	m.OCRTest() // 先读入上次的结果
	t := &m.otest
	t.mu.Lock()
	if t.state.Running {
		t.mu.Unlock()
		return fmt.Errorf("OCR 引擎对比正在进行")
	}
	samples, err := m.ocrSamples(n)
	if err != nil {
		t.mu.Unlock()
		return err
	}
	if len(samples) == 0 {
		t.mu.Unlock()
		return fmt.Errorf("还没有 OCR 识别出文字的文件，或它们所在的磁盘未接入")
	}
	ctx, cancel := context.WithCancel(m.ctx)
	t.cancel = cancel
	t.state = OCRTestState{Running: true, Total: len(samples) * len(models), Report: t.state.Report}
	t.mu.Unlock()
	m.logf("开始 OCR 引擎对比：抽样 %d 个文件，比较 %d 个认字模型", len(samples), len(models))
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer cancel()
		r := m.runOCRTest(ctx, samples, models)
		if b, err := json.MarshalIndent(r, "", " "); err == nil {
			_ = os.WriteFile(m.ocrTestPath(), b, 0o600)
		}
		t.mu.Lock()
		t.state.Running, t.state.Report, t.state.Note, t.cancel = false, r, "", nil
		t.mu.Unlock()
		m.logf("OCR 引擎对比完成：%s", strings.ReplaceAll(OCRTestText(r, false), "\n", "；"))
	}()
	return nil
}

// ocrSamples 随机抽 n 个 OCR 识别出了文字的文件（蓄水池抽样；磁盘上的文件本身、所在磁盘已接入的）。
func (m *Manager) ocrSamples(n int) ([]ocrPick, error) {
	rows, err := m.DB.R.Query(`SELECT id, volume_id, rel_path, ext, size FROM files WHERE inner='' AND ocr=1 AND state=1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	offline := map[string]bool{}
	for _, v := range m.offlineVolumes() {
		offline[v] = true
	}
	var out []ocrPick
	seen := 0
	for rows.Next() {
		var s ocrPick
		if err := rows.Scan(&s.id, &s.vol, &s.rel, &s.ext, &s.size); err != nil {
			return nil, err
		}
		if offline[s.vol] {
			continue
		}
		seen++
		if len(out) < n {
			out = append(out, s)
		} else if j := rand.IntN(seen); j < n {
			out[j] = s
		}
	}
	return out, rows.Err()
}

// ocrTestRunner 用认字模型 model 认一个文件（测试里换成模拟的）。
type ocrTestRunner func(ctx context.Context, model, path, ext string) (doc.Result, time.Duration)

func (m *Manager) poolOCRTest() ocrTestRunner {
	pool := m.OCRPool
	if pool == nil {
		pool = m.Pool
	}
	base := m.extractOptions(m.Config(), true)
	base.OCRPageLimit = ocrTestPages
	return func(ctx context.Context, model, path, ext string) (doc.Result, time.Duration) {
		o := base
		o.OCR = m.ocrCfg.WithModel(model)
		o.OCR.Threads = 1
		t0 := time.Now()
		res := pool.Run(ctx, path, ext, o, worker.Timeouts{Idle: 10 * time.Minute, Total: time.Hour}, nil)
		return res, time.Since(t0)
	}
}

var newOCRTestRunner = func(m *Manager) ocrTestRunner { return m.poolOCRTest() }

func (m *Manager) runOCRTest(ctx context.Context, samples []ocrPick, models []string) *OCRTestReport {
	r := &OCRTestReport{StartedAt: time.Now(), Base: models[0]}
	run := newOCRTestRunner(m)
	type got struct {
		text string
		ms   int64
		err  string
	}
	results := make([]map[string]got, len(samples))
	paths := make([]string, len(samples))
	pages := make([]int, len(samples))
	for i, s := range samples {
		results[i] = map[string]got{}
		paths[i], _ = m.AbsPath(s.vol, s.rel)
	}
	done := 0
	// 按模型逐个来：同一个模型连着认，OCR 组件不用每个文件都重新启动。每个模型先认一遍样张（启动组件、
	// 加载模型，不计时）
	sample, cleanup, err := writeOCRSample()
	if err == nil {
		defer cleanup()
	}
	for _, model := range models {
		if ctx.Err() != nil {
			break
		}
		m.otest.mu.Lock()
		m.otest.state.Note = "正在用 " + extract.OCRModelName(model) + " 认"
		m.otest.mu.Unlock()
		if err == nil {
			run(ctx, model, sample, "png")
		}
		for i, s := range samples {
			if ctx.Err() != nil || paths[i] == "" {
				continue
			}
			res, d := run(ctx, model, paths[i], s.ext)
			if ctx.Err() != nil {
				break
			}
			g := got{ms: d.Milliseconds()}
			if res.Status == doc.StatusOK || res.Status == doc.StatusNoContent {
				g.text = res.Text
				pages[i] = max(pages[i], res.OCRPages, 1)
			} else {
				g.err = truncate(res.Err, 200)
			}
			results[i][model] = g
			done++
			m.otest.mu.Lock()
			m.otest.state.Done = done
			m.otest.mu.Unlock()
		}
	}
	r.Canceled = ctx.Err() != nil
	type acc struct {
		ms                            int64
		agreeW, coverW, backW, weight float64
		chars, baseChars, both        int
	}
	accs := map[string]*acc{}
	rows := map[string]*OCRTestModel{}
	for _, model := range models {
		accs[model], rows[model] = &acc{}, &OCRTestModel{Name: model}
	}
	for i, s := range samples {
		base, hasBase := results[i][r.Base]
		if len(results[i]) == 0 {
			continue
		}
		r.Sampled++
		f := OCRTestFile{ID: s.id, Path: paths[i], Ext: s.ext, Size: s.size, Pages: pages[i]}
		for _, model := range models {
			g, ok := results[i][model]
			if !ok {
				continue
			}
			row, a := rows[model], accs[model]
			it := OCRTestFileItem{Model: model, Ms: g.ms, Err: g.err}
			if g.err != "" {
				row.Failed++
				f.Items = append(f.Items, it)
				continue
			}
			chars := countChars(g.text)
			it.Chars, it.Sample = chars, sampleText(g.text, 300)
			row.Files++
			row.Pages += pages[i]
			row.Chars += chars
			a.ms += g.ms
			if hasBase && base.err == "" {
				bc := countChars(base.text)
				it.Agree = agreementCapped(g.text, base.text)
				it.Cover, it.CoverBack = 100*bigramCoverage(base.text, g.text), 100*bigramCoverage(g.text, base.text)
				if it.Garbled = garbled(g.text, base.text, it.Cover, it.CoverBack); it.Garbled {
					row.Garbled++
				}
				w := float64(max(bc, chars, 1))
				a.agreeW += it.Agree * w
				a.coverW += it.Cover * w
				a.backW += it.CoverBack * w
				a.weight += w
				a.chars += chars
				a.baseChars += bc
				a.both++
				if it.Agree < 90 {
					row.LowFiles++
				}
			}
			f.Items = append(f.Items, it)
		}
		r.Files = append(r.Files, f)
	}
	for _, model := range models {
		row, a := rows[model], accs[model]
		if row.Pages > 0 {
			row.MsPage = float64(a.ms) / float64(row.Pages)
		}
		if a.weight > 0 {
			row.Agree, row.Cover, row.CoverBack = a.agreeW/a.weight, a.coverW/a.weight, a.backW/a.weight
		}
		if a.baseChars > 0 {
			row.CharsVsBase = 100 * float64(a.chars-a.baseChars) / float64(a.baseChars)
		}
		r.Models = append(r.Models, *row)
	}
	r.FinishedAt = time.Now()
	return r
}

// garbled 判断 text 是不是认出了乱码：基准认出的大多是汉字，它的汉字却不到一半；或者认出了不少字，和基准
// 却几乎没有共同的内容（0.2.8 某位用户的一个文件，Windows OCR 认出的字比 PP-OCRv6 还多，相同的却不到 1%）。
func garbled(text, base string, cover, back float64) bool {
	if countChars(text) < 50 {
		return false
	}
	return (hanRatio(text) < 0.5 && hanRatio(base) >= 0.7) || (cover < 10 && back < 10)
}

// hanRatio 返回不计空白时汉字所占的比例。
func hanRatio(s string) float64 {
	n, han := 0, 0
	for _, r := range s {
		if unicode.IsSpace(r) {
			continue
		}
		n++
		if unicode.Is(unicode.Han, r) {
			han++
		}
	}
	if n == 0 {
		return 0
	}
	return float64(han) / float64(n)
}

// countChars 返回不计空白的字数。
func countChars(s string) int {
	n := 0
	for _, r := range s {
		if r > ' ' && r != '　' {
			n++
		}
	}
	return n
}

// sampleText 返回开头 n 个字（空白合并成一个空格）。
func sampleText(s string, n int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return string(r)
}

// agreementCapped 和 agreement 相同，但只比较前 8000 个字（编辑距离的计算量是两段长度之积）。
func agreementCapped(a, b string) float64 {
	const maxRunes = 8000
	cut := func(s string) string {
		if r := []rune(s); len(r) > maxRunes {
			return string(r[:maxRunes])
		}
		return s
	}
	return agreement(cut(a), cut(b))
}

// OCRTestText 把报告写成文字（日志和诊断信息里用）。samples 时加上各模型认出的开头一段文字（是文件内容，
// 导出诊断信息时单独存一个文件）。文件路径用 logx.P 标记，导出诊断信息时可以换成代号。
func OCRTestText(r *OCRTestReport, samples bool) string {
	if r == nil {
		return "还没有做过 OCR 引擎对比"
	}
	var b strings.Builder
	if !samples {
		fmt.Fprintf(&b, "OCR 引擎对比 %s，抽样 %d 个文件（PDF 每个最多认 %d 页，各模型都用 1 个线程），用时 %.0f 秒", r.StartedAt.Format("2006-01-02 15:04"),
			r.Sampled, ocrTestPages, r.FinishedAt.Sub(r.StartedAt).Seconds())
		if r.Canceled {
			b.WriteString("（中途停止）")
		}
		b.WriteString("\n")
		for _, x := range r.Models {
			fmt.Fprintf(&b, "%s：认成功 %d 个文件 %d 页，失败 %d 个；每页 %.2f 秒；认出 %d 字", extract.OCRModelName(x.Name), x.Files, x.Pages, x.Failed,
				x.MsPage/1000, x.Chars)
			if x.Name != r.Base {
				fmt.Fprintf(&b, "，比 %s %+.2f%%，相同的字 %.2f%%，相同不到 90%% 的文件 %d 个；不看顺序：%s认出的它认到 %.2f%%，它认出的%s也有 %.2f%%",
					extract.OCRModelName(r.Base), x.CharsVsBase, x.Agree, x.LowFiles, extract.OCRModelName(r.Base), x.Cover, extract.OCRModelName(r.Base), x.CoverBack)
				if x.Garbled > 0 {
					fmt.Fprintf(&b, "；疑似认出乱码 %d 个文件", x.Garbled)
				}
			}
			b.WriteString("\n")
		}
	}
	if len(r.Files) > 0 {
		b.WriteString("\n各文件：\n")
		for _, f := range r.Files {
			fmt.Fprintf(&b, "#%d %s（%s，认了 %d 页）", f.ID, logx.P(f.Path), fmtBytes(f.Size), f.Pages)
			for _, it := range f.Items {
				switch {
				case it.Err != "":
					fmt.Fprintf(&b, " ｜ %s 失败：%s", extract.OCRModelName(it.Model), it.Err)
				case it.Model == r.Base:
					fmt.Fprintf(&b, " ｜ %s %d 字 %.1f 秒", extract.OCRModelName(it.Model), it.Chars, float64(it.Ms)/1000)
				default:
					fmt.Fprintf(&b, " ｜ %s %d 字 %.1f 秒 相同 %.1f%% 不看顺序 %.1f%%/%.1f%%", extract.OCRModelName(it.Model), it.Chars, float64(it.Ms)/1000,
						it.Agree, it.Cover, it.CoverBack)
					if it.Garbled {
						b.WriteString(" 疑似乱码")
					}
				}
			}
			b.WriteString("\n")
			if samples {
				for _, it := range f.Items {
					if it.Err == "" {
						fmt.Fprintf(&b, "  【%s】%s\n", extract.OCRModelName(it.Model), it.Sample)
					}
				}
			}
		}
	}
	return strings.TrimSpace(b.String())
}
