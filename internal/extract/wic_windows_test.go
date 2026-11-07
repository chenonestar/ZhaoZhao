package extract

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"

	"github.com/chenonestar/zhaozhao/internal/testutil"
)

// Windows 自带的解码组件能读出 Go 的解码器不支持的 TIFF：JPEG 压缩、4 位灰度；多页逐页读出，可以从中间某页开始。
func TestWICDecodesUnsupportedTIFF(t *testing.T) {
	src := image.NewGray(image.Rect(0, 0, 64, 48))
	for i := range src.Pix {
		src.Pix[i] = 200
	}
	for y := 10; y < 20; y++ {
		for x := 8; x < 56; x++ {
			src.Set(x, y, color.Gray{20})
		}
	}
	var jb bytes.Buffer
	if err := jpeg.Encode(&jb, src, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	four := bytes.Repeat([]byte{0xAA}, 64/2*48) // 每个像素 4 位，值都是 10（转成 8 位约 170）
	raw := testutil.TIFF(binary.LittleEndian, []testutil.TIFFPage{
		{W: 64, H: 48, Compression: 7, Data: jb.Bytes()},
		{W: 64, H: 48, BPS: 4, Data: four},
	})
	p := filepath.Join(t.TempDir(), "扫描件（JPEG 压缩）.tif")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	for i, off := range tiffPages(raw) {
		if _, _, err := tiffPageImage(raw, off); err == nil {
			t.Fatalf("第 %d 页 Go 的解码器应解不开", i+1)
		}
	}
	var got []*image.Gray
	if err := wicDecodeImpl(p, 0, func(i, n int, img *image.Gray) error {
		if n != 2 || i != len(got) {
			t.Fatalf("第 %d/%d 页", i+1, n)
		}
		got = append(got, img)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Bounds().Dx() != 64 || got[1].Bounds().Dy() != 48 {
		t.Fatalf("%d 页", len(got))
	}
	if v := got[0].GrayAt(30, 30).Y; v < 180 || v > 220 {
		t.Fatalf("JPEG 页背景 %d", v)
	}
	if v := got[0].GrayAt(30, 15).Y; v > 60 {
		t.Fatalf("JPEG 页深色条 %d", v)
	}
	if v := got[1].GrayAt(5, 5).Y; v < 160 || v > 180 {
		t.Fatalf("4 位页 %d", v)
	}
	n := 0
	if err := wicDecodeImpl(p, 1, func(i, _ int, img *image.Gray) error { n++; return nil }); err != nil || n != 1 {
		t.Fatalf("从第 2 页开始：%d 页 %v", n, err)
	}
	if err := wicDecodeImpl(filepath.Join(t.TempDir(), "没有这个文件.tif"), 0, func(int, int, *image.Gray) error { return nil }); err == nil {
		t.Fatal("文件不存在应报错")
	}
}
