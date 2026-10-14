package extract

import (
	"errors"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unicode/utf16"
	"unsafe"

	"github.com/ebitengine/purego"
)

// PDFium（Chrome 用的 PDF 引擎）在运行时加载，不需要 cgo：找得到 pdfium.dll / libpdfium.so 就用它
// 提取文字、把扫描页渲染成图片做 OCR（任何图片格式都能处理，包括 JBIG2、JPEG 2000）；
// 找不到就退回纯 Go 的解析器。PDFium 不是线程安全的，所有调用都在 pdfiumMu 里进行。

var (
	pdfiumOnce sync.Once
	pdfiumErr  error
	pdfiumPath string
	pdfiumLib  uintptr
	pdfiumMu   sync.Mutex
)

var fpdf struct {
	InitLibrary         func()
	LoadMemDocument64   func(data unsafe.Pointer, size uintptr, password *byte) uintptr
	GetLastError        func() uintptr
	CloseDocument       func(doc uintptr)
	GetPageCount        func(doc uintptr) int32
	GetPageSizeByIndexF func(doc uintptr, index int32, size *[2]float32) int32
	LoadPage            func(doc uintptr, index int32) uintptr
	ClosePage           func(page uintptr)
	TextLoadPage        func(page uintptr) uintptr
	TextClosePage       func(tp uintptr)
	TextCountChars      func(tp uintptr) int32
	TextGetText         func(tp uintptr, start, count int32, buf *uint16) int32
	BitmapCreate        func(w, h, alpha int32) uintptr
	BitmapFillRect      func(bmp uintptr, left, top, w, h int32, color uint32) int32
	BitmapGetBuffer     func(bmp uintptr) *byte
	BitmapGetStride     func(bmp uintptr) int32
	BitmapDestroy       func(bmp uintptr)
	RenderPageBitmap    func(bmp, page uintptr, x, y, w, h, rotate, flags int32)
}

// pdfiumCandidates 返回可能的 PDFium 库位置：环境变量、程序目录下的 components/pdfium、程序目录。
func pdfiumCandidates() []string {
	name := "libpdfium.so"
	switch runtime.GOOS {
	case "windows":
		name = "pdfium.dll"
	case "darwin":
		name = "libpdfium.dylib"
	}
	var c []string
	if p := os.Getenv("ZHAOZHAO_PDFIUM"); p != "" {
		c = append(c, p)
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		c = append(c, filepath.Join(dir, "components", "pdfium", name), filepath.Join(dir, name))
	}
	return c
}

// loadPDFium 加载 PDFium（每个进程一次）。
func loadPDFium() error {
	pdfiumOnce.Do(func() {
		var lib uintptr
		var errs []string
		for _, p := range pdfiumCandidates() {
			if _, err := os.Stat(p); err != nil {
				continue
			}
			h, err := openLibrary(p)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", p, err))
				continue
			}
			lib, pdfiumPath = h, p
			break
		}
		if lib == 0 {
			pdfiumErr = errors.New("未找到 PDFium")
			if len(errs) > 0 {
				pdfiumErr = fmt.Errorf("无法加载 PDFium：%s", strings.Join(errs, "；"))
			}
			return
		}
		pdfiumLib = lib
		pdfiumErr = registerPDFium(lib)
		if pdfiumErr == nil {
			fpdf.InitLibrary()
		}
	})
	return pdfiumErr
}

func registerPDFium(lib uintptr) (err error) {
	cur := ""
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("PDFium 缺少函数 %s（版本太旧？）", cur)
		}
	}()
	reg := func(fptr any, name string) {
		cur = name
		purego.RegisterLibFunc(fptr, lib, name)
	}
	reg(&fpdf.InitLibrary, "FPDF_InitLibrary")
	reg(&fpdf.LoadMemDocument64, "FPDF_LoadMemDocument64")
	reg(&fpdf.GetLastError, "FPDF_GetLastError")
	reg(&fpdf.CloseDocument, "FPDF_CloseDocument")
	reg(&fpdf.GetPageCount, "FPDF_GetPageCount")
	reg(&fpdf.GetPageSizeByIndexF, "FPDF_GetPageSizeByIndexF")
	reg(&fpdf.LoadPage, "FPDF_LoadPage")
	reg(&fpdf.ClosePage, "FPDF_ClosePage")
	reg(&fpdf.TextLoadPage, "FPDFText_LoadPage")
	reg(&fpdf.TextClosePage, "FPDFText_ClosePage")
	reg(&fpdf.TextCountChars, "FPDFText_CountChars")
	reg(&fpdf.TextGetText, "FPDFText_GetText")
	reg(&fpdf.BitmapCreate, "FPDFBitmap_Create")
	reg(&fpdf.BitmapFillRect, "FPDFBitmap_FillRect")
	reg(&fpdf.BitmapGetBuffer, "FPDFBitmap_GetBuffer")
	reg(&fpdf.BitmapGetStride, "FPDFBitmap_GetStride")
	reg(&fpdf.BitmapDestroy, "FPDFBitmap_Destroy")
	reg(&fpdf.RenderPageBitmap, "FPDF_RenderPageBitmap")
	return nil
}

// PDFiumPath 返回已加载的 PDFium 路径；没有加载成功返回空串和原因。
func PDFiumPath() (string, error) {
	if err := loadPDFium(); err != nil {
		return "", err
	}
	return pdfiumPath, nil
}

