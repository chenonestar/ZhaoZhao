//go:build linux

package worker

import "syscall"

// setParentDeathSignal：主进程退出时内核向子进程发 SIGKILL，不留孤儿进程。
func setParentDeathSignal(a *syscall.SysProcAttr) { a.Pdeathsig = syscall.SIGKILL }
