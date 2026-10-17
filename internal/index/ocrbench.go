package index

import (
	"context"
	"crypto/sha1"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chenonestar/zhaozhao/internal/awake"
	"github.com/chenonestar/zhaozhao/internal/doc"
	"github.com/chenonestar/zhaozhao/internal/extract"
	"github.com/chenonestar/zhaozhao/internal/sysinfo"
	"github.com/chenonestar/zhaozhao/internal/worker"
)

// OCR 测速：用随包的样张（A4、200 dpi 的扫描页，tools/make_ocr_sample.py 生成）比较几种「进程数 × 线程数」
// 搭配，每种都识别同样多的页（等于线程预算），按全部识别完的时间算每分钟页数。每台电脑测一次，结果按电脑
// 保存在索引库里（便携版带到别的电脑会重新测）；用固定的样张，不同搭配、不同电脑的结果可以直接比较。

//go:embed ocrsample.png
var ocrSample []byte

// OCRBench 是一台电脑上的 OCR 测速结果。
type OCRBench struct {
	Machine string         `json:"machine"` // 电脑（处理器、核心数、内存）连同 OCR 组件的指纹
	CPU     string         `json:"cpu"`
	Cores   int            `json:"cores"`
	MemGB   float64        `json:"memGB"`
	At      int64          `json:"at"`     // 测速时间（Unix 秒）
	Budget  int            `json:"budget"` // 测速时的线程预算（物理核心数）
	Plans   []OCRBenchPlan `json:"plans"`
	Best    int            `json:"best"` // Plans 里最快的下标；-1 表示全部失败
	Err     string         `json:"err,omitempty"`
}

// OCRBenchPlan 是一种搭配的实测结果。
type OCRBenchPlan struct {
	Procs   int     `json:"procs"`
	Threads int     `json:"threads"`
	PerMin  float64 `json:"perMin"`        // 每分钟页数（0 表示失败）
	SlotMem uint64  `json:"slotMem"`       // 每个名额（OCR 进程 + 解析进程）的内存峰值，字节
	Err     string  `json:"err,omitempty"` // 失败的原因
}

func (p OCRBenchPlan) plan() ocrPlan { return ocrPlan{p.Procs, p.Threads} }

// best 返回最快的搭配；没有成功的结果时 ok 为 false。
func (b *OCRBench) best() (OCRBenchPlan, bool) {
	if b == nil || b.Best < 0 || b.Best >= len(b.Plans) {
		return OCRBenchPlan{}, false
	}
	return b.Plans[b.Best], true
}

// Summary 是写进日志、诊断信息的一行说明。
func (b *OCRBench) Summary() string {
	var parts []string
	for _, p := range b.Plans {
		if p.Err != "" {
			parts = append(parts, fmt.Sprintf("%s 失败（%s）", p.plan(), p.Err))
			continue
		}
		s := fmt.Sprintf("%s 每分钟 %.1f 页", p.plan(), p.PerMin)
		if p.SlotMem > 0 {
			s += fmt.Sprintf("（每个约 %d MB）", p.SlotMem>>20)
		}
		parts = append(parts, s)
	}
	s := strings.Join(parts, "，")
	if bp, ok := b.best(); ok {
		s += "；最快：" + bp.plan().String()
	}
	return s
}

// ocrMachineKey 是电脑连同 OCR 组件的指纹：处理器、核心数、内存（GB）、OCR 程序及其参数。
// 任何一项变了（换了电脑、加了内存、升级了 OCR 组件）都要重新测速。
func ocrMachineKey(m sysinfo.Machine, c extract.OCRConfig) string {
	comp := filepath.Base(c.Command)
	if st, err := os.Stat(c.Command); err == nil {
		comp += fmt.Sprintf("/%d", st.Size())
	}
	s := fmt.Sprintf("%s|%d|%d|%d|%s|%s", m.CPU, m.Cores, m.Logical, (m.MemTotal+(1<<29))>>30, comp, strings.Join(c.Args, " "))
	h := sha1.Sum([]byte(s))
	return hex.EncodeToString(h[:8])
}

const (
	metaOCRBench    = "ocr_bench"
	ocrBenchKeepMax = 8 // 最多保存几台电脑的结果
)

// loadOCRBench 读出指纹为 key 的电脑的测速结果；没有时返回 nil。
func (m *Manager) loadOCRBench(key string) *OCRBench {
	for _, b := range m.allOCRBench() {
		if b.Machine == key {
			b := b
			return &b
		}
	}
	return nil
}

func (m *Manager) allOCRBench() []OCRBench {
	v, _ := m.DB.GetMeta(metaOCRBench)
	var all []OCRBench
	_ = json.Unmarshal([]byte(v), &all)
	return all
}

// saveOCRBench 保存一台电脑的测速结果（b 为 nil 时删除指纹为 key 的结果），最近的在前。
func (m *Manager) saveOCRBench(key string, b *OCRBench) error {
	all := []OCRBench{}
	if b != nil {
		all = append(all, *b)
	}
	for _, x := range m.allOCRBench() {
		if x.Machine != key && len(all) < ocrBenchKeepMax {
			all = append(all, x)
		}
	}
	data, _ := json.Marshal(all)
	return m.DB.SetMeta(metaOCRBench, string(data))
}

