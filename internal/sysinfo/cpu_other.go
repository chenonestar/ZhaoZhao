//go:build !windows

package sysinfo

import "time"

// CPUMonitor 在 Windows 以外的系统上取不到数据（Read 返回的 OK 为 false）。
type CPUMonitor struct{}

func NewCPUMonitor(childrenCPU func() (time.Duration, bool)) *CPUMonitor { return &CPUMonitor{} }

func (c *CPUMonitor) Read() CPUStat { return CPUStat{} }

func PowerPlan() string { return "" }

func CPULimit() (maxMHz, limitMHz uint32) { return 0, 0 }
