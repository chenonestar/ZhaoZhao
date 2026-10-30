//go:build windows

package main

import (
	"testing"
	"time"
)

// 程序里有图标资源（测试前用 packaging/tools/winres 生成），托盘能按尺寸取到；找别的找找不出错。
func TestAppIconAndOthers(t *testing.T) {
	for _, size := range []int{16, 20, 24, 32, 48} {
		if appIcon(size) == 0 {
			t.Fatalf("取不到 %d 像素的程序图标（没有生成资源文件？）", size)
		}
	}
	if others := runningOthers(t.TempDir()); len(others) != 0 {
		t.Logf("本机正在运行的其他找找：%+v", others)
	}
	if processStart().After(time.Now()) || time.Since(processStart()) > time.Hour {
		t.Fatalf("进程启动时间不对：%v", processStart())
	}
	if !waitProcessExit(1<<30, time.Second) {
		t.Fatal("不存在的进程应当作已结束")
	}
}
