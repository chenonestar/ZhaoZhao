package index

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenonestar/zhaozhao/internal/search"
	"github.com/chenonestar/zhaozhao/internal/store"
	"github.com/chenonestar/zhaozhao/internal/volume"
)

// fakeProv 用若干临时目录模拟多块磁盘，可随时「拔出」或「换盘符」。
type fakeProv struct {
	mu   sync.Mutex
	vols map[string]*fakeVol
}

type fakeVol struct {
	info   volume.Info
	online bool
}

func (p *fakeProv) add(id, label, mount string, online bool) {
	p.addKind(id, label, mount, online, volume.KindRemovable)
}

func (p *fakeProv) addKind(id, label, mount string, online bool, kind string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.vols == nil {
		p.vols = map[string]*fakeVol{}
	}
	p.vols[id] = &fakeVol{info: volume.Info{ID: id, Label: label, Kind: kind, Mount: mount}, online: online}
}

func (p *fakeProv) set(id string, online bool, mount string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v := p.vols[id]
	v.online = online
	if mount != "" {
		v.info.Mount = mount
	}
}

func (p *fakeProv) Detect(path string) (volume.Info, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	abs, _ := filepath.Abs(path)
	for _, v := range p.vols {
		rel, err := filepath.Rel(v.info.Mount, abs)
		if err == nil && !strings.HasPrefix(rel, "..") {
			if rel == "." {
				rel = ""
			}
			return v.info, filepath.ToSlash(rel), nil
		}
	}
	return volume.Info{}, "", fmt.Errorf("no volume for %s", path)
}

func (p *fakeProv) List() []volume.Info {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []volume.Info
	for _, v := range p.vols {
		if v.online {
			out = append(out, v.info)
		}
	}
	return out
}

type env struct {
	t    *testing.T
	m    *Manager
	eng  *search.Engine
	prov *fakeProv
	data string
}

func newEnv(t *testing.T, prov *fakeProv) *env {
	t.Helper()
	data := t.TempDir()
	db, err := store.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	m := New(db, prov, nil, data)
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Stop(); db.Close() })
	return &env{t: t, m: m, eng: &search.Engine{DB: db, Res: m, Norm: m.Norm}, prov: prov, data: data}
}

func (e *env) names(q string) []string {
	r, err := e.eng.Search(context.Background(), search.Params{Query: q, Limit: 100})
	if err != nil {
		e.t.Fatalf("%q: %v", q, err)
	}
	var out []string
	for _, x := range r.Results {
		out = append(out, x.Name)
	}
	return out
}

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("超时等待: %s", what)
}

func (e *env) waitIdle() {
	e.t.Helper()
	time.Sleep(200 * time.Millisecond)
	waitFor(e.t, "索引空闲", 20*time.Second, func() bool {
		s := e.m.Status()
		return !s.Scanning && s.Totals.Pending == 0 && len(s.Current) == 0
	})
}

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func has(list []string, n string) bool {
	for _, x := range list {
		if x == n {
			return true
		}
	}
	return false
}

func TestIndexAndWatch(t *testing.T) {
	dir := t.TempDir()
	prov := &fakeProv{}
	prov.addKind("v1", "本地盘", dir, true, volume.KindLocal)
	write(t, filepath.Join(dir, "docs", "合同.txt"), "违约金为合同总额的百分之五")
	write(t, filepath.Join(dir, ".hidden", "秘密.txt"), "隐藏目录里的内容 xyzzy")
	write(t, filepath.Join(dir, "node_modules", "pkg", "a.txt"), "依赖目录 xyzzy")
	write(t, filepath.Join(dir, "docs", "big.txt"), strings.Repeat("a", 10))
	write(t, filepath.Join(dir, "bin.dat"), "\x00\x01\x02\x03\x00")
	e := newEnv(t, prov)
	if _, err := e.m.AddRoot(dir); err != nil {
		t.Fatal(err)
	}
	e.waitIdle()

	if got := e.names("违约金"); !has(got, "合同.txt") {
		t.Fatalf("内容搜索: %v", got)
	}
	if got := e.names("xyzzy"); !has(got, "秘密.txt") || has(got, "a.txt") {
		t.Fatalf("隐藏目录应纳入、node_modules 应排除: %v", got)
	}
	if got := e.names("bin.dat"); !has(got, "bin.dat") { // F-IDX-11：无法提取内容的文件也可按文件名搜索
		t.Fatalf("文件名搜索: %v", got)
	}

	// 新增文件被实时监听发现
	write(t, filepath.Join(dir, "docs", "新增.txt"), "监听到的新内容 quokka")
	waitFor(t, "新文件可被搜到", 15*time.Second, func() bool { return has(e.names("quokka"), "新增.txt") })
	// 修改
	write(t, filepath.Join(dir, "docs", "新增.txt"), "改过的内容 wombat")
	waitFor(t, "修改后可被搜到", 15*time.Second, func() bool { return has(e.names("wombat"), "新增.txt") })
	if has(e.names("quokka"), "新增.txt") {
		t.Fatal("旧内容应不再命中")
	}
	// 删除
	os.Remove(filepath.Join(dir, "docs", "新增.txt"))
	waitFor(t, "删除后不再命中", 15*time.Second, func() bool { return !has(e.names("wombat"), "新增.txt") })
	// 新建目录及其中的文件
	write(t, filepath.Join(dir, "newdir", "sub", "x.txt"), "嵌套新目录 platypus")
	waitFor(t, "新目录内文件可被搜到", 15*time.Second, func() bool { return has(e.names("platypus"), "x.txt") })
	// 目录整体改名
	os.Rename(filepath.Join(dir, "newdir"), filepath.Join(dir, "renamed"))
	waitFor(t, "改名后路径更新", 15*time.Second, func() bool {
		r, _ := e.eng.Search(context.Background(), search.Params{Query: "platypus"})
		return len(r.Results) == 1 && strings.Contains(r.Results[0].Path, "renamed")
	})
}

