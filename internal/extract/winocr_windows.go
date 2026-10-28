package extract

import (
	"errors"
	"fmt"
	"image"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows 自带的 OCR（Windows.Media.Ocr，截图识字、PowerToys 用的就是它）。通过 Windows 运行时（WinRT）的
// COM 接口直接调用，不启动别的程序。打包测试里认模拟扫描页每页约 0.4 秒（1 个线程），准确率和 PP-OCRv6 相同；
// 要求系统装了中文识别语言（中文版 Windows 一般自带）。
//
// 接口编号和方法顺序取自 Windows SDK 的元数据（与微软 windows-rs 生成的绑定核对过）。WinRT 对象在一个固定的
// 系统线程上创建和使用。

var (
	modCombase                 = windows.NewLazySystemDLL("combase.dll")
	procRoInitialize           = modCombase.NewProc("RoInitialize")
	procRoGetActivationFactory = modCombase.NewProc("RoGetActivationFactory")
	procWindowsCreateString    = modCombase.NewProc("WindowsCreateString")
	procWindowsDeleteString    = modCombase.NewProc("WindowsDeleteString")
	procWindowsGetStringRaw    = modCombase.NewProc("WindowsGetStringRawBuffer")

	iidIOcrEngineStatics      = windows.GUID{Data1: 0x5bffa85a, Data2: 0x3384, Data3: 0x3540, Data4: [8]byte{0x99, 0x40, 0x69, 0x91, 0x20, 0xd4, 0x28, 0xa8}}
	iidILanguageFactory       = windows.GUID{Data1: 0x9b0252ac, Data2: 0x0c27, Data3: 0x44f8, Data4: [8]byte{0xb7, 0x92, 0x97, 0x93, 0xfb, 0x66, 0xc6, 0x3e}}
	iidISoftwareBitmapStatics = windows.GUID{Data1: 0xdf0385db, Data2: 0x672f, Data3: 0x4a9d, Data4: [8]byte{0x80, 0x6e, 0xc2, 0x44, 0x2f, 0x34, 0x3e, 0x86}}
	iidIBufferFactory         = windows.GUID{Data1: 0x71af914d, Data2: 0xc10f, Data3: 0x484b, Data4: [8]byte{0xbc, 0x50, 0x14, 0xbc, 0x62, 0x3b, 0x3a, 0x27}}
	iidIBufferByteAccess      = windows.GUID{Data1: 0x905a0fef, Data2: 0xbc53, Data3: 0x11df, Data4: [8]byte{0x8c, 0x49, 0x00, 0x1e, 0x4f, 0xc6, 0x86, 0xda}}
	iidIAsyncInfo             = windows.GUID{Data1: 0x00000036, Data2: 0x0000, Data3: 0x0000, Data4: [8]byte{0xc0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}}
)

// 方法在虚函数表里的序号（IUnknown 占 0～2，IInspectable 占 3～5）。
const (
	vQueryInterface        = 0
	vStaticsMaxDimension   = 6  // IOcrEngineStatics::get_MaxImageDimension
	vStaticsTryCreate      = 9  // IOcrEngineStatics::TryCreateFromLanguage
	vLanguageCreate        = 6  // ILanguageFactory::CreateLanguage
	vBufferFactoryCreate   = 6  // IBufferFactory::Create
	vBufferSetLength       = 8  // IBuffer::put_Length
	vByteAccessBuffer      = 3  // IBufferByteAccess::Buffer
	vBitmapCopyFromBuffer  = 9  // ISoftwareBitmapStatics::CreateCopyFromBuffer
	vEngineRecognizeAsync  = 6  // IOcrEngine::RecognizeAsync
	vAsyncGetResults       = 8  // IAsyncOperation<T>::GetResults
	vAsyncInfoStatus       = 7  // IAsyncInfo::get_Status
	vAsyncInfoErrorCode    = 8  // IAsyncInfo::get_ErrorCode
	vAsyncInfoClose        = 10 // IAsyncInfo::Close
	vResultLines           = 6  // IOcrResult::get_Lines
	vLineWords             = 6  // IOcrLine::get_Words
	vWordText              = 7  // IOcrWord::get_Text
	vVectorGetAt           = 6  // IVectorView<T>::GetAt
	vVectorSize            = 7  // IVectorView<T>::get_Size
	pixelFormatBgra8       = 87 // BitmapPixelFormat::Bgra8
	asyncStarted           = 0
	asyncCompleted         = 1
	roInitMultithreaded    = 1
	winOCRDefaultMaxDimPix = 10000
)

// winOCRLang 是识别语言：简体中文（同时认得英文字母和数字）。
const winOCRLang = "zh-Hans-CN"

type winOCRReq struct {
	img  image.Image
	done chan winOCRResp
}

type winOCRResp struct {
	text string
	err  error
}

// winOCREngine 是本进程里的 Windows OCR：一个锁定系统线程的协程持有 WinRT 对象，按顺序处理请求。
type winOCREngine struct {
	lang   string
	reqs   chan winOCRReq
	maxDim int
}

var (
	winOnce   sync.Once
	winEngine *winOCREngine
	winErr    error
)

// winOCR 返回本进程认中文的 Windows OCR（第一次调用时创建）；系统没有中文识别语言等原因用不了时返回错误。
func winOCR() (*winOCREngine, error) {
	winOnce.Do(func() { winEngine, winErr = newWinOCR(winOCRLang) })
	return winEngine, winErr
}

// newWinOCR 创建认 lang 语言（如 "zh-Hans-CN"、"en-US"）的 Windows OCR。
func newWinOCR(lang string) (*winOCREngine, error) {
	ready := make(chan error, 1)
	e := &winOCREngine{lang: lang, reqs: make(chan winOCRReq)}
	go e.loop(ready)
	if err := <-ready; err != nil {
		return nil, err
	}
	return e, nil
}

// WinOCRAvailable 说明这台电脑能不能用 Windows 自带的 OCR 认中文。
func WinOCRAvailable() error {
	_, err := winOCR()
	return err
}

func hstring(s string) (uintptr, error) {
	u, err := windows.UTF16FromString(s)
	if err != nil {
		return 0, err
	}
	var h uintptr
	if hr, _, _ := procWindowsCreateString.Call(uintptr(unsafe.Pointer(&u[0])), uintptr(len(u)-1), uintptr(unsafe.Pointer(&h))); failed(hr) {
		return 0, fmt.Errorf("WindowsCreateString 0x%08X", uint32(hr))
	}
	return h, nil
}

func hstringText(h uintptr) string {
	if h == 0 {
		return ""
	}
	var n uint32
	p, _, _ := procWindowsGetStringRaw.Call(h, uintptr(unsafe.Pointer(&n)))
	if p == 0 || n == 0 {
		return ""
	}
	s := *(**uint16)(unsafe.Pointer(&p)) // 系统返回的字符串地址
	return windows.UTF16ToString(unsafe.Slice(s, n))
}

func freeHstring(h uintptr) {
	if h != 0 {
		procWindowsDeleteString.Call(h)
	}
}

// rtFactory 取 WinRT 类 class 的激活工厂（静态方法或构造方法所在的接口 iid）。
func rtFactory(class string, iid *windows.GUID) (*comObject, error) {
	h, err := hstring(class)
	if err != nil {
		return nil, err
	}
	defer freeHstring(h)
	var f *comObject
	if hr, _, _ := procRoGetActivationFactory.Call(h, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&f))); failed(hr) {
		return nil, fmt.Errorf("取不到 %s（0x%08X）", class, uint32(hr))
	}
	return f, nil
}

