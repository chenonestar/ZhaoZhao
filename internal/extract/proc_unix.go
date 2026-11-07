//go:build !windows

package extract

import (
	"os/exec"
	"syscall"
)

// setProcAttr 让子进程独立成组，超时时可连同其子进程一起终止。
func setProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
