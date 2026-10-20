package index

import (
	"bytes"
	"database/sql"
	"image"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenonestar/zhaozhao/internal/extract"
	"github.com/chenonestar/zhaozhao/internal/scanpdf"
	"github.com/chenonestar/zhaozhao/internal/store"
	"github.com/chenonestar/zhaozhao/internal/volume"
)

func copyFixture(t *testing.T, name, dst string) {
	t.Helper()
	copyFixtureFrom(t, "legacy", name, dst)
}

func copyFixtureFrom(t *testing.T, sub, name, dst string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", sub, name))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func fileByName(t *testing.T, db *store.DB, name string) (state, prio int, errMsg string) {
	t.Helper()
	if err := db.R.QueryRow(`SELECT state, prio, err FROM files WHERE name=?`, name).Scan(&state, &prio, &errMsg); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return
}

// 老格式文件先在普通通道用内置解析器读取；读不了的排到老格式转换队列，用 LibreOffice 转换。
func TestLegacyNativeThenConvert(t *testing.T) {
	t.Cleanup(extract.CleanupWorker) // 本测试在进程内提取：结束常驻的 LibreOffice
	dir := t.TempDir()
	copyFixture(t, "合同.doc", filepath.Join(dir, "合同.doc"))
	copyFixture(t, "汇报.ppt", filepath.Join(dir, "汇报.dps"))
	os.WriteFile(filepath.Join(dir, "符号链接格式.xls"), []byte("ID;PWXL;N;E\nC;Y1;X1;K\"SYLKWORD\"\nE\n"), 0o644)
	prov := &fakeProv{}
	prov.addKind("v1", "本地", dir, true, volume.KindLocal)
	e := newEnv(t, prov)
	if _, err := e.m.AddRoot(dir); err != nil {
		t.Fatal(err)
	}
	e.waitIdle()
	for _, n := range []string{"合同.doc", "汇报.dps"} {
		if st, prio, msg := fileByName(t, e.m.DB, n); st != store.StateDone || prio != prioFast {
			t.Errorf("%s：state %d prio %d %s", n, st, prio, msg)
		}
	}
	if !has(e.names("海獭"), "合同.doc") || !has(e.names("树袋熊"), "汇报.dps") {
		t.Error("内置解析的内容搜不到")
	}
	st, prio, msg := fileByName(t, e.m.DB, "符号链接格式.xls")
	if prio != prioLegacy {
		t.Errorf("内置解析不了的文件应排到老格式转换队列：state %d prio %d %s", st, prio, msg)
	}
	if extract.SofficePath() != "" {
		if st != store.StateDone || !has(e.names("SYLKWORD"), "符号链接格式.xls") {
			t.Errorf("LibreOffice 转换：state %d %s", st, msg)
		}
	} else if st != store.StateFailed {
		t.Errorf("没有 LibreOffice 时应失败：state %d %s", st, msg)
	}
}

// 从 0.1.x 升级：待处理的老格式文件从老格式转换队列移到普通队列，以前转换失败的重新排队；
// 改为只用 LibreOffice 时再移回去。
func TestLegacyQueueSync(t *testing.T) {
	t.Cleanup(extract.CleanupWorker) // 本测试在进程内提取：结束常驻的 LibreOffice
	dir := t.TempDir()
	copyFixture(t, "合同.doc", filepath.Join(dir, "合同.doc"))
	copyFixture(t, "报销.xls", filepath.Join(dir, "报销.xls"))
	prov := &fakeProv{}
	prov.addKind("v1", "本地", dir, true, volume.KindLocal)
	e := newEnvWith(t, prov, `{"legacyLOOnly":true,"configVersion":2}`, func(m *Manager) { m.PauseAll() })
	if _, err := e.m.AddRoot(dir); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "扫描完成", 20*time.Second, func() bool { c, _ := e.m.DB.AllCounts(); return c.Total == 2 })
	if _, prio, _ := fileByName(t, e.m.DB, "合同.doc"); prio != prioLegacy {
		t.Fatalf("只用 LibreOffice 时应排在老格式转换队列：prio %d", prio)
	}
	// 模拟 0.1.x 的索引库：没有 legacy_mode，有一个转换失败的文件
	e.m.DB.W.Exec(`DELETE FROM meta WHERE k='legacy_mode'`)
	e.m.DB.W.Exec(`UPDATE files SET state=?, err='格式转换超时（2m0s）' WHERE name='报销.xls'`, store.StateFailed)
	c := e.m.Config()
	c.LegacyLOOnly = false
	if err := e.m.SetConfig(c); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"合同.doc", "报销.xls"} {
		if st, prio, _ := fileByName(t, e.m.DB, n); st != store.StatePending || prio != prioFast {
			t.Errorf("%s：state %d prio %d", n, st, prio)
		}
	}
	c.LegacyLOOnly = true
	e.m.SetConfig(c)
	if _, prio, _ := fileByName(t, e.m.DB, "报销.xls"); prio != prioLegacy {
		t.Errorf("改回只用 LibreOffice：prio %d", prio)
	}
	c.LegacyLOOnly = false
	e.m.SetConfig(c)
	e.m.ResumeAll()
	e.waitIdle()
	for _, n := range []string{"合同.doc", "报销.xls"} {
		if st, _, msg := fileByName(t, e.m.DB, n); st != store.StateDone {
			t.Errorf("%s：state %d %s", n, st, msg)
		}
	}
}

