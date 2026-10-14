package index

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/chenonestar/zhaozhao/internal/doc"
	"github.com/chenonestar/zhaozhao/internal/extract"
	"github.com/chenonestar/zhaozhao/internal/logx"
	"github.com/chenonestar/zhaozhao/internal/worker"
)

// 老格式测试：从已索引的老格式文件中随机抽样，分别用内置解析器和 LibreOffice 提取，
// 比较成功率、用时和文字覆盖率。只统计数字，报告里不含文件内容。

// LegacyTestRow 是一个扩展名的统计。
type LegacyTestRow struct {
	Ext         string  `json:"ext"`
	Files       int     `json:"files"`
	NativeOK    int     `json:"nativeOk"`    // 内置解析成功
	NeedLO      int     `json:"needLo"`      // 内置解析不支持（正常索引时会转交 LibreOffice）
	LOOK        int     `json:"loOk"`        // LibreOffice 成功
	BothOK      int     `json:"bothOk"`      // 两者都成功
	OnlyLO      int     `json:"onlyLo"`      // 只有 LibreOffice 成功
	OnlyNative  int     `json:"onlyNative"`  // 只有内置解析成功
	AvgCoverage float64 `json:"avgCoverage"` // 两者都成功时，LibreOffice 提取的文字有多少被内置解析覆盖（按相邻两字计）
	MinCoverage float64 `json:"minCoverage"`
	LowCoverage int     `json:"lowCoverage"` // 覆盖率低于 90% 的文件数
	NativeMs    float64 `json:"nativeMs"`    // 平均用时（毫秒）
	LOMs        float64 `json:"loMs"`
}

// LegacyTestCase 是一个需要关注的文件：两者结果不一致或覆盖率低。
type LegacyTestCase struct {
	ID          int64   `json:"id"`
	Path        string  `json:"path"`
	Ext         string  `json:"ext"`
	Size        int64   `json:"size"`
	Native      string  `json:"native"`
	NativeErr   string  `json:"nativeErr,omitempty"`
	NativeChars int     `json:"nativeChars"`
	LO          string  `json:"lo"`
	LOErr       string  `json:"loErr,omitempty"`
	LOChars     int     `json:"loChars"`
	Coverage    float64 `json:"coverage"`
}

// LegacyTestReport 是一次老格式测试的结果。
type LegacyTestReport struct {
	StartedAt  time.Time        `json:"startedAt"`
	FinishedAt time.Time        `json:"finishedAt"`
	Sampled    int              `json:"sampled"`
	Canceled   bool             `json:"canceled,omitempty"`
	Rows       []LegacyTestRow  `json:"rows"`
	Problems   []LegacyTestCase `json:"problems"`
}

// LegacyTestState 是老格式测试的进度和最近一次结果。
type LegacyTestState struct {
	Running bool              `json:"running"`
	Done    int               `json:"done"`
	Total   int               `json:"total"`
	Err     string            `json:"err,omitempty"`
	Report  *LegacyTestReport `json:"report,omitempty"`
}

type legacyTester struct {
	mu     sync.Mutex
	state  LegacyTestState
	cancel context.CancelFunc
	loaded bool
}

func (m *Manager) legacyTestPath() string { return filepath.Join(m.DataDir, "legacy-test.json") }

// LegacyTest 返回老格式测试的进度和最近一次结果。
func (m *Manager) LegacyTest() LegacyTestState {
	t := &m.ltest
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.loaded {
		t.loaded = true
		if b, err := os.ReadFile(m.legacyTestPath()); err == nil {
			var r LegacyTestReport
			if json.Unmarshal(b, &r) == nil {
				t.state.Report = &r
			}
		}
	}
	return t.state
}

// CancelLegacyTest 停止正在进行的老格式测试（已完成的部分照样出报告）。
func (m *Manager) CancelLegacyTest() {
	t := &m.ltest
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cancel != nil {
		t.cancel()
	}
}

type legacySample struct {
	id       int64
	vol, rel string
	ext      string
	size     int64
}

// StartLegacyTest 在后台开始老格式测试：每种扩展名最多抽 perExt 个文件。
func (m *Manager) StartLegacyTest(perExt int) error {
	if perExt <= 0 {
		perExt = 20
	}
	m.LegacyTest() // 先读入上次的结果
	t := &m.ltest
	t.mu.Lock()
	if t.state.Running {
		t.mu.Unlock()
		return fmt.Errorf("老格式测试正在进行")
	}
	samples, err := m.legacySamples(perExt)
	if err != nil {
		t.mu.Unlock()
		return err
	}
	if len(samples) == 0 {
		t.mu.Unlock()
		return fmt.Errorf("还没有已索引的老格式文件（doc、xls、ppt、wps 等），或它们所在的磁盘未接入")
	}
	ctx, cancel := context.WithCancel(m.ctx)
	t.cancel = cancel
	t.state = LegacyTestState{Running: true, Total: len(samples), Report: t.state.Report}
	t.mu.Unlock()
	m.logf("开始老格式测试：抽样 %d 个文件", len(samples))
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer cancel()
		r := m.runLegacyTest(ctx, samples)
		if b, err := json.MarshalIndent(r, "", " "); err == nil {
			_ = os.WriteFile(m.legacyTestPath(), b, 0o600)
		}
		t.mu.Lock()
		t.state.Running = false
		t.state.Report = r
		t.cancel = nil
		t.mu.Unlock()
		m.logf("老格式测试完成：%s", strings.ReplaceAll(LegacyTestText(r, true), "\n", "；"))
	}()
	return nil
}

