//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func shellExecute(verb, target string) error {
	v, _ := windows.UTF16PtrFromString(verb)
	t, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.ShellExecute(0, v, t, nil, nil, windows.SW_SHOWNORMAL)
}

// openFile 用默认程序打开文件。
func openFile(path string) error { return shellExecute("open", path) }

// revealFile 打开所在文件夹并选中文件。
func revealFile(path string) error {
	cmd := exec.Command("explorer.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `explorer.exe /select,"` + path + `"`}
	return cmd.Start()
}

// browserCandidates 是 Chromium 内核浏览器的常见位置，用于以「应用窗口」形式显示界面。
func browserCandidates() []string {
	var c []string
	for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles", "LOCALAPPDATA"} {
		base := os.Getenv(env)
		if base == "" {
			continue
		}
		c = append(c,
			filepath.Join(base, "Microsoft", "Edge", "Application", "msedge.exe"),
			filepath.Join(base, "Google", "Chrome", "Application", "chrome.exe"))
	}
	return c
}

// launchUI 以无地址栏的应用窗口打开界面；找不到 Edge/Chrome 时用默认浏览器。
func launchUI(url string) error {
	for _, b := range browserCandidates() {
		if _, err := os.Stat(b); err == nil {
			cmd := exec.Command(b, "--app="+url, "--window-size=1280,800", "--no-first-run")
			if err := cmd.Start(); err == nil {
				return nil
			}
		}
	}
	return shellExecute("open", url)
}

var (
	procEnumWindows         = user32.NewProc("EnumWindows")
	procGetWindowText       = user32.NewProc("GetWindowTextW")
	procGetClassName        = user32.NewProc("GetClassNameW")
	procIsWindowVisible     = user32.NewProc("IsWindowVisible")
	procIsIconic            = user32.NewProc("IsIconic")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procKeybdEvent          = user32.NewProc("keybd_event")
	procSetErrorMode        = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetErrorMode")

	enumMu    sync.Mutex
	enumFound []uintptr
	enumCB    = syscall.NewCallback(func(h, _ uintptr) uintptr {
		if isUIWindow(h) {
			enumFound = append(enumFound, h)
		}
		return 1 // 继续枚举
	})
)

const (
	wmSysCommand = 0x0112
	wmClose      = 0x0010
	scMinimize   = 0xF020
	scRestore    = 0xF120
	uiTitle      = "找找"
)

func windowString(proc *windows.LazyProc, h uintptr) string {
	buf := make([]uint16, 256)
	n, _, _ := proc.Call(h, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return windows.UTF16ToString(buf[:n])
}

// isUIWindow 判断是不是界面所在的浏览器应用窗口：Chromium 顶层窗口、标题为「找找」或「找找 v0.2.11」、可见或已最小化。
// 只按标题找会误中资源管理器里同名的文件夹窗口，或浏览器内部不可见的辅助窗口。
func isUIWindow(h uintptr) bool {
	if !strings.HasPrefix(windowString(procGetClassName, h), "Chrome_WidgetWin_") {
		return false
	}
	if t := windowString(procGetWindowText, h); t != uiTitle && !strings.HasPrefix(t, uiTitle+" v") { // 0.2.11 起标题带版本号
		return false
	}
	vis, _, _ := procIsWindowVisible.Call(h)
	icon, _, _ := procIsIconic.Call(h)
	return vis != 0 || icon != 0
}

func uiWindows() []uintptr {
	enumMu.Lock()
	defer enumMu.Unlock()
	enumFound = nil
	procEnumWindows.Call(enumCB, 0)
	return append([]uintptr(nil), enumFound...)
}

// bringToFront 还原并激活窗口。Windows 限制后台程序抢焦点，按一下 Alt 是公认的绕过办法。
func bringToFront(h uintptr) {
	if icon, _, _ := procIsIconic.Call(h); icon != 0 {
		procPostMessage.Call(h, wmSysCommand, scRestore, 0)
	}
	const vkMenu, keyUp = 0x12, 0x0002
	procKeybdEvent.Call(vkMenu, 0, 0, 0)
	procSetForegroundWindow.Call(h)
	procKeybdEvent.Call(vkMenu, 0, keyUp, 0)
}

// showOrLaunchUI 呼出已有的界面窗口，没有就新开一个。
func showOrLaunchUI(url string) error {
	if ws := uiWindows(); len(ws) > 0 {
		bringToFront(ws[0])
		return nil
	}
	return launchUI(url)
}

// hideUI 把界面窗口最小化。用窗口自己的「最小化」命令，而不是外部隐藏：
// Chromium 窗口被外部 ShowWindow 隐藏后常常停止绘制，再显示时一片空白。
func hideUI() bool {
	ws := uiWindows()
	for _, h := range ws {
		procPostMessage.Call(h, wmSysCommand, scMinimize, 0)
	}
	return len(ws) > 0
}

// closeUI 关闭界面窗口（退出时调用，避免留下连不上后台的空白窗口）。
func closeUI() {
	for _, h := range uiWindows() {
		procPostMessage.Call(h, wmClose, 0, 0)
	}
}

// suppressErrorDialogs 关闭系统错误弹窗。子进程会继承这个设置：组件缺少 DLL（例如 VC++ 运行库）时
// 立即以错误码退出，而不是弹出一个看不见的对话框一直卡住。
func suppressErrorDialogs() {
	const semFailCriticalErrors, semNoGPFaultErrorBox, semNoOpenFileErrorBox = 0x0001, 0x0002, 0x8000
	procSetErrorMode.Call(semFailCriticalErrors | semNoGPFaultErrorBox | semNoOpenFileErrorBox)
}