func TestOfflineVolume(t *testing.T) {
	mountA := t.TempDir()
	prov := &fakeProv{}
	prov.add("usb1", "备份盘A", mountA, true)
	write(t, filepath.Join(mountA, "归档", "旧合同.txt"), "归档的旧合同 内容 aardvark")
	e := newEnv(t, prov)
	if _, err := e.m.AddRoot(mountA); err != nil {
		t.Fatal(err)
	}
	e.waitIdle()
	if !has(e.names("aardvark"), "旧合同.txt") {
		t.Fatal("在线时应能搜到")
	}

	// 拔出：仍可搜索，标记离线，打开时提示需接入的磁盘
	prov.set("usb1", false, "")
	waitFor(t, "卷标记为离线", 15*time.Second, func() bool {
		r, _ := e.eng.Search(context.Background(), search.Params{Query: "aardvark"})
		return len(r.Results) == 1 && !r.Results[0].Online
	})
	r, _ := e.eng.Search(context.Background(), search.Params{Query: "aardvark vol:备份盘A"})
	if len(r.Results) != 1 || r.Results[0].VolLabel != "备份盘A" {
		t.Fatalf("%+v", r.Results)
	}
	pv, err := e.eng.Preview(r.Results[0].ID, search.Params{Query: "aardvark"})
	if err != nil || len(pv.Hits) != 1 {
		t.Fatalf("离线预览: %v %+v", err, pv)
	}
	f, _ := e.m.DB.GetFile(r.Results[0].ID)
	_, err = e.m.OpenTarget(f)
	oe, ok := err.(*OfflineError)
	if !ok || oe.Label != "备份盘A" {
		t.Fatalf("应提示需接入的磁盘: %v", err)
	}

	// 离线期间，另一处（模拟换了台电脑改动）发生变化；以新盘符重新接入
	mountB := t.TempDir()
	write(t, filepath.Join(mountB, "归档", "旧合同.txt"), "归档的旧合同 内容 aardvark")
	write(t, filepath.Join(mountB, "归档", "新增文件.txt"), "离线期间新增 capybara")
	prov.set("usb1", true, mountB) // 盘符变化
	waitFor(t, "重新接入后自动对账", 20*time.Second, func() bool { return has(e.names("capybara"), "新增文件.txt") })
	r, _ = e.eng.Search(context.Background(), search.Params{Query: "aardvark"})
	if len(r.Results) != 1 || !r.Results[0].Online || !strings.HasPrefix(r.Results[0].Path, mountB) {
		t.Fatalf("盘符变化后路径应指向新位置: %+v", r.Results)
	}
	// 重新接入的卷上删除的文件在对账后消失
	os.Remove(filepath.Join(mountB, "归档", "旧合同.txt"))
	e.m.RescanAll()
	waitFor(t, "对账删除", 15*time.Second, func() bool { return !has(e.names("aardvark"), "旧合同.txt") })
}