func TestLegacyTestReport(t *testing.T) {
	if extract.SofficePath() == "" {
		t.Skip("没有 LibreOffice")
	}
	t.Cleanup(extract.CleanupWorker)
	dir := t.TempDir()
	copyFixture(t, "合同.doc", filepath.Join(dir, "合同.doc"))
	copyFixture(t, "脚注.doc", filepath.Join(dir, "脚注.doc"))
	copyFixture(t, "报销.xls", filepath.Join(dir, "报销.xls"))
	copyFixture(t, "汇报.ppt", filepath.Join(dir, "汇报.ppt"))
	copyFixture(t, "合同.rtf", filepath.Join(dir, "合同.rtf"))
	os.WriteFile(filepath.Join(dir, "符号链接格式.xls"), []byte("ID;PWXL;N;E\nC;Y1;X1;K\"SYLKWORD\"\nE\n"), 0o644)
	prov := &fakeProv{}
	prov.addKind("v1", "本地", dir, true, volume.KindLocal)
	e := newEnv(t, prov)
	if err := e.m.StartLegacyTest(10); err == nil {
		t.Fatal("没有老格式文件时应报错")
	}
	if _, err := e.m.AddRoot(dir); err != nil {
		t.Fatal(err)
	}
	e.waitIdle()
	if err := e.m.StartLegacyTest(10); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "老格式测试完成", 3*time.Minute, func() bool { return !e.m.LegacyTest().Running })
	st := e.m.LegacyTest()
	r := st.Report
	if r == nil || r.Sampled != 6 || st.Done != 6 {
		t.Fatalf("%+v", st)
	}
	text := LegacyTestText(r, false)
	t.Log(text)
	for _, w := range r.Rows {
		if w.Ext == "doc" && (w.BothOK != 2 || w.AvgCoverage < 0.9) {
			t.Errorf("doc：%+v", w)
		}
		if w.Ext == "xls" && w.NeedLO != 1 {
			t.Errorf("xls：%+v", w)
		}
	}
	// 结果保存下来，重启后还能看到
	m2 := &Manager{DataDir: e.m.DataDir}
	if m2.LegacyTest().Report == nil {
		t.Error("报告应保存到数据目录")
	}
}

func TestBigramCoverage(t *testing.T) {
	cases := []struct {
		ref, got string
		want     float64
	}{
		{"甲乙丙\n丁戊", "丁戊\t甲乙丙", 1}, // 顺序、分隔不同不影响
		{"甲乙丙", "甲乙", 0.5},
		{"ABC", "abc", 1},
		{"<number>\n甲乙", "甲乙", 1}, // LibreOffice 的页码占位符不算
		{"", "随便", 1},
	}
	for _, c := range cases {
		if got := bigramCoverage(c.ref, c.got); got != c.want {
			t.Errorf("%q vs %q = %v，应为 %v", c.ref, c.got, got, c.want)
		}
	}
}

