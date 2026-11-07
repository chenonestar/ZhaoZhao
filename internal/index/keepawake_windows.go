package index

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var keepAwakeSupported = func() bool { return true }

var (
	procPowerCreateRequest = kernel32.NewProc("PowerCreateRequest")
	procPowerSetRequest    = kernel32.NewProc("PowerSetRequest")
	procPowerClearRequest  = kernel32.NewProc("PowerClearRequest")
	powerRequest           windows.Handle
	powerReason            *uint16 // 系统保存这个说明（powercfg /requests 能看到），保持引用
)

// reasonContext 是 REASON_CONTEXT：版本、标志，以及一个联合体（简单说明只用第一个指针，按最大成员留足空间）。
type reasonContext struct {
	version uint32
	flags   uint32
	reason  *uint16
	_       [2]uintptr
}

const (
	powerRequestContextSimpleString = 0x1
	powerRequestSystemRequired      = 1 // PowerRequestSystemRequired：系统不自动睡眠，显示器可以关
)

// setKeepAwake 提出或撤销「不要自动睡眠」的电源请求（PowerSetRequest，Windows 7 起支持，不随线程变化）。
func setKeepAwake(on bool) error {
	if powerRequest == 0 {
		if !on {
			return nil
		}
		powerReason, _ = windows.UTF16PtrFromString("找找正在建立索引")
		rc := reasonContext{flags: powerRequestContextSimpleString, reason: powerReason}
		h, _, err := procPowerCreateRequest.Call(uintptr(unsafe.Pointer(&rc)))
		if windows.Handle(h) == windows.InvalidHandle || h == 0 {
			return err
		}
		powerRequest = windows.Handle(h)
	}
	proc := procPowerClearRequest
	if on {
		proc = procPowerSetRequest
	}
	if r, _, err := proc.Call(uintptr(powerRequest), powerRequestSystemRequired); r == 0 {
		return err
	}
	return nil
}