func TestOfflineDuringIndexKeepsPending(t *testing.T) {
	mount := t.TempDir()
	prov := &fakeProv{}
	prov.add("usb2", "盘B", mount, true)
	e := newEnv(t, prov)
	e.m.PauseAll()
	write(t, filepath.Join(mount, "a.txt"), "待提取内容 narwhal")
	if _, err := e.m.AddRoot(mount); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "文件名入库", 10*time.Second, func() bool { return has(e.names("a.txt"), "a.txt") })
	if has(e.names("narwhal"), "a.txt") {
		t.Fatal("暂停时不应提取内容")
	}
	e.m.ResumeAll()
	waitFor(t, "恢复后提取", 10*time.Second, func() bool { return has(e.names("narwhal"), "a.txt") })
}

func TestRootNesting(t *testing.T) {
	dir := t.TempDir()
	prov := &fakeProv{}
	prov.addKind("v1", "本地", dir, true, volume.KindLocal)
	os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755)
	e := newEnv(t, prov)
	if _, err := e.m.AddRoot(filepath.Join(dir, "a")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.AddRoot(filepath.Join(dir, "a", "b")); err == nil {
		t.Fatal("嵌套在已有索引根内应被拒绝")
	}
	if _, err := e.m.AddRoot(dir); err == nil {
		t.Fatal("包含已有索引根应被拒绝")
	}
}

func TestSizeLimitAndRules(t *testing.T) {
	dir := t.TempDir()
	prov := &fakeProv{}
	prov.addKind("v1", "本地", dir, true, volume.KindLocal)
	write(t, filepath.Join(dir, "big.txt"), strings.Repeat("hello big content ", 200))
	write(t, filepath.Join(dir, "skip.tmp"), "临时文件 giraffe")
	write(t, filepath.Join(dir, "keep.txt"), "保留的 giraffe")
	e := newEnv(t, prov)
	cfg := e.m.Config()
	cfg.MaxTextMB = 0 // 任何非空文本都超限
	cfg.ExcludeExts = []string{"tmp"}
	cfg.MaxTextMB = 0
	if err := e.m.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.AddRoot(dir); err != nil {
		t.Fatal(err)
	}
	e.waitIdle()
	if has(e.names("giraffe"), "skip.tmp") {
		t.Fatal("扩展名排除应生效")
	}
	if !has(e.names("big.txt"), "big.txt") {
		t.Fatal("超限文件仍应可按文件名搜索")
	}
	if has(e.names("hello"), "big.txt") {
		t.Fatal("超限文件不应提取内容")
	}
	// 调大上限后，对账时自动补提取
	cfg.MaxTextMB = 100
	e.m.SetConfig(cfg)
	waitFor(t, "放宽限制后补提取", 15*time.Second, func() bool { return has(e.names("hello"), "big.txt") })
}

func TestFailedAndRetry(t *testing.T) {
	dir := t.TempDir()
	prov := &fakeProv{}
	prov.addKind("v1", "本地", dir, true, volume.KindLocal)
	// 损坏的 docx
	write(t, filepath.Join(dir, "坏的.docx"), "this is not a zip")
	e := newEnv(t, prov)
	if _, err := e.m.AddRoot(dir); err != nil {
		t.Fatal(err)
	}
	e.waitIdle()
	fl, _ := e.m.DB.ListFailed(10)
	if len(fl) != 1 || fl[0].Name != "坏的.docx" || !strings.Contains(fl[0].Err, "损坏") {
		t.Fatalf("%+v", fl)
	}
	if !has(e.names("坏的"), "坏的.docx") {
		t.Fatal("失败文件仍可按文件名搜索")
	}
	if err := e.m.RetryFailed(nil); err != nil {
		t.Fatal(err)
	}
	e.waitIdle()
}

func TestDuplicateReuse(t *testing.T) {
	dir := t.TempDir()
	prov := &fakeProv{}
	prov.addKind("v1", "本地", dir, true, volume.KindLocal)
	write(t, filepath.Join(dir, "a.txt"), "完全相同的内容 koala")
	e := newEnv(t, prov)
	if _, err := e.m.AddRoot(dir); err != nil {
		t.Fatal(err)
	}
	e.waitIdle()
	data, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	os.WriteFile(filepath.Join(dir, "b-copy.txt"), data, 0o644)
	waitFor(t, "副本被索引", 15*time.Second, func() bool {
		r, _ := e.eng.Search(context.Background(), search.Params{Query: "koala"})
		return len(r.Results) == 1 && len(r.Results[0].Dups) == 1 // 内容相同的文件合并为一条
	})
}

