package index

import (
	"bytes"
	"fmt"
	"github.com/chenonestar/zhaozhao/internal/extract"
	"github.com/chenonestar/zhaozhao/internal/store"
	"github.com/chenonestar/zhaozhao/internal/volume"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOCRCandidates(t *testing.T) {
	for _, c := range []struct {
		budget int
		want   string
		def    ocrPlan
	}{
		{1, "[{1 1}]", ocrPlan{1, 1}},
		{4, "[{1 4} {2 2}]", ocrPlan{1, 4}},
		{8, "[{1 8} {2 4} {4 2}]", ocrPlan{2, 4}},
		{16, "[{1 16} {2 8} {4 4}]", ocrPlan{4, 4}},
	} {
		if got := sprint(ocrCandidates(c.budget)); got != c.want {
			t.Errorf("预算 %d：%s，应为 %s", c.budget, got, c.want)
		}
		if got := ocrDefaultPlan(c.budget); got != c.def {
			t.Errorf("预算 %d 的默认搭配 %v，应为 %v", c.budget, got, c.def)
		}
	}
	if p := ocrManualPlan(8, 8); p != (ocrPlan{8, 1}) {
		t.Errorf("手动 8 个进程、8 核：%v", p)
	}
}

func sprint(ps []ocrPlan) string {
	var parts []string
	for _, p := range ps {
		parts = append(parts, fmt.Sprintf("{%d %d}", p.procs, p.threads))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// 模拟：每种搭配有固定速度，每 30 秒走一步。应把所有搭配都测一遍，选最快的；预算变化后换一套，回来时直接用测过的结果。
func TestOCRTunerPicksFastest(t *testing.T) {
	speed := map[ocrPlan]float64{{1, 8}: 12, {2, 4}: 15, {4, 2}: 18, {1, 4}: 9, {2, 2}: 7}
	tu := newOCRTuner()
	var now time.Duration
	var pages float64
	var notes []string
	run := func(budget int, dur time.Duration, saturated bool) ocrPlan {
		var p ocrPlan
		for end := now + dur; now < end; now += 30 * time.Second {
			var note string
			p, _, note = tu.step(budget, saturated, int64(pages), now)
			if note != "" {
				notes = append(notes, note)
			}
			if saturated {
				pages += speed[p] / 2
			}
		}
		return p
	}
	if p := run(8, 10*time.Second, true); p != (ocrPlan{2, 4}) {
		t.Fatalf("开始时应用默认搭配：%v", p)
	}
	if tu.describe() == "" {
		t.Fatal("比较期间应有说明")
	}
	if p := run(8, 40*time.Minute, true); p != (ocrPlan{4, 2}) {
		t.Fatalf("应选最快的 4×2：%v，%v", p, notes)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "选用 4 个进程 × 2 线程") {
		t.Fatalf("说明：%v", notes)
	}
	if tu.describe() != "" {
		t.Fatal("选定后不应再显示比较中")
	}
	// 普通文件又开始处理：预算减半，换一套搭配比较
	if p := run(4, 30*time.Minute, true); p != (ocrPlan{1, 4}) {
		t.Fatalf("预算 4 应选 1×4：%v", p)
	}
	// 回到只剩 OCR：直接用以前测出的结果
	if p := run(8, 30*time.Second, true); p != (ocrPlan{4, 2}) {
		t.Fatalf("应直接用测过的 4×2：%v", p)
	}
	// 不满负荷时不测（排队的文件不够）：不会改变
	if p := run(8, 3*time.Hour, false); p != (ocrPlan{4, 2}) {
		t.Fatalf("%v", p)
	}
	// 2 小时后复查：速度变了（比如别的程序占了 CPU），重新选
	speed[ocrPlan{4, 2}] = 5
	if p := run(8, 3*time.Hour, true); p != (ocrPlan{2, 4}) {
		t.Fatalf("复查后应改用 2×4：%v", p)
	}
}

// 页数太少时延长测量（最长 15 分钟）。
func TestOCRTunerWaitsForEnoughPages(t *testing.T) {
	tu := newOCRTuner()
	tu.step(8, true, 0, 0)
	if _, changed, _ := tu.step(8, true, 3, ocrWarmup+ocrMeasureMin+time.Second); changed {
		t.Fatal("只有 3 页时不应结束测量")
	}
	if _, changed, _ := tu.step(8, true, 5, ocrWarmup+ocrMeasureMax+time.Second); !changed {
		t.Fatal("超过最长时间后应结束测量、换下一种")
	}
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
	if p := e.m.ocrPlanNow(); p != ocrDefaultPlan(max(2, cores/2)) {
		t.Fatalf("改回自动后应从默认搭配开始：%v", p)
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
