//go:build !windows

package index

import "context"

// onBattery 判断是否使用电池供电（仅 Windows 实现）。
func onBattery() bool { return false }

// throttleForUser 在用户正在操作电脑时放慢后台处理（仅 Windows 实现）。
func throttleForUser(ctx context.Context) {}