// OCR 集成：设置 ZHAOZHAO_OCR 与 ZHAOZHAO_OCR_FIXTURES（见 extract 包的 TestOCR）。
func TestOCRPipeline(t *testing.T) {
	fx := os.Getenv("ZHAOZHAO_OCR_FIXTURES")
	if fx == "" || os.Getenv("ZHAOZHAO_OCR") == "" {
		t.Skip("未设置 OCR 环境")
	}
	dir := t.TempDir()
	prov := &fakeProv{}
	prov.addKind("v1", "本地", dir, true, volume.KindLocal)
	for _, n := range []string{"scan.png", "scanned.pdf", "icon.png"} {
		b, err := os.ReadFile(filepath.Join(fx, n))
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, n), b, 0o644)
	}
	write(t, filepath.Join(dir, "普通.txt"), "普通文本里也有违约金三个字")
	e := newEnv(t, prov)
	if _, err := e.m.AddRoot(dir); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "OCR 完成", 90*time.Second, func() bool {
		r, _ := e.eng.Search(context.Background(), search.Params{Query: "违约金"})
		ocr := 0
		for _, x := range r.Results {
			if x.OCR {
				ocr++
			}
		}
		return len(r.Results) >= 3 && ocr >= 2
	})
	r, _ := e.eng.Search(context.Background(), search.Params{Query: "差旅费"})
	if len(r.Results) != 1 || r.Results[0].Name != "scanned.pdf" || !r.Results[0].OCR || r.Results[0].Snippets[0].Loc != "第2页" {
		t.Fatalf("扫描 PDF 的 OCR 结果应带页码: %+v", r.Results)
	}
	if got := e.names("name:icon"); !has(got, "icon.png") {
		t.Fatalf("小图仍可按文件名搜索: %v", got)
	}
	// 复制一份图片：按内容哈希复用识别结果，合并为一条
	b, _ := os.ReadFile(filepath.Join(dir, "scan.png"))
	os.WriteFile(filepath.Join(dir, "scan-copy.png"), b, 0o644)
	waitFor(t, "副本复用 OCR 结果", 30*time.Second, func() bool {
		r, _ := e.eng.Search(context.Background(), search.Params{Query: "HT-2025-0888 ext:png"})
		return len(r.Results) == 1 && len(r.Results[0].Dups) == 1 && r.Results[0].OCR
	})
	st := e.m.Status()
	if st.OCRQueue != 0 {
		t.Fatalf("OCR 队列应已清空: %+v", st.OCRQueue)
	}
}

