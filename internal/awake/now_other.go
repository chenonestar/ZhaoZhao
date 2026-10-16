//go:build !windows

package awake

import "time"

var monoStart = time.Now()

// now：Linux 的 CLOCK_MONOTONIC、macOS 的 mach_absolute_time 本来就不含挂起的时间。
func now() time.Duration { return time.Since(monoStart) }
