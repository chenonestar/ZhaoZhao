// Package testutil 是几个包的测试共用的样本生成函数。
package testutil

import (
	"bytes"
	"encoding/binary"
)

// TIFFPage 是手工拼 TIFF 时的一页：灰度，Data 是一个条带里的数据（已按 Compression 压缩）。
type TIFFPage struct {
	W, H        int
	BPS         int    // 每个像素几位（默认 8）
	Compression int    // 1 不压缩（默认），7 JPEG
	Data        []byte // 条带数据
	ByteCount   int    // 文件头里声明的条带字节数（0 表示 len(Data)；声明得比实际多可以模拟末尾残缺的文件）
}

// TIFF 手工拼一个（多页）灰度 TIFF。
func TIFF(bo binary.ByteOrder, pages []TIFFPage) []byte {
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
	for i, pg := range pages {
		bps, comp, count := pg.BPS, pg.Compression, pg.ByteCount
		if bps == 0 {
			bps = 8
		}
		if comp == 0 {
			comp = 1
		}
		if count == 0 {
			count = len(pg.Data)
		}
		const n = 9
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
		entry(256, 3, uint32(pg.W)) // ImageWidth
		entry(257, 3, uint32(pg.H)) // ImageLength
		entry(258, 3, uint32(bps))  // BitsPerSample
		entry(259, 3, uint32(comp)) // Compression
		entry(262, 3, 1)            // Photometric：黑为 0
		entry(273, 4, pix)          // StripOffsets
		entry(277, 3, 1)            // SamplesPerPixel
		entry(278, 3, uint32(pg.H)) // RowsPerStrip
		entry(279, 4, uint32(count))
		next := uint32(0)
		if i < len(pages)-1 {
			next = pix + uint32(len(pg.Data))
		}
		put32(next)
		b.Write(pg.Data)
	}
	return b.Bytes()
}

// MultiPageTIFF 手工拼一个多页的 8 位灰度 TIFF（不压缩）：第 i 页 w×h，像素值都是 vals[i]。
func MultiPageTIFF(bo binary.ByteOrder, w, h int, vals []byte) []byte {
	pages := make([]TIFFPage, len(vals))
	for i, v := range vals {
		pages[i] = TIFFPage{W: w, H: h, Data: bytes.Repeat([]byte{v}, w*h)}
	}
	return TIFF(bo, pages)
}
