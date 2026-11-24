//go:build !windows

package sysinfo

// MemoryModules 只在 Windows 上取得到。
func MemoryModules() []MemModule { return nil }
