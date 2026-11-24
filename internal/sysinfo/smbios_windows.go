//go:build windows

package sysinfo

import "unsafe"

var procGetSystemFirmwareTable = kernel32.NewProc("GetSystemFirmwareTable")

// MemoryModules 读出主板报告的内存插槽（取不到时为空）。
func MemoryModules() []MemModule {
	const rsmb = 'R'<<24 | 'S'<<16 | 'M'<<8 | 'B'
	n, _, _ := procGetSystemFirmwareTable.Call(rsmb, 0, 0, 0)
	if n <= 8 || n > 1<<20 {
		return nil
	}
	buf := make([]byte, n)
	if r, _, _ := procGetSystemFirmwareTable.Call(rsmb, 0, uintptr(unsafe.Pointer(&buf[0])), n); r == 0 || r > n {
		return nil
	}
	// RawSMBIOSData：4 个字节的版本信息、4 个字节的表长度，然后是表
	l := int(uint32(buf[4]) | uint32(buf[5])<<8 | uint32(buf[6])<<16 | uint32(buf[7])<<24)
	if l <= 0 || 8+l > len(buf) {
		return nil
	}
	return parseSMBIOSMemory(buf[8 : 8+l])
}
