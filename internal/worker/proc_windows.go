//go:build windows

package worker

import (
	"os/exec"
	"strconv"
	"syscall"
)

const (
	createNoWindow      = 0x08000000
	belowNormalPriority = 0x00004000
)

func prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow | belowNormalPriority}
}

// 子进程在创建时已带低优先级标志。
func lowerPriority(cmd *exec.Cmd) {}

func killTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	k := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	k.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	_ = k.Run()
}
