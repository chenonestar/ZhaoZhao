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
				Side      int    `json:"max_side_len"` // 修好的组件：这张图单独指定找文字的图多大
			}
			json.Unmarshal(sc.Bytes(), &req)
			name := strings.TrimSuffix(filepath.Base(req.ImagePath), filepath.Ext(req.ImagePath))
			if big || req.Side == 2048 {
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

// 大幅面页用大图找字：单独的 OCR 进程（找文字用 2048 像素的图），常驻的进程不受影响；设置了大幅面页也缩小、
// Windows 自带 OCR 时不用。
func TestBigPageOCR(t *testing.T) {
	t.Setenv("ZZ_FAKE_OCR", "1")
	defer ShutdownOCR()
	path := filepath.Join(t.TempDir(), "page.png")
	writePNG(t, path, grayPage(1))
	c := OCRConfig{Command: os.Args[0], Args: []string{"-test.run=^$"}, MinSide: 50, PageSec: 30}
	if !IsBigPage(image.Pt(2338, 3307)) || IsBigPage(image.Pt(1654, 2339)) || IsBigPage(image.Pt(1700, 2200)) {
		t.Fatal("A3 页算大幅面，A4、Letter 不算")
	}
	big := newBigOCR(c)
	if text, err := big.recognize(path); err != nil || !strings.Contains(text, "big-") {
		t.Fatalf("大幅面页应用大图找字：%q %v", text, err)
	}
	if text, _ := ocrImage(c, path); strings.Contains(text, "big-") {
		t.Fatal("常驻的进程不受影响")
	}
	big.close()
	if big.p != nil || big.pages != 1 {
		t.Fatal("文件识别完结束大图进程")
	}
	c.SmallBigPages = true
	if newBigOCR(c) != nil {
		t.Fatal("大幅面页也缩小时不用大图")
	}
	c.SmallBigPages, c.Engine = false, OCRModelWin
	if newBigOCR(c) != nil {
		t.Fatal("Windows 自带 OCR 不用")
	}
	// 修好的组件：不另开进程，常驻的进程这张图单独用大图找字
	c.Engine, c.Fixed, c.UseFixed = "", os.Args[0], true
	if newBigOCR(c) != nil {
		t.Fatal("用修好的组件时不另开进程")
	}
	if text, isBig, err := recognizePage(c, nil, path, image.Pt(2338, 3307)); err != nil || !isBig || !strings.Contains(text, "big-") {
		t.Fatalf("大幅面页应单独用大图找字：%q %v %v", text, isBig, err)
	}
	if text, isBig, _ := recognizePage(c, nil, path, image.Pt(1654, 2339)); isBig || strings.Contains(text, "big-") {
		t.Fatal("A4 页照常")
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

// 文字框的字高：取四个角里短的那条边（横排、竖排都对），记下中位数和最小的一成。
func TestBoxStats(t *testing.T) {
	if h := boxHeight([][2]float64{{0, 0}, {300, 0}, {300, 30}, {0, 30}}); h != 30 {
		t.Fatalf("横排：%v", h)
	}
	if h := boxHeight([][2]float64{{0, 0}, {20, 0}, {20, 400}, {0, 400}}); h != 20 {
		t.Fatalf("竖排：%v", h)
	}
	setLastBoxes([]float64{40, 10, 30, 20, 50, 60, 70, 80, 90, 100})
	if s := LastBoxes(); s.N != 10 || s.Median != 60 || s.P10 != 20 || !strings.Contains(s.String(), "字高中位数 60 像素") {
		t.Fatalf("%+v %s", s, s)
	}
	setLastBoxes(nil)
	if LastBoxes().String() != "没有文字框" {
		t.Fatal(LastBoxes().String())
	}
}
