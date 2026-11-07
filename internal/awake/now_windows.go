package awake

import (
	"syscall"
	"time"
	"unsafe"
)

var procUnbiased = syscall.NewLazyDLL("kernel32.dll").NewProc("QueryUnbiasedInterruptTime")

// now 用 QueryUnbiasedInterruptTime：开机以来的时间，不含睡眠和休眠（单位 100 纳秒）。
func now() time.Duration {
	var t uint64
	if r, _, _ := procUnbiased.Call(uintptr(unsafe.Pointer(&t))); r == 0 {
		return time.Since(fallbackStart)
	}
	return time.Duration(t) * 100
}

var fallbackStart = time.Now()
