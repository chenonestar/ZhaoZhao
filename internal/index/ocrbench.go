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
	"slices"
	"sort"
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
// 搭配，按全部识别完的时间算每分钟页数。各种搭配交替测两轮（两轮差得多时再测一轮），取中位数：0.2.7 每种
// 只测一次、每个进程只认 1 页，同一台电脑上午、下午两次测速，超线程一次快 2%、一次快 15%，量化版一次快 15%、
// 一次快 1.6%，按下午的结果用上了超线程，实际反而慢。每台电脑测一次，结果按电脑保存在索引库里（便携版带到
// 别的电脑会重新测）；用固定的样张，不同搭配、不同电脑的结果可以直接比较。

//go:embed ocrsample.png
var ocrSample []byte

// OCRBench 是一台电脑上的 OCR 测速结果。
type OCRBench struct {
	Machine string         `json:"machine"` // 电脑（处理器、核心数、内存）连同 OCR 组件的指纹
	CPU     string         `json:"cpu"`
	Cores   int            `json:"cores"`
	Logical int            `json:"logical,omitempty"` // 逻辑处理器数（多于 Cores 表示有超线程）
	MemGB   float64        `json:"memGB"`
	At      int64          `json:"at"`     // 测速时间（Unix 秒）
	Budget  int            `json:"budget"` // 测速时的线程预算（物理核心数）
	Plans   []OCRBenchPlan `json:"plans"`
	Best    int            `json:"best"` // Plans 里最快的下标；-1 表示全部失败
	Err     string         `json:"err,omitempty"`
	// PlanModel 是比较搭配时用的认字模型；Models 是用最快的搭配比较各个认字模型的结果（只供参考）；
	// Model 是设置里认字模型为「自动」时用的：比较搭配时用的模型（PP-OCRv6 small 普通版，用不了时用组件自带的）。
	PlanModel string          `json:"planModel,omitempty"`
	Models    []OCRBenchModel `json:"models,omitempty"`
	Model     string          `json:"model,omitempty"`
	// CPUNote 是测速期间 CPU 的情况：实际频率、全部程序和别的程序各占几个逻辑处理器、电源计划（只有 Windows 取得到）
	CPUNote string `json:"cpuNote,omitempty"`
}

// cpuNote 把测速期间的 CPU 情况连同电源计划写成一句话。
func cpuNote(c sysinfo.CPUStat) string {
	s := c.String()
	if p := sysinfo.PowerPlan(); p != "" {
		if s != "" {
			s += "，"
		}
		s += "电源计划「" + p + "」"
	}
	return s
}

// OCRBenchModel 是一种认字模型的实测结果（用最快的搭配）。
type OCRBenchModel struct {
	Name   string  `json:"name"`
	PerMin float64 `json:"perMin"`          // 每分钟页数（0 表示失败）
	Agree  float64 `json:"agree,omitempty"` // 认出的字和比较搭配时用的模型一致的比例（%）
	Err    string  `json:"err,omitempty"`
}

// modelPerMin 返回某个认字模型实测的每分钟页数（没测过、失败时为 0）。
func (b *OCRBench) modelPerMin(model string) float64 {
	if b == nil {
		return 0
	}
	for _, x := range b.Models {
		if x.Name == model && x.Err == "" {
			return x.PerMin
		}
	}
	return 0
}

// OCRBenchPlan 是一种搭配的实测结果。
type OCRBenchPlan struct {
	Procs   int       `json:"procs"`
	Threads int       `json:"threads"`
	PerMin  float64   `json:"perMin"`         // 每分钟页数（各轮的中位数；0 表示失败）
	Runs    []float64 `json:"runs,omitempty"` // 各轮的每分钟页数
	SlotMem uint64    `json:"slotMem"`        // 每个名额（OCR 进程 + 解析进程）的内存峰值，字节
	CPU     float64   `json:"cpu,omitempty"`  // 测速期间子进程实际平均占用几个逻辑处理器（0 表示取不到，只有 Windows 能取到）
	Err     string    `json:"err,omitempty"`  // 失败的原因
}

func (p OCRBenchPlan) plan() ocrPlan { return ocrPlan{p.Procs, p.Threads} }

// best 返回最快的搭配；没有成功的结果时 ok 为 false。
func (b *OCRBench) best() (OCRBenchPlan, bool) {
	if b == nil || b.Best < 0 || b.Best >= len(b.Plans) {
		return OCRBenchPlan{}, false
	}
	return b.Plans[b.Best], true
}

// smt：这种搭配的总线程数超过物理核心数 cores（用上了超线程）。
func (p OCRBenchPlan) smt(cores int) bool { return p.Procs*p.Threads > cores }

