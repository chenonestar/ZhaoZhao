package index

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenonestar/zhaozhao/internal/doc"
	"github.com/chenonestar/zhaozhao/internal/extract"
	"github.com/chenonestar/zhaozhao/internal/store"
	"github.com/chenonestar/zhaozhao/internal/sysinfo"
	"github.com/chenonestar/zhaozhao/internal/testutil"
	"github.com/chenonestar/zhaozhao/internal/volume"
	"github.com/chenonestar/zhaozhao/internal/worker"
)

func TestOCRCandidates(t *testing.T) {
	for _, c := range []struct {
		budget, maxProcs int
		want             string
	}{
		{1, 32, "[{1 1}]"},
		{2, 32, "[{2 1} {1 2}]"},
		{4, 32, "[{4 1} {2 2} {1 4}]"},
		{6, 32, "[{6 1} {3 2} {1 6}]"},
		{8, 32, "[{8 1} {4 2} {2 4} {1 8}]"},
		{16, 32, "[{16 1} {8 2} {4 4} {2 8}]"},
		{8, 3, "[{3 2} {2 4} {1 8}]"}, // 内存只够 3 个进程：每个进程多分线程把预算用满
		{8, 1, "[{1 8}]"},
	} {
		if got := sprint(ocrCandidates(c.budget, c.maxProcs)); got != c.want {
			t.Errorf("预算 %d、最多 %d 个进程：%s，应为 %s", c.budget, c.maxProcs, got, c.want)
		}
	}
	if p := ocrManualPlan(8, 8); p != (ocrPlan{8, 1}) {
		t.Errorf("手动 8 个进程、8 核：%v", p)
	}
}

// 按实测效率估算各种搭配的速度，选出线程预算下最快的，把预算用满。
func TestOCRBestPlan(t *testing.T) {
	// 6 核：6×1 每分钟 30 页，3×2 36 页，1×6 20 页 → 每线程 5、6、3.33 页
	b := &OCRBench{Budget: 6, Logical: 6, Plans: []OCRBenchPlan{
		{Procs: 6, Threads: 1, PerMin: 30}, {Procs: 3, Threads: 2, PerMin: 36}, {Procs: 1, Threads: 6, PerMin: 20}, {Procs: 2, Threads: 3, Err: "内存不足"},
	}}
	phys, smt := b.curves()
	if len(phys) != 3 || len(smt) != 0 || phys.at(2) != 6 || phys.at(1) != 5 {
		t.Fatalf("效率曲线：%+v %+v", phys, smt)
	}
	if e := phys.at(4); e < 4.66 || e > 4.67 { // 2 线程和 6 线程之间插值
		t.Fatalf("插值：%v", e)
	}
	if e := phys.at(12); e != phys.at(6)/2 { // 比测过的都多：多出来的线程不再加快
		t.Fatalf("外推：%v", e)
	}
	for _, c := range []struct {
		budget, maxProcs int
		want             ocrPlan
	}{
		{6, 32, ocrPlan{3, 2}}, // 只剩 OCR：实测最快的
		{3, 32, ocrPlan{1, 3}}, // 普通文件还在处理：预算 3，1×3（估计 16 页）比 3×1（15 页）快，以前是 2×1，只用 2 个线程
		{6, 2, ocrPlan{2, 3}},  // 内存只够 2 个进程：每个进程 3 线程，把预算用满
		{1, 32, ocrPlan{1, 1}},
	} {
		if got := phys.bestPlan(c.budget, c.maxProcs); got != c.want {
			t.Errorf("预算 %d、最多 %d 个进程：%v，应为 %v", c.budget, c.maxProcs, got, c.want)
		}
	}
	// 每个进程 1 线程效率最高时，预算 3 是 3×1
	b1 := &OCRBench{Budget: 4, Plans: []OCRBenchPlan{{Procs: 4, Threads: 1, PerMin: 26}, {Procs: 2, Threads: 2, PerMin: 20}, {Procs: 1, Threads: 4, PerMin: 16}}}
	if p1, _ := b1.curves(); p1.bestPlan(3, 32) != (ocrPlan{3, 1}) || p1.bestPlan(2, 32) != (ocrPlan{2, 1}) {
		t.Fatal("每个进程 1 线程最快时应多开进程")
	}
	// 还没测速：每个进程 1 线程，进程数受限时多分线程
	var none effCurve
	if none.bestPlan(4, 32) != (ocrPlan{4, 1}) || none.bestPlan(8, 3) != (ocrPlan{3, 2}) {
		t.Fatal("还没测速时的搭配")
	}
	if ocrBudget(8, true) != 4 || ocrBudget(8, false) != 8 || ocrBudget(2, true) != 2 || ocrBudget(1, true) != 1 {
		t.Fatal("线程预算")
	}
}

