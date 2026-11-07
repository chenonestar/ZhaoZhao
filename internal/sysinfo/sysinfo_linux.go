package sysinfo

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// Linux 只用于开发和测试。

func cpuName() string { return procField("/proc/cpuinfo", "model name") }

// PhysicalCores：按逻辑处理器数。
func PhysicalCores() int { return runtime.NumCPU() }

// Memory 返回物理内存总量和可用量（字节）。
func Memory() (total, avail uint64) {
	return procKB("/proc/meminfo", "MemTotal"), procKB("/proc/meminfo", "MemAvailable")
}

// PeakMem 返回进程的内存峰值（VmHWM，字节）。
func PeakMem(pid int) uint64 { return procKB("/proc/"+strconv.Itoa(pid)+"/status", "VmHWM") }

// procField 返回 /proc 文件里第一个「名称: 值」行的值。
func procField(p, name string) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if ok && strings.TrimSpace(k) == name {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func procKB(p, name string) uint64 {
	n, _ := strconv.ParseUint(strings.TrimSuffix(procField(p, name), " kB"), 10, 64)
	return n << 10
}