// 0.2.0 把「保护工作簿」的 .xls 记成了已加密：升级后重新排队一次，能读出内容的就读出来。
func TestRetryProtectedXls(t *testing.T) {
	t.Cleanup(extract.CleanupWorker)
	dir := t.TempDir()
	copyFixture(t, "保护工作簿.xls", filepath.Join(dir, "保护工作簿.xls"))
	copyFixture(t, "有打开密码.xls", filepath.Join(dir, "有打开密码.xls"))
	prov := &fakeProv{}
	prov.addKind("v1", "本地", dir, true, volume.KindLocal)
	e := newEnv(t, prov)
	if _, err := e.m.AddRoot(dir); err != nil {
		t.Fatal(err)
	}
	e.waitIdle()
	if st, _, msg := fileByName(t, e.m.DB, "保护工作簿.xls"); st != store.StateDone || !has(e.names("狐獴"), "保护工作簿.xls") {
		t.Fatalf("保护工作簿.xls：state %d %s", st, msg)
	}
	if st, _, _ := fileByName(t, e.m.DB, "有打开密码.xls"); st != store.StateEncrypted {
		t.Fatalf("有打开密码.xls 应为已加密：state %d", st)
	}
	// 模拟 0.2.0 的结果：两个都记为已加密，且还没做过重新排队
	e.m.DB.W.Exec(`UPDATE files SET state=?, err='文件已加密' WHERE ext='xls'`, store.StateEncrypted)
	e.m.DB.W.Exec(`DELETE FROM meta WHERE k='xls_default_password'`)
	e.m.retryProtectedXls()
	e.m.kick()
	e.waitIdle()
	if st, _, _ := fileByName(t, e.m.DB, "保护工作簿.xls"); st != store.StateDone {
		t.Errorf("重新排队后应读出内容：state %d", st)
	}
	if st, _, _ := fileByName(t, e.m.DB, "有打开密码.xls"); st != store.StateEncrypted {
		t.Errorf("有打开密码的仍应为已加密：state %d", st)
	}
	// 只做一次
	e.m.DB.W.Exec(`UPDATE files SET state=? WHERE name='保护工作簿.xls'`, store.StateEncrypted)
	e.m.retryProtectedXls()
	if st, _, _ := fileByName(t, e.m.DB, "保护工作簿.xls"); st != store.StateEncrypted {
		t.Errorf("重新排队只应做一次")
	}
}

// 0.2.1 把用默认密码加密的 xlsx 记成了已加密：升级后重新排队一次，原因写明是否需要打开密码。
func TestRetryProtectedOOXML(t *testing.T) {
	dir := t.TempDir()
	copyFixtureFrom(t, "encrypted", "默认密码.xlsx", filepath.Join(dir, "填报.xlsx"))
	copyFixtureFrom(t, "encrypted", "打开密码.xlsx", filepath.Join(dir, "有密码.xlsx"))
	prov := &fakeProv{}
	prov.addKind("v1", "本地", dir, true, volume.KindLocal)
	e := newEnv(t, prov)
	if _, err := e.m.AddRoot(dir); err != nil {
		t.Fatal(err)
	}
	e.waitIdle()
	if st, _, msg := fileByName(t, e.m.DB, "填报.xlsx"); st != store.StateDone || !has(e.names("雪豹"), "填报.xlsx") {
		t.Fatalf("填报.xlsx：state %d %s", st, msg)
	}
	if st, _, msg := fileByName(t, e.m.DB, "有密码.xlsx"); st != store.StateEncrypted || !strings.Contains(msg, "需要打开密码") {
		t.Fatalf("有密码.xlsx 应为已加密并注明需要打开密码：state %d %s", st, msg)
	}
	// 模拟 0.2.1 的结果：两个都记为已加密，且还没做过重新排队
	e.m.DB.W.Exec(`UPDATE files SET state=?, err='文件已加密' WHERE ext='xlsx'`, store.StateEncrypted)
	e.m.DB.W.Exec(`DELETE FROM meta WHERE k='ooxml_default_password'`)
	e.m.retryProtectedOOXML()
	e.m.kick()
	e.waitIdle()
	if st, _, _ := fileByName(t, e.m.DB, "填报.xlsx"); st != store.StateDone {
		t.Errorf("重新排队后应读出内容：state %d", st)
	}
	if st, _, _ := fileByName(t, e.m.DB, "有密码.xlsx"); st != store.StateEncrypted {
		t.Errorf("有打开密码的仍应为已加密：state %d", st)
	}
	e.m.DB.W.Exec(`UPDATE files SET state=? WHERE name='填报.xlsx'`, store.StateEncrypted)
	e.m.retryProtectedOOXML()
	if st, _, _ := fileByName(t, e.m.DB, "填报.xlsx"); st != store.StateEncrypted {
		t.Errorf("重新排队只应做一次")
	}
}

