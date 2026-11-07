package extract

import (
	"fmt"
	"image"
	"math"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 用 Windows 自带的图片解码组件（WIC，照片查看器用的就是它）读图片：Go 的解码器不支持的 TIFF
// （JPEG 压缩、2/4 位色深、旧式 JPEG 等）和其他 Windows 能打开的图片，都能读出来。
// 通过 COM 接口调用：先创建 WIC 工厂，按文件名创建解码器，逐帧（多页 TIFF 每页一帧）转成 8 位灰度后取出像素。

var (
	modOle32             = windows.NewLazySystemDLL("ole32.dll")
	procCoInitializeEx   = modOle32.NewProc("CoInitializeEx")
	procCoUninitialize   = modOle32.NewProc("CoUninitialize")
	procCoCreateInstance = modOle32.NewProc("CoCreateInstance")

	modWindowsCodecs           = windows.NewLazySystemDLL("windowscodecs.dll")
	procWICConvertBitmapSource = modWindowsCodecs.NewProc("WICConvertBitmapSource")

	clsidWICImagingFactory  = windows.GUID{Data1: 0xcacaf262, Data2: 0x9370, Data3: 0x4615, Data4: [8]byte{0xa1, 0x3b, 0x9f, 0x55, 0x39, 0xda, 0x4c, 0x0a}}
	iidIWICImagingFactory   = windows.GUID{Data1: 0xec5ec8a9, Data2: 0xc395, Data3: 0x4314, Data4: [8]byte{0x9c, 0x77, 0x54, 0xd7, 0xa9, 0x35, 0xff, 0x70}}
	guidWICPixelFormat8Gray = windows.GUID{Data1: 0x6fddc324, Data2: 0x4e03, Data3: 0x4bfe, Data4: [8]byte{0xb1, 0x85, 0x3d, 0x77, 0x76, 0x8d, 0xc9, 0x08}}
)

// 接口方法在虚函数表里的序号（IUnknown 占 0～2）。
const (
	vRelease                   = 2
	vFactoryCreateDecoderFile  = 3  // IWICImagingFactory::CreateDecoderFromFilename
	vFactoryCreateBitmapScaler = 11 // IWICImagingFactory::CreateBitmapScaler
	vDecoderGetFrameCount      = 12 // IWICBitmapDecoder::GetFrameCount
	vDecoderGetFrame           = 13 // IWICBitmapDecoder::GetFrame
	vSourceGetSize             = 3  // IWICBitmapSource::GetSize
	vSourceCopyPixels          = 7  // IWICBitmapSource::CopyPixels
	vScalerInitialize          = 8  // IWICBitmapScaler::Initialize
)

// wicMaxPixels：超过这么多像素的页先缩小（内存；OCR 也用不到这么高的分辨率）。
const wicMaxPixels = 50_000_000

// comObject 是 COM 对象：第一个字段指向虚函数表（这里只用到前十几个方法）。
type comObject struct{ vtbl *[32]uintptr }

// method 返回第 i 个方法的地址。
func (o *comObject) method(i int) uintptr { return o.vtbl[i] }

func (o *comObject) ptr() uintptr { return uintptr(unsafe.Pointer(o)) }

func comRelease(o *comObject) {
	if o != nil {
		syscall.SyscallN(o.method(vRelease), o.ptr())
	}
}

func hrErr(step string, hr uintptr) error {
	return fmt.Errorf("Windows 图片解码失败（%s，0x%08X）", step, uint32(hr))
}

func failed(hr uintptr) bool { return int32(hr) < 0 }

// wicDecodeImpl 用 WIC 逐页读出图片（从第 from 页开始，页码从 0 起），每页调用 fn（n 是总页数）。
func wicDecodeImpl(p string, from int, fn func(i, n int, img *image.Gray) error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if hr, _, _ := procCoInitializeEx.Call(0, 0); !failed(hr) { // COINIT_MULTITHREADED；已经初始化过也返回成功
		defer procCoUninitialize.Call()
	}
	var factory *comObject
	if hr, _, _ := procCoCreateInstance.Call(uintptr(unsafe.Pointer(&clsidWICImagingFactory)), 0, 1, // CLSCTX_INPROC_SERVER
		uintptr(unsafe.Pointer(&iidIWICImagingFactory)), uintptr(unsafe.Pointer(&factory))); failed(hr) {
		return hrErr("创建解码组件", hr)
	}
	defer comRelease(factory)
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return err
	}
	var dec *comObject
	if hr, _, _ := syscall.SyscallN(factory.method(vFactoryCreateDecoderFile), factory.ptr(), uintptr(unsafe.Pointer(name)), 0,
		windows.GENERIC_READ, 0, uintptr(unsafe.Pointer(&dec))); failed(hr) {
		return hrErr("打开图片", hr)
	}
	defer comRelease(dec)
	var count uint32
	if hr, _, _ := syscall.SyscallN(dec.method(vDecoderGetFrameCount), dec.ptr(), uintptr(unsafe.Pointer(&count))); failed(hr) {
		return hrErr("读取页数", hr)
	}
	for i := from; i < int(count); i++ {
		var frame *comObject
		if hr, _, _ := syscall.SyscallN(dec.method(vDecoderGetFrame), dec.ptr(), uintptr(i), uintptr(unsafe.Pointer(&frame))); failed(hr) {
			return hrErr(fmt.Sprintf("读取第 %d 页", i+1), hr)
		}
		img, err := wicGray(factory, frame)
		comRelease(frame)
		if err != nil {
			return fmt.Errorf("第 %d 页：%w", i+1, err)
		}
		if err := fn(i, int(count), img); err != nil {
			return err
		}
	}
	return nil
}

