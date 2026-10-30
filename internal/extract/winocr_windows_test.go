package extract

import (
	"image"
	"image/draw"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// textImage 把几行英文画成白底黑字的灰度图（约 32 像素高的字，接近 200 dpi 扫描件上的正文）。
func textImage(t *testing.T, lines ...string) *image.Gray {
	f, err := opentype.Parse(goregular.TTF)
	if err != nil {
		t.Fatal(err)
	}
	face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: 32, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		t.Fatal(err)
	}
	img := image.NewGray(image.Rect(0, 0, 1400, 80+60*len(lines)))
	draw.Draw(img, img.Bounds(), image.White, image.Point{}, draw.Src)
	d := font.Drawer{Dst: img, Src: image.Black, Face: face}
	for i, l := range lines {
		d.Dot = fixed.P(40, 70+60*i)
		d.DrawString(l)
	}
	return img
}

// Windows 自带的 OCR 能认出图片里的文字。CI 的 Windows 是英文版，用英文识别语言测（中文识别语言要另外安装）。
func TestWinOCR(t *testing.T) {
	e, err := newWinOCR("en-US")
	if err != nil {
		t.Skipf("这台电脑用不了 Windows 自带的 OCR：%v", err)
	}
	img := textImage(t, "The quick brown fox jumps over the lazy dog.", "Invoice number 20241027, total 3580 yuan.")
	t1 := time.Now()
	text, err := e.Recognize(img, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("用时 %v，识别结果：%q", time.Since(t1), text)
	for _, want := range []string{"quick brown fox", "lazy dog", "20241027"} {
		if !strings.Contains(text, want) {
			t.Errorf("没认出 %q", want)
		}
	}
	if n := strings.Count(text, "\n"); n != 2 {
		t.Errorf("应为 2 行，实际 %d 行", n)
	}
	// 转了方向的页：转回来再认；正着的横向页不转
	rec := func(i image.Image) (string, error) { return e.Recognize(i, time.Minute) }
	if text, deg, err := winOCRBest(rotateGray(img, 90), rec); err != nil || deg != 270 || !strings.Contains(text, "quick brown fox") {
		t.Errorf("转了 90° 的图：转 %d° 认出 %q %v", deg, text, err)
	}
	if text, deg, err := winOCRBest(img, rec); err != nil || deg != 0 || !strings.Contains(text, "quick brown fox") {
		t.Errorf("正着的横向图：转 %d° 认出 %q %v", deg, text, err)
	}
	// 超过最大边长的图片先缩小再认，不报错
	big := image.NewGray(image.Rect(0, 0, e.maxDim+500, 300))
	draw.Draw(big, big.Bounds(), image.White, image.Point{}, draw.Src)
	draw.Draw(big, img.Bounds(), img, image.Point{}, draw.Src)
	if _, err := e.Recognize(big, time.Minute); err != nil {
		t.Errorf("大图：%v", err)
	}
	// 彩色图片（RGBA）也能认
	rgba := image.NewRGBA(img.Bounds())
	draw.Draw(rgba, rgba.Bounds(), img, image.Point{}, draw.Src)
	if text, err := e.Recognize(rgba, time.Minute); err != nil || !strings.Contains(text, "quick brown fox") {
		t.Errorf("彩色图片：%q %v", text, err)
	}
}

// 连续认多次，每次之间做一次垃圾回收（会缩小空闲的栈，下次识别时栈再长大、挪位置）：
// 0.2.9 交给 Windows 的写回位置在栈上，栈一挪就拿到空值，解析进程崩溃。
func TestWinOCRRepeat(t *testing.T) {
	e, err := newWinOCR("en-US")
	if err != nil {
		t.Skipf("这台电脑用不了 Windows 自带的 OCR：%v", err)
	}
	img := textImage(t, "The quick brown fox jumps over the lazy dog.")
	for i := 0; i < 30; i++ {
		runtime.GC()
		if text, err := e.Recognize(img, time.Minute); err != nil || !strings.Contains(text, "quick brown fox") {
			t.Fatalf("第 %d 次：%q %v", i+1, text, err)
		}
	}
}

// 本机装有中文识别语言时，模型列表里有 Windows 自带的 OCR，解析进程走它认图片文件。
func TestWinOCRChinese(t *testing.T) {
	if err := WinOCRAvailable(); err != nil {
		t.Skipf("没有中文识别语言：%v", err)
	}
	c := OCRConfig{Command: "unused.exe", PageSec: 60}
	if !c.HasModel(OCRModelWin) || c.WithModel(OCRModelWin).Engine != OCRModelWin {
		t.Fatal("有中文识别语言时应能选 Windows 自带的 OCR")
	}
}
