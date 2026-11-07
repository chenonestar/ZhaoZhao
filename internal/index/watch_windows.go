//go:build windows

package index

import (
	"path/filepath"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// winWatcher 用一个 ReadDirectoryChangesW 句柄递归监听整个索引根，
// 比逐目录监听占用的句柄少得多。
//
// 必须使用重叠 I/O：同步调用阻塞期间，从别的线程 CloseHandle 会一直等到该调用返回，
// 也就是程序退出或卸载磁盘时永远卡住。重叠 I/O 配合 CancelIoEx 可以随时安全地停止。
type winWatcher struct {
	h        windows.Handle
	root     string
	events   chan string
	overflow chan struct{}
	stop     windows.Handle // 手动重置事件：请求循环退出
	done     chan struct{}
	once     sync.Once
}

const (
	fileListDirectory      = 0x0001
	notifyFilter           = 0x1 | 0x2 | 0x8 | 0x10 | 0x40 // 文件名、目录名、大小、写入时间、创建时间
	fileFlagBackupSemantic = 0x02000000
)

func newRecursiveWatcher(root string, _ func(abs, name string) bool) (recursiveWatcher, error) {
	p, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, fileListDirectory,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, fileFlagBackupSemantic|windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return nil, err
	}
	stop, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	w := &winWatcher{h: h, root: root, stop: stop, events: make(chan string, 8192),
		overflow: make(chan struct{}, 1), done: make(chan struct{})}
	go w.loop()
	return w, nil
}

func (w *winWatcher) flagOverflow() {
	select {
	case w.overflow <- struct{}{}:
	default:
	}
}

func (w *winWatcher) loop() {
	defer close(w.done)
	defer windows.CloseHandle(w.h)
	ioEvent, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return
	}
	defer windows.CloseHandle(ioEvent)
	buf := make([]byte, 256<<10)
	for {
		ov := windows.Overlapped{HEvent: ioEvent}
		_ = windows.ResetEvent(ioEvent)
		var n uint32
		if err := windows.ReadDirectoryChanges(w.h, &buf[0], uint32(len(buf)), true, notifyFilter, &n, &ov, 0); err != nil {
			return // 目录消失或句柄无效
		}
		r, _ := windows.WaitForMultipleObjects([]windows.Handle{ioEvent, w.stop}, false, windows.INFINITE)
		if r != windows.WAIT_OBJECT_0 {
			// 被要求停止：取消挂起的读取并等它结束，之后 buf 和 ov 才能安全释放
			_ = windows.CancelIoEx(w.h, &ov)
			_ = windows.GetOverlappedResult(w.h, &ov, &n, true)
			return
		}
		if err := windows.GetOverlappedResult(w.h, &ov, &n, false); err != nil {
			return
		}
		if n == 0 { // 系统缓冲区溢出，丢了事件
			w.flagOverflow()
			continue
		}
		off := uint32(0)
		for off < n {
			info := (*windows.FileNotifyInformation)(unsafe.Pointer(&buf[off]))
			name := windows.UTF16ToString(unsafe.Slice(&info.FileName, info.FileNameLength/2))
			select {
			case w.events <- filepath.Join(w.root, name):
			default:
				w.flagOverflow()
			}
			if info.NextEntryOffset == 0 {
				break
			}
			off += info.NextEntryOffset
		}
	}
}

func (w *winWatcher) Events() <-chan string     { return w.events }
func (w *winWatcher) Overflow() <-chan struct{} { return w.overflow }

// Close 请求循环退出并等它释放句柄；可以安全地从任何线程调用。
func (w *winWatcher) Close() {
	w.once.Do(func() {
		_ = windows.SetEvent(w.stop)
		<-w.done
		windows.CloseHandle(w.stop)
	})
}
