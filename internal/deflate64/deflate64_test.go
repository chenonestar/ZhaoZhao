package deflate64

import (
	"bytes"
	"compress/flate"
	"io"
	"math/rand/v2"
	"testing"
)

// bitWriter 按 Deflate 的位序写：普通字段低位在前，Huffman 码高位在前。
type bitWriter struct {
	out   []byte
	acc   uint64
	nbits uint
}

func (w *bitWriter) bits(v uint64, n uint) {
	w.acc |= v << w.nbits
	w.nbits += n
	for w.nbits >= 8 {
		w.out = append(w.out, byte(w.acc))
		w.acc >>= 8
		w.nbits -= 8
	}
}

func (w *bitWriter) code(c uint64, n uint) { // Huffman 码：高位在前
	var r uint64
	for i := uint(0); i < n; i++ {
		r = r<<1 | (c>>i)&1
	}
	w.bits(r, n)
}

// 固定 Huffman 表（RFC 1951 3.2.6）
func (w *bitWriter) lit(v int) {
	switch {
	case v < 144:
		w.code(uint64(0x30+v), 8)
	case v < 256:
		w.code(uint64(0x190+v-144), 9)
	case v < 280:
		w.code(uint64(v-256), 7)
	default:
		w.code(uint64(0xC0+v-280), 8)
	}
}

func (w *bitWriter) flush() []byte {
	if w.nbits > 0 {
		w.out = append(w.out, byte(w.acc))
	}
	return w.out
}

// 手工构造用到 Deflate64 独有编码的数据：长度码 285（基数 3 + 16 位）、距离码 30 和 31（32 KB 以外）。
func TestDeflate64ExtendedCodes(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	var want []byte
	w := &bitWriter{}
	w.bits(1, 1) // 最后一块
	w.bits(1, 2) // 固定 Huffman
	for i := 0; i < 52000; i++ {
		b := byte(r.IntN(256))
		want = append(want, b)
		w.lit(int(b))
	}
	match := func(length, dist int) {
		w.lit(285)
		w.bits(uint64(length-3), 16)
		code, base := 30, 32769
		if dist >= 49153 {
			code, base = 31, 49153
		}
		w.code(uint64(code), 5)
		w.bits(uint64(dist-base), 14)
		for i := 0; i < length; i++ {
			want = append(want, want[len(want)-dist])
		}
	}
	match(60000, 40000) // 距离码 30
	match(3, 50000)     // 距离码 31、长度码 285 的最小值
	match(65538, 65536) // 最大长度、最大距离
	w.lit(256)
	got, err := io.ReadAll(NewReader(bytes.NewReader(w.flush())))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("解出 %d 字节，应为 %d 字节，内容不同", len(got), len(want))
	}
}

// 不用 Deflate64 独有编码的普通 Deflate 数据（且没有长度 258 的匹配）也能解开：两者在这部分完全相同。
func TestDeflate64CompatibleStream(t *testing.T) {
	var src bytes.Buffer
	for i := 0; i < 2000; i++ {
		src.WriteString("合同条款违约金付款方式 ")
		src.WriteByte(byte(i))
	}
	var c bytes.Buffer
	fw, _ := flate.NewWriter(&c, flate.HuffmanOnly) // 只有字面量，没有匹配
	fw.Write(src.Bytes())
	fw.Close()
	got, err := io.ReadAll(NewReader(&c))
	if err != nil || !bytes.Equal(got, src.Bytes()) {
		t.Fatalf("%v：%d / %d", err, len(got), src.Len())
	}
}

// 损坏的数据返回错误，不崩溃。
func FuzzDeflate64(f *testing.F) {
	w := &bitWriter{}
	w.bits(1, 1)
	w.bits(1, 2)
	for _, b := range []byte("hello hello hello") {
		w.lit(int(b))
	}
	w.lit(285)
	w.bits(5, 16)
	w.code(0, 5)
	w.lit(256)
	f.Add(w.flush())
	f.Fuzz(func(t *testing.T, data []byte) {
		io.Copy(io.Discard, io.LimitReader(NewReader(bytes.NewReader(data)), 1<<22))
	})
}
