//go:build !windows

package extract

// pinProcess 只在 Windows 上实现。
func pinProcess(pid, threads int) string { return "" }

const pinSupported = false