func makeZip(t *testing.T, p string, files map[string]string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for n, c := range files {
		w, _ := zw.Create(n)
		w.Write([]byte(c))
	}
	zw.Close()
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestContainers(t *testing.T) {
	dir := t.TempDir()
	prov := &fakeProv{}
	prov.addKind("v1", "本地", dir, true, volume.KindLocal)
	makeZip(t, filepath.Join(dir, "归档", "2024.zip"), map[string]string{
		"合同/采购合同.txt": "采购合同里的违约金条款 hedgehog",
		"其他.txt":      "别的内容",
	})
	eml := "From: a@example.com\r\nTo: b@example.com\r\nSubject: 关于报销的通知\r\nDate: Mon, 07 Jul 2025 10:30:00 +0800\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n请提交差旅费单据 tapir\r\n"
	write(t, filepath.Join(dir, "mail", "通知.eml"), eml)
	e := newEnv(t, prov)
	if _, err := e.m.AddRoot(dir); err != nil {
		t.Fatal(err)
	}
	e.waitIdle()

	r, _ := e.eng.Search(context.Background(), search.Params{Query: "hedgehog"})
	if len(r.Results) != 1 {
		t.Fatalf("%+v", r.Results)
	}
	got := r.Results[0]
	want := filepath.Join(dir, "归档", "2024.zip") + " › " + filepath.FromSlash("合同/采购合同.txt")
	if got.Path != want || got.Name != "采购合同.txt" {
		t.Fatalf("结果路径应为「容器路径 › 内部路径」: %q (want %q)", got.Path, want)
	}
	// path: 同时匹配容器内部路径
	if got := e.names(`path:"2024.zip › 合同" 违约金`); !has(got, "采购合同.txt") {
		t.Fatalf("path: 应能匹配内部路径: %v", got)
	}
	// 邮件：from:/subject: 过滤，日期作为修改时间
	if got := e.names("from:a@example.com tapir"); !has(got, "通知.eml") {
		t.Fatalf("%v", got)
	}
	if got := e.names("subject:报销 type:邮件"); !has(got, "通知.eml") {
		t.Fatalf("%v", got)
	}
	if got := e.names("tapir modified:2025-07"); has(got, "通知.eml") {
		// eml 文件本身的修改时间是文件系统时间，不是发送日期
		t.Logf("提示：顶层 .eml 按文件修改时间过滤")
	}
	pv, _ := e.eng.Preview(r.Results[0].ID, search.Params{Query: "hedgehog"})
	if pv == nil || len(pv.Hits) != 1 {
		t.Fatalf("预览: %+v", pv)
	}

	// 压缩包更新：旧内部条目消失、新条目出现
	time.Sleep(1100 * time.Millisecond)
	makeZip(t, filepath.Join(dir, "归档", "2024.zip"), map[string]string{"新文件.txt": "更新后才有 pangolin"})
	waitFor(t, "压缩包更新后重建内部条目", 20*time.Second, func() bool {
		return has(e.names("pangolin"), "新文件.txt") && !has(e.names("hedgehog"), "采购合同.txt")
	})
	// 删除压缩包：内部条目一并消失
	os.Remove(filepath.Join(dir, "归档", "2024.zip"))
	waitFor(t, "删除压缩包后内部条目消失", 20*time.Second, func() bool { return !has(e.names("pangolin"), "新文件.txt") })
}

func TestRootsBackupRoundTrip(t *testing.T) {
	dir := t.TempDir()
	prov := &fakeProv{}
	prov.addKind("usb9", "备份盘Z", dir, true, volume.KindRemovable)
	os.MkdirAll(filepath.Join(dir, "归档"), 0o755)
	e := newEnv(t, prov)
	if _, err := e.m.AddRoot(filepath.Join(dir, "归档")); err != nil {
		t.Fatal(err)
	}
	ro := true
	if err := e.m.SetRootOpts(1, RootOpts{IncludeExts: []string{"docx"}, FollowLinks: &ro}); err != nil {
		t.Fatal(err)
	}
	// 用管理器写出的真实备份，在全新的数据目录里恢复
	fresh := t.TempDir()
	b, err := os.ReadFile(filepath.Join(e.data, "roots.json"))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(fresh, "roots.json"), b, 0o600)
	db2, err := store.Open(fresh)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if n := RestoreRoots(db2, fresh); n != 1 {
		t.Fatalf("恢复数量 %d", n)
	}
	rs, _ := db2.ListRoots()
	v, _ := db2.GetVolume("usb9")
	if len(rs) != 1 || rs[0].RelPath != "归档" || v == nil || v.Label != "备份盘Z" || !strings.Contains(string(rs[0].Opts), "docx") {
		t.Fatalf("%+v %+v", rs, v)
	}
}

// 重启：数据目录里已有索引根时，启动不应崩溃，索引保留且立即可搜索，并对账启动期间的变化（F-BG-02、F-BG-08）。
func TestRestartKeepsIndex(t *testing.T) {
	dir := t.TempDir()
	data := t.TempDir()
	prov := &fakeProv{}
	prov.addKind("v1", "本地", dir, true, volume.KindLocal)
	write(t, filepath.Join(dir, "a.txt"), "重启前的内容 gazelle")
	write(t, filepath.Join(dir, "gone.txt"), "将被删除 impala")

	start := func() (*Manager, *search.Engine, func()) {
		db, err := store.Open(data)
		if err != nil {
			t.Fatal(err)
		}
		m := New(db, prov, nil, data)
		if err := m.Start(); err != nil {
			t.Fatal(err)
		}
		return m, &search.Engine{DB: db, Res: m, Norm: m.Norm}, func() { m.Stop(); db.Close() }
	}
	names := func(eng *search.Engine, q string) []string {
		r, err := eng.Search(context.Background(), search.Params{Query: q})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, x := range r.Results {
			out = append(out, x.Name)
		}
		return out
	}
	m1, eng1, stop1 := start()
	if _, err := m1.AddRoot(dir); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "首次索引", 20*time.Second, func() bool { return has(names(eng1, "gazelle"), "a.txt") && has(names(eng1, "impala"), "gone.txt") })
	stop1()

	// 程序没运行期间发生的变化
	os.Remove(filepath.Join(dir, "gone.txt"))
	write(t, filepath.Join(dir, "offline-new.txt"), "停机期间新增 okapi")

	_, eng2, stop2 := start() // 不再 AddRoot
	defer stop2()
	if !has(names(eng2, "gazelle"), "a.txt") {
		t.Fatal("重启后已有索引应立即可搜索")
	}
	waitFor(t, "启动对账", 20*time.Second, func() bool {
		return has(names(eng2, "okapi"), "offline-new.txt") && !has(names(eng2, "impala"), "gone.txt")
	})
}

