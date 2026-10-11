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
type winWatcher struct {
	h        windows.Handle
	root     string
	events   chan string
	overflow chan struct{}
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
		windows.OPEN_EXISTING, fileFlagBackupSemantic, 0)
	if err != nil {
		return nil, err
	}
	w := &winWatcher{h: h, root: root, events: make(chan string, 8192), overflow: make(chan struct{}, 1)}
	go w.loop()
	return w, nil
}

func (w *winWatcher) loop() {
	buf := make([]byte, 256<<10)
	for {
		var n uint32
		err := windows.ReadDirectoryChanges(w.h, &buf[0], uint32(len(buf)), true, notifyFilter, &n, nil, 0)
		if err != nil {
			return // 句柄被关闭或目录消失
		}
		if n == 0 {
			select {
			case w.overflow <- struct{}{}:
			default:
			}
			continue
		}
		off := uint32(0)
		for {
			info := (*windows.FileNotifyInformation)(unsafe.Pointer(&buf[off]))
			nameLen := info.FileNameLength / 2
			name := windows.UTF16ToString(unsafe.Slice(&info.FileName, nameLen))
			select {
			case w.events <- filepath.Join(w.root, name):
			default:
				select {
				case w.overflow <- struct{}{}:
				default:
				}
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
func (w *winWatcher) Close()                    { w.once.Do(func() { windows.CloseHandle(w.h) }) }
