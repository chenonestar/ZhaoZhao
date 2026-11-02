//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
	"unsafe"

	"github.com/chenonestar/zhaozhao/internal/logx"
	"golang.org/x/sys/windows"
)

// runningOthers 找出本机上正在运行、用别的数据目录的找找：进程名和本程序相同的进程，按程序位置推出数据目录，
// 读那里的 instance.json，里面记的进程号是这个进程、连得上，就是在运行的找找主程序（解析子进程和主程序同名、
// 同一个数据目录，但进程号对不上）。0.2.10 及以前的版本也能这样找到。不按父进程区分主程序和子进程：从别的
// 找找（或名字相同的程序）里启动的主程序会被当成子进程漏掉。
func runningOthers(dataDir string) []otherInstance {
	self, _ := os.Executable()
	names := map[string]bool{strings.ToLower(filepath.Base(self)): true, "zhaozhao.exe": true}
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		startNotes = append(startNotes, "找别的找找：列不出进程："+err.Error())
		return nil
	}
	defer windows.CloseHandle(snap)
	pids := map[string][]uint32{} // 数据目录 → 这些进程
	exes := map[string]string{}
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if !names[strings.ToLower(windows.UTF16ToString(e.ExeFile[:]))] || int(e.ProcessID) == os.Getpid() {
			continue
		}
		exe := processExe(e.ProcessID)
		if exe == "" {
			startNotes = append(startNotes, fmt.Sprintf("找别的找找：进程 %d 取不到程序位置", e.ProcessID))
			continue
		}
		dd := dataDirOfExe(exe)
		if dd == "" || samePath(dd, dataDir) {
			continue
		}
		key := strings.ToLower(filepath.Clean(absPath(dd)))
		pids[key] = append(pids[key], e.ProcessID)
		exes[key] = exe
	}
	var out []otherInstance
	for key, ps := range pids {
		dd := filepath.Dir(exes[key])
		if d := dataDirOfExe(exes[key]); d != "" {
			dd = d
		}
		o, ok := probeInstance(dd)
		if ok && slices.Contains(ps, uint32(o.PID)) {
			o.Exe = processExe(uint32(o.PID))
			out = append(out, o)
			continue
		}
		startNotes = append(startNotes, fmt.Sprintf("找别的找找：%s 有 %d 个同名进程，但没确认是在运行的找找（instance.json 里的进程 %d，连得上 %v）",
			logx.P(dd), len(ps), o.PID, ok))
	}
	return out
}

func processExe(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, 1024)
	n := uint32(len(buf))
	if windows.QueryFullProcessImageName(h, 0, &buf[0], &n) != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}

// waitProcessExit 等进程 pid 结束，最多 timeout。
func waitProcessExit(pid int, timeout time.Duration) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return true // 已经没有这个进程
	}
	defer windows.CloseHandle(h)
	ev, _ := windows.WaitForSingleObject(h, uint32(timeout.Milliseconds()))
	return ev == windows.WAIT_OBJECT_0
}

// processStart 返回本进程被创建的时间（Windows 记下的）：从双击图标到程序开始运行的这段也算进启动用时。
func processStart() time.Time {
	var c, e, k, u windows.Filetime
	if windows.GetProcessTimes(windows.CurrentProcess(), &c, &e, &k, &u) != nil {
		return time.Now()
	}
	return time.Unix(0, c.Nanoseconds())
}

var (
	procTaskDialogIndirect = windows.NewLazySystemDLL("comctl32.dll").NewProc("TaskDialogIndirect")
	procMessageBox         = user32.NewProc("MessageBoxW")
)