// 有超线程的电脑：测速时再比较用上全部逻辑处理器的搭配；明显更快时只剩 OCR 时用它，预算减半时仍只用物理核心。
func TestOCRSMT(t *testing.T) {
	for _, c := range []struct {
		cores, logical, maxProcs int
		want                     string
	}{
		{6, 12, 32, "[{6 1} {3 2} {1 6} {12 1} {6 2}]"},
		{4, 8, 32, "[{4 1} {2 2} {1 4} {8 1} {4 2}]"},
		{6, 6, 32, "[{6 1} {3 2} {1 6}]"},               // 没有超线程
		{6, 12, 10, "[{6 1} {3 2} {1 6} {10 1} {6 2}]"}, // 内存只够 10 个进程
		{6, 12, 6, "[{6 1} {3 2} {1 6} {6 2}]"},         // 只够 6 个进程：12×1 变成 6×2，不重复
	} {
		if got := sprint(ocrBenchCandidates(c.cores, c.logical, c.maxProcs)); got != c.want {
			t.Errorf("%d 核 %d 线程、最多 %d 个进程：%s，应为 %s", c.cores, c.logical, c.maxProcs, got, c.want)
		}
	}
	b := &OCRBench{Budget: 4, Logical: 8, Plans: []OCRBenchPlan{
		{Procs: 4, Threads: 1, PerMin: 20}, {Procs: 2, Threads: 2, PerMin: 18}, {Procs: 1, Threads: 4, PerMin: 12},
		{Procs: 8, Threads: 1, PerMin: 26}, {Procs: 4, Threads: 2, PerMin: 22},
	}}
	if !b.smtFaster() {
		t.Fatal("8×1 比 4×1 快 30%，应用上超线程")
	}
	e := newEnvWith(t, &fakeProv{}, "", nil)
	m := e.m
	m.ocr.mu.Lock()
	m.ocrBenchLocked()
	m.ocr.bench = b
	full, fb := m.autoPlanLocked(4, false, 0)
	half, hb := m.autoPlanLocked(4, true, 0)
	m.ocr.mu.Unlock()
	if full != (ocrPlan{8, 1}) || fb != 8 || hb != 2 || half.procs*half.threads != 2 {
		t.Fatalf("只剩 OCR：%v（预算 %d）；预算减半：%v（预算 %d）", full, fb, half, hb)
	}
	b.Plans[3].PerMin, b.Plans[4].PerMin = 20.5, 19 // 只快一点：不值得
	if b.smtFaster() {
		t.Fatal("超线程只快 2.5% 时不用")
	}
	if !strings.Contains(b.Summary(), "用上超线程只快 2%，不到 10%") {
		t.Fatal(b.Summary())
	}
}

// 普通文件、老格式还在处理时 OCR 也用全部核心（OCR 进程是最低优先级，只用它们剩下的 CPU）。
func TestOCRFullWhileOthersBusy(t *testing.T) {
	e := newEnvWith(t, &fakeProv{}, "", func(m *Manager) {
		useFakeOCR(t, m, "10ms")
		m.ocr.cores = 6
	})
	m := e.m
	m.ocr.mu.Lock()
	m.ocrBenchLocked()
	m.ocr.bench = &OCRBench{Budget: 6, Logical: 6, Plans: []OCRBenchPlan{
		{Procs: 6, Threads: 1, PerMin: 33}, {Procs: 3, Threads: 2, PerMin: 36}, {Procs: 1, Threads: 6, PerMin: 18}}}
	m.ocr.mu.Unlock()
	s := &m.sched
	s.lanes[prioFast].wantWork.Store(true)
	defer s.lanes[prioFast].wantWork.Store(false)
	m.ocrTuneStep()
	if p := m.ocrPlanNow(); p != (ocrPlan{3, 2}) {
		t.Fatalf("普通文件还在处理时 OCR 也应用全部核心：%v", p)
	}
}

// 设置里开了「用电脑时放慢」：用户正在用电脑时 OCR 只用一半核心，正在识别的文件从下一页起
// 减少线程；离开后恢复。默认不开：用户在用电脑也用全部核心。
func TestOCRHalfWhileUserActive(t *testing.T) {
	idle := time.Hour
	old := userIdleTime
	userIdleTime = func() time.Duration { return idle }
	t.Cleanup(func() { userIdleTime = old })
	e := newEnvWith(t, &fakeProv{}, `{"ocrHalfWhenActive":true,"configVersion":3}`, func(m *Manager) {
		useFakeOCR(t, m, "10ms")
		m.ocr.cores = 6
	})
	m := e.m
	m.ocr.mu.Lock()
	m.ocrBenchLocked()
	// 6 核：3×2 最快；预算减半为 3 时 3×1 最快
	m.ocr.bench = &OCRBench{Budget: 6, Logical: 6, Plans: []OCRBenchPlan{
		{Procs: 6, Threads: 1, PerMin: 33}, {Procs: 3, Threads: 2, PerMin: 36}, {Procs: 1, Threads: 6, PerMin: 18}}}
	m.ocr.mu.Unlock()
	m.ocrTuneStep()
	if p := m.ocrPlanNow(); p != (ocrPlan{3, 2}) {
		t.Fatalf("没人用电脑时应用全部核心：%v", p)
	}
	// 一个文件正在按 2 线程识别，队列里还有文件（不是收尾）
	ctl := &worker.Ctl{}
	s := &m.sched
	s.mu.Lock()
	s.inflight[-1] = &job{ctl: ctl, ocrThreads: 2, path: "大文件.pdf"}
	s.mu.Unlock()
	s.lanes[prioOCR].wantWork.Store(true)
	defer s.lanes[prioOCR].wantWork.Store(false)
	idle = 10 * time.Second
	m.ocrTuneStep()
	if p := m.ocrPlanNow(); p != (ocrPlan{3, 1}) {
		t.Fatalf("用户正在用电脑时应只用一半：%v", p)
	}
	if _, _, note := m.ocrStatus(); !strings.Contains(note, "正在用电脑") {
		t.Fatalf("界面应说明：%q", note)
	}
	if ctl.OCRThreads() != 1 {
		t.Fatalf("正在识别的文件应从下一页起减到 1 线程：%d", ctl.OCRThreads())
	}
	idle = time.Hour
	m.ocrTuneStep()
	if p := m.ocrPlanNow(); p != (ocrPlan{3, 2}) || ctl.OCRThreads() != 0 {
		t.Fatalf("离开后应恢复：%v，正在识别的文件 %d 线程", p, ctl.OCRThreads())
	}
	// 关掉这个设置（默认）：用户在用电脑也用全部核心
	c := m.Config()
	c.SlowWhenActive = false
	m.SetConfig(c)
	idle = 10 * time.Second
	m.ocrTuneStep()
	if p := m.ocrPlanNow(); p != (ocrPlan{3, 2}) {
		t.Fatalf("默认不因为用户在用电脑减半：%v", p)
	}
	if _, _, note := m.ocrStatus(); strings.Contains(note, "正在用电脑") {
		t.Fatalf("没开这个设置不应提示：%q", note)
	}
	s.mu.Lock()
	delete(s.inflight, -1)
	s.mu.Unlock()
}

