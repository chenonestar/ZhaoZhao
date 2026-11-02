package extract

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenonestar/zhaozhao/internal/doc"
	"github.com/chenonestar/zhaozhao/internal/scanpdf"
)

// 测试二进制自己充当 OCR 组件：返回「fakepage-图片文件名」，据此核对页码对应关系。
func TestMain(m *testing.M) {
	if os.Getenv("ZZ_FAKE_OCR") != "" {
		fmt.Println("OCR init completed.")
		big := false // 大图找字（--maxSideLen=2048）时多认出很多字，测大图再认
		for _, a := range os.Args {
			big = big || a == "--maxSideLen=2048"
		}
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			var req struct {
				ImagePath string `json:"image_path"`
			}
			json.Unmarshal(sc.Bytes(), &req)
			name := strings.TrimSuffix(filepath.Base(req.ImagePath), filepath.Ext(req.ImagePath))
			if big {
				name = "big-" + strings.Repeat("小字", 300)
			}
			fmt.Printf(`{"code":100,"data":[{"text":"fakepage-%s"}]}`+"\n", name)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func grayPage(seed int) image.Image {
	img := image.NewGray(image.Rect(0, 0, 400, 300))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	// 几行「文字」（深色横条），渲染后不会被当作空白页
	for y := 40 + seed; y < 260; y += 30 {
		for x := 30; x < 370; x++ {
			for k := 0; k < 8; k++ {
				img.Set(x, y+k, color.Gray{0})
			}
		}
	}
	return img
}

// 扫描版 PDF 逐页识别：页码对应正确、逐页报告进度。没有 PDFium 时 JBIG2 页跳过并注明原因；
// 有 PDFium 时整页渲染后识别（这里的 JBIG2 数据是无法解码的假数据，渲染出空白页，不送去识别）。
func TestScannedPDFPageByPage(t *testing.T) {
	t.Setenv("ZZ_FAKE_OCR", "1")
	defer ShutdownOCR()
	var b bytes.Buffer
	pages := []scanpdf.Page{{Image: grayPage(1)}, {Image: grayPage(2)}, {JBIG2: true}, {Image: grayPage(4)}}
	if err := scanpdf.Write(&b, pages); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "扫描件.pdf")
	os.WriteFile(p, b.Bytes(), 0o644)

	opt := DefaultOptions()
	opt.OCR = OCRConfig{Command: os.Args[0], Args: []string{"-test.run=^$"}, MinSide: 50, PageSec: 30}
	var prog []string
	opt.SetProgress(func(s string) { prog = append(prog, s) })
	r := Extract(p, "pdf", opt)
	if r.Status != doc.StatusOK || !r.OCR {
		t.Fatalf("%+v", r)
	}
	for _, want := range []string{"fakepage-p1", "fakepage-p2", "fakepage-p4"} {
		if !strings.Contains(r.Text, want) {
			t.Fatalf("缺少 %s: %q", want, r.Text)
		}
	}
	_, noPDFium := PDFiumPath()
	if strings.Contains(r.Text, "fakepage-p3") || (noPDFium != nil && !strings.Contains(r.Err, "JBIG2")) {
		t.Fatalf("JBIG2 页应跳过: text=%q err=%q", r.Text, r.Err)
	}
	if len(r.Locs) != 3 || r.Locs[2].Label != "第4页" {
		t.Fatalf("页码定位: %+v", r.Locs)
	}
	if len(prog) < 4 || prog[len(prog)-1] != "第 4/4 页" {
		t.Fatalf("进度: %v", prog)
	}
}

// 字少时大图再认：认出的字少、笔画多的页用大图再认，认出的多就用它；笔画少的页、没打开这个选项时不再认。
func TestOCRPageBigRetry(t *testing.T) {
	t.Setenv("ZZ_FAKE_OCR", "1")
	defer ShutdownOCR()
	path := filepath.Join(t.TempDir(), "page.png")
	writePNG(t, path, grayPage(1))
	c := OCRConfig{Command: os.Args[0], Args: []string{"-test.run=^$"}, MinSide: 50, PageSec: 30}
	ink := func(n int) func() int { return func() int { return n } }
	if text, _ := ocrPage(c, path, ink(200_000)); strings.Contains(text, "big-") {
		t.Fatal("没打开选项时不应再认")
	}
	c.Adaptive = true
	if text, _ := ocrPage(c, path, ink(1000)); strings.Contains(text, "big-") {
		t.Fatal("笔画少的页不应再认")
	}
	if text, err := ocrPage(c, path, ink(200_000)); err != nil || !strings.Contains(text, "big-") {
		t.Fatalf("字少、笔画多的页应用大图再认：%q %v", text, err)
	}
	if text, _ := ocrImage(c, path); strings.Contains(text, "big-") {
		t.Fatal("大图那遍用临时进程，常驻进程不受影响")
	}
	if n := darkPixels(grayPage(1).(*image.Gray)); n < 10_000 || darkPixelsOf(path) != n {
		t.Fatalf("深色像素：%d / %d", n, darkPixelsOf(path))
	}
}

func writePNG(t *testing.T, path string, img image.Image) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}
