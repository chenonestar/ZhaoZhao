package extract

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenonestar/zhaozhao/internal/doc"

	"github.com/chenonestar/zhaozhao/internal/testutil"
)

// 多页 TIFF 逐页解码：每一页都是自己的内容，原数据不被改动（不再每页复制整个文件）。
func TestTIFFPagesNoCopy(t *testing.T) {
	for _, bo := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		vals := []byte{10, 120, 250}
		raw := testutil.MultiPageTIFF(bo, 40, 30, vals)
		orig := append([]byte(nil), raw...)
		pages := tiffPages(raw)
		if len(pages) != len(vals) {
			t.Fatalf("%v：%d 页", bo, len(pages))
		}
		for i, off := range pages {
			data, err := tiffPagePNG(raw, off)
			if err != nil {
				t.Fatalf("%v 第 %d 页：%v", bo, i+1, err)
			}
			img, err := png.Decode(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			if r, _, _, _ := img.At(5, 5).RGBA(); byte(r>>8) != vals[i] || img.Bounds().Dx() != 40 {
				t.Fatalf("%v 第 %d 页像素 %d，应为 %d", bo, i+1, r>>8, vals[i])
			}
		}
		if !bytes.Equal(raw, orig) {
			t.Fatal("原数据被改动了")
		}
	}
	if _, err := tiffPagePNG([]byte("II*"), 8); err == nil {
		t.Fatal("太短的数据应报错")
	}
}

// 最后一个条带声明的字节数比文件里剩下的多（扫描软件常见的写法）：缺的部分补 0，照样解码，不再报 unexpected EOF。
func TestTIFFShortLastStrip(t *testing.T) {
	data := bytes.Repeat([]byte{200}, 40*30)
	raw := testutil.TIFF(binary.LittleEndian, []testutil.TIFFPage{{W: 40, H: 30, Data: data, ByteCount: len(data) + 64}})
	pages := tiffPages(raw)
	if len(pages) != 1 {
		t.Fatal(pages)
	}
	out, err := tiffPagePNG(raw, pages[0])
	if err != nil {
		t.Fatal(err)
	}
	img, _ := png.Decode(bytes.NewReader(out))
	if r, _, _, _ := img.At(10, 10).RGBA(); r>>8 != 200 {
		t.Fatalf("像素 %d", r>>8)
	}
}

// Go 的解码器解不开的 TIFF 页（JPEG 压缩、4 位色深等）从这一页起改用 Windows 自带的解码组件；
// 不是 Windows 时照旧报原来的原因；Windows 也解不开时两个原因都写上。
func TestTIFFFallsBackToWindows(t *testing.T) {
	t.Setenv("ZZ_FAKE_OCR", "1")
	defer ShutdownOCR()
	page := testutil.TIFFPage{W: 120, H: 100, Data: bytes.Repeat([]byte{90}, 120*100)}
	jpegPage := testutil.TIFFPage{W: 120, H: 100, Compression: 7, Data: []byte("这里假装是 JPEG 数据")}
	p := filepath.Join(t.TempDir(), "扫描.tif")
	os.WriteFile(p, testutil.TIFF(binary.LittleEndian, []testutil.TIFFPage{page, jpegPage}), 0o644)
	opt := DefaultOptions()
	opt.OCR = OCRConfig{Command: os.Args[0], Args: []string{"-test.run=^$"}, MinSide: 50, PageSec: 30}
	var prog []string
	opt.SetProgress(func(s string) { prog = append(prog, s) })

	old := wicDecode
	defer func() { wicDecode = old }()
	var from int
	wicDecode = func(path string, f int, fn func(i, n int, img *image.Gray) error) error {
		from = f
		for i := f; i < 2; i++ {
			if err := fn(i, 2, image.NewGray(image.Rect(0, 0, 120, 100))); err != nil {
				return err
			}
		}
		return nil
	}
	r := Extract(p, "tif", opt)
	if r.Status != doc.StatusOK || from != 1 || strings.Count(r.Text, "fakepage") != 2 || len(r.Locs) != 2 || prog[len(prog)-1] != "第 2/2 页" {
		t.Fatalf("应从第 2 页起改用 Windows 解码：%+v，从 %d，进度 %v", r, from, prog)
	}

	wicDecode = func(string, int, func(int, int, *image.Gray) error) error { return errNoWIC }
	if r := Extract(p, "tif", opt); r.Status != doc.StatusFailed || !strings.HasPrefix(r.Err, "TIFF 第 2 页无法解码") {
		t.Fatalf("不是 Windows 时照旧报原因：%+v", r)
	}
	wicDecode = func(string, int, func(int, int, *image.Gray) error) error {
		return errors.New("Windows 图片解码失败（打开图片，0x88982F50）")
	}
	if r := Extract(p, "tif", opt); r.Status != doc.StatusFailed || !strings.Contains(r.Err, "TIFF 第 2 页无法解码") || !strings.Contains(r.Err, "0x88982F50") {
		t.Fatalf("两个原因都应写上：%+v", r)
	}
	// BMP 等格式 Go 解不开时也改用 Windows 的
	bad := filepath.Join(t.TempDir(), "坏.bmp")
	os.WriteFile(bad, []byte("BM 不是真的位图"), 0o644)
	wicDecode = func(path string, f int, fn func(i, n int, img *image.Gray) error) error {
		return fn(0, 1, image.NewGray(image.Rect(0, 0, 300, 200)))
	}
	if r := Extract(bad, "bmp", opt); r.Status != doc.StatusOK || !strings.Contains(r.Text, "fakepage") {
		t.Fatalf("%+v", r)
	}
}
