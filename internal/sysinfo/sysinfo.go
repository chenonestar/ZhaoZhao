// Package sysinfo 读取本机的硬件信息：处理器型号、物理核心数、内存，以及进程的内存峰值。
// OCR 按这些信息决定开几个进程、每个进程几个线程，并按电脑分别保存实测结果。
package sysinfo

import (
	"runtime"
	"strings"
)

// Machine 是一台电脑的硬件概况。
type Machine struct {
	CPU      string // 处理器型号
	Cores    int    // 物理核心数
	Logical  int    // 逻辑处理器数
	MemTotal uint64 // 物理内存总量（字节）
}

// This 返回本机的硬件概况。
func This() Machine {
	return Machine{CPU: CPUName(), Cores: PhysicalCores(), Logical: runtime.NumCPU(), MemTotal: memTotal()}
}

func memTotal() uint64 {
	t, _ := Memory()
	return t
}

// CPUName 返回处理器型号（去掉多余空格）；取不到时返回处理器架构。
func CPUName() string {
	if s := strings.Join(strings.Fields(cpuName()), " "); s != "" {
		return s
	}
	return runtime.GOARCH
}
