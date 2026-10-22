package index

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"github.com/chenonestar/zhaozhao/internal/awake"
	"image"
	"image/color"
	"image/png"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenonestar/zhaozhao/internal/extract"
	"github.com/chenonestar/zhaozhao/internal/search"
	"github.com/chenonestar/zhaozhao/internal/store"
	"github.com/chenonestar/zhaozhao/internal/volume"
	"github.com/chenonestar/zhaozhao/internal/worker"
)

// 测试二进制自己充当 OCR 组件（RapidOCR-json 的管道协议），Windows、Linux 都能跑。
// 也能充当解析子进程（ZZ_AS_WORKER=1；参数里没有 -zzdelay 的才是解析子进程，有的是它启动的假 OCR）。
func TestMain(m *testing.M) {
	fakeOCRArg := false
	for _, a := range os.Args {
		fakeOCRArg = fakeOCRArg || strings.HasPrefix(a, "-zzdelay=")
	}
	if os.Getenv("ZZ_AS_WORKER") == "1" && !fakeOCRArg {
		worker.Serve()
		os.Exit(0)
	}
	if os.Getenv("ZZ_FAKE_OCR") != "" {
		fakeOCR(os.Getenv("ZZ_FAKE_OCR"))
		os.Exit(0)
	}
	// 当作没人在用电脑（OCR 不因此减半），需要时由测试换掉
	userIdleTime = func() time.Duration { return time.Hour }
	// OCR 测速默认用模拟的（立即返回，进程越多越快），需要时由测试换掉
	newOCRBenchRunner = func(m *Manager, pool *worker.Pool, sample string) ocrBenchRunner {
		return func(ctx context.Context, p ocrPlan, pages int) (benchRun, error) {
			return benchRun{perMin: float64(10*p.procs - p.threads), slotMem: 200 << 20}, nil
		}
	}
	os.Exit(m.Run())
}

func fakeOCR(delay string) {
	d, _ := time.ParseDuration(delay)
	fmt.Println("OCR init completed.")
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		time.Sleep(d)
		fmt.Println(`{"code":100,"data":[{"text":"fakeocrword"}]}`)
	}
}

func useFakeOCR(t *testing.T, m *Manager, delay string) {
	t.Helper()
	t.Setenv("ZZ_FAKE_OCR", delay)
	// 参数里带上延迟：常驻的 OCR 进程按命令行区分，不会复用上一个测试留下的（延迟不同的）进程
	m.ocrCfg = extract.OCRConfig{Command: os.Args[0], Args: []string{"-test.run=^$", "-zzdelay=" + delay}, MinSide: 50, PageSec: 60}
}

func writePNG(t *testing.T, p string, seed int) {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, 120, 120))
	img.Set(seed%120, seed/120, color.Gray{uint8(seed)}) // 每张图内容不同，避免按哈希复用
	var b bytes.Buffer
	png.Encode(&b, img)
	if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newEnvWith 按给定设置创建管理器，start 前可调整（例如换成假 OCR）。
func newEnvWith(t *testing.T, prov *fakeProv, cfg string, before func(m *Manager)) *env {
	t.Helper()
	data := t.TempDir()
	if cfg != "" {
		os.WriteFile(filepath.Join(data, "config.json"), []byte(cfg), 0o600)
	}
	db, err := store.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	m := New(db, prov, nil, data)
	if before != nil {
		before(m)
	}
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Stop(); db.Close() })
	return &env{t: t, m: m, prov: prov, data: data}
}

// v0.1.2 的死锁场景：网络盘 + 大量 OCR 文件 + 名额紧张。旧调度在完成十几个文件后就会永远卡住。
func TestNetworkOCRNoDeadlock(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 40; i++ {
		writePNG(t, filepath.Join(dir, fmt.Sprintf("img%02d.png", i)), i)
	}
	for i := 0; i < 20; i++ {
		write(t, filepath.Join(dir, fmt.Sprintf("t%02d.txt", i)), fmt.Sprintf("普通文本 %d", i))
	}
	prov := &fakeProv{}
	prov.addKind("v1", "共享", dir, true, volume.KindNetwork)
	e := newEnvWith(t, prov, `{"workers":2,"networkReadConc":1,"ocrWorkers":1,"legacyWorkers":1,"configVersion":2}`,
		func(m *Manager) { useFakeOCR(t, m, "50ms") })
	if _, err := e.m.AddRoot(dir); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "全部提取完成", 60*time.Second, func() bool {
		c, _ := e.m.DB.AllCounts()
		return c.Total == 60 && c.Pending == 0
	})
	c, _ := e.m.DB.AllCounts()
	if c.Done != 60 {
		t.Fatalf("应全部提取成功: %+v", c)
	}
	// 临时副本已全部删除
	if left, _ := filepath.Glob(filepath.Join(os.TempDir(), "zhaozhao-net-*")); len(left) > 0 {
		t.Fatalf("网络盘临时副本没有删除: %v", left)
	}
}

