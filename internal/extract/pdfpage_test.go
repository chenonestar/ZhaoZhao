package extract

import (
	"bytes"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenonestar/zhaozhao/internal/doc"
	"github.com/chenonestar/zhaozhao/internal/scanpdf"
)

func TestGarbledText(t *testing.T) {
	for _, c := range []struct {
		s    string
		want bool
	}{
		{"违约金为合同总额的百分之五", false},
		{"Invoice number 20250707 has been paid in full.", false},
		{"Le système a été mis à jour à 10 h, après vérification.", false},
		{"第1页", false},
		{" ", true},                // 私用区
		{"�����合同", true},                           // 替换符
		{"ºÏÍ¬±àºÅ£ºHT-2025 Î¥Ô¼½ðÎªºÏÍ¬×Ü¶îµÄ°Ù·ÖÖ®Îå", true},                 // GBK 被当成西文
		{"ÄÖ", false},                                                       // 太短，不判断
	} {
		if got := garbledText(c.s); got != c.want {
			t.Errorf("%q：%v，应为 %v", c.s, got, c.want)
		}
	}
	if classifyPage("第 3 页", 0.95) != pageOCRKeepText || classifyPage("第 3 页", 0.1) != pageUseText ||
		classifyPage(strings.Repeat("正文内容", 20), 0.95) != pageUseText || classifyPage("", 0) != pageOCR {
		t.Error("classifyPage")
	}
}

func darkPage() image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 600, 400))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	for y := 40; y < 360; y += 40 {
		for x := 40; x < 560; x++ {
			for k := 0; k < 10; k++ {
				img.Set(x, y+k, color.Black)
			}
		}
	}
	return img
}

// 扫描页上叠了少量文字（页码）：仍然 OCR 正文，并保留这些文字；乱码文字层的页改用 OCR。
func TestPDFScanPageWithOverlayText(t *testing.T) {
	needPDFium(t)
	t.Setenv("ZZ_FAKE_OCR", "1")
	defer ShutdownOCR()
	var b bytes.Buffer
	scanpdf.Write(&b, []scanpdf.Page{
		{Image: darkPage(), Text: "Page 1 of 3"},
		{Image: darkPage(), Text: strings.Repeat("Electronic text layer that is long enough. ", 3)},
		{Image: darkPage(), PUAText: "HELLO WORLD GARBLED TEXT"},
	})
	p := filepath.Join(t.TempDir(), "叠字扫描件.pdf")
	os.WriteFile(p, b.Bytes(), 0o644)

	// 先看 PDFium 取出的第 3 页文字确实是私用区乱码
	pdfiumMu.Lock()
	d, err := openPDFium(p)
	if err != nil {
		pdfiumMu.Unlock()
		t.Fatal(err)
	}
	t3, cov, _ := d.page(2, true)
	_, cov1, _ := d.page(0, true)
	d.close()
	pdfiumMu.Unlock()
	t.Logf("第 3 页文字 %q，第 1 页图片覆盖 %.2f", t3, cov1)
	if cov1 < 0.9 || cov < 0.9 {
		t.Errorf("图片覆盖率：%.2f %.2f", cov1, cov)
	}

	// 没有 OCR：第 1、3 页需要 OCR，先保存第 1、2 页的文字
	r := Extract(p, "pdf", DefaultOptions())
	if r.Status != doc.StatusOK || !r.NeedOCR || !strings.Contains(r.Text, "Page 1 of 3") || !strings.Contains(r.Text, "Electronic text layer") {
		t.Fatalf("无 OCR：%+v", r)
	}
	opt := DefaultOptions()
	opt.OCR = OCRConfig{Command: os.Args[0], Args: []string{"-test.run=^$"}, MinSide: 50, PageSec: 30}
	r = Extract(p, "pdf", opt)
	t.Logf("%q", r.Text)
	for _, want := range []string{"Page 1 of 3", "fakepage-p1", "Electronic text layer", "fakepage-p3"} {
		if !strings.Contains(r.Text, want) {
			t.Errorf("缺少 %q：%q", want, r.Text)
		}
	}
	if strings.Contains(r.Text, "fakepage-p2") {
		t.Error("文字足够多的页不应 OCR")
	}
	if strings.ContainsRune(r.Text, 0xE048) {
		t.Error("乱码文字层不应进入索引")
	}
}
