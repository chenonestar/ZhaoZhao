//go:build !windows && !linux

package sysinfo

import "runtime"

func cpuName() string { return "" }

// PhysicalCores：按逻辑处理器数。
func PhysicalCores() int { return runtime.NumCPU() }

// Memory：取不到，返回 0。
func Memory() (total, avail uint64) { return 0, 0 }

// PeakMem：取不到，返回 0。
func PeakMem(pid int) uint64 { return 0 }