func sprint(ps []ocrPlan) string {
	var parts []string
	for _, p := range ps {
		parts = append(parts, fmt.Sprintf("{%d %d}", p.procs, p.threads))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// 测速：每种搭配识别同样多的页（线程预算），交替测两轮，选最快的；某种失败不影响其余的，全部失败时没有结果。
func TestRunOCRBench(t *testing.T) {
	speed := map[ocrPlan]float64{{4, 1}: 26, {2, 2}: 20, {1, 4}: 16}
	var calls []string
	run := func(ctx context.Context, model string, p ocrPlan, pages int) (benchRun, error) {
		calls = append(calls, fmt.Sprintf("%v:%d", p, pages))
		if p == (ocrPlan{2, 2}) {
			return benchRun{}, errors.New("内存不足")
		}
		return benchRun{perMin: speed[p], slotMem: 300 << 20, cpu: float64(p.procs*p.threads) + 1.5}, nil
	}
	b := runOCRBench(context.Background(), 4, ocrCandidates(4, 32), nil, run, nil)
	if bp, ok := b.best(); !ok || bp.plan() != (ocrPlan{4, 1}) || b.Err != "" || len(b.Plans) != 3 || b.Plans[1].Err != "内存不足" {
		t.Fatalf("%+v", b)
	}
	// 失败的搭配再试一次（同时启动很多个组件时偶尔有一个启动失败）
	if strings.Join(calls, " ") != "4 个进程 × 1 线程:4 2 个进程 × 2 线程:4 2 个进程 × 2 线程:4 1 个进程 × 4 线程:4 4 个进程 × 1 线程:4 1 个进程 × 4 线程:4" {
		t.Fatalf("每种都应识别 4 页，失败的再试一次，第二轮不再测：%v", calls)
	}
	if s := b.Summary(); !strings.Contains(s, "4 个进程 × 1 线程 每分钟 26.0 页（各轮 26.0/26.0，每个约 300 MB，实际占用约 5.5 个逻辑处理器）") || !strings.Contains(s, "最快：4 个进程 × 1 线程") {
		t.Fatal(s)
	}
	if ocrBenchPages(2) != 4 || ocrBenchPages(8) != 8 {
		t.Fatal("预算太小时页数加倍")
	}
	bad := runOCRBench(context.Background(), 2, ocrCandidates(2, 32), nil, func(context.Context, string, ocrPlan, int) (benchRun, error) {
		return benchRun{}, errors.New("OCR 组件启动失败")
	}, nil)
	if _, ok := bad.best(); ok || bad.Err != "OCR 组件启动失败" {
		t.Fatalf("%+v", bad)
	}
}

// 认字模型：先用新模型比较搭配，再用最快的搭配比较量化版、旧模型和 Windows 自带的 OCR（只供参考）；自动时
// 总是用新模型的普通版，量化版再快、样张上认的字再一样也不换。新模型在这台电脑上用不了时，用旧模型比较搭配、
// 用旧模型。
func TestRunOCRBenchModels(t *testing.T) {
	text := map[string]string{"v6": "通知各部门做好安全检查工作", "v6q": "通知各部门做好安全检查工作", "v4": "通知各部门做奸安全检查工伤",
		"win": "通知各部门做好安全检查工作"}
	speed := map[string]float64{"v6": 20, "v6q": 24, "v4": 18, "win": 60}
	var calls []string
	run := func(ctx context.Context, model string, p ocrPlan, pages int) (benchRun, error) {
		calls = append(calls, model+"@"+p.String())
		if speed[model] == 0 {
			return benchRun{}, errors.New("组件异常退出")
		}
		return benchRun{perMin: speed[model] * float64(p.procs) / float64(p.procs+p.threads-1), text: text[model]}, nil
	}
	models := []string{"v6", "v6q", "v4", "win"}
	b := runOCRBench(context.Background(), 2, ocrCandidates(2, 32), models, run, nil)
	if b.PlanModel != "v6" || b.Model != "v6" || len(b.Models) != 4 {
		t.Fatalf("%+v", b)
	}
	if b.Models[2].Name != "v4" || b.Models[2].Agree > 90 || b.Models[1].Agree != 100 || b.Models[3].Name != "win" {
		t.Fatalf("一致比例：%+v", b.Models)
	}
	for _, c := range calls {
		if (strings.HasPrefix(c, "v6q@") || strings.HasPrefix(c, "win@")) && c != "v6q@2 个进程 × 1 线程" && c != "win@2 个进程 × 1 线程" {
			t.Fatalf("量化版和 Windows 自带的 OCR 不用来比较搭配：%v", calls)
		}
	}
	if s := b.Summary(); !strings.Contains(s, "自动时用 PP-OCRv6 small") || strings.Contains(s, "自动时用 PP-OCRv6 small（量化版）") {
		t.Fatal(s)
	}
	// 新模型用不了：用旧模型比较搭配
	speed["v6"], speed["v6q"] = 0, 0
	calls = nil
	b = runOCRBench(context.Background(), 2, ocrCandidates(2, 32), models, run, nil)
	if _, ok := b.best(); !ok || b.PlanModel != "v4" || b.Model != "v4" || b.Models[0].Name != "v6" || b.Models[0].Err == "" {
		t.Fatalf("%+v", b)
	}
	if strings.Contains(strings.Join(calls, " "), "v6q@") {
		t.Fatalf("新模型用不了时不用再试量化版：%v", calls)
	}
}

// 测速结果忽高忽低（同一种搭配两轮差 10% 以上）时再测一轮，取中位数。超线程要每一轮都快 10% 以上才用。
func TestRunOCRBenchRounds(t *testing.T) {
	seq := map[ocrPlan][]float64{{6, 1}: {30, 40, 31}, {3, 2}: {28, 28, 28}, {1, 6}: {15, 15, 15}, {12, 1}: {36, 33, 34}, {6, 2}: {30, 30, 30}}
	n := map[ocrPlan]int{}
	run := func(ctx context.Context, model string, p ocrPlan, pages int) (benchRun, error) {
		v := seq[p][n[p]]
		n[p]++
		return benchRun{perMin: v}, nil
	}
	b := runOCRBench(context.Background(), 6, ocrBenchCandidates(6, 12, 32), nil, run, nil)
	b.Logical = 12
	if n[ocrPlan{6, 1}] != 3 || n[ocrPlan{3, 2}] != 3 {
		t.Fatalf("6×1 两轮差 33%%，应全部再测一轮：%v", n)
	}
	if b.Plans[0].PerMin != 31 || len(b.Plans[0].Runs) != 3 {
		t.Fatalf("应取中位数：%+v", b.Plans[0])
	}
	// 12×1 的中位数 34 比 6×1 的 31 快 9.7%：不到 10%，不用超线程
	if bp, _ := b.best(); bp.plan() != (ocrPlan{12, 1}) || b.smtFaster() {
		t.Fatalf("%+v", b.Plans)
	}
	if s := b.Summary(); !strings.Contains(s, "用上超线程只快 10%，不到 10%") {
		t.Fatal(s)
	}
	// 中位数快 16%，但第二轮反而慢 17%（物理核心那轮碰巧快）：不稳定，不用
	b.Plans[3].PerMin, b.Plans[3].Runs = 36, []float64{36, 33, 36}
	if b.smtFaster() {
		t.Fatal("有一轮超线程不快时不用")
	}
	b.Plans[0].Runs, b.Plans[3].Runs = []float64{30, 30, 31}, []float64{36, 34, 36}
	if !b.smtFaster() {
		t.Fatal("每轮都快 10% 以上时用超线程")
	}
}

// 指纹：换了电脑（处理器、核心数、内存）或 OCR 组件（程序、参数）都不同；同一台电脑每次相同。
func TestOCRMachineKey(t *testing.T) {
	m := sysinfo.Machine{CPU: "Intel Core i5-8250U", Cores: 4, Logical: 8, MemTotal: 8 << 30}
	c := extract.OCRConfig{Command: "/nonexistent/RapidOCR-json.exe", Args: []string{"--models=models"}}
	k := ocrMachineKey(m, c)
	if k != ocrMachineKey(m, c) {
		t.Fatal("同一台电脑指纹应相同")
	}
	m2 := m
	m2.MemTotal = 8<<30 - 100<<20 // 可用于显卡的内存不同，总量略有差别：仍算同一台
	if ocrMachineKey(m2, c) != k {
		t.Fatal("内存相差不到半 GB 应算同一台")
	}
	for _, x := range []sysinfo.Machine{{CPU: "AMD Ryzen 5 5600", Cores: 4, Logical: 8, MemTotal: 8 << 30}, {CPU: m.CPU, Cores: 4, Logical: 8, MemTotal: 16 << 30}, {CPU: m.CPU, Cores: 6, Logical: 12, MemTotal: 8 << 30}} {
		if ocrMachineKey(x, c) == k {
			t.Fatalf("%+v 应是不同的电脑", x)
		}
	}
	c2 := c
	c2.Args = []string{"--models=models_v5"}
	if ocrMachineKey(m, c2) == k {
		t.Fatal("OCR 参数不同应重新测速")
	}
}

// 测速结果按电脑保存：便携版在几台电脑之间带来带去，各自用各自的结果；最多保存 8 台。
func TestOCRBenchSavedPerMachine(t *testing.T) {
	e := newEnvWith(t, &fakeProv{}, "", nil)
	for i := 0; i < 10; i++ {
		b := &OCRBench{Machine: fmt.Sprintf("pc%d", i), Best: 0, Plans: []OCRBenchPlan{{Procs: i + 1, Threads: 1, PerMin: 10}}}
		if err := e.m.saveOCRBench(b.Machine, b); err != nil {
			t.Fatal(err)
		}
	}
	if b := e.m.loadOCRBench("pc9"); b == nil || b.Plans[0].Procs != 10 {
		t.Fatalf("%+v", b)
	}
	if e.m.loadOCRBench("pc1") != nil || e.m.loadOCRBench("pc2") == nil || len(e.m.allOCRBench()) != ocrBenchKeepMax {
		t.Fatal("应只保留最近 8 台")
	}
	_ = e.m.saveOCRBench("pc5", nil)
	if e.m.loadOCRBench("pc5") != nil || e.m.loadOCRBench("pc6") == nil {
		t.Fatal("删除一台的结果")
	}
}

// testMachine：测速测试用固定的电脑（4 核、没有超线程），比较的搭配不随运行测试的电脑变化。
var testMachine = sysinfo.Machine{CPU: "测试用处理器", Cores: 4, Logical: 4, MemTotal: 16 << 30}

// fakeBench 记录测速调用，速度按 speed（没有的搭配：进程越多越快）。
type fakeBench struct {
	mu    sync.Mutex
	calls []ocrPlan
	speed map[ocrPlan]float64
	check func(m *Manager)
}

func (f *fakeBench) install(t *testing.T) {
	old := newOCRBenchRunner
	newOCRBenchRunner = func(m *Manager, pool *worker.Pool, sample string) ocrBenchRunner {
		return func(ctx context.Context, model string, p ocrPlan, pages int) (benchRun, error) {
			if st, err := os.Stat(sample); err != nil || st.Size() != int64(len(ocrSample)) {
				return benchRun{}, fmt.Errorf("样张没写出来：%v", err)
			}
			f.mu.Lock()
			f.calls = append(f.calls, p)
			check := f.check
			v, ok := f.speed[p]
			f.mu.Unlock()
			if check != nil {
				check(m)
			}
			if !ok {
				v = float64(10*p.procs - p.threads)
			}
			return benchRun{perMin: v, slotMem: 200 << 20}, nil
		}
	}
	t.Cleanup(func() { newOCRBenchRunner = old })
}

func (f *fakeBench) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// 第一次有图片要识别时先测速（普通文件暂停），按最快的搭配开进程；结果保存后重启不再测，
// 换了 OCR 组件（指纹不同）重新测；「重新测速」没有要识别的文件也会测。
func TestOCRBenchBeforeFirstOCR(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 6; i++ {
		writePNG(t, filepath.Join(dir, fmt.Sprintf("img%d.png", i)), i)
		write(t, filepath.Join(dir, fmt.Sprintf("t%d.txt", i)), fmt.Sprintf("普通文本 %d", i))
	}
	fb := &fakeBench{speed: map[ocrPlan]float64{{4, 1}: 20, {2, 2}: 25, {1, 4}: 15}}
	var during []string
	fb.check = func(m *Manager) {
		s := &m.sched
		s.mu.Lock()
		busy := s.lanes[prioFast].running + s.lanes[prioOCR].running
		s.mu.Unlock()
		during = append(during, fmt.Sprintf("测速中=%v 其他在处理=%d", m.ocrBenching.Load(), busy))
	}
	fb.install(t)
	prov := &fakeProv{}
	prov.add("v1", "数据", dir, true)
	data := t.TempDir()
	open := func() *Manager {
		db, err := store.Open(data)
		if err != nil {
			t.Fatal(err)
		}
		m := New(db, prov, nil, data)
		useFakeOCR(t, m, "10ms")
		m.ocr.cores = 4
		m.ocr.machine = testMachine
		if err := m.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { m.Stop(); db.Close() })
		return m
	}
	m := open()
	if _, err := m.AddRoot(dir); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "全部处理完", 30*time.Second, func() bool {
		c, _ := m.DB.AllCounts()
		return c.Total == 12 && c.Pending == 0
	})
	if fb.count() != 6 {
		t.Fatalf("应测 3 种搭配、各两轮：%v", fb.calls)
	}
	for _, d := range during {
		if d != "测速中=true 其他在处理=0" {
			t.Fatalf("测速时其他文件应暂停：%v", during)
		}
	}
	info := m.OCRBenchInfo()
	if bp, ok := info.Bench.best(); !ok || bp.plan() != (ocrPlan{2, 2}) || info.Bench.Cores == 0 || info.Bench.CPU == "" {
		t.Fatalf("%+v", info.Bench)
	}
	m.ocrTuneStep()
	if p := m.ocrPlanNow(); p.threads != 2 {
		t.Fatalf("应按测速结果每个进程 2 线程：%v", p)
	}
	m.Stop()

	// 重启：同一台电脑直接用保存的结果
	m = open()
	if m.OCRBenchInfo().Bench == nil {
		t.Fatal("重启后应读出保存的结果")
	}
	if err := m.RebenchOCR(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "重新测速", 20*time.Second, func() bool { return fb.count() == 12 && !m.ocrBenching.Load() })
	m.Stop()

	// 换了 OCR 组件：重新测
	db, _ := store.Open(data)
	m2 := New(db, prov, nil, data)
	useFakeOCR(t, m2, "11ms")
	m2.ocr.cores = 4
	m2.ocr.machine = testMachine
	if m2.OCRBenchInfo().Bench != nil {
		t.Fatal("OCR 组件变了应没有结果")
	}
	db.Close()
}

