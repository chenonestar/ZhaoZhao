package extract

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/richardlehane/mscfb"
)

// errNative 表示内置解析器处理不了这个文件（版本太老、结构异常等），应交给 LibreOffice。
type errNative struct{ why string }

func (e errNative) Error() string { return e.why }

func nativeUnsupported(format string, a ...any) error { return errNative{fmt.Sprintf(format, a...)} }

var errNativeEncrypted = errors.New("文件已加密")

var oleMagic = []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}

// oleStreams 读取 OLE 复合文档根目录下的指定流，同时返回根目录下全部流的名称。
func oleStreams(p string, names ...string) (map[string][]byte, map[string]bool, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	d, err := mscfb.New(f)
	if err != nil {
		return nil, nil, nativeUnsupported("不是有效的 OLE 复合文档: %v", err)
	}
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	out := map[string][]byte{}
	all := map[string]bool{}
	for e, err := d.Next(); err == nil; e, err = d.Next() {
		if len(e.Path) != 0 {
			continue
		}
		all[e.Name] = true
		if want[e.Name] {
			b, err := io.ReadAll(d)
			if err != nil {
				return nil, nil, nativeUnsupported("读取 %s 流失败: %v", e.Name, err)
			}
			out[e.Name] = b
		}
	}
	return out, all, nil
}

func le16(b []byte, off int) int {
	if off < 0 || off+2 > len(b) {
		return 0
	}
	return int(binary.LittleEndian.Uint16(b[off:]))
}

func le32(b []byte, off int) int {
	if off < 0 || off+4 > len(b) {
		return 0
	}
	return int(binary.LittleEndian.Uint32(b[off:]))
}

// fileMagic 读取文件开头的若干字节，判断真实格式（扩展名经常不可信：.doc 可能是 RTF、HTML，.xls 可能是网页表格）。
func fileMagic(p string) []byte {
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	b := make([]byte, 512)
	n, _ := io.ReadFull(f, b)
	return b[:n]
}

func hasPrefixFold(b []byte, s string) bool {
	b = bytes.TrimLeft(b, " \t\r\n\xef\xbb\xbf")
	return len(b) >= len(s) && bytes.EqualFold(b[:len(s)], []byte(s))
}