// 以前提取的 PDF 里，扫描页上叠了页码的只取到了页码：后台复查一次，重新排队做 OCR。
func TestRecheckPDFs(t *testing.T) {
	if _, err := extract.PDFiumPath(); err != nil {
		t.Skip("没有 PDFium")
	}
	dir := t.TempDir()
	img := image.NewGray(image.Rect(0, 0, 1600, 1200))
	for i := range img.Pix {
		img.Pix[i] = uint8((i*7919 + i/1600*104729) % 251) // 噪点：让 JPEG 足够大
	}
	var b bytes.Buffer
	scanpdf.Write(&b, []scanpdf.Page{{Image: img, Text: "Page 1 of 1"}})
	write(t, filepath.Join(dir, "扫描合同.pdf"), b.String())
	write(t, filepath.Join(dir, "普通.txt"), "普通文本")
	prov := &fakeProv{}
	prov.addKind("v1", "本地", dir, true, volume.KindLocal)
	e := newEnvWith(t, prov, "", func(m *Manager) { useFakeOCR(t, m, "1ms") })
	e.eng = newEngine(e.m)
	if _, err := e.m.AddRoot(dir); err != nil {
		t.Fatal(err)
	}
	e.waitIdle()
	if !has(e.names("fakeocrword"), "扫描合同.pdf") {
		t.Fatal("新提取的扫描页（叠了页码）应做 OCR")
	}
	// 模拟 0.2.0 的结果：只取到了页码，没有 OCR；还没复查过
	e.m.DB.Tx(func(tx *sql.Tx) error {
		var id int64
		tx.QueryRow(`SELECT id FROM files WHERE name='扫描合同.pdf'`).Scan(&id)
		return e.m.DB.PutResult(tx, id, store.StateDone, "", "", false, &store.Text{Text: "Page 1 of 1"}, "page 1 of 1", "扫描合同")
	})
	e.m.DB.W.Exec(`DELETE FROM meta WHERE k='pdf_recheck'`)
	if has(e.names("fakeocrword"), "扫描合同.pdf") {
		t.Fatal("模拟失败")
	}
	e.m.recheckPDFs()
	e.waitIdle()
	if !has(e.names("fakeocrword"), "扫描合同.pdf") {
		t.Error("复查后应重新提取并 OCR")
	}
	if v, _ := e.m.DB.GetMeta("pdf_recheck"); v != "done" {
		t.Errorf("复查应只做一次：%q", v)
	}
}

// 文件移动、改名后内容没变：复用原来的提取结果（OCR 结果），不重新提取。
func TestMoveReusesExtraction(t *testing.T) {
	dir := t.TempDir()
	writePNG(t, filepath.Join(dir, "扫描.png"), 7)
	os.MkdirAll(filepath.Join(dir, "归档"), 0o755)
	prov := &fakeProv{}
	prov.addKind("v1", "本地", dir, true, volume.KindLocal)
	e := newEnvWith(t, prov, "", func(m *Manager) { useFakeOCR(t, m, "1ms") })
	if _, err := e.m.AddRoot(dir); err != nil {
		t.Fatal(err)
	}
	e.waitIdle()
	textID := func(name string) (id int64, state int, ocr int) {
		e.m.DB.R.QueryRow(`SELECT text_id, state, ocr FROM files WHERE name=?`, name).Scan(&id, &state, &ocr)
		return
	}
	old, st, _ := textID("扫描.png")
	if old == 0 || st != store.StateDone {
		t.Fatalf("先应 OCR 完成：text_id %d state %d", old, st)
	}
	if err := os.Rename(filepath.Join(dir, "扫描.png"), filepath.Join(dir, "归档", "改名后的扫描.png")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "移动后的文件处理完成", 20*time.Second, func() bool {
		_, st, _ := textID("改名后的扫描.png")
		var n int
		e.m.DB.R.QueryRow(`SELECT count(*) FROM files WHERE name='扫描.png'`).Scan(&n)
		return st == store.StateDone && n == 0
	})
	got, _, ocr := textID("改名后的扫描.png")
	if got != old || ocr != 1 {
		t.Errorf("应复用原来的 OCR 结果：text_id %d → %d，ocr %d", old, got, ocr)
	}
	// 先移出索引目录（记录被删除，提取结果进回收区），过一会儿再移回来：从回收区复用
	outside := filepath.Join(t.TempDir(), "暂存.png")
	if err := os.Rename(filepath.Join(dir, "归档", "改名后的扫描.png"), outside); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "移出后记录删除", 20*time.Second, func() bool {
		var n, r int
		e.m.DB.R.QueryRow(`SELECT count(*) FROM files WHERE ext='png'`).Scan(&n)
		e.m.DB.R.QueryRow(`SELECT count(*) FROM recycled WHERE text_id=?`, old).Scan(&r)
		return n == 0 && r == 1
	})
	if err := os.Rename(outside, filepath.Join(dir, "又移回来.png")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "移回后处理完成", 20*time.Second, func() bool { _, st, _ := textID("又移回来.png"); return st == store.StateDone })
	if got, _, ocr := textID("又移回来.png"); got != old || ocr != 1 {
		t.Errorf("应从回收区复用：text_id %d → %d，ocr %d", old, got, ocr)
	}
	var r int
	e.m.DB.R.QueryRow(`SELECT count(*) FROM recycled`).Scan(&r)
	if r != 0 {
		t.Errorf("复用后回收区应清空：%d", r)
	}
}

