//go:build !windows

package sysinfo

// DiskMedia 说明路径所在的磁盘是固态硬盘还是机械硬盘（仅 Windows 实现）。
func DiskMedia(p string) string { return "" }
