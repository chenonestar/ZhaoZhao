//go:build windows

package extract

import (
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// openLibrary 加载 DLL：用完整路径加载，并让它依赖的 DLL 也从同一目录查找。
func openLibrary(path string) (uintptr, error) {
	h, err := windows.LoadLibraryEx(path, 0, windows.LOAD_WITH_ALTERED_SEARCH_PATH)
	return uintptr(h), err
}

var (
	loadMemOnce sync.Once
	loadMemProc uintptr
)

// loadMemDocument 打开内存里的 PDF，失败时返回 PDFium 的错误码。
// PDFium 在 Windows 上用 SetLastError 报告错误，而 Go 每次调用 DLL 函数前都会把它清零，
// 所以不能事后再调 FPDF_GetLastError（总是 0）：直接用这次调用返回时取到的 GetLastError。
func loadMemDocument(data []byte) (uintptr, uint32) {
	loadMemOnce.Do(func() {
		loadMemProc, _ = windows.GetProcAddress(windows.Handle(pdfiumLib), "FPDF_LoadMemDocument64")
	})
	if loadMemProc == 0 {
		h := fpdf.LoadMemDocument64(unsafe.Pointer(&data[0]), uintptr(len(data)), nil)
		return h, 0
	}
	r, _, e := syscall.SyscallN(loadMemProc, uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)), 0)
	if r != 0 {
		return r, 0
	}
	return 0, uint32(e)
}
