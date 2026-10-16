package index

import (
	"runtime"
	"unsafe"
)

var procLogicalProcInfo = kernel32.NewProc("GetLogicalProcessorInformation")

// slpi 是 SYSTEM_LOGICAL_PROCESSOR_INFORMATION：处理器掩码、关系类型，以及 16 字节的联合体。
type slpi struct {
	mask         uintptr
	relationship uint32
	_            [2]uint64
}

// physicalCores 返回物理核心数（超线程的两个逻辑处理器算一个核心）。OCR 的计算是满负荷的，
// 线程数超过物理核心后几乎不再变快。取不到时返回逻辑处理器数。
func physicalCores() int {
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
