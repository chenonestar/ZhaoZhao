package index

import (
	"context"
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
