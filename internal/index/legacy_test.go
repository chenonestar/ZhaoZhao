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