func comCall(o *comObject, i int, args ...uintptr) uintptr {
	hr, _, _ := syscall.SyscallN(o.method(i), append([]uintptr{o.ptr()}, args...)...)
	return hr
}

func queryInterface(o *comObject, iid *windows.GUID) (*comObject, error) {
	var out *comObject
	if hr := comCall(o, vQueryInterface, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&out))); failed(hr) {
		return nil, fmt.Errorf("QueryInterface 0x%08X", uint32(hr))
	}
	return out, nil
}

type winOCRObjects struct {
	engine, bitmaps, buffers *comObject
}

func (e *winOCREngine) loop(ready chan<- error) {
	runtime.LockOSThread()                                                                              // WinRT 对象一直在这个线程上用
	if hr, _, _ := procRoInitialize.Call(roInitMultithreaded); failed(hr) && uint32(hr) != 0x80010106 { // RPC_E_CHANGED_MODE：已按别的方式初始化，照样能用
		ready <- fmt.Errorf("Windows 运行时初始化失败（0x%08X）", uint32(hr))
		return
	}
	o, err := e.setup()
	ready <- err
	if err != nil {
		return
	}
	for r := range e.reqs {
		text, err := e.recognize(o, r.img)
		r.done <- winOCRResp{text, err}
	}
}

