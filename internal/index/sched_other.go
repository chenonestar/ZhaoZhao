//go:build !windows

package index

import "time"

// userIdleTime 返回用户多久没用键盘、鼠标了（仅 Windows 实现；其他系统当作一直没人用，测试里换掉）。
var userIdleTime = func() time.Duration { return 1 << 62 }

// onBattery 判断是否使用电池供电（仅 Windows 实现）。
func onBattery() bool { return false }
