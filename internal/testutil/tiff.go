// Package testutil 是几个包的测试共用的样本生成函数。
package testutil

import (
	"bytes"
	"encoding/binary"
)

// MultiPageTIFF 手工拼一个多页的灰度 TIFF（不压缩）：第 i 页 w×h，像素值都是 vals[i]。
func MultiPageTIFF(bo binary.ByteOrder, w, h int, vals []byte) []byte {
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
