//go:build windows

package extract

import (
	"fmt"
	"runtime"
	"sync"

	"github.com/chenonestar/zhaozhao/internal/sysinfo"
	"golang.org/x/sys/windows"
)

var (
	procSetProcessAffinityMask = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetProcessAffinityMask")
	pinOnce                    sync.Once
	pinSlot                    = -1
)

// pinSlotOf 返回这个解析进程的 OCR 名额序号：同时运行的解析进程各占一个（用命名互斥量，进程结束时自动放开），
// 拿最小的空着的号。
func pinSlotOf() int {
	pinOnce.Do(func() {
		for k := 0; k < 64; k++ {
			name, _ := windows.UTF16PtrFromString(fmt.Sprintf(`Local\ZhaoZhao-ocrpin-%d`, k))
			h, err := windows.CreateMutex(nil, false, name)
			if err == windows.ERROR_ALREADY_EXISTS {
				windows.CloseHandle(h)
				continue
			}
			if h == 0 {
				return
			}
			pinSlot = k // 句柄一直留着，进程结束时系统关闭
			return
		}
	})
	return pinSlot
}

// pinProcess 把进程 pid 固定在这个解析进程的名额对应的逻辑处理器上，返回写进日志的说明（失败时为空）。
func pinProcess(pid, threads int) string {
	k := pinSlotOf()
	if k < 0 {
		return ""
	}
	mask := pinMask(k, threads, sysinfo.PhysicalCores(), runtime.NumCPU())
	h, err := windows.OpenProcess(windows.PROCESS_SET_INFORMATION|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	if r, _, _ := procSetProcessAffinityMask.Call(uintptr(h), uintptr(mask)); r == 0 {
		return ""
	}
	return fmt.Sprintf("固定在逻辑处理器 %s", maskText(mask))
}

const pinSupported = true
