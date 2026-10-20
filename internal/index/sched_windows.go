//go:build windows

package index

import (
	"context"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32          = windows.NewLazySystemDLL("user32.dll")
	kernel32        = windows.NewLazySystemDLL("kernel32.dll")
	procLastInput   = user32.NewProc("GetLastInputInfo")
	procPowerStatus = kernel32.NewProc("GetSystemPowerStatus")
	procTickCount   = kernel32.NewProc("GetTickCount")
)

type lastInputInfo struct {
	cbSize uint32
	dwTime uint32
}

type systemPowerStatus struct {
	ACLineStatus        byte
	BatteryFlag         byte
	BatteryLifePercent  byte
	SystemStatusFlag    byte
	BatteryLifeTime     uint32
	BatteryFullLifeTime uint32
}

// onBattery：笔记本使用电池时暂停 OCR 和格式转换（F-BG-06）。
func onBattery() bool {
	var s systemPowerStatus
	r, _, _ := procPowerStatus.Call(uintptr(unsafe.Pointer(&s)))
	return r != 0 && s.ACLineStatus == 0
}

func idleMillis() uint32 {
	li := lastInputInfo{cbSize: uint32(unsafe.Sizeof(lastInputInfo{}))}
	r, _, _ := procLastInput.Call(uintptr(unsafe.Pointer(&li)))
	if r == 0 {
		return 1 << 30
	}
	tick, _, _ := procTickCount.Call()
	return uint32(tick) - li.dwTime
}

// userIdleTime 返回用户多久没用键盘、鼠标了（测试里换掉）。
var userIdleTime = func() time.Duration { return time.Duration(idleMillis()) * time.Millisecond }

// throttleForUser：检测到用户正在操作（3 秒内有键鼠输入）时，每处理完一个文件先让出一会儿（F-BG-04）。
func throttleForUser(ctx context.Context) {
	if idleMillis() >= 3000 {
		return
	}
	select {
	case <-time.After(300 * time.Millisecond):
	case <-ctx.Done():
	}
}