// 0.2.1 手动设的 OCR 并发数（比如 8）升级后改回自动，并提示一次；保存后不再提示。
func TestConfigMigrationOCRAuto(t *testing.T) {
	prov := &fakeProv{}
	e := newEnvWith(t, prov, `{"ocrWorkers":8,"configVersion":2}`, nil)
	if c := e.m.Config(); c.OCRWorkers != 0 || c.Version != configVersion {
		t.Fatalf("%+v", c)
	}
	ns := e.m.Notices(0)
	if len(ns) != 1 || !strings.Contains(ns[0].Message, "从手动设的 8 改为「自动」") {
		t.Fatalf("应提示一次：%+v", ns)
	}
	if got := e.m.loadConfig(); got.OCRWorkers != 0 || len(e.m.cfgNotes) != 0 {
		t.Fatalf("升级后的设置应已保存，不再提示：%+v %v", got, e.m.cfgNotes)
	}
}

// 手动设了进程数：每个进程的线程数 = 核心数 ÷ 进程数；通道并发数和传给 OCR 的线程数都按它。
func TestOCRManualPlanApplied(t *testing.T) {
	prov := &fakeProv{}
	e := newEnvWith(t, prov, `{"ocrWorkers":2,"configVersion":3}`, nil)
	cores := e.m.physCores()
	p := e.m.ocrPlanNow()
	if p != ocrManualPlan(2, cores) || e.m.laneSizes(e.m.Config())[prioOCR] != 2 {
		t.Fatalf("%v", p)
	}
	if o := e.m.extractOptions(e.m.Config(), true); o.OCR.Threads != p.threads {
		t.Fatalf("传给 OCR 的线程数 %d，应为 %d", o.OCR.Threads, p.threads)
	}
	c := e.m.Config()
	c.OCRWorkers = 0
	e.m.SetConfig(c)
	if p := e.m.ocrPlanNow(); p.threads != 1 || p.procs > ocrBudget(cores, false) {
		t.Fatalf("改回自动、还没测速时应每个进程 1 线程：%v", p)
	}
}

