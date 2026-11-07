//go:build !windows

package extract

import (
	"unsafe"

	"github.com/ebitengine/purego"
)

func openLibrary(path string) (uintptr, error) {
	return purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_LOCAL)
}

// loadMemDocument 打开内存里的 PDF，失败时返回 PDFium 的错误码。
func loadMemDocument(data []byte) (uintptr, uint32) {
	h := fpdf.LoadMemDocument64(unsafe.Pointer(&data[0]), uintptr(len(data)), nil)
	if h != 0 {
		return h, 0
	}
	return 0, uint32(fpdf.GetLastError())
}