// legacySamples 按扩展名随机抽样（蓄水池抽样），只抽磁盘上的文件本身、所在磁盘已接入的。
func (m *Manager) legacySamples(perExt int) ([]legacySample, error) {
	exts := extract.LegacyExts()
	in := strings.TrimSuffix(strings.Repeat("?,", len(exts)), ",")
	args := make([]any, len(exts))
	for i, e := range exts {
		args[i] = e
	}
	rows, err := m.DB.R.Query(`SELECT id, volume_id, rel_path, ext, size FROM files WHERE inner='' AND size>0 AND ext IN (`+in+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	offline := map[string]bool{}
	for _, v := range m.offlineVolumes() {
		offline[v] = true
	}
	res := map[string][]legacySample{}
	seen := map[string]int{}
	for rows.Next() {
		var s legacySample
		if err := rows.Scan(&s.id, &s.vol, &s.rel, &s.ext, &s.size); err != nil {
			return nil, err
		}
		if offline[s.vol] {
			continue
		}
		seen[s.ext]++
		if n := seen[s.ext]; n <= perExt {
			res[s.ext] = append(res[s.ext], s)
		} else if j := rand.IntN(n); j < perExt {
			res[s.ext][j] = s
		}
	}
	var out []legacySample
	for _, e := range exts {
		out = append(out, res[e]...)
	}
	return out, rows.Err()
}

func (m *Manager) runLegacyTest(ctx context.Context, samples []legacySample) *LegacyTestReport {
	r := &LegacyTestReport{StartedAt: time.Now()}
	cfg := m.Config()
	rows := map[string]*LegacyTestRow{}
	type acc struct {
		nativeMs, loMs, cov float64
		nNative, nLO        int
	}
	accs := map[string]*acc{}
	for i, s := range samples {
		if ctx.Err() != nil {
			r.Canceled = true
			break
		}
		path, ok := m.AbsPath(s.vol, s.rel)
		if !ok {
			continue
		}
		row := rows[s.ext]
		if row == nil {
			row = &LegacyTestRow{Ext: s.ext, MinCoverage: 1}
			rows[s.ext] = row
			accs[s.ext] = &acc{}
		}
		a := accs[s.ext]
		opts := m.extractOptions(cfg, false)
		opts.LegacyMode = extract.LegacyNative
		t0 := time.Now()
		nr := m.Pool.Run(ctx, path, s.ext, opts, worker.Timeouts{Total: opts.Timeout + 15*time.Second}, nil)
		nd := time.Since(t0)
		pool := m.Pool
		if m.LegacyPool != nil {
			pool = m.LegacyPool
		}
		opts.LegacyMode = extract.LegacyLO
		t0 = time.Now()
		lr := pool.Run(ctx, path, s.ext, opts, worker.Timeouts{Total: extract.LegacyTimeout(opts.Timeout) + 30*time.Second}, nil)
		ld := time.Since(t0)
		if ctx.Err() != nil {
			r.Canceled = true
			break
		}
		r.Sampled++
		row.Files++
		nOK, lOK := nr.Status == doc.StatusOK, lr.Status == doc.StatusOK
		if nr.NeedConvert {
			row.NeedLO++
		} else {
			a.nativeMs += float64(nd.Milliseconds())
			a.nNative++
		}
		a.loMs += float64(ld.Milliseconds())
		a.nLO++
		c := LegacyTestCase{ID: s.id, Path: path, Ext: s.ext, Size: s.size, Native: legacyStatusName(nr), NativeErr: nr.Err,
			LO: legacyStatusName(lr), LOErr: lr.Err, NativeChars: len([]rune(nr.Text)), LOChars: len([]rune(lr.Text))}
		problem := false
		switch {
		case nOK && lOK:
			row.NativeOK++
			row.LOOK++
			row.BothOK++
			c.Coverage = bigramCoverage(lr.Text, nr.Text)
			a.cov += c.Coverage
			if c.Coverage < row.MinCoverage {
				row.MinCoverage = c.Coverage
			}
			if c.Coverage < 0.9 {
				row.LowCoverage++
				problem = true
			}
		case nOK:
			row.NativeOK++
			row.OnlyNative++
			problem = lr.Status != doc.StatusEncrypted
		case lOK:
			row.LOOK++
			row.OnlyLO++
			problem = !nr.NeedConvert // 内置解析不支持的会自动转交 LibreOffice，不算问题
		default:
			problem = nr.Status != lr.Status && !nr.NeedConvert
		}
		if problem && len(r.Problems) < 100 {
			r.Problems = append(r.Problems, c)
		}
		m.ltest.mu.Lock()
		m.ltest.state.Done = i + 1
		m.ltest.mu.Unlock()
	}
	for _, e := range extract.LegacyExts() {
		row := rows[e]
		if row == nil {
			continue
		}
		a := accs[e]
		if a.nNative > 0 {
			row.NativeMs = a.nativeMs / float64(a.nNative)
		}
		if a.nLO > 0 {
			row.LOMs = a.loMs / float64(a.nLO)
		}
		if row.BothOK > 0 {
			row.AvgCoverage = a.cov / float64(row.BothOK)
		} else {
			row.MinCoverage = 0
		}
		r.Rows = append(r.Rows, *row)
	}
	sort.SliceStable(r.Problems, func(i, j int) bool { return r.Problems[i].Coverage < r.Problems[j].Coverage })
	r.FinishedAt = time.Now()
	return r
}

func legacyStatusName(r doc.Result) string {
	if r.NeedConvert {
		return "不支持"
	}
	return statusNames[r.Status]
}

// loPlaceholder 是 LibreOffice 转换演示文稿时写出的页码、日期等占位符，不是文件内容。
var loPlaceholder = regexp.MustCompile(`<(number|date/time|header|footer)>`)

// bigramCoverage 计算 ref 中相邻两字有多少比例出现在 got 中。只在连续的文字和数字内部取相邻两字，
// 不跨空白和标点：两个提取器的换行、单元格分隔、文本框的先后顺序不同，不影响结果。
func bigramCoverage(ref, got string) float64 {
	const maxRunes = 400_000
	pairs := func(s string, f func([2]rune)) {
		var prev rune
		n := 0
		for _, r := range s {
			if n >= maxRunes {
				return
			}
			n++
			if !unicode.IsLetter(r) && !unicode.IsNumber(r) {
				prev = 0
				continue
			}
			r = unicode.ToLower(r)
			if prev != 0 {
				f([2]rune{prev, r})
			}
			prev = r
		}
	}
	have := map[[2]rune]struct{}{}
	pairs(got, func(p [2]rune) { have[p] = struct{}{} })
	total, hit := 0, 0
	pairs(loPlaceholder.ReplaceAllString(ref, " "), func(p [2]rune) {
		total++
		if _, ok := have[p]; ok {
			hit++
		}
	})
	if total == 0 {
		return 1
	}
	return float64(hit) / float64(total)
}

// LegacyTestText 把报告写成文字（日志和诊断信息里用）。brief 时只有各扩展名的统计。
// 文件路径用 logx.P 标记，导出诊断信息时可以换成代号。
func LegacyTestText(r *LegacyTestReport, brief bool) string {
	if r == nil {
		return "还没有做过老格式测试"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "老格式测试 %s，抽样 %d 个文件，用时 %.0f 秒", r.StartedAt.Format("2006-01-02 15:04"), r.Sampled, r.FinishedAt.Sub(r.StartedAt).Seconds())
	if r.Canceled {
		b.WriteString("（中途停止）")
	}
	b.WriteString("\n")
	for _, w := range r.Rows {
		fmt.Fprintf(&b, "%s：%d 个，内置解析成功 %d、不支持 %d，LibreOffice 成功 %d；都成功 %d 个，文字覆盖率平均 %.1f%%、最低 %.1f%%、低于 90%% 的 %d 个；只有 LibreOffice 成功 %d，只有内置成功 %d；平均用时 内置 %.0f ms、LibreOffice %.0f ms\n",
			w.Ext, w.Files, w.NativeOK, w.NeedLO, w.LOOK, w.BothOK, w.AvgCoverage*100, w.MinCoverage*100, w.LowCoverage, w.OnlyLO, w.OnlyNative, w.NativeMs, w.LOMs)
	}
	if brief {
		return strings.TrimSpace(b.String())
	}
	if len(r.Problems) > 0 {
		b.WriteString("\n需要关注的文件：\n")
		for _, c := range r.Problems {
			fmt.Fprintf(&b, "#%d %s（%s）内置：%s %d 字 %s ｜ LibreOffice：%s %d 字 %s ｜ 覆盖率 %.1f%%\n", c.ID, logx.P(c.Path), fmtBytes(c.Size),
				c.Native, c.NativeChars, c.NativeErr, c.LO, c.LOChars, c.LOErr, c.Coverage*100)
		}
	}
	return b.String()
}