// askChoice 弹出对话框让用户从 buttons 里选一个（每个按钮可以用「\n」分出第二行说明），返回所选的序号，
// 关掉对话框时返回 -1。用系统的任务对话框（大按钮带说明）；用不了时（没有新式控件库）退回到「是/否/取消」。
func askChoice(title, main, content string, buttons []string) int {
	runtime.LockOSThread() // 对话框在这个线程上跑自己的消息循环
	defer runtime.UnlockOSThread()
	if procTaskDialogIndirect.Find() == nil {
		if i, ok := taskDialog(title, main, content, buttons); ok {
			return i
		}
	}
	text := main + "\n\n" + content + "\n"
	labels := []string{"是", "否", "取消"}
	for i, b := range buttons {
		if i < len(labels) {
			text += "\n" + labels[i] + "：" + strings.ReplaceAll(b, "\n", "（") + "）"
		}
	}
	flags := uintptr(0x3) | 0x20 // MB_YESNOCANCEL | MB_ICONQUESTION
	if len(buttons) == 2 {
		flags = 0x4 | 0x20 // MB_YESNO
	}
	r := messageBox(title, text, flags)
	switch r {
	case 6: // IDYES
		return 0
	case 7: // IDNO
		return 1
	case 2: // IDCANCEL
		if len(buttons) > 2 {
			return 2
		}
	}
	return -1
}

func messageBox(title, text string, flags uintptr) uintptr {
	t, _ := windows.UTF16PtrFromString(title)
	m, _ := windows.UTF16PtrFromString(text)
	r, _, _ := procMessageBox.Call(0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), flags|0x10000) // MB_SETFOREGROUND
	return r
}

// showMessage 弹出只有「确定」的提示。
func showMessage(title, text string) { messageBox(title, text, 0x40) } // MB_ICONINFORMATION

// taskDialog 用 TaskDialogIndirect 显示带说明的大按钮。TASKDIALOGCONFIG 是按 1 字节对齐的结构（commctrl.h
// 用 pshpack1），x64 上 160 字节，这里按偏移逐个写；按钮（TASKDIALOG_BUTTON）每个 12 字节。
func taskDialog(title, main, content string, buttons []string) (int, bool) {
	var keep [][]uint16 // 结构里只存地址，字符串要一直留着
	str := func(s string) uintptr {
		u, _ := windows.UTF16FromString(s)
		keep = append(keep, u)
		return uintptr(unsafe.Pointer(&u[0]))
	}
	btns := make([]byte, 12*len(buttons))
	for i, b := range buttons {
		*(*int32)(unsafe.Pointer(&btns[12*i])) = int32(100 + i)
		*(*uintptr)(unsafe.Pointer(&btns[12*i+4])) = str(b)
	}
	cfg := make([]byte, 160)
	put32 := func(off int, v uint32) { *(*uint32)(unsafe.Pointer(&cfg[off])) = v }
	putPtr := func(off int, v uintptr) { *(*uintptr)(unsafe.Pointer(&cfg[off])) = v }
	inst, _, _ := procGetModuleHandle.Call(0)
	put32(0, 160)
	putPtr(12, inst)
	put32(20, 0x10|0x08) // TDF_USE_COMMAND_LINKS | TDF_ALLOW_DIALOG_CANCELLATION
	putPtr(28, str(title))
	icon := uintptr(0xFFFD) // TD_INFORMATION_ICON
	if appIcon(32) != 0 {
		icon = appIconID // 程序自己的图标
	}
	putPtr(36, icon)
	putPtr(44, str(main))
	putPtr(52, str(content))
	put32(60, uint32(len(buttons)))
	putPtr(64, uintptr(unsafe.Pointer(&btns[0])))
	put32(72, 100)
	var pressed int32
	hr, _, _ := procTaskDialogIndirect.Call(uintptr(unsafe.Pointer(&cfg[0])), uintptr(unsafe.Pointer(&pressed)), 0, 0)
	runtime.KeepAlive(keep)
	runtime.KeepAlive(btns)
	if hr != 0 {
		return -1, false
	}
	if pressed >= 100 && int(pressed) < 100+len(buttons) {
		return int(pressed) - 100, true
	}
	return -1, true // 关掉了对话框（IDCANCEL）
}