// 设置里的列表字段不能是 null（界面对 null 调用 join 会整页崩溃）。
func TestConfigListsNeverNull(t *testing.T) {
	prov := &fakeProv{}
	prov.addKind("v1", "本地", t.TempDir(), true, volume.KindLocal)
	e := newEnv(t, prov)
	for _, c := range []Config{e.m.Config(), DefaultConfig("")} {
		b, _ := json.Marshal(c)
		if strings.Contains(string(b), "null") {
			t.Fatalf("配置 JSON 里不应有 null: %s", b)
		}
	}
	c := e.m.Config()
	c.ExcludeGlobs, c.ExcludeExts = nil, nil // 模拟界面提交了 null
	if err := e.m.SetConfig(c); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(e.m.Config())
	if strings.Contains(string(b), "null") {
		t.Fatalf("保存后配置不应出现 null: %s", b)
	}
}

// 扫描进行中删除索引根：扫描应立即停止，之后不能残留该索引根的任何文件（问题 6）。
func TestRemoveRootDuringScan(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 60; i++ {
		for j := 0; j < 50; j++ {
			write(t, filepath.Join(dir, fmt.Sprintf("d%02d", i), fmt.Sprintf("f%03d.txt", j)), "内容")
		}
	}
	prov := &fakeProv{}
	prov.addKind("v1", "本地", dir, true, volume.KindLocal)
	e := newEnv(t, prov)
	r, err := e.m.AddRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "扫描开始写入", 10*time.Second, func() bool { c, _ := e.m.DB.AllCounts(); return c.Total > 0 })
	if err := e.m.RemoveRoot(r.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond) // 留时间给仍在进行的扫描和提取
	c, _ := e.m.DB.AllCounts()
	if c.Total != 0 {
		t.Fatalf("删除索引根后不应残留文件，实际 %d 个", c.Total)
	}
	// 换一个目录作为新索引根，只应看到新目录的文件
	other := t.TempDir()
	prov.addKind("v2", "另一盘", other, true, volume.KindLocal)
	write(t, filepath.Join(other, "新的.txt"), "新目录的内容 meerkat")
	if _, err := e.m.AddRoot(other); err != nil {
		t.Fatal(err)
	}
	e.waitIdle()
	c, _ = e.m.DB.AllCounts()
	if c.Total != 1 || !has(e.names("meerkat"), "新的.txt") {
		t.Fatalf("应只有新目录的 1 个文件: %+v", c)
	}
}

// 旧版本残留的孤儿记录在启动时清理。
func TestOrphansCleanedOnStart(t *testing.T) {
	data := t.TempDir()
	db, err := store.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	rid, _ := db.AddRoot("v1", "a")
	err = db.Tx(func(tx *sql.Tx) error {
		return store.UpsertFile(tx, &store.File{RootID: rid, VolumeID: "v1", RelPath: "a/x.txt", Name: "x.txt"}, "x txt")
	})
	if err != nil {
		t.Fatal(err)
	}
	db.W.Exec(`DELETE FROM roots`) // 模拟旧版本：索引根删了，文件还在
	if c, _ := db.AllCounts(); c.Total != 1 {
		t.Fatal("准备数据失败")
	}
	m := New(db, &fakeProv{}, nil, data)
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	// Windows 上不关闭数据库，临时目录删不掉，测试会失败
	t.Cleanup(func() { m.Stop(); db.Close() })
	if c, _ := db.AllCounts(); c.Total != 0 {
		t.Fatalf("启动时应清理孤儿记录: %+v", c)
	}
	// 索引根已删除时写入被拒绝
	err = db.Tx(func(tx *sql.Tx) error {
		return store.UpsertFile(tx, &store.File{RootID: rid, VolumeID: "v1", RelPath: "a/y.txt", Name: "y.txt"}, "y txt")
	})
	if !errors.Is(err, store.ErrRootGone) {
		t.Fatalf("应返回 ErrRootGone: %v", err)
	}
}

