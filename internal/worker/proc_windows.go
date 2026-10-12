//go:build windows

package worker

import (
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	createNoWindow      = 0x08000000
	belowNormalPriority = 0x00004000
)

func prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow | belowNormalPriority}
}

var (
	jobOnce sync.Once
	job     windows.Handle
)

// killOnCloseJob 返回一个「句柄关闭即结束全部成员进程」的作业对象。句柄只由主进程持有，
// 主进程无论正常退出、崩溃还是被强制结束，系统都会结束解析子进程以及它们启动的
// LibreOffice、OCR 进程（子进程默认继承作业），不会留下孤儿进程。
func killOnCloseJob() windows.Handle {
	jobOnce.Do(func() {
		h, err := windows.CreateJobObject(nil, nil)
		if err != nil {
			return
		}
		var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		if _, err := windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
			windows.CloseHandle(h)
			return
		}
		job = h
	})
	return job
}

// lowerPriority 在子进程启动后调用：低优先级已在创建时设置，这里把它加入作业对象。
func lowerPriority(cmd *exec.Cmd) {
	j := killOnCloseJob()
	if j == 0 || cmd.Process == nil {
		return
	}
	ph, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return
	}
	_ = windows.AssignProcessToJobObject(j, ph)
	windows.CloseHandle(ph)
}

func killTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	k := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	k.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	_ = k.Run()
}
