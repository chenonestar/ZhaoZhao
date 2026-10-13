//go:build windows

package main

import (
	"fmt"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procGlobalMemoryStatusEx = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")
	procGetDriveType         = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetDriveTypeW")
)

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

// sysInfo 返回系统概况：Windows 版本、处理器数、内存。
func sysInfo() string {
	v := windows.RtlGetVersion()
	s := fmt.Sprintf("Windows %d.%d.%d，%d 个逻辑处理器", v.MajorVersion, v.MinorVersion, v.BuildNumber, runtime.NumCPU())
	m := memoryStatusEx{length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	if r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m))); r != 0 {
		s += fmt.Sprintf("，内存 %.1f GB（可用 %.1f GB）", float64(m.totalPhys)/(1<<30), float64(m.availPhys)/(1<<30))
	}
	return s
}

// driveKind 说明路径所在的盘是本地盘、移动盘还是网络盘。
func driveKind(p string) string {
	root := filepath.VolumeName(p) + `\`
	if len(root) > 3 && root[:2] == `\\` {
		return "网络路径"
	}
	r, _ := windows.UTF16PtrFromString(root)
	t, _, _ := procGetDriveType.Call(uintptr(unsafe.Pointer(r)))
	switch t {
	case 2:
		return "移动盘"
	case 3:
		return "本地盘"
	case 4:
		return "网络盘"
	case 5:
		return "光盘"
	case 6:
		return "内存盘"
	}
	return "未知类型的盘"
}