// 由「第 i/n 页」的进度累计识别页数；文件处理完补上最后一页，单张图片算一页。
func TestOCRPageCounting(t *testing.T) {
	m := &Manager{}
	j := &job{class: prioOCR}
	for _, p := range []string{"读取 PDF 结构", "第 1/5 页", "第 2/5 页", "第 4/5 页", "第 4/5 页"} {
		m.countOCRProgress(j, p)
	}
	if n := m.ocr.pages.Load(); n != 3 {
		t.Fatalf("第 4 页开始时应已完成 3 页：%d", n)
	}
	m.finishOCRPages(j)
	img := &job{class: prioOCR}
	m.finishOCRPages(img)
	if n := m.ocr.pages.Load(); n != 6 {
		t.Fatalf("5 页的 PDF 加一张图片应为 6 页：%d", n)
	}
}

// 本地盘上太小的图片扫描时就排除（不进 OCR 队列），正常大小的照常识别。
func TestSmallImagesSkippedAtScan(t *testing.T) {
	dir := t.TempDir()
	tiny := image.NewGray(image.Rect(0, 0, 30, 30))
	var b bytes.Buffer
	png.Encode(&b, tiny)
	os.WriteFile(filepath.Join(dir, "图标.png"), b.Bytes(), 0o644)
	writePNG(t, filepath.Join(dir, "扫描.png"), 7) // 120×120，大于假 OCR 的 MinSide 50
	prov := &fakeProv{}
	prov.addKind("v1", "本地", dir, true, volume.KindLocal)
	e := newEnvWith(t, prov, "", func(m *Manager) { useFakeOCR(t, m, "1ms") })
	e.m.PauseAll() // 先只看扫描的结果
	if _, err := e.m.AddRoot(dir); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		e.m.DB.R.QueryRow(`SELECT count(*) FROM files WHERE name IN ('扫描.png','图标.png')`).Scan(&n)
		if n == 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st, _, msg := fileByName(t, e.m.DB, "图标.png"); st != store.StateMetaOnly || msg != extract.ErrTooSmall {
		t.Fatalf("太小的图片扫描时就应排除：state %d %q", st, msg)
	}
	if st, prio, _ := fileByName(t, e.m.DB, "扫描.png"); st != store.StatePending || prio != prioOCR {
		t.Fatalf("正常的图片应排进 OCR 队列：state %d prio %d", st, prio)
	}
	e.m.ResumeAll()
	e.waitIdle()
	if st, _, msg := fileByName(t, e.m.DB, "扫描.png"); st != store.StateDone {
		t.Fatalf("正常的图片应识别：state %d %s", st, msg)
	}
}

