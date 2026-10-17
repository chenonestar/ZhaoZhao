package extract

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"testing"

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
