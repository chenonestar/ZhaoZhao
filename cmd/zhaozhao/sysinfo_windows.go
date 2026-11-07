//go:build windows

package main

import (
	"fmt"
	"path/filepath"
	"unsafe"

	"github.com/chenonestar/zhaozhao/internal/sysinfo"
	"golang.org/x/sys/windows"
)

var procGetDriveType = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetDriveTypeW")

// sysInfo 返回系统概况：Windows 版本、处理器、内存。
func sysInfo() string {
	v := windows.RtlGetVersion()
	m := sysinfo.This()
	s := fmt.Sprintf("Windows %d.%d.%d，%s（%d 个物理核心、%d 个逻辑处理器）", v.MajorVersion, v.MinorVersion, v.BuildNumber, m.CPU, m.Cores, m.Logical)
	if total, avail := sysinfo.Memory(); total > 0 {
		s += fmt.Sprintf("，内存 %.1f GB（可用 %.1f GB）", float64(total)/(1<<30), float64(avail)/(1<<30))
	}
	if maxMHz, limit := sysinfo.CPULimit(); maxMHz > 0 {
		s += fmt.Sprintf("，CPU 最高 %.1f GHz", float64(maxMHz)/1000)
		if limit > 0 && limit < maxMHz {
			s += fmt.Sprintf("（现在限制在 %.1f GHz）", float64(limit)/1000)
		}
	}
	if p := sysinfo.PowerPlan(); p != "" {
		s += "，电源计划「" + p + "」"
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
