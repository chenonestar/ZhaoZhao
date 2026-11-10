package extract

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenonestar/zhaozhao/internal/doc"
	"github.com/chenonestar/zhaozhao/internal/scanpdf"
)

func sharePDF(t *testing.T, n int) string {
	var b bytes.Buffer
	var pages []scanpdf.Page
	for i := 1; i <= n; i++ {
		pages = append(pages, scanpdf.Page{Image: grayPage(i)})
	}
	if err := scanpdf.Write(&b, pages); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "大文件.pdf")
	os.WriteFile(p, b.Bytes(), 0o644)
	return p
}

// 多个进程认同一个 PDF：帮手从最后一页往前认主力没占的页，认完一页写一页；主力收齐帮手的页拼成整份结果，
// 帮手占了却没认完（已退出）的页自己补上。
func TestSharedPDFPages(t *testing.T) {
	if _, err := PDFiumPath(); err != nil {
		t.Skip("没有 PDFium")
	}
	t.Setenv("ZZ_FAKE_OCR", "1")
	defer ShutdownOCR()
	p := sharePDF(t, 6)
	opt := DefaultOptions()
	opt.OCR = OCRConfig{Command: os.Args[0], Args: []string{"-test.run=^$"}, MinSide: 50, PageSec: 30}

	// 帮手：主力已经占了第 1、2 页；帮手从第 6 页往前认，要它停下时就停
	share := pageShare{t.TempDir()}
	share.writePages([]int{1, 2, 3, 4, 5, 6})
	share.claim(1, true)
	share.claim(2, true)
	h := opt
	h.OCRShare, h.OCRHelper = share.dir, true
	var prog []string
	h.SetProgress(func(s string) { prog = append(prog, s) })
	r := Extract(p, "pdf", h)
	if r.Status == doc.StatusFailed || r.OCRPages != 4 || r.Text != "" { // 帮手的结果在共享目录里，自己没有文字
		t.Fatalf("帮手应认第 3～6 页：%+v", r)
	}
	if len(prog) != 4 || prog[0] != "帮忙识别第 6 页" {
		t.Fatalf("帮手从最后一页往前认：%v", prog)
	}
	for pg := 3; pg <= 6; pg++ {
		if x, ok := share.get(pg); !ok || !strings.Contains(x.Text, "fakepage-p") {
			t.Fatalf("第 %d 页：%+v %v", pg, x, ok)
		}
	}
	os.WriteFile(share.path("stop"), nil, 0o600)
	share2 := pageShare{t.TempDir()}
	share2.writePages([]int{1, 2, 3})
	os.WriteFile(share2.path("stop"), nil, 0o600)
	h.OCRShare = share2.dir
	if r := Extract(p, "pdf", h); r.Status == doc.StatusFailed || r.OCRPages != 0 {
		t.Fatalf("要帮手停下时不再认：%+v", r)
	}

	// 主力：第 5、6 页帮手认完了；第 4 页帮手占了没认完、已经没有帮手在运行（idle）：自己补上
	main := pageShare{t.TempDir()}
	main.claim(4, false)
	main.claim(5, false)
	main.claim(6, false)
	main.put(5, sharedPage{Text: "帮手认的第五页"})
	main.put(6, sharedPage{Skip: "识别超时"})
	os.WriteFile(main.path("idle"), nil, 0o600)
	m := opt
	m.OCRShare = main.dir
	prog = nil
	m.SetProgress(func(s string) { prog = append(prog, s) })
	r = Extract(p, "pdf", m)
	if r.Status != doc.StatusOK {
		t.Fatalf("%+v", r)
	}
	for _, want := range []string{"fakepage-p1", "fakepage-p3", "fakepage-p4", "帮手认的第五页"} {
		if !strings.Contains(r.Text, want) {
			t.Fatalf("缺少 %s：%q", want, r.Text)
		}
	}
	if strings.Count(r.Text, "fakepage-p4") != 1 || !strings.Contains(r.Err, "1 页识别超时") || len(r.Locs) != 5 {
		t.Fatalf("第 4 页认一次、第 6 页注明超时：%+v", r)
	}
	if pages := main.readPages(); len(pages) != 6 {
		t.Fatalf("主力应写下要认的页：%v", pages)
	}
	if last := prog[len(prog)-1]; last != "第 6/6 页" && last != "第 5/6 页" {
		t.Fatalf("进度按全部完成的页算：%v", prog)
	}
}