// 用真实的解析子进程测速（假 OCR 每页 50 毫秒）：每分钟页数和每个名额的内存峰值都量得出来。
func TestPoolBenchReal(t *testing.T) {
	t.Setenv("ZZ_AS_WORKER", "1")
	t.Setenv("ZZ_FAKE_OCR", "50ms")
	exe, _ := os.Executable()
	pool := worker.NewPool(exe, 1)
	pool.Args = []string{"-test.run=^$"}
	defer pool.Close()
	opts := extract.DefaultOptions()
	opts.OCR = extract.OCRConfig{Command: exe, Args: []string{"-test.run=^$", "-zzdelay=50ms"}, MinSide: 50, PageSec: 60}
	sample, cleanup, err := writeOCRSample()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	r, err := poolBench(pool, opts, sample)(context.Background(), "", ocrPlan{2, 1}, 4)
	if err != nil {
		t.Fatal(err)
	}
	perMin, mem := r.perMin, r.slotMem
	// 实际占用的 CPU 只有 Windows 量得出来（作业对象累计；假 OCR 只是等待，几乎不占 CPU，不检查大小）
	if runtime.GOOS != "windows" && r.cpu != 0 {
		t.Fatalf("实际占用 CPU %.2f", r.cpu)
	}
	// 2 个进程各识别 2 页、每页 50 毫秒：最快每分钟 2400 页
	if perMin < 100 || perMin > 2400 {
		t.Fatalf("每分钟 %.0f 页", perMin)
	}
	if _, live, _ := pool.Stats(); live != 2 {
		t.Fatalf("应有 2 个解析进程：%d", live)
	}
	if (runtime.GOOS == "linux" || runtime.GOOS == "windows") && mem < 4<<20 {
		t.Fatalf("内存峰值 %d", mem)
	}
	opts.OCR.Command = filepath.Join(t.TempDir(), "没有这个程序")
	if _, err := poolBench(pool, opts, sample)(context.Background(), "", ocrPlan{1, 1}, 1); err == nil {
		t.Fatal("OCR 组件启动不了时应报错")
	}
}