// 扫描记录的大小/修改时间与提取时读到的不一致（网络盘目录缓存、扫描时文件还在复制）：
// 以提取时读到的为准完成索引，不能把结果当成过期丢掉、让文件永远停在待处理反复提取。
func TestStaleScanSigStillCompletes(t *testing.T) {
	dir := t.TempDir()
	prov := &fakeProv{}
	prov.addKind("v1", "共享", dir, true, volume.KindNetwork)
	write(t, filepath.Join(dir, "合同.txt"), "网络盘上的文件 wombat")
	e := newEnv(t, prov)
	if _, err := e.m.AddRoot(dir); err != nil {
		t.Fatal(err)
	}
	e.waitIdle()
	e.m.PauseAll()
	if _, err := e.m.DB.W.Exec(`UPDATE files SET state=?, mtime=mtime-3600, size=size+1`, store.StatePending); err != nil {
		t.Fatal(err)
	}
	e.m.ResumeAll()
	waitFor(t, "文件提取完成", 10*time.Second, func() bool {
		c, _ := e.m.DB.AllCounts()
		return c.Total == 1 && c.Done == 1
	})
	if !has(e.names("wombat"), "合同.txt") {
		t.Fatal("应能搜到内容")
	}
}

// 有文件要处理时阻止电脑自动睡眠，处理完恢复。
func TestKeepAwakeWhileBusy(t *testing.T) {
	var mu sync.Mutex
	var calls []bool
	oldSup, oldSet, oldEvery, oldIdle := keepAwakeSupported, keepAwakeSet, keepAwakeEvery, keepAwakeIdleFor
	keepAwakeSupported = func() bool { return true }
	keepAwakeSet = func(on bool) error { mu.Lock(); calls = append(calls, on); mu.Unlock(); return nil }
	keepAwakeEvery, keepAwakeIdleFor = 20*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() {
		keepAwakeSupported, keepAwakeSet, keepAwakeEvery, keepAwakeIdleFor = oldSup, oldSet, oldEvery, oldIdle
	})

	dir := t.TempDir()
	for i := range 30 {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d.txt", i)), []byte("内容"), 0o644)
	}
	prov := &fakeProv{}
	prov.addKind("v1", "本地", dir, true, volume.KindLocal)
	e := newEnv(t, prov)
	if _, err := e.m.AddRoot(dir); err != nil {
		t.Fatal(err)
	}
	e.waitIdle()
	// 处理期间阻止睡眠；处理完、空闲超过 keepAwakeIdleFor 后恢复（中间短暂空闲不来回切换）
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n, first, last := len(calls), false, true
		if n > 0 {
			first, last = calls[0], calls[n-1]
		}
		mu.Unlock()
		if n >= 2 && first && !last && !e.m.keepingAwake.Load() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("应先阻止、处理完后恢复：%v", calls)
}

// 几秒就索引完的任务也要立即阻止睡眠（不等下一次定时检查）：Windows 冒烟测试里整个索引 1 秒就做完了。
func TestKeepAwakeEngagesForShortJob(t *testing.T) {
	var mu sync.Mutex
	var calls []bool
	oldSup, oldSet, oldEvery := keepAwakeSupported, keepAwakeSet, keepAwakeEvery
	keepAwakeSupported = func() bool { return true }
	keepAwakeSet = func(on bool) error { mu.Lock(); calls = append(calls, on); mu.Unlock(); return nil }
	keepAwakeEvery = time.Hour // 定时检查不会发生
	t.Cleanup(func() { keepAwakeSupported, keepAwakeSet, keepAwakeEvery = oldSup, oldSet, oldEvery })

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("内容"), 0o644)
	prov := &fakeProv{}
	prov.addKind("v1", "本地", dir, true, volume.KindLocal)
	// 先暂停：循环第一次检查时不阻止睡眠；之后加入目录、恢复，任务只有一秒，等不到下一次定时检查
	e := newEnvWith(t, prov, "", func(m *Manager) { m.PauseAll() })
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	if len(calls) != 0 {
		t.Fatalf("暂停时不应阻止睡眠：%v", calls)
	}
	mu.Unlock()
	if _, err := e.m.AddRoot(dir); err != nil {
		t.Fatal(err)
	}
	e.m.ResumeAll()
	e.waitIdle()
	mu.Lock()
	defer mu.Unlock()
	if len(calls) == 0 || !calls[0] {
		t.Fatalf("有任务时应立即阻止睡眠：%v", calls)
	}
}