// 网络盘文件经本机副本提取：结果、去重、错误信息都和直接读取一致，错误里不出现临时路径。
func TestNetworkCopyExtraction(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.txt"), "网络盘上的内容 quokka")
	write(t, filepath.Join(dir, "b.txt"), "网络盘上的内容 quokka") // 内容相同：复用
	write(t, filepath.Join(dir, "坏的.docx"), "not a zip")
	prov := &fakeProv{}
	prov.addKind("v1", "共享", dir, true, volume.KindNetwork)
	e := newEnvWith(t, prov, "", nil)
	e.eng = newEngine(e.m)
	if _, err := e.m.AddRoot(dir); err != nil {
		t.Fatal(err)
	}
	e.waitIdle()
	if c, _ := e.m.DB.AllCounts(); c.Done != 2 {
		t.Fatalf("两个文本都应提取完成: %+v", c)
	}
	if got := e.names("quokka"); len(got) == 0 { // 内容相同的文件在结果里合并为一条
		t.Fatalf("%v", got)
	}
	fl, _ := e.m.DB.ListFailed(10)
	if len(fl) != 1 || strings.Contains(fl[0].Err, "zhaozhao-net-") {
		t.Fatalf("失败原因不应出现临时副本路径: %+v", fl)
	}
}

// 卡住检测：有任务却长时间没有进展时，状态里给出时长，日志里写明各线程位置。
func TestStallDetection(t *testing.T) {
	oldAfter, oldEvery := stallAfter, watchEvery
	stallAfter, watchEvery = 2*time.Second, 200*time.Millisecond
	t.Cleanup(func() { stallAfter, watchEvery = oldAfter, oldEvery })
	var logBuf syncBuf
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	dir := t.TempDir()
	writePNG(t, filepath.Join(dir, "slow.png"), 1)
	prov := &fakeProv{}
	prov.addKind("v1", "本地", dir, true, volume.KindLocal)
	e := newEnvWith(t, prov, "", func(m *Manager) { useFakeOCR(t, m, "6s") }) // 一张图识别 6 秒，期间没有进度
	if _, err := e.m.AddRoot(dir); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "判定为疑似卡住", 15*time.Second, func() bool { return e.m.Status().StalledSec > 0 })
	waitFor(t, "日志记录卡住", 5*time.Second, func() bool { return strings.Contains(logBuf.String(), "疑似卡住") })
	s := logBuf.String()
	if !strings.Contains(s, "slow.png") || !strings.Contains(s, "goroutine") {
		t.Fatalf("日志应包含卡住的文件和线程位置:\n%s", s)
	}
	st := e.m.Status()
	if len(st.Current) != 1 || st.Current[0].Stage != "OCR 识别" || st.Current[0].Kind != "OCR" {
		t.Fatalf("处理列表应显示步骤: %+v", st.Current)
	}
}

func newEngine(m *Manager) *search.Engine { return &search.Engine{DB: m.DB, Res: m, Norm: m.Norm} }

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// 旧版本保存的设置（没有版本号）：OCR 并发数从旧默认值 1 改为自动，Ctrl+Alt+F 清除；
// 保存过一次之后不再重复升级（用户自己设的 1 不会被改掉）。
func TestConfigMigration(t *testing.T) {
	prov := &fakeProv{}
	e := newEnvWith(t, prov, `{"ocrWorkers":1,"hotkey":"Ctrl+Alt+F","workers":3}`, nil)
	c := e.m.Config()
	if c.OCRWorkers != 0 || c.Hotkey != "" || c.Workers != 3 || c.LogLevel != "detail" || c.Version != configVersion {
		t.Fatalf("%+v", c)
	}
	c.OCRWorkers = 1
	c.Version = 0 // 模拟界面没有带回版本号
	if err := e.m.SetConfig(c); err != nil {
		t.Fatal(err)
	}
	if got := e.m.loadConfig(); got.OCRWorkers != 1 {
		t.Fatalf("保存后的设置不应再次升级: %+v", got)
	}
}