// wicGray 把一帧转成 8 位灰度并取出像素；太大的先缩小。
func wicGray(factory, src *comObject) (*image.Gray, error) {
	var w, h uint32
	if hr, _, _ := syscall.SyscallN(src.method(vSourceGetSize), src.ptr(), uintptr(unsafe.Pointer(&w)), uintptr(unsafe.Pointer(&h))); failed(hr) {
		return nil, hrErr("读取尺寸", hr)
	}
	if w == 0 || h == 0 {
		return nil, fmt.Errorf("图片尺寸为 0")
	}
	if px := float64(w) * float64(h); px > wicMaxPixels {
		k := math.Sqrt(wicMaxPixels / px)
		nw, nh := max(1, uint32(float64(w)*k)), max(1, uint32(float64(h)*k))
		var scaler *comObject
		if hr, _, _ := syscall.SyscallN(factory.method(vFactoryCreateBitmapScaler), factory.ptr(), uintptr(unsafe.Pointer(&scaler))); failed(hr) {
			return nil, hrErr("缩小图片", hr)
		}
		defer comRelease(scaler)
		if hr, _, _ := syscall.SyscallN(scaler.method(vScalerInitialize), scaler.ptr(), src.ptr(), uintptr(nw), uintptr(nh), 3); failed(hr) { // Fant 插值
			return nil, hrErr("缩小图片", hr)
		}
		src, w, h = scaler, nw, nh
	}
	var gray *comObject
	if hr, _, _ := procWICConvertBitmapSource.Call(uintptr(unsafe.Pointer(&guidWICPixelFormat8Gray)), src.ptr(), uintptr(unsafe.Pointer(&gray))); failed(hr) {
		return nil, hrErr("转成灰度", hr)
	}
	defer comRelease(gray)
	stride := (w + 3) &^ 3
	buf := make([]byte, int(stride)*int(h))
	if hr, _, _ := syscall.SyscallN(gray.method(vSourceCopyPixels), gray.ptr(), 0, uintptr(stride), uintptr(len(buf)), uintptr(unsafe.Pointer(&buf[0]))); failed(hr) {
		return nil, hrErr("读取像素", hr)
	}
	return &image.Gray{Pix: buf, Stride: int(stride), Rect: image.Rect(0, 0, int(w), int(h))}, nil
}
