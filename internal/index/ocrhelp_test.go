package index

import (
	"bytes"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenonestar/zhaozhao/internal/extract"
	"github.com/chenonestar/zhaozhao/internal/scanpdf"
)

// OCR 队列见底、只剩一个大 PDF 在认时，空着的进程当帮手从后往前一起认：每页只认一次，结果拼成整份文件。
func TestOCRHelpersSharePDF(t *testing.T) {
	if _, err := extract.PDFiumPath(); err != nil {
		t.Skip("没有 PDFium")
	}
	old := ocrTuneEvery
	ocrTuneEvery = 100 * time.Millisecond
	t.Cleanup(func() { ocrTuneEvery = old })
	dir := t.TempDir()
	const n = 30
	var pages []scanpdf.Page
	for i := 0; i < n; i++ {
		img := image.NewGray(image.Rect(0, 0, 300, 200))
		for k := range img.Pix {
			img.Pix[k] = 255
		}
		for x := 20; x < 280; x++ {
			for y := 40 + i%5; y < 50+i%5; y++ {
				img.Set(x, y, color.Gray{0})
			}
		}
		pages = append(pages, scanpdf.Page{Image: img})
	}
	var b bytes.Buffer
	if err := scanpdf.Write(&b, pages); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "大扫描件.pdf"), b.Bytes(), 0o644)
	logf := filepath.Join(t.TempDir(), "ocr.log")
	prov := &fakeProv{}
	prov.add("v1", "数据", dir, true)
	e := newEnvWith(t, prov, "", func(m *Manager) {
		useFakeOCR(t, m, "80ms")
		m.ocrCfg.Args = append(m.ocrCfg.Args, "-zzlog="+logf)
		m.ocr.cores = 4
	})
	if _, err := e.m.AddRoot(dir); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "加帮手", 30*time.Second, func() bool { return e.m.ocrHelpers.Load() > 0 })
	waitFor(t, "识别完", 60*time.Second, func() bool {
		s := e.m.Status()
		return !s.Scanning && s.Totals.Pending == 0 && len(s.Current) == 0
	})
	data, _ := os.ReadFile(logf)
	if got := strings.Count(string(data), "\n"); got != n {
		t.Fatalf("每页只认一次：认了 %d 次（%d 页）", got, n)
	}
	var id int64
	if err := e.m.DB.R.QueryRow(`SELECT id FROM files WHERE name='大扫描件.pdf'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	txt, err := e.m.DB.GetText(id)
	if err != nil || txt == nil || strings.Count(txt.Text, "fakeocrword") != n || len(txt.Locs) != n {
		t.Fatalf("应拼成整份文件：%v %+v", err, txt)
	}
	waitFor(t, "帮手退出", 10*time.Second, func() bool { return e.m.ocrHelpers.Load() == 0 })
	if m, _ := filepath.Glob(filepath.Join(os.TempDir(), "zhaozhao-ocrshare-*")); len(m) > 0 {
		t.Logf("共享目录（可能是别的测试的）：%v", m)
	}
}