func (e *winOCREngine) setup() (*winOCRObjects, error) {
	statics, err := rtFactory("Windows.Media.Ocr.OcrEngine", &iidIOcrEngineStatics)
	if err != nil {
		return nil, fmt.Errorf("系统没有 Windows OCR：%v", err)
	}
	defer comRelease(statics)
	var maxDim uint32
	if comCall(statics, vStaticsMaxDimension, uintptr(unsafe.Pointer(&maxDim))); maxDim == 0 {
		maxDim = winOCRDefaultMaxDimPix
	}
	e.maxDim = int(maxDim)
	langs, err := rtFactory("Windows.Globalization.Language", &iidILanguageFactory)
	if err != nil {
		return nil, err
	}
	defer comRelease(langs)
	tag, err := hstring(e.lang)
	if err != nil {
		return nil, err
	}
	defer freeHstring(tag)
	var lang *comObject
	if hr := comCall(langs, vLanguageCreate, tag, uintptr(unsafe.Pointer(&lang))); failed(hr) {
		return nil, fmt.Errorf("创建识别语言失败（0x%08X）", uint32(hr))
	}
	defer comRelease(lang)
	o := &winOCRObjects{}
	if hr := comCall(statics, vStaticsTryCreate, uintptr(unsafe.Pointer(lang)), uintptr(unsafe.Pointer(&o.engine))); failed(hr) || o.engine == nil {
		if e.lang == winOCRLang {
			return nil, errors.New("系统没有安装中文（简体）识别语言（设置 → 时间和语言 → 语言 → 中文 → 选项 → 光学字符识别）")
		}
		return nil, fmt.Errorf("系统没有安装 %s 识别语言", e.lang)
	}
	if o.bitmaps, err = rtFactory("Windows.Graphics.Imaging.SoftwareBitmap", &iidISoftwareBitmapStatics); err != nil {
		return nil, err
	}
	if o.buffers, err = rtFactory("Windows.Storage.Streams.Buffer", &iidIBufferFactory); err != nil {
		return nil, err
	}
	return o, nil
}

// bgra 把图片转成 BGRA 像素（太大时先缩小到 OCR 允许的最大边长以内）。
func bgra(img image.Image, maxDim int) ([]byte, int, int) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	scale := 1
	for w/scale > maxDim || h/scale > maxDim {
		scale++
	}
	ow, oh := w/scale, h/scale
	out := make([]byte, ow*oh*4)
	g, isGray := img.(*image.Gray)
	for y := 0; y < oh; y++ {
		for x := 0; x < ow; x++ {
			sx, sy := b.Min.X+x*scale, b.Min.Y+y*scale
			i := (y*ow + x) * 4
			if isGray {
				v := g.Pix[(sy-g.Rect.Min.Y)*g.Stride+(sx-g.Rect.Min.X)]
				out[i], out[i+1], out[i+2] = v, v, v
			} else {
				r, gg, bb, a := img.At(sx, sy).RGBA()
				// 透明的地方当白底
				out[i] = uint8((bb + (0xffff - a)) >> 8)
				out[i+1] = uint8((gg + (0xffff - a)) >> 8)
				out[i+2] = uint8((r + (0xffff - a)) >> 8)
			}
			out[i+3] = 255
		}
	}
	return out, ow, oh
}

