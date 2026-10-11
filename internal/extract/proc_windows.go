//go:build windows

package extract

import (
	"os/exec"
	"strconv"
	"syscall"
)

// setProcAttr 隐藏控制台窗口，超时时用 taskkill 终止整个进程树。
func setProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	cmd.Cancel = func() error {
		k := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
		k.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
		return k.Run()
	}
}
