//go:build !windows

package worker

import (
	"os/exec"
	"syscall"
	"time"
)

// ChildrenCPU 返回全部子进程累计用掉的 CPU 时间（仅 Windows 实现：靠作业对象累计）。
func ChildrenCPU() (time.Duration, bool) { return 0, false }

func prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	setParentDeathSignal(cmd.SysProcAttr)
}

// lowerPriority 降低子进程优先级，后台索引不影响正常使用电脑（F-BG-04）。
func lowerPriority(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Setpriority(syscall.PRIO_PROCESS, cmd.Process.Pid, 10)
	}
}

func killTree(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