// 队列收尾：OCR 队列空了、正在识别的文件比进程少时，把空出来的线程分给它，剩下的页识别得快些；完成后恢复。
func TestOCRTailBoost(t *testing.T) {
	old := ocrTuneEvery
	ocrTuneEvery = 100 * time.Millisecond
	t.Cleanup(func() { ocrTuneEvery = old })
	dir := t.TempDir()
	vals := make([]byte, 10)
	for i := range vals {
		vals[i] = byte(20 * i)
	}
	if err := os.WriteFile(filepath.Join(dir, "扫描.tif"), testutil.MultiPageTIFF(binary.LittleEndian, 120, 120, vals), 0o644); err != nil {
		t.Fatal(err)
	}
	prov := &fakeProv{}
	prov.add("v1", "数据", dir, true)
	e := newEnvWith(t, prov, "", func(m *Manager) {
		useFakeOCR(t, m, "300ms")
		m.ocr.cores = 4
	})
	if _, err := e.m.AddRoot(dir); err != nil {
		t.Fatal(err)
	}
	var boosted int
	waitFor(t, "收尾时多分线程", 20*time.Second, func() bool {
		s := &e.m.sched
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, j := range s.inflight {
			if j.ctl != nil && j.ctl.OCRThreads() > 0 {
				boosted = j.ctl.OCRThreads()
				return true
			}
		}
		return false
	})
	p := e.m.ocrPlanNow()
	if boosted <= p.threads || boosted*1 > p.procs*p.threads {
		t.Fatalf("搭配 %v，收尾时改用 %d 线程", p, boosted)
	}
	if _, _, note := e.m.ocrStatus(); !strings.Contains(note, "队列已空") {
		t.Fatalf("界面应说明：%q", note)
	}
	waitFor(t, "识别完", 20*time.Second, func() bool {
		c, _ := e.m.DB.AllCounts()
		return c.Pending == 0 && c.Done == 1
	})
	waitFor(t, "收尾结束", 5*time.Second, func() bool {
		_, _, note := e.m.ocrStatus()
		return note == ""
	})
}

