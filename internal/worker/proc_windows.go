//go:build windows

package worker

import (
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	createNoWindow      = 0x08000000
	belowNormalPriority = 0x00004000
	idlePriority        = 0x00000040
)

// prepare 设置子进程的创建参数：不显示窗口，「低于正常」优先级（lowest 时用最低的「空闲」优先级）。
// 子进程再启动的 LibreOffice、RapidOCR 继承同样的优先级。
func prepare(cmd *exec.Cmd, lowest bool) {
	prio := uint32(belowNormalPriority)
	if lowest {
		prio = idlePriority
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow | prio}
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

// jobAccounting 是 JOBOBJECT_BASIC_ACCOUNTING_INFORMATION（时间以 100 纳秒为单位）。
type jobAccounting struct {
	TotalUserTime, TotalKernelTime, ThisPeriodTotalUserTime, ThisPeriodTotalKernelTime int64
	TotalPageFaultCount, TotalProcesses, ActiveProcesses, TotalTerminatedProcesses     uint32
}

// ChildrenCPU 返回全部子进程（解析、转换、OCR 进程，以及它们启动的 LibreOffice、RapidOCR）累计用掉的
// CPU 时间，包括已经结束的。它们都在同一个作业对象里，系统替我们累计。取不到时 ok 为 false。
func ChildrenCPU() (d time.Duration, ok bool) {
	j := killOnCloseJob()
	if j == 0 {
		return 0, false
	}
	var a jobAccounting
	if err := windows.QueryInformationJobObject(j, windows.JobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&a)), uint32(unsafe.Sizeof(a)), nil); err != nil {
		return 0, false
	}
	return time.Duration(a.TotalUserTime+a.TotalKernelTime) * 100, true
}

// lowerPriority 在子进程启动后调用：低优先级已在创建时设置，这里把它加入作业对象。
func lowerPriority(cmd *exec.Cmd, lowest bool) {
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
