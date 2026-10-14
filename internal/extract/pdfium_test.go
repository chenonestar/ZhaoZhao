package extract

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenonestar/zhaozhao/internal/scanpdf"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

// textPDF 生成有文字层的测试 PDF。
func textPDF(lines ...string) []byte { return simplePDF(lines...) }

func needPDFium(t *testing.T) {
	t.Helper()
	if _, err := PDFiumPath(); err != nil {
		t.Skipf("没有 PDFium（%v）：设置 ZHAOZHAO_PDFIUM 指向 libpdfium.so/pdfium.dll 后测试", err)
	}
}

func TestPDFiumText(t *testing.T) {
	needPDFium(t)
	p := filepath.Join(t.TempDir(), "中文 路径.pdf")
	os.WriteFile(p, textPDF("Hello PDFium page one", "Second page quokka 42"), 0o644)
	pdfiumMu.Lock()
	d, err := openPDFium(p)
	if err != nil {
		pdfiumMu.Unlock()
		t.Fatal(err)
	}
	n := d.pages()
	t1, _ := d.text(0)
	t2, _ := d.text(1)
	d.close()
	pdfiumMu.Unlock()
	if n != 2 || !strings.Contains(t1, "Hello PDFium page one") || !strings.Contains(t2, "quokka 42") {
		t.Fatalf("%d 页：%q %q", n, t1, t2)
	}
}

// 扫描页渲染：任何图片格式都能渲染成整页图像（包括 JBIG2，这里是无法解码的假数据，渲染成空白页而不是报错）。
func TestPDFiumRender(t *testing.T) {
	needPDFium(t)
	img := image.NewRGBA(image.Rect(0, 0, 600, 400))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	for x := 100; x < 500; x++ { // 一条黑线
		for y := 195; y < 205; y++ {
			img.Set(x, y, color.Black)
		}
	}
	var b bytes.Buffer
	scanpdf.Write(&b, []scanpdf.Page{{Image: img}, {JBIG2: true}})
	dir := t.TempDir()
	p := filepath.Join(dir, "scan.pdf")
	os.WriteFile(p, b.Bytes(), 0o644)
	pdfiumMu.Lock()
	defer pdfiumMu.Unlock()
	d, err := openPDFium(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	g, err := d.render(0, 200, 3500)
	if err != nil {
		t.Fatal(err)
	}
	// 600x400 pt 的页（scanpdf 按 72dpi）→ 200dpi 约 1667x1111
	sz := g.Rect.Size()
	if sz.X < 1600 || sz.X > 1700 || sz.Y < 1080 || sz.Y > 1140 {
		t.Errorf("渲染尺寸 %v", sz)
	}
	dark := 0
	for _, v := range g.Pix {
		if v < 100 {
			dark++
		}
	}
	if dark < 1000 {
		t.Errorf("渲染结果里应有黑线，暗点 %d 个", dark)
	}
	if g, err := d.render(1, 200, 3500); err != nil || !blankImage(g) {
		t.Errorf("无法解码的 JBIG2 页应渲染成空白页：%v", err)
	}
}

func TestPDFiumEncrypted(t *testing.T) {
	needPDFium(t)
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain.pdf")
	os.WriteFile(plain, textPDF("secret text"), 0o644)
	enc := filepath.Join(dir, "enc.pdf")
	if err := encryptPDF(plain, enc, "pw123"); err != nil {
		t.Fatal(err)
	}
	pdfiumMu.Lock()
	_, err := openPDFium(enc)
	pdfiumMu.Unlock()
	if err != errPDFPassword {
		t.Fatalf("加密 PDF 应识别为已加密：%v", err)
	}
}

func encryptPDF(in, out, pw string) error {
	conf := model.NewAESConfiguration(pw, pw, 256)
	return api.EncryptFile(context.Background(), in, out, conf)
}

func TestCheckPDFium(t *testing.T) {
	st := CheckPDFium()
	t.Logf("%+v", st)
	if _, err := PDFiumPath(); err == nil && !st.OK {
		t.Fatal(st.Message)
	}
}
