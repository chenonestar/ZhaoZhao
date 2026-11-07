//go:build !windows

package main

import (
	"fmt"
	"runtime"

	"github.com/chenonestar/zhaozhao/internal/sysinfo"
)

func sysInfo() string {
	m := sysinfo.This()
	s := fmt.Sprintf("%s/%s，%s（%d 个逻辑处理器）", runtime.GOOS, runtime.GOARCH, m.CPU, m.Logical)
	if m.MemTotal > 0 {
		s += fmt.Sprintf("，内存 %.1f GB", float64(m.MemTotal)/(1<<30))
	}
	return s
}

func driveKind(p string) string { return "本地盘" }