func (e *winOCREngine) recognize(o *winOCRObjects, img image.Image) (string, error) {
	px, w, h := bgra(img, e.maxDim)
	if w == 0 || h == 0 {
		return "", nil
	}
	var buf *comObject
	if hr := comCall(o.buffers, vBufferFactoryCreate, uintptr(len(px)), uintptr(unsafe.Pointer(&buf))); failed(hr) {
		return "", fmt.Errorf("Windows OCR：分配内存失败（0x%08X）", uint32(hr))
	}
	defer comRelease(buf)
	access, err := queryInterface(buf, &iidIBufferByteAccess)
	if err != nil {
		return "", err
	}
	var dst *byte
	hr := comCall(access, vByteAccessBuffer, uintptr(unsafe.Pointer(&dst)))
	comRelease(access)
	if failed(hr) || dst == nil {
		return "", fmt.Errorf("Windows OCR：取内存失败（0x%08X）", uint32(hr))
	}
	copy(unsafe.Slice(dst, len(px)), px)
	comCall(buf, vBufferSetLength, uintptr(len(px)))
	var bmp *comObject
	if hr := comCall(o.bitmaps, vBitmapCopyFromBuffer, uintptr(unsafe.Pointer(buf)), pixelFormatBgra8, uintptr(w), uintptr(h), uintptr(unsafe.Pointer(&bmp))); failed(hr) {
		return "", fmt.Errorf("Windows OCR：创建图片失败（0x%08X）", uint32(hr))
	}
	defer comRelease(bmp)
	var op *comObject
	if hr := comCall(o.engine, vEngineRecognizeAsync, uintptr(unsafe.Pointer(bmp)), uintptr(unsafe.Pointer(&op))); failed(hr) {
		return "", fmt.Errorf("Windows OCR：识别失败（0x%08X）", uint32(hr))
	}
	defer comRelease(op)
	info, err := queryInterface(op, &iidIAsyncInfo)
	if err != nil {
		return "", err
	}
	defer comRelease(info)
	var status int32
	for {
		if hr := comCall(info, vAsyncInfoStatus, uintptr(unsafe.Pointer(&status))); failed(hr) {
			return "", fmt.Errorf("Windows OCR：查询状态失败（0x%08X）", uint32(hr))
		}
		if status != asyncStarted {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if status != asyncCompleted {
		var code int32
		comCall(info, vAsyncInfoErrorCode, uintptr(unsafe.Pointer(&code)))
		comCall(info, vAsyncInfoClose)
		return "", fmt.Errorf("Windows OCR：识别失败（状态 %d，0x%08X）", status, uint32(code))
	}
	var res *comObject
	hr = comCall(op, vAsyncGetResults, uintptr(unsafe.Pointer(&res)))
	comCall(info, vAsyncInfoClose)
	if failed(hr) || res == nil {
		return "", fmt.Errorf("Windows OCR：取结果失败（0x%08X）", uint32(hr))
	}
	defer comRelease(res)
	return resultText(res)
}

// resultText 按行取出识别结果。中文的每个字（或词）是一个「词」，词之间不加空格；两边都是字母、数字时加一个空格。
func resultText(res *comObject) (string, error) {
	lines, err := vectorItems(res, vResultLines)
	if err != nil {
		return "", err
	}
	var out []byte
	for _, line := range lines {
		words, err := vectorItems(line, vLineWords)
		comRelease(line)
		if err != nil {
			return "", err
		}
		var ws []string
		for _, w := range words {
			var hs uintptr
			if !failed(comCall(w, vWordText, uintptr(unsafe.Pointer(&hs)))) {
				ws = append(ws, hstringText(hs))
				freeHstring(hs)
			}
			comRelease(w)
		}
		out = append(out, joinWords(ws)...)
		out = append(out, '\n')
	}
	return string(out), nil
}

// vectorItems 调用 o 的第 getter 个方法取一个 IVectorView，返回其中的全部对象（调用方负责释放）。
func vectorItems(o *comObject, getter int) ([]*comObject, error) {
	var vec *comObject
	if hr := comCall(o, getter, uintptr(unsafe.Pointer(&vec))); failed(hr) || vec == nil {
		return nil, fmt.Errorf("Windows OCR：取结果失败（0x%08X）", uint32(hr))
	}
	defer comRelease(vec)
	var n uint32
	if hr := comCall(vec, vVectorSize, uintptr(unsafe.Pointer(&n))); failed(hr) {
		return nil, fmt.Errorf("Windows OCR：取结果失败（0x%08X）", uint32(hr))
	}
	items := make([]*comObject, 0, n)
	for i := uint32(0); i < n; i++ {
		var it *comObject
		if hr := comCall(vec, vVectorGetAt, uintptr(i), uintptr(unsafe.Pointer(&it))); failed(hr) {
			for _, x := range items {
				comRelease(x)
			}
			return nil, fmt.Errorf("Windows OCR：取结果失败（0x%08X）", uint32(hr))
		}
		items = append(items, it)
	}
	return items, nil
}

// winOCRImage 用 Windows 自带的 OCR 识别一张图片。
func winOCRImage(img image.Image, timeout time.Duration) (string, error) {
	e, err := winOCR()
	if err != nil {
		return "", err
	}
	return e.Recognize(img, timeout)
}

// Recognize 识别一张图片，超过 timeout 返回 errOCRTimeout。
func (e *winOCREngine) Recognize(img image.Image, timeout time.Duration) (string, error) {
	r := winOCRReq{img: img, done: make(chan winOCRResp, 1)}
	tm := time.NewTimer(timeout)
	defer tm.Stop()
	select {
	case e.reqs <- r:
	case <-tm.C:
		return "", fmt.Errorf("%w（等 Windows OCR 超过 %.0f 秒）", errOCRTimeout, timeout.Seconds())
	}
	select {
	case resp := <-r.done:
		return resp.text, resp.err
	case <-tm.C:
		return "", fmt.Errorf("%w（单张图片超过 %.0f 秒）", errOCRTimeout, timeout.Seconds())
	}
}