// ocrBenchRunner 实测一种搭配：pages 页平均分给各个进程，返回每分钟页数和每个名额的内存峰值。
type ocrBenchRunner func(ctx context.Context, p ocrPlan, pages int) (perMin float64, slotMem uint64, err error)

// ocrBenchPages：每种搭配识别的页数。等于线程预算，各种搭配的工作量相同（N 个进程 × 1 线程时每个进程
// 识别 1 页，1 个进程 × N 线程时识别 N 页）；预算太小时加倍，结果才稳定。
func ocrBenchPages(budget int) int {
	if budget < 4 {
		return budget * 2
	}
	return budget
}

// runOCRBench 依次实测各种搭配。某种搭配失败（比如内存不够）不影响其余的；全部失败时 Best 为 -1。
func runOCRBench(ctx context.Context, budget int, cands []ocrPlan, run ocrBenchRunner, progress func(i, n int, p ocrPlan)) *OCRBench {
	b := &OCRBench{Budget: budget, Best: -1, At: time.Now().Unix()}
	pages := ocrBenchPages(budget)
	for i, c := range cands {
		if ctx.Err() != nil {
			break
		}
		if progress != nil {
			progress(i, len(cands), c)
		}
		perMin, mem, err := run(ctx, c, pages)
		p := OCRBenchPlan{Procs: c.procs, Threads: c.threads, PerMin: perMin, SlotMem: mem}
		if err != nil {
			p.PerMin, p.Err = 0, truncate(err.Error(), 200)
			b.Err = p.Err
		}
		b.Plans = append(b.Plans, p)
		if p.Err == "" && (b.Best < 0 || p.PerMin > b.Plans[b.Best].PerMin) {
			b.Best = len(b.Plans) - 1
		}
	}
	if b.Best >= 0 {
		b.Err = ""
	}
	return b
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// poolBench 用进程池实测：先让每个进程识别一页（启动 OCR 组件、加载模型，不计时），
// 再同时开始，按全部识别完的时间算每分钟页数（不含电脑睡眠的时间）。
func poolBench(pool *worker.Pool, opts extract.Options, sample string) ocrBenchRunner {
	return func(ctx context.Context, pl ocrPlan, pages int) (float64, uint64, error) {
		pool.SetSize(pl.procs)
		o := opts
		o.OCR.Threads = pl.threads
		o.ReportMem = true
		t := worker.Timeouts{Total: 10 * time.Minute}
		round := func(each int) (time.Duration, uint64, error) {
			var wg sync.WaitGroup
			var mu sync.Mutex
			var mem uint64
			var firstErr error
			start := awake.Now()
			for i := 0; i < pl.procs; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for k := 0; k < each; k++ {
						res := pool.Run(ctx, sample, "png", o, t, nil)
						mu.Lock()
						mem = max(mem, res.Mem)
						if res.Status != doc.StatusOK && firstErr == nil {
							firstErr = errors.New(res.Err)
						}
						mu.Unlock()
						if res.Status != doc.StatusOK {
							return
						}
					}
				}()
			}
			wg.Wait()
			return awake.Since(start), mem, firstErr
		}
		if _, _, err := round(1); err != nil {
			return 0, 0, err
		}
		each := max(1, pages/pl.procs)
		d, mem, err := round(each)
		if err != nil {
			return 0, 0, err
		}
		return float64(each*pl.procs) / d.Minutes(), mem, nil
	}
}

// writeOCRSample 把样张写成临时文件（OCR 组件按路径读图片），返回路径和删除它的函数。
func writeOCRSample() (string, func(), error) {
	dir, err := os.MkdirTemp("", "zhaozhao-ocrbench-")
	if err != nil {
		return "", nil, err
	}
	p := filepath.Join(dir, "sample.png")
	if err := os.WriteFile(p, ocrSample, 0o600); err != nil {
		os.RemoveAll(dir)
		return "", nil, err
	}
	return p, func() { os.RemoveAll(dir) }, nil
}

// BenchOCR 在本机实测各种搭配（命令行 --ocr-bench 用，发布前在 Windows 上用真实的 OCR 组件跑一遍）。
// pool 是 OCR 用的进程池，cfg 是 OCR 组件，progress 报告进度。
func BenchOCR(ctx context.Context, pool *worker.Pool, cfg extract.OCRConfig, progress func(string)) (*OCRBench, error) {
	if !cfg.Enabled() {
		return nil, errors.New("没有找到 OCR 组件")
	}
	sample, cleanup, err := writeOCRSample()
	if err != nil {
		return nil, err
	}
	defer cleanup()
	mc := sysinfo.This()
	total, avail := sysinfo.Memory()
	budget := mc.Cores
	cands := ocrCandidates(budget, nextMemCap(0, ocrMemSlots(total, avail, ocrSlotMemDefault, 0)))
	opts := extract.DefaultOptions()
	opts.OCR = cfg
	b := runOCRBench(ctx, budget, cands, poolBench(pool, opts, sample), func(i, n int, p ocrPlan) {
		progress(fmt.Sprintf("第 %d/%d 种：%s", i+1, n, p))
	})
	b.Machine, b.CPU, b.Cores, b.MemGB = ocrMachineKey(mc, cfg), mc.CPU, mc.Cores, float64(mc.MemTotal)/(1<<30)
	return b, nil
}