// PDFium 的错误码（FPDF_GetLastError）。
const (
	fpdfErrFile     = 2
	fpdfErrFormat   = 3
	fpdfErrPassword = 4
	fpdfErrSecurity = 5
)

var errPDFPassword = errors.New("文件已加密")

// pdfDoc 是一个打开的 PDF 文档。调用方必须持有 pdfiumMu。
type pdfDoc struct {
	h    uintptr
	data []byte // PDFium 直接读这块内存，关闭文档前不能释放
}

// openPDFium 打开 PDF。整个文件读进内存交给 PDFium（不经过 PDFium 的文件名处理，中文路径没有问题）。
func openPDFium(p string) (*pdfDoc, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, errors.New("空文件")
	}
	h, code := loadMemDocument(data)
	if h == 0 {
		switch code {
		case fpdfErrPassword, fpdfErrSecurity:
			return nil, errPDFPassword
		case fpdfErrFormat:
			return nil, errors.New("不是有效的 PDF 文件")
		default:
			return nil, fmt.Errorf("PDFium 无法打开（错误 %d）", code)
		}
	}
	return &pdfDoc{h: h, data: data}, nil
}

func (d *pdfDoc) close() {
	fpdf.CloseDocument(d.h)
	runtime.KeepAlive(d.data)
	d.data = nil
}

func (d *pdfDoc) pages() int { return int(fpdf.GetPageCount(d.h)) }

// text 返回第 i 页（从 0 开始）的文字。
func (d *pdfDoc) text(i int) (string, error) {
	page := fpdf.LoadPage(d.h, int32(i))
	if page == 0 {
		return "", fmt.Errorf("第 %d 页无法读取", i+1)
	}
	defer fpdf.ClosePage(page)
	tp := fpdf.TextLoadPage(page)
	if tp == 0 {
		return "", fmt.Errorf("第 %d 页的文字无法读取", i+1)
	}
	defer fpdf.TextClosePage(tp)
	n := fpdf.TextCountChars(tp)
	if n <= 0 {
		return "", nil
	}
	buf := make([]uint16, n+1)
	got := fpdf.TextGetText(tp, 0, n, &buf[0])
	if got <= 0 {
		return "", nil
	}
	if int(got) > len(buf) {
		got = int32(len(buf))
	}
	u := buf[:got]
	for len(u) > 0 && u[len(u)-1] == 0 {
		u = u[:len(u)-1]
	}
	// PDFium 用 \r\n 分行；0xFFFE、0x02 是行末断字符号
	s := strings.Map(func(r rune) rune {
		switch {
		case r == '\r':
			return -1
		case r == '\n' || r == '\t':
			return r
		case r == 0x02 || r == 0xFFFE || r < 0x20:
			return -1
		}
		return r
	}, string(utf16.Decode(u)))
	return s, nil
}

// render 把第 i 页渲染成灰度图：dpi 为分辨率，长边不超过 maxSide 像素。
func (d *pdfDoc) render(i int, dpi float64, maxSide int) (*image.Gray, error) {
	var size [2]float32
	if fpdf.GetPageSizeByIndexF(d.h, int32(i), &size) == 0 || size[0] <= 0 || size[1] <= 0 {
		return nil, fmt.Errorf("第 %d 页没有尺寸", i+1)
	}
	scale := dpi / 72
	w, h := float64(size[0])*scale, float64(size[1])*scale
	if m := max(w, h); m > float64(maxSide) {
		w, h = w*float64(maxSide)/m, h*float64(maxSide)/m
	}
	iw, ih := max(int32(w), 1), max(int32(h), 1)
	page := fpdf.LoadPage(d.h, int32(i))
	if page == 0 {
		return nil, fmt.Errorf("第 %d 页无法读取", i+1)
	}
	defer fpdf.ClosePage(page)
	bmp := fpdf.BitmapCreate(iw, ih, 0)
	if bmp == 0 {
		return nil, fmt.Errorf("第 %d 页渲染失败（内存不足？）", i+1)
	}
	defer fpdf.BitmapDestroy(bmp)
	fpdf.BitmapFillRect(bmp, 0, 0, iw, ih, 0xFFFFFFFF)
	const fpdfAnnot, fpdfGrayscale, fpdfPrinting = 0x01, 0x08, 0x800
	fpdf.RenderPageBitmap(bmp, page, 0, 0, iw, ih, 0, fpdfAnnot|fpdfGrayscale|fpdfPrinting)
	stride := int(fpdf.BitmapGetStride(bmp))
	buf := fpdf.BitmapGetBuffer(bmp)
	if buf == nil || stride < int(iw)*4 {
		return nil, fmt.Errorf("第 %d 页渲染失败", i+1)
	}
	src := unsafe.Slice(buf, stride*int(ih))
	img := image.NewGray(image.Rect(0, 0, int(iw), int(ih)))
	for y := 0; y < int(ih); y++ {
		row := src[y*stride:]
		out := img.Pix[y*img.Stride:]
		for x := 0; x < int(iw); x++ {
			b, g, r := uint32(row[x*4]), uint32(row[x*4+1]), uint32(row[x*4+2]) // BGRx
			out[x] = uint8((299*r + 587*g + 114*b) / 1000)
		}
	}
	return img, nil
}
