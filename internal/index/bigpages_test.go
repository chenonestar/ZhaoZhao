package index

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chenonestar/zhaozhao/internal/doc"
	"github.com/chenonestar/zhaozhao/internal/volume"
)

// 检查大幅面页：0.2.12 及以前 OCR 识别的 PDF 逐个查，有大幅面页的重新排到 OCR 队列，没有的记下查过了；
// 0.2.13 起识别的、Windows 自带 OCR 识别的、不是 PDF 的不查。
func TestBigPageCheck(t *testing.T) {
	old := bigPageRunner
	t.Cleanup(func() { bigPageRunner = old })
	bigPageRunner = func(m *Manager, ctx context.Context, path string) doc.Result {
		if strings.HasSuffix(path, "图纸.pdf") {
			return doc.Result{Status: doc.StatusOK, BigPages: 3}
		}
		return doc.Result{Status: doc.StatusOK}
	}
	dir := t.TempDir()
	prov := &fakeProv{}
	prov.addKind("v1", "本地", dir, true, volume.KindLocal)
	e := newEnvWith(t, prov, `{"workers":1,"ocrWorkers":1,"legacyWorkers":1,"configVersion":2}`,
		func(m *Manager) { useFakeOCR(t, m, "10ms") })
	e.m.PauseAll()
	r, err := e.m.AddRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	e.waitIdle()
	add := func(name, ext string, big int, model string) {
		if _, err := e.m.DB.W.Exec(`INSERT INTO files(root_id,volume_id,rel_path,inner,name,ext,kind,size,mtime,state,err,hash,text_len,ocr,meta,prio,indexed_at,ocr_model,ocr_big)
VALUES(?,?,?,'',?,?,'',100,1,1,'','',0,1,'',?,1,?,?)`, r.ID, r.VolumeID, name, name, ext, prioOCR, model, big); err != nil {
			t.Fatal(err)
		}
	}
	add("图纸.pdf", "pdf", 0, "v6")
	add("合同.pdf", "pdf", 0, "v6")
	add("新识别.pdf", "pdf", 1, "v6")
	add("快速.pdf", "pdf", 0, "win")
	add("照片.jpg", "jpg", 0, "v6")
	if st := e.m.BigPages(); st.Candidates != 2 {
		t.Fatalf("要查的应是 2 个旧 PDF：%+v", st)
	}
	if err := e.m.StartBigPages(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "检查完成", 10*time.Second, func() bool { return !e.m.BigPages().Running })
	st := e.m.BigPages()
	if st.Done != 2 || st.Found != 1 || st.Pages != 3 || st.Candidates != 0 {
		t.Fatalf("%+v", st)
	}
	state := func(name string) (s, big int) {
		e.m.DB.R.QueryRow(`SELECT state, ocr_big FROM files WHERE name=?`, name).Scan(&s, &big)
		return
	}
	if s, _ := state("图纸.pdf"); s != 0 {
		t.Fatal("有大幅面页的应重新排队")
	}
	if s, big := state("合同.pdf"); s != 1 || big != 1 {
		t.Fatal("没有大幅面页的记下查过了")
	}
	if err := e.m.StartBigPages(); err == nil {
		t.Fatal("都查过了时应提示没有要查的")
	}
}

// 小字多的扫描件：每页找文字都用大图（MaxSide），默认不开。
func TestOCRSmallTextSetting(t *testing.T) {
	prov := &fakeProv{}
	prov.addKind("v1", "本地", t.TempDir(), true, volume.KindLocal)
	e := newEnvWith(t, prov, `{"workers":1,"ocrWorkers":1,"legacyWorkers":1,"configVersion":2}`,
		func(m *Manager) { useFakeOCR(t, m, "10ms") })
	cfg := e.m.Config()
	if o := e.m.extractOptions(cfg, true); o.OCR.MaxSide != 0 || cfg.OCRSmallText {
		t.Fatalf("默认不开：%+v", o.OCR)
	}
	cfg.OCRSmallText = true
	if err := e.m.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if o := e.m.extractOptions(e.m.Config(), true); o.OCR.MaxSide != 2048 {
		t.Fatalf("打开后每页都用大图：%+v", o.OCR)
	}
}
