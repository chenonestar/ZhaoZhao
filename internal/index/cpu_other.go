//go:build !windows

package index

import "runtime"

// physicalCores：非 Windows 只用于开发和测试，按逻辑处理器数。
func physicalCores() int { return runtime.NumCPU() }
