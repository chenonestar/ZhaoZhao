package index

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenonestar/zhaozhao/internal/extract"
	"github.com/chenonestar/zhaozhao/internal/store"
	"github.com/chenonestar/zhaozhao/internal/volume"
)

func copyFixture(t *testing.T, name, dst string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "legacy", name))
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
