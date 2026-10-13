//go:build !windows

package main

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strings"
)

func sysInfo() string {
	s := fmt.Sprintf("%s/%s，%d 个逻辑处理器", runtime.GOOS, runtime.GOARCH, runtime.NumCPU())
	if f, err := os.Open("/proc/meminfo"); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "MemTotal:") {
				var kb float64
				fmt.Sscanf(strings.TrimPrefix(sc.Text(), "MemTotal:"), "%f", &kb)
				s += fmt.Sprintf("，内存 %.1f GB", kb/(1<<20))
			}
		}
	}
	return s
}

func driveKind(p string) string { return "本地盘" }