// OCR 剩余时间按页数估算：队列里的 PDF 按已识别的平均每页大小估页数，图片算一页，正在识别的按剩下的页数。
func TestOCREtaByPages(t *testing.T) {
	dir := t.TempDir()
	prov := &fakeProv{}
	prov.addKind("v1", "本地", dir, true, volume.KindLocal)
	e := newEnv(t, prov)
	e.m.PauseAll()
	r, err := e.m.AddRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	e.waitIdle()
	add := func(name, ext string, size int64) int64 {
		res, err := e.m.DB.W.Exec(`INSERT INTO files(root_id,volume_id,rel_path,inner,name,ext,kind,size,mtime,state,err,hash,text_len,ocr,meta,prio,indexed_at)
VALUES(?,?,?,'',?,?,'',?,1,0,'','',0,0,'',?,0)`, r.ID, r.VolumeID, name, name, ext, size, prioOCR)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	add("a.pdf", "pdf", 10*(200<<10))
	b := add("b.pdf", "pdf", 10*(200<<10))
	for _, n := range []string{"1.jpg", "2.jpg", "3.png"} {
		add(n, "jpg", 100<<10)
	}
	big := add("大.pdf", "pdf", 50*(200<<10))
	m := e.m
	if eta := m.ocrEta(6); eta != 0 {
		t.Fatalf("还没有速度时退回按文件数估算（还没完成过文件，无法估算）：%d", eta)
	}
	m.ocr.mu.Lock()
	m.ocr.pdfBytes, m.ocr.pdfPages = 100*(200<<10), 100 // 平均每页 200 KB
	m.ocr.rate.add(0, 0)
	m.ocr.rate.add(2*time.Minute, 100) // 每分钟 50 页
	m.ocr.mu.Unlock()
	s := &m.sched
	s.mu.Lock()
	s.inflight[big] = &job{f: store.File{ID: big, Ext: "pdf", Size: 50 * (200 << 10)}, lane: s.lanes[prioOCR], pagesTotal: 50, pagesDone: 10}
	s.mu.Unlock()
	// 队列：2 个 PDF 各 10 页 + 3 张图片 = 23 页；正在识别的大文件还剩 40 页；共 63 页，每分钟 50 页
	if eta := m.ocrEta(6); eta != 75 {
		t.Fatalf("剩余时间应为 75 秒：%d", eta)
	}
	// 普通文件阶段记下了真实页数的按真实页数算：b.pdf 虽然只有 2 MB，没有文字层的有 30 页；
	// 大文件记下的 50 页和按大小估算的相同
	m.DB.W.Exec(`UPDATE files SET ocr_pages=30 WHERE id=?`, b)
	m.DB.W.Exec(`UPDATE files SET ocr_pages=50 WHERE id=?`, big)
	if eta := m.ocrEta(6); eta != 99 {
		t.Fatalf("剩余时间应为 99 秒（83 页）：%d", eta)
	}
	s.mu.Lock()
	delete(s.inflight, big)
	s.mu.Unlock()
}

// 内存允许的进程数：在边界附近小幅波动时不变；少了半个名额以上才减少，减少后 10 分钟内不增加。
func TestOCRMemCapHysteresis(t *testing.T) {
	for _, c := range []struct {
		cur   int
		slots float64
		want  int
	}{
		{0, 10.7, 10}, {10, 9.6, 10}, {10, 9.4, 9}, {10, 7.2, 7}, {10, 11.4, 10}, {10, 11.6, 11}, {10, -1, ocrMaxProcs}, {1, 0.2, 1},
	} {
		if got := nextMemCap(c.cur, c.slots); got != c.want {
			t.Errorf("现在 %d 个、内存够 %.1f 个：%d，应为 %d", c.cur, c.slots, got, c.want)
		}
	}
	e := newEnvWith(t, &fakeProv{}, "", nil)
	m := e.m
	m.ocr.mu.Lock()
	defer m.ocr.mu.Unlock()
	m.ocr.memCap = 12
	m.ocr.memCap = m.memCapLocked(9, time.Hour)
	if got := m.memCapLocked(12, time.Hour+5*time.Minute); m.ocr.memCap != 9 || got != 9 {
		t.Fatalf("刚减少 5 分钟不应增加：%d", got)
	}
	if got := m.memCapLocked(12, time.Hour+11*time.Minute); got != 12 {
		t.Fatalf("过了 10 分钟可以增加：%d", got)
	}
}

// OCR 引擎对比：抽样识别过的文件，各模型各认一遍，和基准模型比认出的字、每页用时。
func TestOCRTest(t *testing.T) {
	dir := t.TempDir()
	prov := &fakeProv{}
	prov.addKind("v1", "本地", dir, true, volume.KindLocal)
	e := newEnv(t, prov)
	e.m.PauseAll()
	r, err := e.m.AddRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	e.waitIdle()
	for i, name := range []string{"a.pdf", "b.png", "c.png", "未识别.png"} {
		ocr := 1
		if i == 3 {
			ocr = 0
		}
		if _, err := e.m.DB.W.Exec(`INSERT INTO files(root_id,volume_id,rel_path,inner,name,ext,kind,size,mtime,state,err,hash,text_len,ocr,meta,prio,indexed_at)
VALUES(?,?,?,'',?,?,'',100,1,1,'','',0,?,'',?,1)`, r.ID, r.VolumeID, name, name, extract.Ext(name), ocr, prioOCR); err != nil {
			t.Fatal(err)
		}
	}
	texts := map[string]string{"v6": "关于做好安全生产检查工作的通知", "v6q": "关于做好安全生产检查工作的通", "v4": "关于做奸安全生产检查工伤的通知"}
	old := newOCRTestRunner
	t.Cleanup(func() { newOCRTestRunner = old })
	var calls []string
	newOCRTestRunner = func(m *Manager) ocrTestRunner {
		return func(ctx context.Context, model, path, ext string) (doc.Result, time.Duration) {
			calls = append(calls, model+":"+filepath.Base(path))
			if model == "v4" && strings.HasSuffix(path, "c.png") {
				return doc.Result{Status: doc.StatusFailed, Err: "OCR 组件已退出"}, time.Second
			}
			pages := 0
			if ext == "pdf" {
				pages = 3
			}
			return doc.Result{Status: doc.StatusOK, Text: texts[model], OCRPages: pages}, 600 * time.Millisecond
		}
	}
	samples, err := e.m.ocrSamples(10)
	if err != nil || len(samples) != 3 {
		t.Fatalf("应抽到 3 个识别出文字的文件：%v %v", samples, err)
	}
	rep := e.m.runOCRTest(context.Background(), samples, []string{"v6", "v6q", "v4"})
	// 按模型逐个来，每个模型先认一遍样张
	if len(calls) != 12 || calls[0] != "v6:sample.png" || calls[4] != "v6q:sample.png" {
		t.Fatalf("%v", calls)
	}
	if rep.Base != "v6" || rep.Sampled != 3 || len(rep.Models) != 3 || len(rep.Files) != 3 {
		t.Fatalf("%+v", rep)
	}
	v6, v6q, v4 := rep.Models[0], rep.Models[1], rep.Models[2]
	if v6.Pages != 5 || v6.Files != 3 || v6.MsPage != 360 || v6.Agree != 100 {
		t.Fatalf("普通版：3 个文件 5 页（PDF 3 页），每页 0.36 秒：%+v", v6)
	}
	if v6q.CharsVsBase > -6 || v6q.CharsVsBase < -7 || v6q.Agree < 90 || v6q.Agree >= 100 || v6q.LowFiles != 0 {
		t.Fatalf("量化版少认一个字：%+v", v6q)
	}
	if v4.Failed != 1 || v4.Files != 2 || v4.LowFiles != 2 {
		t.Fatalf("v4：%+v", v4)
	}
	s := OCRTestText(rep, false)
	if !strings.Contains(s, "PP-OCRv6 small（量化版）：认成功 3 个文件 5 页") || !strings.Contains(s, "失败：OCR 组件已退出") || strings.Contains(s, "【") {
		t.Fatal(s)
	}
	if s := OCRTestText(rep, true); !strings.Contains(s, "【PP-OCRv4】关于做奸安全生产检查工伤的通知") {
		t.Fatal(s)
	}
}