// 同一个文件出现在几个压缩包里、压缩包外也有一份：都能搜到，提取文本只存一份。
func TestArchiveChildrenShareText(t *testing.T) {
	dir := t.TempDir()
	prov := &fakeProv{}
	prov.addKind("v1", "本地", dir, true, volume.KindLocal)
	same := "任免审批表 张三 简历 narwhal " + strings.Repeat("内容", 500)
	makeZip(t, filepath.Join(dir, "甲.zip"), map[string]string{"表/张三.txt": same, "别的.txt": "别的内容 quokka"})
	makeZip(t, filepath.Join(dir, "副本", "甲.zip"), map[string]string{"表/张三.txt": same, "别的.txt": "别的内容 quokka"})
	makeZip(t, filepath.Join(dir, "乙.zip"), map[string]string{"张三.txt": same})
	write(t, filepath.Join(dir, "张三.txt"), same)
	e := newEnv(t, prov)
	if _, err := e.m.AddRoot(dir); err != nil {
		t.Fatal(err)
	}
	e.waitIdle()
	var files, texts int
	e.m.DB.R.QueryRow(`SELECT count(*), count(DISTINCT text_id) FROM files WHERE text_id>0 AND (inner LIKE '%张三.txt' OR name='张三.txt')`).Scan(&files, &texts)
	if files != 4 || texts != 1 {
		t.Fatalf("4 份相同内容应共用 1 份文本：%d 个文件，%d 份文本", files, texts)
	}
	// 搜索结果把内容相同的文件合并为一条，其余列在「相同文件」里
	r, err := e.eng.Search(context.Background(), search.Params{Query: "narwhal", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Results) != 1 || len(r.Results[0].Dups) != 3 {
		t.Fatalf("4 份相同内容应合并为一条结果、3 个相同文件：%+v", r.Results)
	}
	var hashed int
	e.m.DB.R.QueryRow(`SELECT count(*) FROM files WHERE inner<>'' AND hash<>''`).Scan(&hashed)
	if hashed != 5 {
		t.Fatalf("压缩包里提取成功的文件都应有内容指纹：%d", hashed)
	}
}

// 提取过但没有文字的文件（只索引文件名）在重新扫描时不应再处理一遍；扫描时按规则跳过的仍会在放宽后补上。
func TestRescanKeepsExtractedNoText(t *testing.T) {
	dir := t.TempDir()
	prov := &fakeProv{}
	prov.addKind("v1", "本地", dir, true, volume.KindLocal)
	write(t, filepath.Join(dir, "空白.txt"), "   \n\n  ")
	write(t, filepath.Join(dir, "有字.txt"), "zebra 斑马")
	e := newEnv(t, prov)
	id, err := e.m.AddRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	e.waitIdle()
	at := func() (state int, indexed int64) {
		if err := e.m.DB.R.QueryRow(`SELECT state,indexed_at FROM files WHERE name='空白.txt'`).Scan(&state, &indexed); err != nil {
			t.Fatal(err)
		}
		return
	}
	st, first := at()
	if st != store.StateMetaOnly || first == 0 {
		t.Fatalf("空白文件应提取过、只索引文件名：state=%d indexed_at=%d", st, first)
	}
	time.Sleep(1100 * time.Millisecond) // indexed_at 以秒计
	for i := 0; i < 2; i++ {
		e.m.RescanRoot(id.ID)
		e.waitIdle()
	}
	if _, again := at(); again != first {
		t.Fatalf("没变化的空白文件被重新处理了：indexed_at %d → %d", first, again)
	}
}

// 同一个索引根正在扫描时：普通请求忽略，要求补扫的请求记下来，扫完再扫一遍（只一遍）。
func TestRootScanNotConcurrent(t *testing.T) {
	dir := t.TempDir()
	prov := &fakeProv{}
	prov.addKind("v1", "本地", dir, true, volume.KindLocal)
	write(t, filepath.Join(dir, "a.txt"), "hello")
	e := newEnv(t, prov)
	r, err := e.m.AddRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	e.waitIdle()
	m := e.m
	m.scanMu.Lock()
	m.scanRun[r.ID] = nil // 假装正在扫描
	m.scanMu.Unlock()
	m.runRootScan(scanReq{rootID: r.ID})
	m.scanMu.Lock()
	if next := m.scanRun[r.ID]; next != nil {
		t.Fatalf("正在扫描时的普通请求应忽略，却记了补扫：%+v", *next)
	}
	m.scanMu.Unlock()
	m.runRootScan(scanReq{rootID: r.ID, again: true})
	m.runRootScan(scanReq{rootID: r.ID, force: true, again: true})
	m.scanMu.Lock()
	next := m.scanRun[r.ID]
	delete(m.scanRun, r.ID)
	m.scanMu.Unlock()
	if next == nil || !next.force || next.sub != "" {
		t.Fatalf("要求补扫的请求应合并成一次（带 force）：%+v", next)
	}
	// 不在扫描时照常扫描，扫完不留记录
	m.runRootScan(scanReq{rootID: r.ID})
	m.scanMu.Lock()
	_, busy := m.scanRun[r.ID]
	m.scanMu.Unlock()
	if busy {
		t.Fatal("扫描结束后应清除正在扫描的记录")
	}
}
