//go:build !linux && !windows

package worker

import "syscall"

func setParentDeathSignal(*syscall.SysProcAttr) {}