// 三条通道同时进行：大量普通文件还没处理完时，OCR 已经在做（旧版要等普通文件全部完成）。
func TestLanesRunConcurrently(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 1500; i++ {
		write(t, filepath.Join(dir, fmt.Sprintf("d%02d", i%20), fmt.Sprintf("t%04d.txt", i)), fmt.Sprintf("普通文本 %d", i))
	}
	for i := 0; i < 3; i++ {
		writePNG(t, filepath.Join(dir, fmt.Sprintf("img%d.png", i)), i)
	}
	prov := &fakeProv{}
	prov.addKind("v1", "本地", dir, true, volume.KindLocal)
	e := newEnvWith(t, prov, `{"workers":1,"ocrWorkers":1,"configVersion":2}`, func(m *Manager) { useFakeOCR(t, m, "1ms") })
	if _, err := e.m.AddRoot(dir); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "OCR 在普通文件处理期间完成", 30*time.Second, func() bool {
		st := e.m.Status()
		return st.Totals.OCR >= 3
	})
	st := e.m.Status()
	t.Logf("OCR 完成时普通文件还剩 %d 个", st.FastQueue)
	if st.FastQueue == 0 {
		t.Fatalf("OCR 应与普通文件同时进行，而不是等普通文件全部完成: %+v", st.Totals)
	}
	waitFor(t, "全部完成", 3*time.Minute, func() bool { c, _ := e.m.DB.AllCounts(); return c.Pending == 0 })
}

// 「正在处理」按进入列表的先后排序：开始处理（计时重置）、用时跨过整秒都不改变位置。
func TestCurrentJobsStableOrder(t *testing.T) {
	m := &Manager{}
	m.sched.inflight = map[int64]*job{}
	base := time.Now()
	for i, id := range []int64{5, 3, 9, 7} {
		m.sched.inflight[id] = &job{f: store.File{ID: id}, path: fmt.Sprint(id), queued: base.Add(time.Duration(i) * time.Millisecond),
			since: awake.Now() - time.Duration(i)*700*time.Millisecond}
	}
	m.sched.inflight[7].queued = m.sched.inflight[9].queued // 同时进入：按编号
	order := func() string {
		var ids []string
		for _, c := range m.currentJobs() {
			ids = append(ids, fmt.Sprint(c.ID))
		}
		return strings.Join(ids, ",")
	}
	if got := order(); got != "5,3,7,9" {
		t.Fatalf("顺序 %s", got)
	}
	m.sched.inflight[5].since = awake.Now() // 5 刚从等待变为开始处理
	if got := order(); got != "5,3,7,9" {
		t.Fatalf("开始处理后位置变了: %s", got)
	}
}

// 设置里开了「用电脑时放慢」、用户正在操作时，每处理完一个文件停一下；默认不停（子进程本来就是低优先级）。
func TestPauseForUser(t *testing.T) {
	oldIdle, oldPause := userIdleTime, userPause
	t.Cleanup(func() { userIdleTime, userPause = oldIdle, oldPause })
	userPause = 300 * time.Millisecond
	idle := time.Second
	userIdleTime = func() time.Duration { return idle }
	m := newEnv(t, &fakeProv{}).m
	took := func() time.Duration {
		t0 := time.Now()
		m.pauseForUser()
		return time.Since(t0)
	}
	if d := took(); d > 100*time.Millisecond {
		t.Fatalf("默认不停：%v", d)
	}
	c := m.Config()
	c.SlowWhenActive = true
	if err := m.SetConfig(c); err != nil {
		t.Fatal(err)
	}
	if d := took(); d < 250*time.Millisecond {
		t.Fatalf("开了设置、用户正在操作时应停一下：%v", d)
	}
	idle = time.Minute
	if d := took(); d > 100*time.Millisecond {
		t.Fatalf("用户没在操作时不停：%v", d)
	}
}
