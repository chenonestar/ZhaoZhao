package extract

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"testing"
)

// multiPageTIFF 手工拼一个多页的灰度 TIFF（不压缩）：第 i 页 w×h，像素值都是 vals[i]。
func multiPageTIFF(bo binary.ByteOrder, w, h int, vals []byte) []byte {
	var b bytes.Buffer
	if bo == binary.LittleEndian {
		b.WriteString("II")
	} else {
		b.WriteString("MM")
	}
	put16 := func(v uint16) { _ = binary.Write(&b, bo, v) }
	put32 := func(v uint32) { _ = binary.Write(&b, bo, v) }
	put16(42)
	put32(8)
	for i, v := range vals {
		const n = 8
		ifd := uint32(b.Len())
		pix := ifd + 2 + n*12 + 4
		put16(n)
		entry := func(tag, typ uint16, val uint32) {
			put16(tag)
			put16(typ)
			put32(1)
			if typ == 3 {
				put16(uint16(val))
				put16(0)
			} else {
				put32(val)
			}
		}
		entry(256, 3, uint32(w))   // ImageWidth
		entry(257, 3, uint32(h))   // ImageLength
		entry(258, 3, 8)           // BitsPerSample
		entry(259, 3, 1)           // Compression：不压缩
		entry(262, 3, 1)           // Photometric：黑为 0
		entry(273, 4, pix)         // StripOffsets
		entry(278, 3, uint32(h))   // RowsPerStrip
		entry(279, 4, uint32(w*h)) // StripByteCounts
		next := uint32(0)
		if i < len(vals)-1 {
			next = pix + uint32(w*h)
		}
		put32(next)
		b.Write(bytes.Repeat([]byte{v}, w*h))
	}
	return b.Bytes()
}

// 多页 TIFF 逐页解码：每一页都是自己的内容，原数据不被改动（不再每页复制整个文件）。
func TestTIFFPagesNoCopy(t *testing.T) {
	for _, bo := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		vals := []byte{10, 120, 250}
		raw := multiPageTIFF(bo, 40, 30, vals)
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
