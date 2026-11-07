package sysinfo

import (
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	kernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	procLogicalProcInfo      = kernel32.NewProc("GetLogicalProcessorInformation")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
	procK32GetProcessMemInfo = kernel32.NewProc("K32GetProcessMemoryInfo")
)

func cpuName() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `HARDWARE\DESCRIPTION\System\CentralProcessor\0`, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	s, _, _ := k.GetStringValue("ProcessorNameString")
	return s
}

// slpi 是 SYSTEM_LOGICAL_PROCESSOR_INFORMATION：处理器掩码、关系类型，以及 16 字节的联合体。
type slpi struct {
	mask         uintptr
	relationship uint32
	_            [2]uint64
}

// PhysicalCores 返回物理核心数（超线程的两个逻辑处理器算一个核心）。OCR 的计算是满负荷的，
// 线程数超过物理核心后几乎不再变快。取不到时返回逻辑处理器数。
func PhysicalCores() int {
	var n uint32
	procLogicalProcInfo.Call(0, uintptr(unsafe.Pointer(&n)))
	sz := uint32(unsafe.Sizeof(slpi{}))
	if n < sz {
		return runtime.NumCPU()
	}
	buf := make([]slpi, n/sz)
	if r, _, _ := procLogicalProcInfo.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n))); r == 0 {
		return runtime.NumCPU()
	}
	cores := 0
	for _, e := range buf[:n/sz] {
		if e.relationship == 0 { // RelationProcessorCore
			cores++
		}
	}
	if cores == 0 {
		return runtime.NumCPU()
	}
	return cores
}

type memoryStatusEx struct {
	length               uint32
	memoryLoad           uint32
	totalPhys, availPhys uint64
	totalPageFile        uint64
	availPageFile        uint64
	totalVirtual         uint64
	availVirtual         uint64
	availExtendedVirtual uint64
}

// Memory 返回物理内存总量和可用量（字节，可用量含可以随时腾出的文件缓存）。取不到时都是 0。
func Memory() (total, avail uint64) {
	m := memoryStatusEx{length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	if r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m))); r == 0 {
		return 0, 0
	}
	return m.totalPhys, m.availPhys
}

// processMemoryCounters 是 PROCESS_MEMORY_COUNTERS。
type processMemoryCounters struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
}

// PeakMem 返回进程的内存峰值（工作集峰值，字节）；取不到时返回 0。
func PeakMem(pid int) uint64 {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_VM_READ, false, uint32(pid))
	if err != nil {
		return 0
	}
	defer windows.CloseHandle(h)
	c := processMemoryCounters{cb: uint32(unsafe.Sizeof(processMemoryCounters{}))}
	if r, _, _ := procK32GetProcessMemInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&c)), uintptr(c.cb)); r == 0 {
		return 0
	}
	return uint64(c.peakWorkingSetSize)
}