// curves 由测速结果得到两条效率曲线：总线程数不超过物理核心的搭配（phys）和用上超线程的搭配（smt）。
func (b *OCRBench) curves() (phys, smt effCurve) {
	if b == nil {
		return nil, nil
	}
	eff := [2]map[int]float64{{}, {}}
	for _, p := range b.Plans {
		if p.Err != "" || p.PerMin <= 0 || p.Procs < 1 || p.Threads < 1 {
			continue
		}
		k := 0
		if p.smt(b.Budget) {
			k = 1
		}
		eff[k][p.Threads] = max(eff[k][p.Threads], p.PerMin/float64(p.Procs*p.Threads))
	}
	curve := func(m map[int]float64) effCurve {
		var c effCurve
		for t, e := range m {
			c = append(c, effPoint{t, e})
		}
		sort.Slice(c, func(i, j int) bool { return c[i].threads < c[j].threads })
		return c
	}
	return curve(eff[0]), curve(eff[1])
}

// ocrSMTGain：用上超线程的搭配要比只用物理核心的最快搭配快这么多才用（差不多时不用：进程多一倍、占内存、
// 发热，实际处理真实文件时还可能更慢）。0.2.7 是 5%，某台电脑测速时快 15%，实际反而慢。
const ocrSMTGain = 1.10

// smtGain 返回用上超线程的最快搭配比只用物理核心的最快搭配快多少（倍数）；每一轮都要快 ocrSMTGain 以上才算
// 稳定地快（steady 为 true）。没有可比的结果（没有超线程，或某一类全部失败）时返回 0。
func (b *OCRBench) smtGain() (gain float64, steady bool) {
	if b == nil {
		return 0, false
	}
	var phys, smt float64
	rounds := 0
	for _, p := range b.Plans {
		if p.Err != "" {
			continue
		}
		if p.smt(b.Budget) {
			smt = max(smt, p.PerMin)
		} else {
			phys = max(phys, p.PerMin)
		}
		rounds = max(rounds, len(p.Runs))
	}
	if phys <= 0 || smt <= 0 {
		return 0, false
	}
	gain, steady = smt/phys, smt > phys*ocrSMTGain
	for i := 0; i < rounds && steady; i++ {
		var ph, sm float64
		for _, p := range b.Plans {
			if p.Err != "" || i >= len(p.Runs) {
				continue
			}
			if p.smt(b.Budget) {
				sm = max(sm, p.Runs[i])
			} else {
				ph = max(ph, p.Runs[i])
			}
		}
		steady = ph > 0 && sm > ph*ocrSMTGain
	}
	return gain, steady
}

// smtFaster：用上超线程每一轮都比只用物理核心快 10% 以上。
func (b *OCRBench) smtFaster() bool {
	_, ok := b.smtGain()
	return ok
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
		var notes []string
		if len(p.Runs) > 1 {
			var rs []string
			for _, r := range p.Runs {
				rs = append(rs, fmt.Sprintf("%.1f", r))
			}
			notes = append(notes, "各轮 "+strings.Join(rs, "/"))
		}
		if p.SlotMem > 0 {
			notes = append(notes, fmt.Sprintf("每个约 %d MB", p.SlotMem>>20))
		}
		if p.CPU > 0 {
			notes = append(notes, fmt.Sprintf("实际占用约 %.1f 个逻辑处理器", p.CPU))
		}
		if len(notes) > 0 {
			s += "（" + strings.Join(notes, "，") + "）"
		}
		parts = append(parts, s)
	}
	s := strings.Join(parts, "，")
	if bp, ok := b.best(); ok {
		s += "；选用：" + bp.plan().String()
		if b.Logical > b.Budget {
			s += smtNote(b)
		}
	}
	if len(b.Models) > 0 {
		var ms []string
		for _, x := range b.Models {
			switch {
			case x.Err != "":
				ms = append(ms, fmt.Sprintf("%s 失败（%s）", extract.OCRModelName(x.Name), x.Err))
			case x.Name == b.PlanModel:
				ms = append(ms, fmt.Sprintf("%s 每分钟 %.1f 页", extract.OCRModelName(x.Name), x.PerMin))
			default:
				ms = append(ms, fmt.Sprintf("%s 每分钟 %.1f 页（认出的字和 %s 一致 %.1f%%）", extract.OCRModelName(x.Name), x.PerMin,
					extract.OCRModelName(b.PlanModel), x.Agree))
			}
		}
		s += "。认字模型（" + map[bool]string{true: "比较搭配用的是 " + extract.OCRModelName(b.PlanModel), false: "按最快的搭配"}[b.PlanModel != ""] +
			"）：" + strings.Join(ms, "，")
		if b.Model != "" {
			s += "；自动时用 " + extract.OCRModelName(b.Model)
		}
	}
	if b.CPUNote != "" {
		s += "。测速期间" + b.CPUNote
	}
	return s
}

