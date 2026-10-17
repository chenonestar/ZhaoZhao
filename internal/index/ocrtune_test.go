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

// 按实测最快的「每个进程几个线程」换算各种线程预算下的搭配。
func TestOCRPlanFor(t *testing.T) {
	for _, c := range []struct {
		budget, maxProcs, best int
		want                   ocrPlan
	}{
		{4, 32, 1, ocrPlan{4, 1}},
		{2, 32, 1, ocrPlan{2, 1}}, // 普通文件还在处理：预算减半，仍是每个进程 1 线程
		{8, 32, 2, ocrPlan{4, 2}},
		{6, 32, 4, ocrPlan{2, 3}}, // 6 ÷ 4 四舍五入是 2 个进程
		{8, 3, 1, ocrPlan{3, 2}},  // 内存只够 3 个进程
		{1, 32, 1, ocrPlan{1, 1}},
		{4, 32, 8, ocrPlan{1, 4}},
	} {
		if got := ocrPlanFor(c.budget, c.maxProcs, c.best); got != c.want {
			t.Errorf("预算 %d、最多 %d 个进程、最快 %d 线程：%v，应为 %v", c.budget, c.maxProcs, c.best, got, c.want)
		}
	}
	if ocrBudget(8, true) != 4 || ocrBudget(8, false) != 8 || ocrBudget(2, true) != 2 || ocrBudget(1, true) != 1 {
		t.Fatal("线程预算")
	}
}

// 进程数按内存限制：留给用户至少 1 GB（或总内存的 15%），OCR 进程现在占着的加回来；
// 内存紧张时立即减少，宽裕时要多出半个名额才增加。
func TestOCRMemCap(t *testing.T) {
	const gb = 1 << 30
	if s := ocrMemSlots(16*gb, 10*gb, gb/2, 0); s < 15 || s > 16 {
		t.Fatalf("16 GB、可用 10 GB、每个 0.5 GB：%.1f", s) // (10 - 2.4) / 0.5 = 15.2
	}
	if s := ocrMemSlots(4*gb, 1*gb+gb/2, gb/2, 0); s != 1 {
		t.Fatalf("4 GB、可用 1.5 GB：%.1f", s) // (1.5 - 1) / 0.5
	}
	if s := ocrMemSlots(4*gb, gb/2, gb/2, 2); s != 1 {
		t.Fatalf("两个 OCR 进程占着 1 GB：%.1f", s) // (0.5 + 1 - 1) / 0.5
	}
	if ocrMemSlots(0, 0, gb, 0) != -1 || nextMemCap(3, -1) != ocrMaxProcs {
		t.Fatal("取不到内存信息时不限制")
	}
	if nextMemCap(0, 3.2) != 3 || nextMemCap(3, 2.9) != 2 || nextMemCap(3, 4.4) != 3 || nextMemCap(3, 4.6) != 4 || nextMemCap(0, 0.3) != 1 {
		t.Fatal("内存上限的回差不对")
	}
}

func TestTailThreads(t *testing.T) {
	p := ocrPlan{4, 1}
	if tailThreads(p, 4, 1) != 4 || tailThreads(p, 4, 2) != 2 || tailThreads(p, 4, 3) != 0 || tailThreads(p, 4, 4) != 0 || tailThreads(p, 4, 0) != 0 {
		t.Fatal("4×1")
	}
	if tailThreads(ocrPlan{2, 2}, 4, 1) != 4 || tailThreads(ocrPlan{1, 4}, 4, 1) != 0 {
		t.Fatal("2×2 / 1×4")
	}
}

func sprint(ps []ocrPlan) string {
	var parts []string
	for _, p := range ps {
		parts = append(parts, fmt.Sprintf("{%d %d}", p.procs, p.threads))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// 测速：每种搭配识别同样多的页（线程预算），选最快的；某种失败不影响其余的，全部失败时没有结果。
func TestRunOCRBench(t *testing.T) {
	speed := map[ocrPlan]float64{{4, 1}: 26, {2, 2}: 20, {1, 4}: 16}
	var calls []string
	run := func(ctx context.Context, p ocrPlan, pages int) (float64, uint64, error) {
		calls = append(calls, fmt.Sprintf("%v:%d", p, pages))
		if p == (ocrPlan{2, 2}) {
			return 0, 0, errors.New("内存不足")
		}
		return speed[p], 300 << 20, nil
	}
	b := runOCRBench(context.Background(), 4, ocrCandidates(4, 32), run, nil)
	if bp, ok := b.best(); !ok || bp.plan() != (ocrPlan{4, 1}) || b.Err != "" || len(b.Plans) != 3 || b.Plans[1].Err != "内存不足" {
		t.Fatalf("%+v", b)
	}
	if strings.Join(calls, " ") != "4 个进程 × 1 线程:4 2 个进程 × 2 线程:4 1 个进程 × 4 线程:4" {
		t.Fatalf("每种都应识别 4 页：%v", calls)
	}
	if s := b.Summary(); !strings.Contains(s, "4 个进程 × 1 线程 每分钟 26.0 页（每个约 300 MB）") || !strings.Contains(s, "最快：4 个进程 × 1 线程") {
		t.Fatal(s)
	}
	if ocrBenchPages(2) != 4 || ocrBenchPages(8) != 8 {
		t.Fatal("预算太小时页数加倍")
	}
	bad := runOCRBench(context.Background(), 2, ocrCandidates(2, 32), func(context.Context, ocrPlan, int) (float64, uint64, error) {
		return 0, 0, errors.New("OCR 组件启动失败")
	}, nil)
	if _, ok := bad.best(); ok || bad.Err != "OCR 组件启动失败" {
		t.Fatalf("%+v", bad)
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
		return func(ctx context.Context, p ocrPlan, pages int) (float64, uint64, error) {
			if st, err := os.Stat(sample); err != nil || st.Size() != int64(len(ocrSample)) {
				return 0, 0, fmt.Errorf("样张没写出来：%v", err)
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
			return v, 200 << 20, nil
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
	if fb.count() != 3 {
		t.Fatalf("应测 3 种搭配：%v", fb.calls)
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
	waitFor(t, "重新测速", 20*time.Second, func() bool { return fb.count() == 6 && !m.ocrBenching.Load() })
	m.Stop()

	// 换了 OCR 组件：重新测
	db, _ := store.Open(data)
	m2 := New(db, prov, nil, data)
	useFakeOCR(t, m2, "11ms")
	m2.ocr.cores = 4
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
	if p := e.m.ocrPlanNow(); p.threads != 1 || p.procs > ocrBudget(cores, true) {
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
	perMin, mem, err := poolBench(pool, opts, sample)(context.Background(), ocrPlan{2, 1}, 4)
	if err != nil {
		t.Fatal(err)
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
	if _, _, err := poolBench(pool, opts, sample)(context.Background(), ocrPlan{1, 1}, 1); err == nil {
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