// 升级后把以前（新版本能处理了的）失败重新排队一次：压缩包里的条目重新排队它所在的压缩包。
func TestUpgradeRetries(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.txt"), "hello")
	prov := &fakeProv{}
	prov.addKind("v1", "本地", dir, true, volume.KindLocal)
	e := newEnv(t, prov)
	if _, err := e.m.AddRoot(dir); err != nil {
		t.Fatal(err)
	}
	e.waitIdle()
	// 造一个压缩包和它里面以前失败的条目
	for _, q := range []string{
		`INSERT INTO files(root_id,volume_id,rel_path,inner,name,ext,kind,size,mtime,state,err,prio)
		SELECT root_id,volume_id,'包.zip','','包.zip','zip','archive',1,1,1,'',1 FROM files WHERE name='a.txt'`,
		`INSERT INTO files(root_id,volume_id,rel_path,inner,name,ext,kind,size,mtime,state,err,prio)
		SELECT root_id,volume_id,'包.zip','里/x.doc','x.doc','doc','doc',1,1,2,'无法读取（超过解压总量上限、压缩比异常或数据损坏）',1 FROM files WHERE name='a.txt'`,
	} {
		if r, err := e.m.DB.W.Exec(q); err != nil {
			t.Fatal(err)
		} else if n, _ := r.RowsAffected(); n != 1 {
			t.Fatalf("插入了 %d 行", n)
		}
	}
	e.m.PauseAll()                                                // 只看排队，不真去提取（包.zip 并不存在）
	e.m.DB.W.Exec(`DELETE FROM meta WHERE k='retry_zip_methods'`) // 模拟从旧版本升级
	e.m.runUpgradeRetries()
	var st int
	e.m.DB.R.QueryRow(`SELECT state FROM files WHERE rel_path='包.zip' AND inner=''`).Scan(&st)
	if st != store.StatePending {
		t.Fatalf("所在的压缩包应重新排队：state %d", st)
	}
	e.m.DB.W.Exec(`UPDATE files SET state=1 WHERE rel_path='包.zip' AND inner=''`)
	e.m.runUpgradeRetries()
	e.m.DB.R.QueryRow(`SELECT state FROM files WHERE rel_path='包.zip' AND inner=''`).Scan(&st)
	if st != store.StateDone {
		t.Fatal("只应做一次")
	}
}

// 升级后，以前解不开的 TIFF、BMP 和改过扩展名的压缩包重新排队一次；别的失败不动。
func TestRetryWICAndSniffOnUpgrade(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.txt"), "x")
	prov := &fakeProv{}
	prov.add("v1", "数据", dir, true)
	e := newEnv(t, prov)
	if _, err := e.m.AddRoot(dir); err != nil {
		t.Fatal(err)
	}
	e.waitIdle()
	errs := map[string]string{
		"a.tif": "TIFF 第 1 页无法解码: tiff: unsupported feature: compression value 7",
		"b.tif": "TIFF 第 1 页无法解码: unexpected EOF",
		"c.bmp": "图片无法解码: bmp: unsupported BMP image",
		"d.rar": "无法读取: rardecode: RAR signature not found",
		"e.doc": "文件已损坏",
	}
	for name, msg := range errs {
		if _, err := e.m.DB.W.Exec(`INSERT INTO files(root_id,volume_id,rel_path,inner,name,ext,kind,size,mtime,state,err,prio)
			SELECT root_id,volume_id,?,'',?,'x','',1,1,2,?,1 FROM files WHERE name='a.txt'`, name, name, msg); err != nil {
			t.Fatal(err)
		}
	}
	e.m.PauseAll()
	e.m.DB.W.Exec(`DELETE FROM meta WHERE k='retry_wic_sniff'`)
	e.m.runUpgradeRetries()
	for name := range errs {
		var st int
		e.m.DB.R.QueryRow(`SELECT state FROM files WHERE rel_path=?`, name).Scan(&st)
		if want := map[bool]int{true: store.StateFailed, false: store.StatePending}[name == "e.doc"]; st != want {
			t.Fatalf("%s：state %d，应为 %d", name, st, want)
		}
	}
}