// smtNote 说明为什么用或不用超线程。
func smtNote(b *OCRBench) string {
	gain, ok := b.smtGain()
	switch {
	case ok:
		return fmt.Sprintf("（用上超线程快 %.0f%%，用全部逻辑处理器）", (gain-1)*100)
	case gain > 1:
		return fmt.Sprintf("（用上超线程只快 %.0f%%，不到 %.0f%% 或各轮不稳定，只用物理核心：进程多一倍、占内存，实际处理时未必更快）",
			(gain-1)*100, (ocrSMTGain-1)*100)
	}
	return "（超线程对 OCR 没有帮助，只用物理核心）"
}

// ocrBenchVersion：测速方法变了（比较的搭配不同）时加一，各台电脑都重新测一次。
// 2：0.2.4 起有超线程的电脑还比较用上全部逻辑处理器的搭配。
// 3：0.2.7 起用新认字模型比较搭配，再比较各个认字模型。
// 4：0.2.8 起各种搭配交替测两轮以上取中位数，每个进程至少认 2 页。
const ocrBenchVersion = 4

// ocrMachineKey 是电脑连同 OCR 组件的指纹：处理器、核心数、内存（GB）、OCR 程序及其参数、测速方法。
// 任何一项变了（换了电脑、加了内存、升级了 OCR 组件）都要重新测速。
func ocrMachineKey(m sysinfo.Machine, c extract.OCRConfig) string {
	comp := filepath.Base(c.Command)
	if st, err := os.Stat(c.Command); err == nil {
		comp += fmt.Sprintf("/%d", st.Size())
	}
	s := fmt.Sprintf("%s|%d|%d|%d|%s|%s|b%d", m.CPU, m.Cores, m.Logical, (m.MemTotal+(1<<29))>>30, comp, strings.Join(c.Args, " "), ocrBenchVersion)
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

// benchRun 是实测一种搭配的结果：每分钟页数、每个名额的内存峰值、子进程实际平均占用几个逻辑处理器、
// 认出的文字（比较认字模型时看结果是否一致）。
type benchRun struct {
	perMin  float64
	slotMem uint64
	cpu     float64
	text    string
}

// ocrBenchRunner 用认字模型 model 实测一种搭配：pages 页平均分给各个进程（每个进程至少 ocrBenchMinEach 页）。
type ocrBenchRunner func(ctx context.Context, model string, p ocrPlan, pages int) (benchRun, error)

// runRetry 实测一次，失败时再试一次：同时启动很多个 OCR 组件时偶尔有一个启动失败（0.2.6 在某台电脑上
// 9 个进程同时启动，有一个报文件共享冲突，可能是杀毒软件正在检查），不应因此判这种搭配不行。
func runRetry(ctx context.Context, run ocrBenchRunner, model string, p ocrPlan, pages int) (benchRun, error) {
	r, err := run(ctx, model, p, pages)
	if err != nil && ctx.Err() == nil {
		r, err = run(ctx, model, p, pages)
	}
	return r, err
}

// ocrBenchMinEach：测速时每个进程至少认几页。只认 1 页时，几个进程里最慢的那个决定了结果（某个进程碰巧
// 被别的程序抢了 CPU，整种搭配就显得慢），0.2.7 的结果因此忽高忽低。
const ocrBenchMinEach = 2

// ocrBenchMaxSpread：同一种搭配两轮的结果相差超过这么多（倍）时再测一轮。
const ocrBenchMaxSpread = 1.10

// ocrBenchPages：每种搭配识别的页数。等于线程预算，各种搭配的工作量相近（1 个进程 × N 线程时识别 N 页；
// 每个进程至少认 ocrBenchMinEach 页，N 个进程 × 1 线程时认 2N 页）；预算太小时加倍，结果才稳定。
func ocrBenchPages(budget int) int {
	if budget < 4 {
		return budget * 2
	}
	return budget
}

// runOCRBench 先用认字模型 models[0] 实测各种搭配：交替测两轮（某种搭配两轮差 10% 以上时再测一轮），取各轮
// 的中位数；再用最快的搭配比较其余的认字模型（只供参考，自动时不换）。某种搭配失败（比如内存不够）不影响
// 其余的；全部失败时换下一个模型再比较搭配（新模型在这台电脑上用不了时用组件自带的），都不行时 Best 为 -1。
// models 为空时不指定模型（用组件参数里的）。量化版和 Windows 自带的 OCR 不用来比较搭配。
func runOCRBench(ctx context.Context, budget int, cands []ocrPlan, models []string, run ocrBenchRunner, progress func(i, n int, what string)) *OCRBench {
	b := &OCRBench{Budget: budget, Best: -1, At: time.Now().Unix()}
	pages := ocrBenchPages(budget)
	if len(models) == 0 {
		models = []string{""}
	}
	total := 2*len(cands) + len(models) - 1
	step := 0
	var failed []OCRBenchModel // 比较搭配时全部失败的模型
	var baseText string
	for _, model := range models {
		if model == extract.OCRModelV6Q || model == extract.OCRModelWin {
			continue
		}
		b.Plans, b.Best, b.PlanModel = nil, -1, model
		texts := make([]string, len(cands))
		for _, c := range cands {
			b.Plans = append(b.Plans, OCRBenchPlan{Procs: c.procs, Threads: c.threads})
		}
		// round 测一轮：only 为 nil 时测全部搭配，否则只测 only 为 true 的
		round := func(only func(p OCRBenchPlan) bool) {
			for i, c := range cands {
				p := &b.Plans[i]
				if ctx.Err() != nil || (only != nil && !only(*p)) {
					continue
				}
				if progress != nil {
					progress(step, total, c.String())
				}
				step++
				r, err := runRetry(ctx, run, model, c, pages)
				if err != nil {
					if ctx.Err() == nil {
						p.PerMin, p.CPU, p.Runs, p.Err = 0, 0, nil, truncate(err.Error(), 200)
						b.Err = p.Err
					}
					continue
				}
				p.Runs = append(p.Runs, r.perMin)
				p.SlotMem, p.CPU = max(p.SlotMem, r.slotMem), max(p.CPU, r.cpu)
				if texts[i] == "" {
					texts[i] = r.text
				}
			}
		}
		ok := func(p OCRBenchPlan) bool { return p.Err == "" && len(p.Runs) > 0 }
		round(nil)
		if slices.ContainsFunc(b.Plans, ok) {
			total -= len(cands) - countPlans(b.Plans, ok) // 失败的搭配不再测
			round(ok)
			if spread := func(p OCRBenchPlan) bool { return ok(p) && runSpread(p.Runs) > ocrBenchMaxSpread }; slices.ContainsFunc(b.Plans, spread) {
				total += countPlans(b.Plans, ok)
				round(ok)
			}
		}
		for i := range b.Plans {
			p := &b.Plans[i]
			if !ok(*p) {
				p.Runs = nil
				if p.Err == "" {
					p.Err = "没有测完"
				}
				continue
			}
			p.PerMin = median(p.Runs)
		}
		b.Best = b.choose()
		if b.Best >= 0 || ctx.Err() != nil {
			if b.Best >= 0 {
				baseText = texts[b.Best]
			}
			break
		}
		failed = append(failed, OCRBenchModel{Name: model, Err: b.Err})
		total += 2 * len(cands)
	}
	if b.Best < 0 {
		return b
	}
	b.Err = ""
	if b.PlanModel == "" {
		return b
	}
	best := b.Plans[b.Best]
	b.Models = append(failed, OCRBenchModel{Name: b.PlanModel, PerMin: best.PerMin, Agree: 100})
	for _, model := range models {
		bad := func(name string) bool {
			return slices.ContainsFunc(failed, func(x OCRBenchModel) bool { return x.Name == name })
		}
		// 新模型用不了时不用再试它的量化版
		if model == b.PlanModel || bad(model) || (model == extract.OCRModelV6Q && bad(extract.OCRModelV6)) || ctx.Err() != nil {
			continue
		}
		if progress != nil {
			progress(step, total, "认字模型 "+extract.OCRModelName(model))
		}
		step++
		r, err := runRetry(ctx, run, model, best.plan(), pages)
		x := OCRBenchModel{Name: model, PerMin: r.perMin, Agree: agreement(r.text, baseText)}
		if err != nil {
			x = OCRBenchModel{Name: model, Err: truncate(err.Error(), 200)}
		}
		b.Models = append(b.Models, x)
	}
	// 自动时用比较搭配的模型（普通版）。0.2.7 量化版快 3% 以上、样张上认的字 98% 以上相同就用它，可在某位用户的
	// 两万多页真实扫描件上，量化版比普通版少认 0.4% 的字，173 页少了一成以上，所以只在用户手动选择时用。
	b.Model = b.PlanModel
	return b
}

// choose 返回选用的搭配（Plans 的下标；-1 表示全部失败）：最快的；超线程没有每一轮都快 10% 以上时，只在只用
// 物理核心的搭配里选（0.2.8 打包测试里 2×2 比 2×1 只快 0.1%，以前标成「最快」，实际却按只用物理核心开进程）。
func (b *OCRBench) choose() int {
	_, smtOK := b.smtGain()
	best := -1
	for i, p := range b.Plans {
		if p.Err != "" || p.PerMin <= 0 || (!smtOK && p.smt(b.Budget)) {
			continue
		}
		if best < 0 || p.PerMin > b.Plans[best].PerMin {
			best = i
		}
	}
	return best
}

func countPlans(ps []OCRBenchPlan, f func(OCRBenchPlan) bool) int {
	n := 0
	for _, p := range ps {
		if f(p) {
			n++
		}
	}
	return n
}

// runSpread 返回各轮结果中最大和最小的比值。
func runSpread(rs []float64) float64 {
	lo, hi := slices.Min(rs), slices.Max(rs)
	if lo <= 0 {
		return 0
	}
	return hi / lo
}

// median 返回中位数（偶数个时取中间两个的平均）。
func median(xs []float64) float64 {
	s := slices.Clone(xs)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// agreement 返回两段文字相同的比例（%，按编辑距离，不计空白）。
func agreement(a, b string) float64 {
	ra, rb := []rune(strings.Join(strings.Fields(a), "")), []rune(strings.Join(strings.Fields(b), ""))
	n := max(len(ra), len(rb))
	if n == 0 {
		return 100
	}
	prev, cur := make([]int, len(rb)+1), make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			c := prev[j-1]
			if ra[i-1] != rb[j-1] {
				c++
			}
			cur[j] = min(c, prev[j]+1, cur[j-1]+1)
		}
		prev, cur = cur, prev
	}
	return 100 * (1 - float64(prev[len(rb)])/float64(n))
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// poolBench 用进程池实测：先让每个进程识别一页（启动 OCR 组件、加载模型，不计时），
// 再同时开始（每个进程认 pages÷进程数页，至少 ocrBenchMinEach 页），按全部识别完的时间算每分钟页数（不含电脑睡眠的时间）；同时记下子进程用掉的 CPU 时间，
// 算出实际平均占用几个逻辑处理器（RapidOCR 设了线程数，实际占用可能更多）。
func poolBench(pool *worker.Pool, opts extract.Options, sample string) ocrBenchRunner {
	return func(ctx context.Context, model string, pl ocrPlan, pages int) (benchRun, error) {
		// 每种搭配都用新启动的进程：内存峰值是进程启动以来的，0.2.8 某次 OCR 引擎对比后马上测速，进程刚认过大 PDF，
		// 6×1 记成每个 857 MB（实际约 455 MB）
		pool.CloseIdle()
		pool.SetSize(pl.procs)
		o := opts
		o.OCR = o.OCR.WithModel(model)
		o.OCR.Threads = pl.threads
		o.ReportMem = true
		t := worker.Timeouts{Total: 10 * time.Minute}
		var text string
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
						if res.Status == doc.StatusOK {
							text = res.Text
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
			return benchRun{}, err
		}
		each := max(ocrBenchMinEach, (pages+pl.procs-1)/pl.procs)
		cpu0, cpuOK := worker.ChildrenCPU()
		d, mem, err := round(each)
		if err != nil {
			return benchRun{}, err
		}
		r := benchRun{perMin: float64(each*pl.procs) / d.Minutes(), slotMem: mem, text: text}
		if cpu1, ok := worker.ChildrenCPU(); ok && cpuOK && d > 0 {
			r.cpu = float64(cpu1-cpu0) / float64(d)
		}
		return r, nil
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
	cands := ocrBenchCandidates(budget, mc.Logical, nextMemCap(0, ocrMemSlots(total, avail, ocrSlotMemDefault, 0)))
	opts := extract.DefaultOptions()
	opts.OCR = cfg
	cpu := sysinfo.NewCPUMonitor(worker.ChildrenCPU)
	b := runOCRBench(ctx, budget, cands, cfg.Models(), poolBench(pool, opts, sample), func(i, n int, what string) {
		progress(fmt.Sprintf("第 %d/%d 项：%s", i+1, n, what))
	})
	b.Machine, b.CPU, b.Cores, b.Logical, b.MemGB = ocrMachineKey(mc, cfg), mc.CPU, mc.Cores, mc.Logical, float64(mc.MemTotal)/(1<<30)
	b.CPUNote = cpuNote(cpu.Read())
	return b, nil
}
