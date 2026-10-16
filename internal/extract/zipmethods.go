package extract

import (
	"archive/zip"
	"bytes"
	"compress/bzip2"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"

	"github.com/chenonestar/zhaozhao/internal/deflate64"
	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
	"github.com/ulikunitz/xz/lzma"
)

// zip 条目的压缩方式。Go 的 archive/zip 只支持存储和 Deflate；7-Zip、WinRAR、好压、Bandizip 等
// 软件可以选用其他方式，Windows 资源管理器能解开 Deflate64，新版还能解开更多。

var zipMethodNames = map[uint16]string{
	0: "存储", 1: "Shrink", 6: "Implode", 8: "Deflate", 9: "Deflate64", 12: "BZip2", 14: "LZMA",
	93: "zstd", 95: "xz", 96: "JPEG", 97: "WavPack", 98: "PPMd", 99: "AES 加密",
}

func zipMethodName(m uint16) string {
	if n, ok := zipMethodNames[m]; ok {
		return n
	}
	return fmt.Sprintf("编号 %d", m)
}

// errZipMethod 是不支持的压缩方式。
type errZipMethod struct{ method uint16 }

func (e errZipMethod) Error() string { return "不支持的压缩方式 " + zipMethodName(e.method) }

// zipOpen 打开 zip 条目：标准库支持的直接打开，其余的读原始数据自己解压，并校验长度和 CRC。
func zipOpen(f *zip.File) (io.ReadCloser, error) {
	switch f.Method {
	case zip.Store, zip.Deflate:
		return f.Open()
	}
	raw, err := f.OpenRaw()
	if err != nil {
		return nil, err
	}
	var r io.Reader
	switch f.Method {
	case 9:
		r = deflate64.NewReader(raw)
	case 12:
		r = bzip2.NewReader(raw)
	case 14:
		r, err = zipLZMA(raw, f.UncompressedSize64)
	case 93:
		var d *zstd.Decoder
		if d, err = zstd.NewReader(raw, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(1<<30)); err == nil {
			return &zipCheck{r: d, close: d.Close, f: f, h: crc32.NewIEEE()}, nil
		}
	case 95:
		r, err = xz.NewReader(raw)
	default:
		return nil, errZipMethod{f.Method}
	}
	if err != nil {
		return nil, fmt.Errorf("数据已损坏（%s：%v）", zipMethodName(f.Method), err)
	}
	return &zipCheck{r: r, f: f, h: crc32.NewIEEE()}, nil
}

// zipLZMA：zip 里的 LZMA 先是 2 字节版本、2 字节属性长度和 5 字节属性，再接 LZMA 数据。
// 拼成 .lzma 文件头（属性 + 8 字节原始长度）交给 lzma 解码；数据末尾有没有结束标记都可以。
func zipLZMA(raw io.Reader, size uint64) (io.Reader, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(raw, hdr[:]); err != nil {
		return nil, err
	}
	n := int(binary.LittleEndian.Uint16(hdr[2:]))
	if n != 5 {
		return nil, fmt.Errorf("属性长度 %d", n)
	}
	head := make([]byte, 13)
	if _, err := io.ReadFull(raw, head[:5]); err != nil {
		return nil, err
	}
	binary.LittleEndian.PutUint64(head[5:], size)
	return lzma.NewReader(io.MultiReader(bytes.NewReader(head), raw))
}

var errZipChecksum = errors.New("校验和不对，数据已损坏")

// zipCheck 读出解压后的数据，检查长度和 CRC（与标准库对存储、Deflate 条目的检查相同）。
type zipCheck struct {
	r     io.Reader
	close func()
	f     *zip.File
	h     hash.Hash32
	n     uint64
	err   error
}

func (c *zipCheck) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	n, err := c.r.Read(p)
	c.h.Write(p[:n])
	c.n += uint64(n)
	if c.n > c.f.UncompressedSize64 {
		c.err = errZipChecksum
		return n, c.err
	}
	switch {
	case err == io.EOF:
		if c.n != c.f.UncompressedSize64 || (c.f.CRC32 != 0 && c.h.Sum32() != c.f.CRC32) {
			c.err = errZipChecksum
			return n, c.err
		}
		c.err = io.EOF
	case err != nil:
		c.err = fmt.Errorf("数据已损坏（%s：%v）", zipMethodName(c.f.Method), err)
		return n, c.err
	case c.n == c.f.UncompressedSize64 && c.f.CRC32 != 0 && c.h.Sum32() != c.f.CRC32:
		// 解码器读完正好的长度后可能要再读一次才返回 EOF；内容已经齐了就先校验
		c.err = errZipChecksum
		return n, c.err
	}
	return n, err
}

func (c *zipCheck) Close() error {
	if c.close != nil {
		c.close()
	}
	return nil
}
